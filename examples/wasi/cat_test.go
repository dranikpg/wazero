package main

import (
	"context"
	"io/fs"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/internal/testing/maintester"
	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/wasi_snapshot_preview1"
)

// Test_main ensures the following will work:
//
//	go run cat.go /test.txt
func Test_main(t *testing.T) {
	stdout, _ := maintester.TestMain(t, main, "cat", "/test.txt")
	require.Equal(t, "greet filesystem\n", stdout)
}

func Benchmark_main(b *testing.B) {
	// Choose the context to use for function calls.
	ctx := context.Background()

	// Create a new WebAssembly Runtime.
	r := wazero.NewRuntime()
	defer r.Close(ctx) // This closes everything this Runtime created.

	// Since wazero uses fs.FS, we can use standard libraries to do things like trim the leading path.
	rooted, err := fs.Sub(catFS, "testdata")
	if err != nil {
		b.Fatal(err)
	}

	// Combine the above into our baseline config, overriding defaults (which discards stdout and has no file system).
	config := wazero.NewModuleConfig().WithFS(rooted)

	// Instantiate WASI, which implements system I/O such as console output.
	if _, err = wasi_snapshot_preview1.Instantiate(ctx, r); err != nil {
		b.Fatal(err)
	}

	// Compile the WebAssembly module using the default configuration.
	code, err := r.CompileModule(ctx, catWasm, wazero.NewCompileConfig())
	if err != nil {
		b.Fatal(err)
	}

	b.Run("wasi cat", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			// InstantiateModule runs the "_start" function which is what TinyGo compiles "main" to.
			// * Set the program name (arg[0]) to "wasi" and add args to write "/test.txt" to stdout twice.
			if mod, err := r.InstantiateModule(ctx, code, config.WithArgs("wasi", "/test.txt")); err != nil {
				b.Fatal(err)
			} else {
				mod.Close(ctx)
			}
		}
	})
}
