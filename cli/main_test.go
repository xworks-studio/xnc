package main

import (
	"io"
	"os"
	"testing"
)

// captureStdout redirects os.Stdout while f runs and returns the captured
// output plus f's exit code. runCLI itself lives in main.go.
func captureStdout(t *testing.T, f func() int) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	code := f()
	os.Stdout = old
	_ = w.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), code
}
