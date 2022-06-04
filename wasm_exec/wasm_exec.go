// Package wasm_exec contains imports and state needed by wasm go compiles when
// GOOS=js and GOARCH=wasm.
//
// See /wasm_exec/REFERENCE.md for a deeper dive.
package wasm_exec

import (
	"context"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	internalsys "github.com/tetratelabs/wazero/internal/sys"
	"github.com/tetratelabs/wazero/internal/wasm"
	"github.com/tetratelabs/wazero/sys"
)

// Builder configures the "go" imports used by wasm_exec.js for later use via
// Compile or Instantiate.
type Builder interface {
	NewWasmExec(context.Context, wazero.CompiledModule, wazero.ModuleConfig) (WasmExec, error)
}

func NewWasmExec(
	ctx context.Context,
	r wazero.Runtime,
	compiled wazero.CompiledModule,
	mConfig wazero.ModuleConfig,
) (WasmExec, error) {
	return NewBuilder(r).NewWasmExec(ctx, compiled, mConfig)
}

// NewBuilder returns a new Builder.
func NewBuilder(r wazero.Runtime) Builder {
	return &builder{r: r}
}

type builder struct {
	r wazero.Runtime
	g *jsWasm
}

// moduleBuilder returns a new wazero.ModuleBuilder
func (b *builder) moduleBuilder() wazero.ModuleBuilder {
	g := &jsWasm{values: &values{ids: map[interface{}]uint32{}}}
	b.g = g
	return b.r.NewModuleBuilder("go").
		ExportFunction("runtime.wasmExit", g._wasmExit).
		ExportFunction("runtime.wasmWrite", g._wasmWrite).
		ExportFunction("runtime.resetMemoryDataView", g._resetMemoryDataView).
		ExportFunction("runtime.nanotime1", g._nanotime1).
		ExportFunction("runtime.walltime", g._walltime).
		ExportFunction("runtime.scheduleTimeoutEvent", g._scheduleTimeoutEvent).
		ExportFunction("runtime.clearTimeoutEvent", g._clearTimeoutEvent).
		ExportFunction("runtime.getRandomData", g._getRandomData).
		ExportFunction("syscall/js.finalizeRef", g._finalizeRef).
		ExportFunction("syscall/js.stringVal", g._stringVal).
		ExportFunction("syscall/js.valueGet", g._valueGet).
		ExportFunction("syscall/js.valueSet", g._valueSet).
		ExportFunction("syscall/js.valueDelete", g._valueDelete).
		ExportFunction("syscall/js.valueIndex", g._valueIndex).
		ExportFunction("syscall/js.valueSetIndex", g._valueSetIndex).
		ExportFunction("syscall/js.valueCall", g._valueCall).
		ExportFunction("syscall/js.valueInvoke", g._valueInvoke).
		ExportFunction("syscall/js.valueNew", g._valueNew).
		ExportFunction("syscall/js.valueLength", g._valueLength).
		ExportFunction("syscall/js.valuePrepareString", g._valuePrepareString).
		ExportFunction("syscall/js.valueLoadString", g._valueLoadString).
		ExportFunction("syscall/js.valueInstanceOf", g._valueInstanceOf).
		ExportFunction("syscall/js.copyBytesToGo", g._copyBytesToGo).
		ExportFunction("syscall/js.copyBytesToJS", g._copyBytesToJS).
		ExportFunction("debug", g.debug)
}

func (b *builder) NewWasmExec(ctx context.Context, compiled wazero.CompiledModule, mConfig wazero.ModuleConfig) (WasmExec, error) {
	ns := b.r.NewNamespace(ctx)

	if _, err := b.moduleBuilder().Instantiate(ctx, ns); err != nil {
		return nil, err
	}

	mod, err := ns.InstantiateModule(ctx, compiled, mConfig)
	if err != nil {
		return nil, err
	}
	return &wasmExec{ns: ns, mod: mod, g: b.g}, nil
}

// jsWasm holds defines the "go" imports used by wasm_exec.
//
// Note: This is module-scoped, so only safe when used in a wazero.Namespace
// that only instantiates one module.
type jsWasm struct {
	nextCallbackTimeoutID uint32
	scheduledTimeouts     map[uint32]*time.Timer
	values                *values
	closed                uint64
	_pendingEvent         *event
}

// debug has unknown use, so stubbed.
//
// See https://github.com/golang/go/blob/go1.19beta1/src/cmd/link/internal/wasm/asm.go#L133-L138
func (j *jsWasm) debug(ctx context.Context, mod api.Module, sp uint32) {
}

// _wasmExit converts the GOARCH=wasm stack to be compatible with api.ValueType
// in order to call wasmExit.
func (j *jsWasm) _wasmExit(ctx context.Context, mod api.Module, sp uint32) {
	code := requireReadUint32Le(ctx, mod.Memory(), "code", sp+8)
	j.wasmExit(ctx, mod, code)
}

// wasmExit implements runtime.wasmExit which supports runtime.exit.
//
// See https://github.com/golang/go/blob/go1.19beta1/src/runtime/sys_wasm.go#L28
func (j *jsWasm) wasmExit(ctx context.Context, mod api.Module, code uint32) {
	if j.closed != 0 {
		return
	}
	j.closed = uint64(1) + uint64(code)<<32 // Store exitCode as high-order bits.

	j.nextCallbackTimeoutID = 0
	for k := range j.scheduledTimeouts {
		delete(j.scheduledTimeouts, k)
	}
	j.values.values = j.values.values[:0]
	j.values.goRefCounts = j.values.goRefCounts[:0]
	for k := range j.values.ids {
		delete(j.values.ids, k)
	}
	j.values.idPool = j.values.idPool[:0]
	j._pendingEvent = nil

	//_ = mod.CloseWithExitCode(ctx, code)
}

// _wasmWrite converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call wasmWrite.
func (j *jsWasm) _wasmWrite(ctx context.Context, mod api.Module, sp uint32) {
	fd := requireReadUint64Le(ctx, mod.Memory(), "fd", sp+8)
	p := requireReadUint64Le(ctx, mod.Memory(), "p", sp+16)
	n := requireReadUint32Le(ctx, mod.Memory(), "n", sp+24)
	j.wasmWrite(ctx, mod, fd, p, n)
}

// wasmWrite implements runtime.wasmWrite which supports runtime.write and
// runtime.writeErr. It is only known to be used with fd = 2 (stderr).
//
// See https://github.com/golang/go/blob/go1.19beta1/src/runtime/os_js.go#L29
func (j *jsWasm) wasmWrite(ctx context.Context, mod api.Module, fd, p uint64, n uint32) {
	var writer io.Writer

	switch fd {
	case 1:
		writer = getSysCtx(mod).Stdout()
	case 2:
		writer = getSysCtx(mod).Stderr()
	default:
		// Keep things simple by expecting nothing past 2
		panic(fmt.Errorf("unexpected fd %d", fd))
	}

	if _, err := writer.Write(requireRead(ctx, mod.Memory(), "p", uint32(p), n)); err != nil {
		panic(fmt.Errorf("error writing p: %w", err))
	}
}

// _resetMemoryDataView converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call resetMemoryDataView.
func (j *jsWasm) _resetMemoryDataView(ctx context.Context, mod api.Module, sp uint32) {
	j.resetMemoryDataView(ctx, mod)
}

