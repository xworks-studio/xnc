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
	return captureFD(&os.Stdout, t, f)
}

// captureStderr does the same for os.Stderr (table-mode error lines).
func captureStderr(t *testing.T, f func() int) (string, int) {
	t.Helper()
	return captureFD(&os.Stderr, t, f)
}

func captureFD(fd **os.File, t *testing.T, f func() int) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := *fd
	*fd = w
	code := f()
	*fd = old
	_ = w.Close()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), code
}
