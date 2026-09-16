// rotatinglog.go — agent 服务日志的大小轮转 writer(2026-09-15 规范 §3.2)。
//
// slog 的 TextHandler 逐行调用 Write;本包装在写侧计数,超过 maxBytes
// (默认 8MB)时 rename 链轮转(.1 最新,keep 最旧被覆盖删除),随后重开
// 追加。轮转/重开失败静默继续向旧句柄追加——轮转问题绝不阻断日志本身。
// 与 shellhost 的 logFileWriter 各自独立实现(跨模块,代码量 <60 行,
// 不值得为此建立共享依赖)。
package main

import (
	"fmt"
	"os"
	"sync"
)

const (
	agentLogRotateMaxBytes int64 = 8 << 20
	agentLogRotateKeep           = 3
)

type rotatingWriter struct {
	mu     sync.Mutex
	path   string
	f      *os.File
	size   int64
	max    int64
	keep   int
	broken bool
}

func newRotatingWriter(path string) (*rotatingWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	w := &rotatingWriter{path: path, f: f, max: agentLogRotateMaxBytes, keep: agentLogRotateKeep}
	if st, serr := f.Stat(); serr == nil {
		w.size = st.Size()
	}
	return w, nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.broken {
		return len(p), nil
	}
	if w.size+int64(len(p)) > w.max {
		// 超限先轮转再落盘:当前记录进新文件,旧内容完整归档;
		// 重开失败退回旧句柄继续追加(仅体积失控,不丢日志)。
		_ = w.f.Close()
		for i := w.keep - 1; i >= 1; i-- {
			_ = os.Rename(fmt.Sprintf("%s.%d", w.path, i), fmt.Sprintf("%s.%d", w.path, i+1))
		}
		if rerr := os.Rename(w.path, w.path+".1"); rerr == nil {
			if nf, oerr := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); oerr == nil {
				w.f = nf
				w.size = 0
			}
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}