// resetMemoryDataView signals wasm.OpcodeMemoryGrow happened, indicating any
// cached view of memory should be reset.
//
// See https://github.com/golang/go/blob/9839668b5619f45e293dd40339bf0ac614ea6bee/src/runtime/mem_js.go#L82
func (j *jsWasm) resetMemoryDataView(ctx context.Context, mod api.Module) {
	// TODO: Compiler-based memory.grow callbacks are ignored until we have a generic solution #601
}

// _nanotime1 converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call nanotime1.
func (j *jsWasm) _nanotime1(ctx context.Context, mod api.Module, sp uint32) {
	nanos := j.nanotime1(ctx, mod)
	requireWriteUint64Le(ctx, mod.Memory(), "t", sp+8, nanos)
}

// nanotime1 implements runtime.nanotime which supports time.Since.
//
// See https://github.com/golang/go/blob/go1.19beta1/src/runtime/sys_wasm.s#L184
func (j *jsWasm) nanotime1(ctx context.Context, mod api.Module) uint64 {
	return uint64(getSysCtx(mod).Nanotime(ctx))
}

// _walltime converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call walltime.
func (j *jsWasm) _walltime(ctx context.Context, mod api.Module, sp uint32) {
	sec, nsec := j.walltime(ctx, mod)
	requireWriteUint64Le(ctx, mod.Memory(), "sec", sp+8, uint64(sec))
	requireWriteUint32Le(ctx, mod.Memory(), "nsec", sp+16, uint32(nsec))
}

// walltime implements runtime.walltime which supports time.Now.
//
// See https://github.com/golang/go/blob/go1.19beta1/src/runtime/sys_wasm.s#L188
func (j *jsWasm) walltime(ctx context.Context, mod api.Module) (uint64 int64, uint32 int32) {
	return getSysCtx(mod).Walltime(ctx)
}

// _scheduleTimeoutEvent converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call scheduleTimeoutEvent.
func (j *jsWasm) _scheduleTimeoutEvent(ctx context.Context, mod api.Module, sp uint32) {
	delayMs := requireReadUint64Le(ctx, mod.Memory(), "delay", sp+8)
	id := j.scheduleTimeoutEvent(ctx, mod, delayMs)
	requireWriteUint32Le(ctx, mod.Memory(), "id", sp+16, id)
}

// scheduleTimeoutEvent implements runtime.scheduleTimeoutEvent which supports
// runtime.notetsleepg used by runtime.signal_recv.
//
// Unlike other most functions prefixed by "runtime.", this both launches a
// goroutine and invokes code compiled into wasm "resume".
//
// See https://github.com/golang/go/blob/go1.19beta1/src/runtime/sys_wasm.s#L192
func (j *jsWasm) scheduleTimeoutEvent(ctx context.Context, mod api.Module, delayMs uint64) uint32 {
	delay := time.Duration(delayMs) * time.Millisecond

	resume := mod.ExportedFunction("resume")

	// Invoke resume as an anonymous function, to propagate the context.
	callResume := func() {
		if err := j.failIfClosed(mod); err != nil {
			return
		}
		// While there's a possible error here, panicking won't help as it is
		// on a different goroutine.
		_, _ = resume.Call(ctx)
	}

	return j.scheduleEvent(delay, callResume)
}

// scheduleEvent schedules an event onto another goroutine after d duration and
// returns a handle to remove it (removeEvent).
func (j *jsWasm) scheduleEvent(d time.Duration, f func()) uint32 {
	id := j.nextCallbackTimeoutID
	j.nextCallbackTimeoutID++
	// TODO: this breaks the sandbox (proc.checkTimers is shared), so should
	// be substitutable with a different impl.
	j.scheduledTimeouts[id] = time.AfterFunc(d, f)
	return id
}

// _clearTimeoutEvent converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call clearTimeoutEvent.
func (j *jsWasm) _clearTimeoutEvent(ctx context.Context, mod api.Module, sp uint32) {
	id := requireReadUint32Le(ctx, mod.Memory(), "id", sp+8)
	j.clearTimeoutEvent(id)
}

// clearTimeoutEvent implements runtime.clearTimeoutEvent which supports
// runtime.notetsleepg used by runtime.signal_recv.
//
// See https://github.com/golang/go/blob/go1.19beta1/src/runtime/sys_wasm.s#L196
func (j *jsWasm) clearTimeoutEvent(id uint32) {
	if t := j.removeEvent(id); t != nil {
		if !t.Stop() {
			<-t.C
		}
	}
}

// removeEvent removes an event previously scheduled with scheduleEvent or
// returns nil, if it was already removed.
func (j *jsWasm) removeEvent(id uint32) *time.Timer {
	t, ok := j.scheduledTimeouts[id]
	if ok {
		delete(j.scheduledTimeouts, id)
		return t
	}
	return nil
}

// _getRandomData converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call getRandomData.
func (j *jsWasm) _getRandomData(ctx context.Context, mod api.Module, sp uint32) {
	buf := uint32(requireReadUint64Le(ctx, mod.Memory(), "buf", sp+8))
	bufLen := uint32(requireReadUint64Le(ctx, mod.Memory(), "bufLen", sp+16))

	j.getRandomData(ctx, mod, buf, bufLen)
}

// getRandomData implements runtime.getRandomData, which initializes the seed
// for runtime.fastrand.
//
// See https://github.com/golang/go/blob/go1.19beta1/src/runtime/sys_wasm.s#L200
func (j *jsWasm) getRandomData(ctx context.Context, mod api.Module, buf, bufLen uint32) {
	randSource := getSysCtx(mod).RandSource()

	r := requireRead(ctx, mod.Memory(), "r", buf, bufLen)

	if n, err := randSource.Read(r); err != nil {
		panic(fmt.Errorf("RandSource.Read(r /* len =%d */) failed: %w", bufLen, err))
	} else if uint32(n) != bufLen {
		panic(fmt.Errorf("RandSource.Read(r /* len=%d */) read %d bytes", bufLen, n))
	}
}

// _finalizeRef converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call finalizeRef.
func (j *jsWasm) _finalizeRef(ctx context.Context, mod api.Module, sp uint32) {
	// 32-bits are the ID
	id := requireReadUint32Le(ctx, mod.Memory(), "r", sp+8)
	j.finalizeRef(ctx, mod, id)
}

// finalizeRef implements js.finalizeRef, which is used as a
// runtime.SetFinalizer on the given reference.
//
// See https://github.com/golang/go/blob/go1.19beta1/src/syscall/js/js.go#L61
func (j *jsWasm) finalizeRef(ctx context.Context, mod api.Module, id uint32) {
	j.values.decrement(id)
}

// _stringVal converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call stringVal.
func (j *jsWasm) _stringVal(ctx context.Context, mod api.Module, sp uint32) {
	xAddr := requireReadUint64Le(ctx, mod.Memory(), "xAddr", sp+8)
	xLen := requireReadUint64Le(ctx, mod.Memory(), "xLen", sp+16)
	xRef := j.stringVal(ctx, mod, xAddr, xLen)
	requireWriteUint64Le(ctx, mod.Memory(), "xRef", sp+24, xRef)
}

