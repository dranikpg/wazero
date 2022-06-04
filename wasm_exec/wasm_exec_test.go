package wasm_exec_test

import (
	"context"
	"testing"

	"github.com/tetratelabs/wazero/internal/testing/require"
)

// testCtx is an arbitrary, non-default context. Non-nil also prevents linter errors.
var testCtx = context.WithValue(context.Background(), struct{}{}, "arbitrary")

func Test_argsAndEnv(t *testing.T) {
	stdout, stderr, exitCode, err := compileAndRunJsWasm(testCtx, `package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Println()
	for i, a := range os.Args {
		fmt.Println("args", i, "=", a)
	}
	for i, e := range os.Environ() {
		fmt.Println("environ", i, "=", e)
	}
}`, []string{"a", "b"}, []string{"c=d", "a=b"})

	require.NoError(t, err)
	require.Zero(t, exitCode)
	require.Equal(t, `
args 0 = a
args 1 = b
environ 0 = c=d
environ 1 = a=b
`, stdout)
	require.Equal(t, "", stderr)
}
