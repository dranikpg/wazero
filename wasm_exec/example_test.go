package wasm_exec_test

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"os"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/wasm_exec"
)

// This is an example of how to use `GOARCH=wasm GOOS=js` compiled wasm via a
// sleep function.
//
// See https://github.com/tetratelabs/wazero/tree/main/examples/wasm_exec for another example.
func Example() {
	// Choose the context to use for function calls.
	ctx := context.Background()

	// Create a new WebAssembly Runtime.
	r := wazero.NewRuntime()
	defer r.Close(ctx)

	// Compile the WebAssembly module using the default configuration.
	bin, err := compileJsWasm(`package main

import "time"

func main() {
	time.Sleep(time.Duration(1))
}`)
	if err != nil {
		log.Panicln(err)
	}

	compiled, err := r.CompileModule(ctx, bin, wazero.NewCompileConfig())
	if err != nil {
		log.Panicln(err)
	}

	// Override defaults which discard stdout and fake sleep.
	config := wazero.NewModuleConfig().
		WithStdout(os.Stdout).
		WithStderr(os.Stderr)

	// Instantiate wasm_exec, which invokes the "run" function.
	we, err := wasm_exec.NewWasmExec(ctx, r, compiled, config)
	if err != nil {
		log.Panicln(err)
	}

	// TODO: remove this after memory.grow debugging
	exitCode, err := we.Main(ctx, nil, nil)
	if err != nil {
		log.Panicln(err)
	}

	// Print the exit code
	fmt.Printf("exit_code: %d\n", exitCode)

	// Output:
	// exit_code: 0
}