// stringVal implements js.stringVal, which is used to load the string for
// `js.ValueOf(x)`.
//
// See https://github.com/golang/go/blob/go1.19beta1/src/syscall/js/js.go#L212
//     https://github.com/golang/go/blob/go1.19beta1/misc/wasm/wasm_exec.js#L305-L308
func (j *jsWasm) stringVal(ctx context.Context, mod api.Module, xAddr, xLen uint64) uint64 {
	x := string(requireRead(ctx, mod.Memory(), "x", uint32(xAddr), uint32(xLen)))
	return j.storeRef(ctx, mod, x)
}

// _valueGet converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call valueGet.
func (j *jsWasm) _valueGet(ctx context.Context, mod api.Module, sp uint32) {
	vRef := requireReadUint64Le(ctx, mod.Memory(), "vRef", sp+8)
	pAddr := requireReadUint64Le(ctx, mod.Memory(), "pAddr", sp+16)
	pLen := requireReadUint64Le(ctx, mod.Memory(), "pLen", sp+24)
	xRef := j.valueGet(ctx, mod, vRef, pAddr, pLen)
	sp = j.refreshSP(ctx, mod)
	requireWriteUint64Le(ctx, mod.Memory(), "xRef", sp+32, xRef)
}

// valueGet implements js.valueGet, which is used to load a js.Value property
// by name, ex. `v.Get("address")`.
//
// See https://github.com/golang/go/blob/go1.19beta1/src/syscall/js/js.go#L295
//     https://github.com/golang/go/blob/go1.19beta1/misc/wasm/wasm_exec.js#L311-L316
func (j *jsWasm) valueGet(ctx context.Context, mod api.Module, vRef, pAddr, pLen uint64) uint64 {
	v := j.loadValue(ctx, mod, ref(vRef))
	p := requireRead(ctx, mod.Memory(), "p", uint32(pAddr), uint32(pLen))
	result := j.reflectGet(v, string(p))
	return j.storeRef(ctx, mod, result)
}

// _valueSet converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call valueSet.
func (j *jsWasm) _valueSet(ctx context.Context, mod api.Module, sp uint32) {
	vRef := requireReadUint64Le(ctx, mod.Memory(), "vRef", sp+8)
	pAddr := requireReadUint64Le(ctx, mod.Memory(), "pAddr", sp+16)
	pLen := requireReadUint64Le(ctx, mod.Memory(), "pLen", sp+24)
	xRef := requireReadUint64Le(ctx, mod.Memory(), "xRef", sp+32)
	j.valueSet(ctx, mod, vRef, pAddr, pLen, xRef)
}

// valueSet implements js.valueSet, which is used to store a js.Value property
// by name, ex. `v.Set("address", a)`.
//
// See https://github.com/golang/go/blob/go1.19beta1/src/syscall/js/js.go#L309
//     https://github.com/golang/go/blob/go1.19beta1/misc/wasm/wasm_exec.js#L318-L322
func (j *jsWasm) valueSet(ctx context.Context, mod api.Module, vRef, pAddr, pLen, xRef uint64) {
	v := j.loadValue(ctx, mod, ref(vRef))
	p := requireRead(ctx, mod.Memory(), "p", uint32(pAddr), uint32(pLen))
	x := j.loadValue(ctx, mod, ref(xRef))
	j.reflectSet(v, string(p), x)
}

// _valueDelete converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call valueDelete.
func (j *jsWasm) _valueDelete(ctx context.Context, mod api.Module, sp uint32) {
	vRef := requireReadUint64Le(ctx, mod.Memory(), "vRef", sp+8)
	pAddr := requireReadUint64Le(ctx, mod.Memory(), "pAddr", sp+16)
	pLen := requireReadUint64Le(ctx, mod.Memory(), "pLen", sp+24)
	j.valueDelete(ctx, mod, vRef, pAddr, pLen)
}

// valueDelete implements js.valueDelete, which is used to delete a js.Value property
// by name, ex. `v.Delete("address")`.
//
// See https://github.com/golang/go/blob/go1.19beta1/src/syscall/js/js.go#L321
//     https://github.com/golang/go/blob/go1.19beta1/misc/wasm/wasm_exec.js#L325-L328
func (j *jsWasm) valueDelete(ctx context.Context, mod api.Module, vRef, pAddr, pLen uint64) {
	v := j.loadValue(ctx, mod, ref(vRef))
	p := requireRead(ctx, mod.Memory(), "p", uint32(pAddr), uint32(pLen))
	j.reflectDeleteProperty(v, string(p))
}

// _valueIndex converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call valueIndex.
func (j *jsWasm) _valueIndex(ctx context.Context, mod api.Module, sp uint32) {
	vRef := requireReadUint64Le(ctx, mod.Memory(), "vRef", sp+8)
	i := requireReadUint64Le(ctx, mod.Memory(), "i", sp+16)
	xRef := j.valueIndex(ctx, mod, vRef, i)
	sp = j.refreshSP(ctx, mod)
	requireWriteUint64Le(ctx, mod.Memory(), "xRef", sp+24, xRef)
}

// valueIndex implements js.valueIndex, which is used to load a js.Value property
// by name, ex. `v.Index(0)`.
//
// See https://github.com/golang/go/blob/go1.19beta1/src/syscall/js/js.go#L334
//     https://github.com/golang/go/blob/go1.19beta1/misc/wasm/wasm_exec.js#L331-L334
func (j *jsWasm) valueIndex(ctx context.Context, mod api.Module, vRef, i uint64) uint64 {
	v := j.loadValue(ctx, mod, ref(vRef))
	result := j.reflectGetIndex(v, uint32(i))
	return j.storeRef(ctx, mod, result)
}

// _valueSetIndex converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call valueSetIndex.
func (j *jsWasm) _valueSetIndex(ctx context.Context, mod api.Module, sp uint32) {
	vRef := requireReadUint64Le(ctx, mod.Memory(), "vRef", sp+8)
	i := requireReadUint64Le(ctx, mod.Memory(), "i", sp+16)
	xRef := requireReadUint64Le(ctx, mod.Memory(), "xRef", sp+24)
	j.valueSetIndex(ctx, mod, vRef, i, xRef)
}

// valueSetIndex implements js.valueSetIndex, which is used to store a js.Value property
// by name, ex. `v.SetIndex(0, a)`.
//
// See https://github.com/golang/go/blob/go1.19beta1/src/syscall/js/js.go#L348
//     https://github.com/golang/go/blob/go1.19beta1/misc/wasm/wasm_exec.js#L337-L340
func (j *jsWasm) valueSetIndex(ctx context.Context, mod api.Module, vRef, i, xRef uint64) {
	v := j.loadValue(ctx, mod, ref(vRef))
	x := j.loadValue(ctx, mod, ref(xRef))
	j.reflectSetIndex(v, uint32(i), x)
}

