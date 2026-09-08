// legquic.go — host 腿：raw QUIC（ALPN xnc-host/1，UDP 4433 形态）。
// 首个 bidi stream = 控制流（4B LE 长度前缀 + JSON），datagram = 媒体包
// 原样转发。hello{role:"host", nodeId, token} 必须持有效 HostToken 才可
// 注册（MVP 的任意字符串顶替语义在 XNC 收紧为凭据校验）。
package rtv

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
)

// HostALPN 与 host/src/transport.rs 的 ALPN 常量一致。
const HostALPN = "xnc-host/1"
const ctrlFrameMax = 256 * 1024

func (s *Server) serveHostLeg(addr string) error {
	udp, err := net.ListenUDP("udp", mustResolveUDP(addr))
	if err != nil {
		return fmt.Errorf("host leg listen: %w", err)
	}
	qconf := &quic.Config{
		EnableDatagrams:    true,
		MaxIncomingStreams: 4,
	}
	listener, err := quic.Listen(udp, s.TLSConfig([]string{HostALPN}), qconf)
	if err != nil {
		return fmt.Errorf("host leg quic listen: %w", err)
	}
	s.addrMu.Lock()
	s.hostAddrInfo = udp.LocalAddr().String()
	s.addrMu.Unlock()
	slog.Info("rtv: host leg (raw QUIC) listening", "addr", addr, "alpn", HostALPN)
	go func() {
		for {
			conn, err := listener.Accept(context.Background())
			if err != nil {
				slog.Warn("rtv: host leg accept ended", "err", err)
				return
			}
			go s.handleHostConn(conn)
		}
	}()
	return nil
}

func (s *Server) handleHostConn(conn *quic.Conn) {
	remote := conn.RemoteAddr().String()
	slog.Info("rtv: host connection", "remote", remote)
	ctx := conn.Context()
	defer conn.CloseWithError(0, "bye")

	// 首个 bidi stream = 控制流
	stream, err := conn.AcceptStream(ctx)
	if err != nil {
		slog.Warn("rtv: host control stream accept failed", "remote", remote, "err", err)
		return
	}

	h := &HostSession{
		ConnectedAt: time.Now(),
		closeConn:   func() { conn.CloseWithError(0, "replaced") },
		viewers:     make(map[uint64]Viewer), // nil map 赋值会 panic（AddViewer 路径）
	}
	// 控制写（长度前缀 + JSON）；多 goroutine 并发写需串行化
	var wmu sync.Mutex
	h.sendControl = func(v json.RawMessage) error {
		wmu.Lock()
		defer wmu.Unlock()
		return writeCtrlFrame(stream, v)
	}

	// 媒体 datagram → 扇出（注册前丢弃：h.NodeID 为空，cur != h）
	go func() {
		for {
			data, err := conn.ReceiveDatagram(ctx)
			if err != nil {
				slog.Info("rtv: host datagram loop ended", "remote", remote, "err", err)
				return
			}
			if cur := s.Hub.Host(h.NodeID); cur == h {
				h.fanoutMedia(data)
			}
		}
	}()

	// 控制读取循环（含 hello 注册）
	err = readCtrlFrames(stream, func(v json.RawMessage) bool {
		var m struct {
			Type   string `json:"type"`
			Role   string `json:"role"`
			NodeID string `json:"nodeId"`
			Token  string `json:"token"`
		}
		if err := json.Unmarshal(v, &m); err != nil {
			slog.Warn("rtv: bad host control json", "err", err)
			return true
		}
		switch m.Type {
		case "hello":
			if m.Role != "host" {
				return true
			}
			if !s.validateHostHello(m.NodeID, m.Token) {
				// 无效/未知 token：拒绝注册并断开（防任意顶替劫持）。
				slog.Warn("rtv: host hello rejected (bad token)", "remote", remote)
				_ = h.sendControl(mustJSON(map[string]any{
					"type": "error", "code": "HOST_AUTH_FAILED",
					"message": "invalid host token for this node",
				}))
				return false
			}
			h.NodeID = m.NodeID
			s.Hub.RegisterHost(m.NodeID, h)
		case "config":
			h.cacheConfig(v)
			h.broadcastControl(v)
		case "frameStats", "qosAdjust", "heartbeat", "encoderEvent":
			h.broadcastControl(v)
		default:
			slog.Debug("rtv: host control forwarded", "type", m.Type)
			h.broadcastControl(v)
		}
		return true
	})
	if err != nil && err != io.EOF {
		slog.Info("rtv: host control loop ended", "remote", remote, "err", err)
	}
	if h.NodeID != "" {
		s.Hub.UnregisterHost(h.NodeID, h)
	}
	// 通知所有 viewer
	h.broadcastControl(mustJSON(map[string]any{"type": "hostOffline", "node": h.NodeID}))
}

// readCtrlFrames 读取 4B LE 长度 + JSON 帧；handler 返回 false 停止。
func readCtrlFrames(r io.Reader, fn func(json.RawMessage) bool) error {
	var lenBuf [4]byte
	for {
		if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
			return err
		}
		n := binary.LittleEndian.Uint32(lenBuf[:])
		if n == 0 || n > ctrlFrameMax {
			return fmt.Errorf("bad ctrl frame len %d", n)
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(r, body); err != nil {
			return err
		}
		if !fn(body) {
			return nil
		}
	}
}

func writeCtrlFrame(w io.Writer, v json.RawMessage) error {
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(v)))
	if _, err := w.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err := w.Write(v)
	return err
}

func mustResolveUDP(addr string) *net.UDPAddr {
	a, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		panic(err)
	}
	return a
}
