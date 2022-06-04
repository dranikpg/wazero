package main

import (
	"context"
	"io/fs"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/internal/testing/maintester"
	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/wasm_exec"
)

// Test_main ensures the following will work:
//
//	go run cat.go /test.txt
func Test_main(t *testing.T) {
	stdout, stderr := maintester.TestMain(t, main, "cat", "/test.txt")
	require.Equal(t, "", stderr)
	require.Equal(t, "greet filesystem\n", stdout)
}

func Benchmark_main(b *testing.B) {
	// Choose the context to use for function calls.
	ctx := context.Background()

	// Create a new WebAssembly Runtime.
	r := wazero.NewRuntimeWithConfig(wazero.NewRuntimeConfigCompiler())
	defer r.Close(ctx) // This closes everything this Runtime created.

	// Since wazero uses fs.FS, we can use standard libraries to do things like trim the leading path.
	rooted, err := fs.Sub(catFS, "testdata")
	if err != nil {
		b.Fatal(err)
	}

	// Combine the above into our baseline config, overriding defaults (which discards stdout and has no file system).
	config := wazero.NewModuleConfig().WithFS(rooted)

	// Compile the WebAssembly module using the default configuration.
	code, err := r.CompileModule(ctx, catWasm, wazero.NewCompileConfig())
	if err != nil {
		b.Fatal(err)
	}

	we, err := wasm_exec.NewWasmExec(ctx, r, code, config.WithArgs("wasm_exec", "/test.txt"))
	if err != nil {
		b.Fatal(err)
	}

	b.Run("wasm_exec cat", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if exitCode, err := we.Main(ctx, []string{"wasm_exec", "/test.txt"}, nil); err != nil {
				b.Fatal(err)
			} else if exitCode != 0 {
				b.Fatal("exitCode=", exitCode)
			}
		}
	})
}