// _valueCall converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call valueCall.
func (j *jsWasm) _valueCall(ctx context.Context, mod api.Module, sp uint32) {
	vRef := requireReadUint64Le(ctx, mod.Memory(), "vRef", sp+8)
	mAddr := requireReadUint64Le(ctx, mod.Memory(), "mAddr", sp+16)
	mLen := requireReadUint64Le(ctx, mod.Memory(), "mLen", sp+24)
	argsArray := requireReadUint64Le(ctx, mod.Memory(), "argsArray", sp+32)
	argsLen := requireReadUint64Le(ctx, mod.Memory(), "argsLen", sp+40)
	xRef, ok := j.valueCall(ctx, mod, vRef, mAddr, mLen, argsArray, argsLen)
	sp = j.refreshSP(ctx, mod)
	requireWriteUint64Le(ctx, mod.Memory(), "xRef", sp+56, xRef)
	requireWriteByte(ctx, mod.Memory(), "ok", sp+64, byte(ok))
}

// valueCall implements js.valueCall, which is used to call a js.Value function
// by name, ex. `document.Call("createElement", "div")`.
//
// See https://github.com/golang/go/blob/go1.19beta1/src/syscall/js/js.go#L394
//     https://github.com/golang/go/blob/go1.19beta1/misc/wasm/wasm_exec.js#L343-L358
func (j *jsWasm) valueCall(ctx context.Context, mod api.Module, vRef, mAddr, mLen, argsArray, argsLen uint64) (uint64, uint32) {
	v := j.loadValue(ctx, mod, ref(vRef))
	propertyKey := string(requireRead(ctx, mod.Memory(), "property", uint32(mAddr), uint32(mLen)))
	args := j.loadSliceOfValues(ctx, mod, uint32(argsArray), uint32(argsLen))
	if result, err := j.reflectApply(ctx, mod, v, propertyKey, args); err != nil {
		return j.storeRef(ctx, mod, err), 0
	} else {
		return j.storeRef(ctx, mod, result), 1
	}
}

// _valueInvoke converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call valueInvoke.
func (j *jsWasm) _valueInvoke(ctx context.Context, mod api.Module, sp uint32) {
	vRef := requireReadUint64Le(ctx, mod.Memory(), "vRef", sp+8)
	argsArray := requireReadUint64Le(ctx, mod.Memory(), "argsArray", sp+16)
	argsLen := requireReadUint64Le(ctx, mod.Memory(), "argsLen", sp+24)
	xRef, ok := j.valueInvoke(ctx, mod, vRef, argsArray, argsLen)
	sp = j.refreshSP(ctx, mod)
	requireWriteUint64Le(ctx, mod.Memory(), "xRef", sp+40, xRef)
	requireWriteByte(ctx, mod.Memory(), "ok", sp+48, byte(ok))
}

// valueInvoke implements js.valueInvoke, which is used to call a js.Value, ex.
// `add.Invoke(1, 2)`.
//
// See https://github.com/golang/go/blob/go1.19beta1/src/syscall/js/js.go#L413
//     https://github.com/golang/go/blob/go1.19beta1/misc/wasm/wasm_exec.js#L361-L375
func (j *jsWasm) valueInvoke(ctx context.Context, mod api.Module, vRef, argsArray, argsLen uint64) (uint64, uint32) {
	v := j.loadValue(ctx, mod, ref(vRef))
	args := j.loadSliceOfValues(ctx, mod, uint32(argsArray), uint32(argsLen))
	if result, err := j.reflectApply(ctx, mod, v, nil, args); err != nil {
		return j.storeRef(ctx, mod, err), 0
	} else {
		return j.storeRef(ctx, mod, result), 1
	}
}

// _valueNew converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call valueNew.
func (j *jsWasm) _valueNew(ctx context.Context, mod api.Module, sp uint32) {
	vRef := requireReadUint64Le(ctx, mod.Memory(), "vRef", sp+8)
	argsArray := requireReadUint64Le(ctx, mod.Memory(), "argsArray", sp+16)
	argsLen := requireReadUint64Le(ctx, mod.Memory(), "argsLen", sp+24)
	xRef, ok := j.valueNew(ctx, mod, vRef, argsArray, argsLen)
	sp = j.refreshSP(ctx, mod)
	requireWriteUint64Le(ctx, mod.Memory(), "xRef", sp+40, xRef)
	requireWriteByte(ctx, mod.Memory(), "ok", sp+48, byte(ok))
}

// valueNew implements js.valueNew, which is used to call a js.Value, ex.
// `array.New(2)`.
//
// See https://github.com/golang/go/blob/go1.19beta1/src/syscall/js/js.go#L432
//     https://github.com/golang/go/blob/go1.19beta1/misc/wasm/wasm_exec.js#L380-L391
func (j *jsWasm) valueNew(ctx context.Context, mod api.Module, vRef, argsArray, argsLen uint64) (uint64, uint32) {
	v := j.loadValue(ctx, mod, ref(vRef))
	args := j.loadSliceOfValues(ctx, mod, uint32(argsArray), uint32(argsLen))
	if result, err := j.reflectConstruct(v, args); err != nil {
		return j.storeRef(ctx, mod, err), 0
	} else {
		return j.storeRef(ctx, mod, result), 1
	}
}

// _valueLength converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call valueLength.
func (j *jsWasm) _valueLength(ctx context.Context, mod api.Module, sp uint32) {
	vRef := requireReadUint64Le(ctx, mod.Memory(), "vRef", sp+8)
	length := j.valueLength(ctx, mod, vRef)
	requireWriteUint64Le(ctx, mod.Memory(), "length", sp+16, length)
}

// valueLength implements js.valueLength, which is used to load the length
// property of a value, ex. `array.length`.
//
// See https://github.com/golang/go/blob/go1.19beta1/src/syscall/js/js.go#L372
//     https://github.com/golang/go/blob/go1.19beta1/misc/wasm/wasm_exec.js#L396-L397
func (j *jsWasm) valueLength(ctx context.Context, mod api.Module, vRef uint64) uint64 {
	v := j.loadValue(ctx, mod, ref(vRef))
	return uint64(len(toSlice(v)))
}

func toSlice(v interface{}) []interface{} {
	return (*(v.(*interface{}))).([]interface{})
}

func maybeBuf(v interface{}) ([]byte, bool) {
	if p, ok := v.(*interface{}); ok {
		if b, ok := (*(p)).([]byte); ok {
			return b, true
		}
	}
	return nil, false
}

// _valuePrepareString converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call valuePrepareString.
func (j *jsWasm) _valuePrepareString(ctx context.Context, mod api.Module, sp uint32) {
	vRef := requireReadUint64Le(ctx, mod.Memory(), "vRef", sp+8)
	sAddr, sLen := j.valuePrepareString(ctx, mod, vRef)
	requireWriteUint64Le(ctx, mod.Memory(), "sAddr", sp+16, sAddr)
	requireWriteUint64Le(ctx, mod.Memory(), "sLen", sp+24, sLen)
}

// valuePrepareString implements js.valuePrepareString, which is used to load
// the string for `obj.String()` (via js.jsString) for string, boolean and
// number types.
//
// See https://github.com/golang/go/blob/go1.19beta1/src/syscall/js/js.go#L531
//     https://github.com/golang/go/blob/go1.19beta1/misc/wasm/wasm_exec.js#L402-L405
func (j *jsWasm) valuePrepareString(ctx context.Context, mod api.Module, vRef uint64) (uint64, uint64) {
	v := j.loadValue(ctx, mod, ref(vRef))
	s := j.valueString(v)
	return j.storeRef(ctx, mod, s), uint64(len(s))
}

