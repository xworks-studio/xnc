package main

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// strReader 供 secret-stdin 单测。
func strReader(s string) io.Reader { return strings.NewReader(s) }

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// waitFor 轮询断言(cond 真则通过),超时 fail——孤儿进程检查用。
func waitFor(t *testing.T, d time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal(msg)
}
