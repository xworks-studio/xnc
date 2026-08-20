package session

import (
	"context"
	"io"
	"time"

	"github.com/coder/websocket"
)

const pumpWriteTimeout = 60 * time.Second

// pump 两侧 attach 齐备后的帧粘合：双 goroutine 双向转发 text+binary 帧
// （server 不解析会话帧）；任一方向出错（对端断开/写失败/超时）→
// NotifyClose("peer-disconnect")，由 close 幂等收敛（双侧 CloseNow + 通知）。
func (m *Manager) pump(s *session) {
	go func() {
		m.relay(s.clientWS, s.agentWS)
		m.NotifyClose(s.ID, "peer-disconnect")
	}()
	go func() {
		m.relay(s.agentWS, s.clientWS)
		m.NotifyClose(s.ID, "peer-disconnect")
	}()
}

// relay 流式转发 from → to（Reader/Writer 级流式，避免整帧缓冲）。
// 每帧的读+写共用一个 60s 超时 ctx；任何错误即返回。
func (m *Manager) relay(from, to *websocket.Conn) {
	if from == nil || to == nil {
		return
	}
	buf := make([]byte, 32*1024)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), pumpWriteTimeout)
		typ, r, err := from.Reader(ctx)
		if err != nil {
			cancel()
			return
		}
		w, err := to.Writer(ctx, typ)
		if err != nil {
			cancel()
			return
		}
		if _, err = copyBuf(w, r, buf); err != nil {
			cancel()
			return
		}
		if err = w.Close(); err != nil {
			cancel()
			return
		}
		cancel()
	}
}

func copyBuf(w io.WriteCloser, r io.Reader, buf []byte) (int64, error) {
	var n int64
	for {
		nr, er := r.Read(buf)
		if nr > 0 {
			nw, ew := w.Write(buf[:nr])
			n += int64(nw)
			if ew != nil {
				return n, ew
			}
		}
		if er != nil {
			return n, nil // io.EOF 属正常（帧数据读完）
		}
	}
}