// _valueLoadString converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call valueLoadString.
func (j *jsWasm) _valueLoadString(ctx context.Context, mod api.Module, sp uint32) {
	vRef := requireReadUint64Le(ctx, mod.Memory(), "vRef", sp+8)
	bAddr := requireReadUint64Le(ctx, mod.Memory(), "bAddr", sp+16)
	bLen := requireReadUint64Le(ctx, mod.Memory(), "bLen", sp+24)
	j.valueLoadString(ctx, mod, vRef, bAddr, bLen)
}

// valueLoadString implements js.valueLoadString, which is used copy a string
// value for `obj.String()`.
//
// See https://github.com/golang/go/blob/go1.19beta1/src/syscall/js/js.go#L533
//     https://github.com/golang/go/blob/go1.19beta1/misc/wasm/wasm_exec.js#L410-L412
func (j *jsWasm) valueLoadString(ctx context.Context, mod api.Module, vRef, bAddr, bLen uint64) {
	v := j.loadValue(ctx, mod, ref(vRef))
	s := j.valueString(v)
	b := requireRead(ctx, mod.Memory(), "b", uint32(bAddr), uint32(bLen))
	copy(b, s)
}

// _valueInstanceOf converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call valueInstanceOf.
func (j *jsWasm) _valueInstanceOf(ctx context.Context, mod api.Module, sp uint32) {
	vRef := requireReadUint64Le(ctx, mod.Memory(), "vRef", sp+8)
	tRef := requireReadUint64Le(ctx, mod.Memory(), "tRef", sp+16)
	r := j.valueInstanceOf(ctx, mod, vRef, tRef)
	requireWriteByte(ctx, mod.Memory(), "r", sp+24, byte(r))
}

// valueInstanceOf implements js.valueInstanceOf. ex. `array instanceof String`.
//
// See https://github.com/golang/go/blob/go1.19beta1/src/syscall/js/js.go#L543
//     https://github.com/golang/go/blob/go1.19beta1/misc/wasm/wasm_exec.js#L417-L418
func (j *jsWasm) valueInstanceOf(ctx context.Context, mod api.Module, vRef, tRef uint64) uint32 {
	v := j.loadValue(ctx, mod, ref(vRef))
	t := j.loadValue(ctx, mod, ref(tRef))
	if j.instanceOf(v, t) {
		return 0
	}
	return 1
}

// _copyBytesToGo converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call copyBytesToGo.
func (j *jsWasm) _copyBytesToGo(ctx context.Context, mod api.Module, sp uint32) {
	dstAddr := requireReadUint64Le(ctx, mod.Memory(), "dstAddr", sp+8)
	dstLen := requireReadUint64Le(ctx, mod.Memory(), "dstLen", sp+16)
	srcRef := requireReadUint64Le(ctx, mod.Memory(), "srcRef", sp+32)
	n, ok := j.copyBytesToGo(ctx, mod, dstAddr, dstLen, srcRef)
	requireWriteUint64Le(ctx, mod.Memory(), "n", sp+40, n)
	requireWriteByte(ctx, mod.Memory(), "ok", sp+48, byte(ok))
}

// copyBytesToGo implements js.copyBytesToGo.
//
// Results
//
//	* n is the count of bytes written.
//	* ok is false if the src was not a uint8Array.
//
// See https://github.com/golang/go/blob/go1.19beta1/src/syscall/js/js.go#L569
//     https://github.com/golang/go/blob/go1.19beta1/misc/wasm/wasm_exec.js#L424-L433
func (j *jsWasm) copyBytesToGo(ctx context.Context, mod api.Module, dstAddr, dstLen, srcRef uint64) (n uint64, ok uint32) {
	dst := requireRead(ctx, mod.Memory(), "dst", uint32(dstAddr), uint32(dstLen)) // nolint
	v := j.loadValue(ctx, mod, ref(srcRef))
	if src, ok := maybeBuf(v); ok {
		return uint64(copy(dst, src)), 1
	}
	return 0, 0
}

// _copyBytesToJS converts the GOARCH=wasm stack to be compatible with
// api.ValueType in order to call copyBytesToJS.
func (j *jsWasm) _copyBytesToJS(ctx context.Context, mod api.Module, sp uint32) {
	dstRef := requireReadUint64Le(ctx, mod.Memory(), "dstRef", sp+8)
	srcAddr := requireReadUint64Le(ctx, mod.Memory(), "srcAddr", sp+16)
	srcLen := requireReadUint64Le(ctx, mod.Memory(), "srcLen", sp+24)
	n, ok := j.copyBytesToJS(ctx, mod, dstRef, srcAddr, srcLen)
	requireWriteUint64Le(ctx, mod.Memory(), "n", sp+40, n)
	requireWriteByte(ctx, mod.Memory(), "ok", sp+48, byte(ok))
}

// copyBytesToJS implements js.copyBytesToJS.
//
// Results
//
//	* n is the count of bytes written.
//	* ok is false if the dst was not a uint8Array.
//
// See https://github.com/golang/go/blob/go1.19beta1/src/syscall/js/js.go#L583
//     https://github.com/golang/go/blob/go1.19beta1/misc/wasm/wasm_exec.js#L438-L448
func (j *jsWasm) copyBytesToJS(ctx context.Context, mod api.Module, dstRef, srcAddr, srcLen uint64) (n uint64, ok uint32) {
	src := requireRead(ctx, mod.Memory(), "src", uint32(srcAddr), uint32(srcLen)) // nolint
	v := j.loadValue(ctx, mod, ref(dstRef))
	if dst, ok := maybeBuf(v); ok {
		return uint64(copy(dst, src)), 1
	}
	return 0, 0
}

// reflectGet implements JavaScript's Reflect.get API.
func (j *jsWasm) reflectGet(target interface{}, propertyKey string) interface{} { // nolint
	if target == valueGlobal {
		switch propertyKey {
		case "Object":
			return objectConstructor
		case "Array":
			return arrayConstructor
		case "process":
			return jsProcess
		case "fs":
			return jsFS
		case "Uint8Array":
			return uint8ArrayConstructor
		}
	} else if target == j {
		switch propertyKey {
		case "_pendingEvent":
			return j._pendingEvent
		}
	} else if target == jsFS {
		switch propertyKey {
		case "constants":
			return jsFSConstants
		}
	} else if target == io.EOF {
		switch propertyKey {
		case "code":
			return "EOF"
		}
	} else if s, ok := target.(*jsSt); ok {
		switch propertyKey {
		case "dev":
			return s.dev
		case "ino":
			return s.ino
		case "mode":
			return s.mode
		case "nlink":
			return s.nlink
		case "uid":
			return s.uid
		case "gid":
			return s.gid
		case "rdev":
			return s.rdev
		case "size":
			return s.size
		case "blksize":
			return s.blksize
		case "blocks":
			return s.blocks
		case "atimeMs":
			return s.atimeMs
		case "mtimeMs":
			return s.mtimeMs
		case "ctimeMs":
			return s.ctimeMs
		}
	} else if target == jsFSConstants {
		switch propertyKey {
		case "O_WRONLY":
			return oWRONLY
		case "O_RDWR":
			return oRDWR
		case "O_CREAT":
			return oCREAT
		case "O_TRUNC":
			return oTRUNC
		case "O_APPEND":
			return oAPPEND
		case "O_EXCL":
			return oEXCL
		}
	} else if e, ok := target.(*event); ok { // syscall_js.handleEvent
		switch propertyKey {
		case "id":
			return e.id
		case "this": // ex fs
			return e.this
		case "args":
			return e.args
		}
	}
	panic(fmt.Errorf("TODO: reflectGet(target=%v, propertyKey=%s)", target, propertyKey))
}

