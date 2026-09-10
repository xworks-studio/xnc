package session

import (
	"context"
	"io"
	"time"

	"github.com/coder/websocket"
)

// pumpWriteTimeout 写侧每帧超时：仅约束写方向，防慢消费者（对端停止读取）
// 把转发 goroutine 永久挂死。包级 var 供测试注入。
var pumpWriteTimeout = 60 * time.Second

// pumpReadTimeout 读侧每帧超时，默认 0 = 禁用（background ctx，读侧永不超时）。
// 读写超时必须分离：单向静默（exec 运行 ≥60s 且 client 零输入）是正常流量
// 形态，会话级超时的所有权在 agent（EXEC_RESULT.TimedOut）——server 不得因
// 方向静默代拆健康会话。非 0 值供测试/特殊部署注入读侧兜底。
var pumpReadTimeout time.Duration = 0

// readCtx 构造读侧 ctx：pumpReadTimeout<=0 → background（默认禁用）。
func readCtx() (context.Context, context.CancelFunc) {
	if pumpReadTimeout <= 0 {
		return context.Background(), func() {}
	}
	return context.WithTimeout(context.Background(), pumpReadTimeout)
}

// pump 两侧 attach 齐备后的帧粘合：双 goroutine 双向转发 text+binary 帧
// （server 不解析会话帧）；每帧转发成功即刷新 s.lastActivity（shell idle
// 计时的活跃信号）；任一方向出错（对端断开/写失败/超时）→
// NotifyClose("peer-disconnect")，由 close 幂等收敛（双侧 CloseNow + 通知）。
func (m *Manager) pump(s *session) {
	go func() {
		m.relay(s, s.clientWS, s.agentWS)
		m.NotifyClose(s.ID, "peer-disconnect")
	}()
	go func() {
		m.relay(s, s.agentWS, s.clientWS)
		m.NotifyClose(s.ID, "peer-disconnect")
	}()
}

// relay 流式转发 from → to（Reader/Writer 级流式，避免整帧缓冲）。
// 读侧用 readCtx（默认无超时），写侧独立 60s 超时；任何错误即返回。
func (m *Manager) relay(s *session, from, to *websocket.Conn) {
	if from == nil || to == nil {
		return
	}
	buf := make([]byte, 32*1024)
	for {
		rctx, cancelRead := readCtx()
		typ, r, err := from.Reader(rctx)
		if err != nil {
			cancelRead()
			return
		}
		wctx, cancelWrite := context.WithTimeout(context.Background(), pumpWriteTimeout)
		w, err := to.Writer(wctx, typ)
		if err != nil {
			cancelRead()
			cancelWrite()
			return
		}
		if _, err = copyBuf(w, r, buf); err != nil {
			cancelRead()
			cancelWrite()
			return
		}
		if err = w.Close(); err != nil {
			cancelRead()
			cancelWrite()
			return
		}
		s.lastActivity.Store(time.Now().UnixNano()) // 每帧免锁刷新活跃时间
		cancelWrite()
		cancelRead()
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
