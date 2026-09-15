// rotatinglog_test.go — rotatingWriter 轮转语义(2026-09-15 规范 §3.2)。
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRotatingWriterRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-service.log")

	w, err := newRotatingWriter(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	w.max = 10 // 注入小上限驱动边界

	if _, err := w.Write([]byte("0123456789A")); err != nil { // 11B > 10B → 轮转(旧空)后写入
		t.Fatalf("first write: %v", err)
	}
	if _, err := w.Write([]byte("BCDEFGHIJKL")); err != nil { // 再超限 → 首段进 .1
		t.Fatalf("second write: %v", err)
	}
	b1, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("read .1: %v", err)
	}
	if string(b1) != "0123456789A" {
		t.Fatalf(".1 content = %q, want %q", b1, "0123456789A")
	}
	b0, _ := os.ReadFile(path)
	if string(b0) != "BCDEFGHIJKL" {
		t.Fatalf("current content = %q, want %q", b0, "BCDEFGHIJKL")
	}

	// 连续轮转:keep=3 封顶,.4 不出现、.3 存在。
	for i := 0; i < 5; i++ {
		if _, err := w.Write([]byte("0123456789Z")); err != nil {
			t.Fatalf("loop write %d: %v", i, err)
		}
	}
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Fatalf(".4 should not exist (keep=%d)", agentLogRotateKeep)
	}
	if _, err := os.Stat(path + ".3"); err != nil {
		t.Fatalf(".3 should exist: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