// reflectGet implements JavaScript's Reflect.get API for an index.
func (j *jsWasm) reflectGetIndex(target interface{}, i uint32) interface{} { // nolint
	return toSlice(target)[i]
}

// reflectSet implements JavaScript's Reflect.set API.
func (j *jsWasm) reflectSet(target interface{}, propertyKey string, value interface{}) { // nolint
	if target == j {
		switch propertyKey {
		case "_pendingEvent":
			if value == nil { // syscall_js.handleEvent
				j._pendingEvent = nil
				return
			}
		}
	} else if e, ok := target.(*event); ok { // syscall_js.handleEvent
		switch propertyKey {
		case "result":
			e.result = value
			return
		}
	}
	panic(fmt.Errorf("TODO: reflectSet(target=%v, propertyKey=%s, value=%v)", target, propertyKey, value))
}

// reflectSetIndex implements JavaScript's Reflect.set API for an index.
func (j *jsWasm) reflectSetIndex(target interface{}, i uint32, value interface{}) { // nolint
	panic(fmt.Errorf("TODO: reflectSetIndex(target=%v, i=%d, value=%v)", target, i, value))
}

// reflectDeleteProperty implements JavaScript's Reflect.deleteProperty API
func (j *jsWasm) reflectDeleteProperty(target interface{}, propertyKey string) { // nolint
	panic(fmt.Errorf("TODO: reflectDeleteProperty(target=%v, propertyKey=%s)", target, propertyKey))
}

// reflectApply implements JavaScript's Reflect.apply API
func (j *jsWasm) reflectApply(
	ctx context.Context,
	mod api.Module,
	target interface{},
	propertyKey interface{},
	argumentsList []interface{},
) (interface{}, error) { // nolint
	if target == j {
		switch propertyKey {
		case "_makeFuncWrapper":
			return &funcWrapper{j: j, id: uint32(argumentsList[0].(float64))}, nil
		}
	} else if target == jsFS { // fs_js.go js.fsCall
		// * funcWrapper callback is the last parameter
		//   * arg0 is error and up to one result in arg1
		switch propertyKey {
		case "open":
			// jsFD, err := fsCall("open", name, flags, perm)
			name := argumentsList[0].(string)
			flags := toUint32(argumentsList[1]) // flags are derived from constants like oWRONLY
			perm := toUint32(argumentsList[2])
			result := argumentsList[3].(*funcWrapper)

			fd, err := j.syscallOpen(ctx, mod, name, flags, perm)
			result.call(ctx, mod, jsFS, err, fd) // note: error first

			return nil, nil
		case "fstat":
			// if stat, err := fsCall("fstat", fd); err == nil && stat.Call("isDirectory").Bool()
			fd := toUint32(argumentsList[0])
			result := argumentsList[1].(*funcWrapper)

			stat, err := j.syscallFstat(ctx, mod, fd)
			result.call(ctx, mod, jsFS, err, stat) // note: error first

			return nil, nil
		case "close":
			// if stat, err := fsCall("fstat", fd); err == nil && stat.Call("isDirectory").Bool()
			fd := toUint32(argumentsList[0])
			result := argumentsList[1].(*funcWrapper)

			err := j.syscallClose(ctx, mod, fd)
			result.call(ctx, mod, jsFS, err, true) // note: error first

			return nil, nil
		case "read": // syscall.Read, called by src/internal/poll/fd_unix.go poll.Read.
			// n, err := fsCall("read", fd, buf, 0, len(b), nil)
			fd := toUint32(argumentsList[0])
			buf, ok := maybeBuf(argumentsList[1])
			if !ok {
				return nil, fmt.Errorf("arg[1] is %v not a []byte", argumentsList[1])
			}
			offset := toUint32(argumentsList[2])
			byteCount := toUint32(argumentsList[3])
			_ /* unknown */ = argumentsList[4]
			result := argumentsList[5].(*funcWrapper)

			n, err := j.syscallRead(ctx, mod, fd, buf[offset:offset+byteCount])
			result.call(ctx, mod, jsFS, err, n) // note: error first

			return nil, nil
		case "write":
			// n, err := fsCall("write", fd, buf, 0, len(b), nil)
			fd := toUint32(argumentsList[0])
			buf, ok := maybeBuf(argumentsList[1])
			if !ok {
				return nil, fmt.Errorf("arg[1] is %v not a []byte", argumentsList[1])
			}
			offset := toUint32(argumentsList[2])
			byteCount := toUint32(argumentsList[3])
			_ /* unknown */ = argumentsList[4]
			result := argumentsList[5].(*funcWrapper)

			n, err := j.syscallWrite(ctx, mod, fd, buf[offset:offset+byteCount])
			result.call(ctx, mod, jsFS, err, n) // note: error first

			return nil, nil
		}
	} else if target == jsProcess {
		switch propertyKey {
		case "cwd":
			// cwd := jsProcess.Call("cwd").String()
			// TODO
		}
	} else if stat, ok := target.(*jsSt); ok {
		switch propertyKey {
		case "isDirectory":
			return stat.isDir, nil
		}
	}
	panic(fmt.Errorf("TODO: reflectApply(target=%v, propertyKey=%v, argumentsList=%v)", target, propertyKey, argumentsList))
}

func toUint32(arg interface{}) uint32 {
	if arg == refValueZero {
		return 0
	} else if u, ok := arg.(uint32); ok {
		return u
	}
	return uint32(arg.(float64))
}

// syscallFstat is like syscall.Fstat
func (j *jsWasm) syscallFstat(ctx context.Context, mod api.Module, fd uint32) (*jsSt, error) {
	fsc := getSysCtx(mod).FS(ctx)
	if f, ok := fsc.OpenedFile(ctx, fd); !ok {
		return nil, errorBadFD(fd)
	} else if stat, err := f.File.Stat(); err != nil {
		return nil, err
	} else {
		return &jsSt{
			isDir:   stat.IsDir(),
			dev:     0, // TODO stat.Sys
			ino:     0,
			mode:    uint32(stat.Mode()),
			nlink:   0,
			uid:     0,
			gid:     0,
			rdev:    0,
			size:    uint32(stat.Size()),
			blksize: 0,
			blocks:  0,
			atimeMs: 0,
			mtimeMs: uint32(stat.ModTime().UnixMilli()),
			ctimeMs: 0,
		}, nil
	}
}

func errorBadFD(fd uint32) error {
	return fmt.Errorf("bad file descriptor: %d", fd)
}

// syscallClose is like syscall.Close
func (j *jsWasm) syscallClose(ctx context.Context, mod api.Module, fd uint32) error {
	fsc := getSysCtx(mod).FS(ctx)
	if ok, err := fsc.CloseFile(ctx, fd); !ok {
		return errorBadFD(fd)
	} else {
		return err
	}
}

