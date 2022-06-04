package wasm_exec

import (
	"context"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

type WasmExec interface {
	Main(ctx context.Context, args, environ []string) (exitCode uint32, err error)

	api.Closer
}

type wasmExec struct {
	ns  wazero.Namespace
	mod api.Module
	g   *jsWasm
}

// TODO: after debugging memory crash make it impossible to repeat calling this as
// each iteration leaks memory in wasm.
func (e *wasmExec) Main(ctx context.Context, args, environ []string) (exitCode uint32, err error) {
	e.g.closed = 0

	argc, argv, err := writeArgsAndEnviron(ctx, e.mod.Memory(), args, environ)
	if err != nil {
		return 0, err
	}
	_, err = e.mod.ExportedFunction("run").Call(ctx, uint64(argc), uint64(argv))
	exitCode = uint32(e.g.closed >> 32)
	return
}

func (e *wasmExec) Close(ctx context.Context) error {
	return e.ns.Close(ctx)
}
