package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLogFileWriterLazyOpen:logFileWriter 懒打开——不写日志不创建文件;
// 首次 Write 才创建;重复 Write 不重复打开;追加生效。
func TestLogFileWriterLazyOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "xnc-shell.log")

	// 1. 构造后未 Write:文件不存在(健康会话不留 0 字节文件)。
	w := &logFileWriter{path: path}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file created before first write: %v", err)
	}

	// 2. 首次 Write 创建文件并落盘。
	if n, err := w.Write([]byte("first\n")); err != nil || n != 6 {
		t.Fatalf("first write: n=%d err=%v", n, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after first write: %v", err)
	}
	if string(b) != "first\n" {
		t.Fatalf("content = %q, want %q", b, "first\n")
	}

	// 3. 再次 Write:追加,不重复打开。
	if _, err := w.Write([]byte("second\n")); err != nil {
		t.Fatalf("second write: %v", err)
	}
	b, _ = os.ReadFile(path)
	if string(b) != "first\nsecond\n" {
		t.Fatalf("content = %q, want %q", b, "first\nsecond\n")
	}

	// 4. Close 后文件内容完整。
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	b, _ = os.ReadFile(path)
	if string(b) != "first\nsecond\n" {
		t.Fatalf("content after close = %q", b)
	}
	// Close 幂等(未打开过的 writer 也安全)。
	if err := (&logFileWriter{path: path}).Close(); err != nil {
		t.Fatalf("close on never-opened writer: %v", err)
	}
}

// TestLogFileWriterOpenFailure:打开失败标记 failed,后续 Write 不再重试,
// 不产生错误(调用方 stderr 通道已由 MultiWriter 兜底)。
func TestLogFileWriterOpenFailure(t *testing.T) {
	// 用已存在目录路径充当"文件",OpenFile 必失败。
	dir := t.TempDir()
	w := &logFileWriter{path: dir}
	if _, err := w.Write([]byte("x")); err != nil {
		t.Fatalf("write after failed open should be swallowed, got %v", err)
	}
	if !w.failed {
		t.Fatal("writer not marked failed")
	}
	if w.f != nil {
		t.Fatal("file handle leaked after failed open")
	}
	// 再次 Write:不再尝试打开(打开失败提示只出现一次)。
	if _, err := w.Write([]byte("y")); err != nil {
		t.Fatalf("second write after failure: %v", err)
	}
}

// TestLogFileWriterRotation:超限轮转——旧内容进 .1,新记录进新文件;
// 反复超限时 keep 封顶(最老被覆盖)。max 注入为小值以驱动边界。
func TestLogFileWriterRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "xnc-shell.log")

	w := &logFileWriter{path: path, max: 10}
	defer w.Close()
	if _, err := w.Write([]byte("0123456789A")); err != nil { // 11B > 10B → 先轮转(旧空)再写
		t.Fatalf("first write: %v", err)
	}
	if _, err := w.Write([]byte("BCDEFGHIJKL")); err != nil { // 再超限 → 首轮内容进 .1
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

	// 连续轮转:keep=3 封顶,第 4 轮时 .3 被覆盖(.4 不出现)。
	for i := 0; i < 5; i++ {
		if _, err := w.Write([]byte("0123456789Z")); err != nil {
			t.Fatalf("loop write %d: %v", i, err)
		}
	}
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Fatalf(".4 should not exist (keep=%d)", logRotateKeep)
	}
	if _, err := os.Stat(path + ".3"); err != nil {
		t.Fatalf(".3 should exist: %v", err)
	}
}
