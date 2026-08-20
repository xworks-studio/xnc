package main

import (
	"context"
	"io"
	"os"
	"testing"
)

// runCLIWithStdin pipes input into os.Stdin before running the CLI (used by
// `xnc run <node> -`) and captures stdout like captureStdout. The write end
// closes first so io.ReadAll(os.Stdin) inside the command sees EOF.
func runCLIWithStdin(t *testing.T, input string, args []string) (string, int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(w, input); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	old := os.Stdin
	os.Stdin = r
	out, code := captureStdout(t, func() int {
		return runCLI(context.Background(), args)
	})
	os.Stdin = old
	_ = r.Close()
	return out, code
}

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