// syscallOpen is like syscall.Open
func (j *jsWasm) syscallOpen(ctx context.Context, mod api.Module, name string, flags, perm uint32) (uint32, error) {
	fsc := getSysCtx(mod).FS(ctx)
	return fsc.OpenFile(ctx, name)
}

// syscallRead is like syscall.Read
func (j *jsWasm) syscallRead(ctx context.Context, mod api.Module, fd uint32, p []byte) (n uint32, err error) {
	if r := fdReader(ctx, mod, fd); r == nil {
		err = errorBadFD(fd)
	} else if nRead, e := r.Read(p); e == nil || e == io.EOF {
		// fs_js.go cannot parse io.EOF so coerce it to nil.
		// See https://github.com/golang/go/issues/43913
		n = uint32(nRead)
	} else {
		err = e
	}
	return
}

// syscallWrite is like syscall.Write
func (j *jsWasm) syscallWrite(ctx context.Context, mod api.Module, fd uint32, p []byte) (n uint32, err error) {
	if writer := fdWriter(ctx, mod, fd); writer == nil {
		err = errorBadFD(fd)
	} else if nWritten, e := writer.Write(p); e == nil || e == io.EOF {
		// fs_js.go cannot parse io.EOF so coerce it to nil.
		// See https://github.com/golang/go/issues/43913
		n = uint32(nWritten)
	} else {
		err = e
	}
	return
}

const (
	fdStdin = iota
	fdStdout
	fdStderr
)

// fdReader returns a valid reader for the given file descriptor or nil if ErrnoBadf.
func fdReader(ctx context.Context, mod api.Module, fd uint32) io.Reader {
	sysCtx := getSysCtx(mod)
	if fd == fdStdin {
		return sysCtx.Stdin()
	} else if f, ok := sysCtx.FS(ctx).OpenedFile(ctx, fd); !ok {
		return nil
	} else {
		return f.File
	}
}

// fdWriter returns a valid writer for the given file descriptor or nil if ErrnoBadf.
func fdWriter(ctx context.Context, mod api.Module, fd uint32) io.Writer {
	sysCtx := getSysCtx(mod)
	switch fd {
	case fdStdout:
		return sysCtx.Stdout()
	case fdStderr:
		return sysCtx.Stderr()
	default:
		// Check to see if the file descriptor is available
		if f, ok := sysCtx.FS(ctx).OpenedFile(ctx, fd); !ok || f.File == nil {
			return nil
			// fs.FS doesn't declare io.Writer, but implementations such as
			// os.File implement it.
		} else if writer, ok := f.File.(io.Writer); !ok {
			return nil
		} else {
			return writer
		}
	}
}

// funcWrapper is the result of go's js.FuncOf ("_makeFuncWrapper" here).
type funcWrapper struct {
	j *jsWasm

	// id is managed on the Go side an increments (possibly rolling over).
	id uint32
}

type event struct {
	// funcWrapper.id
	id     uint32
	this   interface{}
	args   []interface{}
	result interface{}
}

func (f *funcWrapper) call(ctx context.Context, mod api.Module, args ...interface{}) interface{} {
	e := &event{
		id:   f.id,
		this: args[0],
		args: args[1:],
	}

	f.j._pendingEvent = e // Note: _pendingEvent reference is cleared during resume!

	if _, err := mod.ExportedFunction("resume").Call(ctx); err != nil {
		if _, ok := err.(*sys.ExitError); ok {
			return nil // allow error-handling to unwind when wasm calls exit due to a panic
		} else {
			panic(err)
		}
	}

	return e.result
}

// reflectConstruct implements JavaScript's Reflect.construct API
func (j *jsWasm) reflectConstruct(target interface{}, argumentsList []interface{}) (interface{}, error) { // nolint
	if target == uint8ArrayConstructor {
		return make([]byte, uint32(argumentsList[0].(float64))), nil
	}
	panic(fmt.Errorf("TODO: reflectConstruct(target=%v, argumentsList=%v)", target, argumentsList))
}

// valueRef returns 8 bytes to represent either the value or a reference to it.
// Any side effects besides memory must be cleaned up on wasmExit.
//
// See https://github.com/golang/go/blob/go1.19beta1/misc/wasm/wasm_exec.js#L135-L183
func (j *jsWasm) storeRef(ctx context.Context, mod api.Module, v interface{}) uint64 { // nolint
	// allow-list because we control all implementations
	if v == nil {
		return uint64(refValueNull)
	} else if b, ok := v.(bool); ok {
		if b {
			return uint64(refValueTrue)
		} else {
			return uint64(refValueFalse)
		}
	} else if jsV, ok := v.(*constVal); ok {
		return uint64(jsV.ref) // constant doesn't need to be stored
	} else if u, ok := v.(uint64); ok {
		return u // float already encoded as a uint64, doesn't need to be stored.
	} else if _, ok := v.(*funcWrapper); ok {
		id := j.values.increment(v)
		return uint64(valueRef(id, typeFlagFunction))
	} else if _, ok := v.(*event); ok {
		id := j.values.increment(v)
		return uint64(valueRef(id, typeFlagFunction))
	} else if _, ok := v.(string); ok {
		id := j.values.increment(v)
		return uint64(valueRef(id, typeFlagString))
	} else if _, ok := v.([]interface{}); ok {
		id := j.values.increment(&v) // []interface{} is not hashable
		return uint64(valueRef(id, typeFlagObject))
	} else if _, ok := v.([]byte); ok {
		id := j.values.increment(&v) // []byte is not hashable
		return uint64(valueRef(id, typeFlagObject))
	} else if v == io.EOF {
		return uint64(refEOF)
	} else if _, ok := v.(*jsSt); ok {
		id := j.values.increment(v)
		return uint64(valueRef(id, typeFlagObject))
	} else if ui, ok := v.(uint32); ok {
		if ui == 0 {
			return uint64(refValueZero)
		}
		return api.EncodeF64(float64(ui)) // numbers are encoded as float and passed through as a ref
	}
	panic(fmt.Errorf("TODO: storeRef(%v)", v))
}

type values struct {
	// Below is needed to avoid exhausting the ID namespace finalizeRef reclaims
	// See https://go-review.googlesource.com/c/go/+/203600

	values      []interface{}          // values indexed by ID, nil
	goRefCounts []uint32               // recount pair-indexed with values
	ids         map[interface{}]uint32 // live values
	idPool      []uint32               // reclaimed IDs (values[i] = nil, goRefCounts[i] nil
}

func (j *values) get(id uint32) interface{} {
	return j.values[id-nextID]
}

func (j *values) increment(v interface{}) uint32 {
	id, ok := j.ids[v]
	if !ok {
		if len(j.idPool) == 0 {
			id, j.values, j.goRefCounts = uint32(len(j.values)), append(j.values, v), append(j.goRefCounts, 0)
		} else {
			id, j.idPool = j.idPool[len(j.idPool)-1], j.idPool[:len(j.idPool)-1]
			j.values[id], j.goRefCounts[id] = v, 0
		}
		j.ids[v] = id
	}
	j.goRefCounts[id]++
	return id + nextID
}

func (j *values) decrement(id uint32) {
	id -= nextID
	j.goRefCounts[id]--
	if j.goRefCounts[id] == 0 {
		j.values[id] = nil
		j.idPool = append(j.idPool, id)
	}
}

var NaN = math.NaN()

// loadValue reads up to 8 bytes at the memory offset `addr` to return the
// value written by storeValue.
//
// See https://github.com/golang/go/blob/go1.19beta1/misc/wasm/wasm_exec.js#L122-L133
func (j *jsWasm) loadValue(ctx context.Context, mod api.Module, ref ref) interface{} { // nolint
	switch ref {
	case refValueNaN:
		return NaN
	case refValueZero:
		return uint32(0)
	case refValueNull:
		return nil
	case refValueTrue:
		return true
	case refValueFalse:
		return false
	case refValueGlobal:
		return valueGlobal
	case refJsGo:
		return j
	case refObjectConstructor:
		return objectConstructor
	case refArrayConstructor:
		return arrayConstructor
	case refJsProcess:
		return jsProcess
	case refJsFS:
		return jsFS
	case refJsFSConstants:
		return jsFSConstants
	case refUint8ArrayConstructor:
		return uint8ArrayConstructor
	case refEOF:
		return io.EOF
	default:
		if (ref>>32)&nanHead != nanHead { // numbers are passed through as a ref
			return api.DecodeF64(uint64(ref))
		}
		return j.values.get(uint32(ref))
	}
}

// loadSliceOfValues returns a slice of `len` values at the memory offset
// `addr`
//
// See https://github.com/golang/go/blob/go1.19beta1/misc/wasm/wasm_exec.js#L191-L199
func (j *jsWasm) loadSliceOfValues(ctx context.Context, mod api.Module, sliceAddr, sliceLen uint32) []interface{} { // nolint
	result := make([]interface{}, 0, sliceLen)
	for i := uint32(0); i < sliceLen; i++ { // nolint
		iRef := requireReadUint64Le(ctx, mod.Memory(), "iRef", sliceAddr+i*8)
		result = append(result, j.loadValue(ctx, mod, ref(iRef)))
	}
	return result
}

// valueString returns the string form of JavaScript string, boolean and number types.
func (j *jsWasm) valueString(v interface{}) string { // nolint
	if s, ok := v.(string); ok {
		return s
	}
	panic(fmt.Errorf("TODO: valueString(%v)", v))
}

// instanceOf returns true if the value is of the given type.
func (j *jsWasm) instanceOf(v, t interface{}) bool { // nolint
	panic(fmt.Errorf("TODO: instanceOf(v=%v, t=%v)", v, t))
}

// failIfClosed returns a sys.ExitError if wasmExit was called.
func (j *jsWasm) failIfClosed(mod api.Module) error {
	if closed := j.closed; closed != 0 {
		return sys.NewExitError(mod.Name(), uint32(closed>>32)) // Unpack the high order bits as the exit code.
	}
	return nil
}

// getSysCtx returns the sys.Context from the module or panics.
func getSysCtx(mod api.Module) *internalsys.Context {
	if internal, ok := mod.(*wasm.CallContext); !ok {
		panic(fmt.Errorf("unsupported wasm.Module implementation: %v", mod))
	} else {
		return internal.Sys
	}
}

// requireRead is like api.Memory except that it panics if the offset and
// byteCount are out of range.
func requireRead(ctx context.Context, mem api.Memory, fieldName string, offset, byteCount uint32) []byte {
	buf, ok := mem.Read(ctx, offset, byteCount)
	if !ok {
		panic(fmt.Errorf("Memory.Read(ctx, %d, %d) out of range of memory size %d reading %s",
			offset, byteCount, mem.Size(ctx), fieldName))
	}
	return buf
}

// requireReadUint32Le is like api.Memory except that it panics if the offset
// is out of range.
func requireReadUint32Le(ctx context.Context, mem api.Memory, fieldName string, offset uint32) uint32 {
	result, ok := mem.ReadUint32Le(ctx, offset)
	if !ok {
		panic(fmt.Errorf("Memory.ReadUint64Le(ctx, %d) out of range of memory size %d reading %s",
			offset, mem.Size(ctx), fieldName))
	}
	return result
}

// requireReadUint64Le is like api.Memory except that it panics if the offset
// is out of range.
func requireReadUint64Le(ctx context.Context, mem api.Memory, fieldName string, offset uint32) uint64 {
	result, ok := mem.ReadUint64Le(ctx, offset)
	if !ok {
		panic(fmt.Errorf("Memory.ReadUint64Le(ctx, %d) out of range of memory size %d reading %s",
			offset, mem.Size(ctx), fieldName))
	}
	return result
}

// requireWrite is like api.Memory except that it panics if the offset
// is out of range.
func requireWrite(ctx context.Context, mem api.Memory, fieldName string, offset uint32, val []byte) {
	if ok := mem.Write(ctx, offset, val); !ok {
		panic(fmt.Errorf("Memory.Write(ctx, %d, %d) out of range of memory size %d writing %s",
			offset, val, mem.Size(ctx), fieldName))
	}
}

// requireWriteByte is like api.Memory except that it panics if the offset
// is out of range.
func requireWriteByte(ctx context.Context, mem api.Memory, fieldName string, offset uint32, val byte) {
	if ok := mem.WriteByte(ctx, offset, val); !ok {
		panic(fmt.Errorf("Memory.WriteByte(ctx, %d, %d) out of range of memory size %d writing %s",
			offset, val, mem.Size(ctx), fieldName))
	}
}

// requireWriteUint32Le is like api.Memory except that it panics if the offset
// is out of range.
func requireWriteUint32Le(ctx context.Context, mem api.Memory, fieldName string, offset uint32, val uint32) {
	if ok := mem.WriteUint32Le(ctx, offset, val); !ok {
		panic(fmt.Errorf("Memory.WriteUint32Le(ctx, %d, %d) out of range of memory size %d writing %s",
			offset, val, mem.Size(ctx), fieldName))
	}
}

// requireWriteUint64Le is like api.Memory except that it panics if the offset
// is out of range.
func requireWriteUint64Le(ctx context.Context, mem api.Memory, fieldName string, offset uint32, val uint64) {
	if ok := mem.WriteUint64Le(ctx, offset, val); !ok {
		panic(fmt.Errorf("Memory.WriteUint64Le(ctx, %d, %d) out of range of memory size %d writing %s",
			offset, val, mem.Size(ctx), fieldName))
	}
}

// refreshSP refreshes the stack pointer, which is needed prior to storeValue
// when in an operation that can trigger a Go event handler.
//
// See https://github.com/golang/go/blob/go1.19beta1/misc/wasm/wasm_exec.js#L210-L213
func (j *jsWasm) refreshSP(ctx context.Context, mod api.Module) uint32 {
	if closed := j.closed; closed != 0 {
		return 0 // Don't execute a wasm function when closed.
	}
	ret, err := mod.ExportedFunction("getsp").Call(ctx) // refresh the stack pointer
	if err != nil {
		panic(fmt.Errorf("error refreshing stack pointer: %w", err))
	}
	return uint32(ret[0])
}

// jsSt is pre-parsed from fs_js.go setStat to avoid thrashing.
type jsSt struct {
	isDir bool

	dev     uint32
	ino     uint32
	mode    uint32
	nlink   uint32
	uid     uint32
	gid     uint32
	rdev    uint32
	size    uint32
	blksize uint32
	blocks  uint32
	atimeMs uint32
	mtimeMs uint32
	ctimeMs uint32
}
