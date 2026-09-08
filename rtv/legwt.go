// legwt.go — 浏览器腿（主）：HTTP/3 WebTransport /wt（UDP 443 形态）。
// viewer 首个 bidi stream = 控制流（4B LE 长度 + JSON，与 host 腿同构）；
// 媒体经 session datagram 下行。连接期经 AuthViewer 回调完成会话 token
// 鉴权（?token= 查询参数，与既有 clientSessionWS 同源语义）。
package rtv

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
)

// wtMuxPath WT 会话路径（浏览器与 host 端约定无关，仅浏览器腿使用）。
const wtMuxPath = "/wt"

// wtSameHost：Origin 的 host 与请求 Host 的 host 一致即放行（忽略端口差，
// 供非规范 WT 公网端口的过渡形态；空 Origin 一并放行——非浏览器客户端
// 如 rtvload 不发 Origin）。
func wtSameHost(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	oh, _, err := net.SplitHostPort(u.Host)
	if err != nil {
		oh = u.Host
	}
	rh, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		rh = r.Host
	}
	return oh != "" && strings.EqualFold(oh, rh)
}

// wtCheckOrigin：空 Origin 放行（非浏览器客户端）；同 host 放行；
// 其余按 wtAllowOrigins 精确匹配（Origin 头原样形态，默认端口省略）。
func (s *Server) wtCheckOrigin(r *http.Request) bool {
	if wtSameHost(r) {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	if s.wtAllowOrigins[origin] {
		return true
	}
	slog.Warn("rtv: wt origin rejected", "origin", origin, "host", r.Host)
	return false
}

func (s *Server) serveWTLeg(addr string) error {
	udp, err := net.ListenUDP("udp", mustResolveUDP(addr))
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	wt := &webtransport.Server{
		H3: &http3.Server{
			Handler:         mux,
			TLSConfig:       s.TLSConfig([]string{"h3"}),
			EnableDatagrams: true,
		},
		// 库缺省做严格同源（含端口）校验：放宽为"同 host 或显式放行表"。
		// 同 host 覆盖嵌入 relay-0（含非规范端口的过渡形态，生产实测踩坑）；
		// 跨 host 外部 relay 的页面 Origin 是主站域（Origin=https://xnc.app
		// vs Host=47.x.x.x），必须经 WTAllowOrigins 显式放行——会话票据
		// 才是本腿的真实凭据，Origin 仅防跨站冒用。
		CheckOrigin: s.wtCheckOrigin,
	}
	mux.HandleFunc(wtMuxPath, func(w http.ResponseWriter, r *http.Request) {
		binding, apiErr := s.authViewer(r.URL.Query().Get("token"))
		if apiErr != nil {
			http.Error(w, apiErr.Message, apiErr.Status)
			return
		}
		sess, err := wt.Upgrade(w, r)
		if err != nil {
			slog.Warn("rtv: wt upgrade failed", "err", err)
			return
		}
		// 关键：handler 必须为会话生命周期阻塞。WebTransport 会话绑定在 HTTP/3
		// CONNECT 流上，handler 返回 → 请求结束 → CONNECT 流关闭 → 会话流控
		// 胶囊（MAX_DATA）不再被处理 → 流写入永久阻塞。
		s.handleWTSession(binding, sess)
	})
	s.addrMu.Lock()
	s.wtAddrInfo = udp.LocalAddr().String()
	s.addrMu.Unlock()
	slog.Info("rtv: webtransport leg listening", "addr", addr)
	go func() {
		if err := wt.Serve(udp); err != nil {
			slog.Error("rtv: wt serve ended", "err", err)
		}
	}()
	return nil
}

// handleWTSession 在 HTTP handler 内运行并阻塞至会话结束（见上方注释）。
func (s *Server) handleWTSession(binding ViewerBinding, sess *webtransport.Session) {
	ctx := sess.Context()
	stream, err := sess.AcceptStream(ctx)
	if err != nil {
		_ = sess.CloseWithError(0, "no control stream")
		return
	}

	v := newWTViewer(s.Hub.NextViewerID(), binding, sess, stream)
	slog.Info("rtv: wt viewer connected", "viewer", v.id, "remote", sess.RemoteAddr(),
		"node", binding.Node)

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.serveViewerLoop(v, stream) // 退出时反注册并关会话
	}()
	// 浏览器→server datagram（无上行媒体；读取并丢弃，维持 datagram 语义）
	go func() {
		for {
			if _, err := sess.ReceiveDatagram(ctx); err != nil {
				return
			}
		}
	}()
	select {
	case <-ctx.Done():
	case <-done:
	}
}

// serveViewerLoop 读 viewer 控制流直至关闭：
// hello 本地处理（绑定节点已在连接期定死，hello 只作绑定触发）；其余
// 原样转发 host（frameLoss/feedback/heartbeat 直通，input 走门控）。
func (s *Server) serveViewerLoop(v Viewer, r io.Reader) {
	cleanup := func() {
		s.Hub.RemoveViewer(v.Node(), v)
		v.Close()
	}
	defer cleanup()

	err := readCtrlFrames(r, func(b json.RawMessage) bool {
		s.Host.Touch(v.Session())
		var m struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			slog.Warn("rtv: bad viewer json", "err", err)
			return true
		}
		switch m.Type {
		case "hello":
			// 未绑定到当前 host 会话才重试（首条 hello 门闩会吞掉 host
			// 离线期间的所有重试，导致 host 回来后 viewer 卡死）
			if m.Role == "viewer" && !s.Hub.IsViewerBound(v.Node(), v) {
				s.Hub.AddViewer(v.Node(), v)
			}
		default:
			if h := s.Hub.Host(v.Node()); h != nil {
				h.forwardToHost(b, v)
			} else {
				_ = v.SendControlJSON(mustJSON(map[string]any{"type": "hostOffline", "node": v.Node()}))
			}
		}
		return true
	})
	slog.Info("rtv: wt viewer control ended", "viewer", v.ID(), "err", err)
}

// ---------------- WT Viewer 适配器 ----------------

type wtViewer struct {
	id        uint64
	binding   ViewerBinding
	sess      *webtransport.Session
	wmu       sync.Mutex
	ctrlWrite func(json.RawMessage) error
}

func newWTViewer(id uint64, binding ViewerBinding, sess *webtransport.Session, stream *webtransport.Stream) *wtViewer {
	v := &wtViewer{id: id, binding: binding, sess: sess}
	v.ctrlWrite = func(b json.RawMessage) error {
		v.wmu.Lock()
		defer v.wmu.Unlock()
		return writeCtrlFrame(stream, b)
	}
	return v
}

func (v *wtViewer) ID() uint64          { return v.id }
func (v *wtViewer) Kind() string        { return "wt" }
func (v *wtViewer) CanControl() bool    { return v.binding.Control }
func (v *wtViewer) CanInput() bool      { return v.binding.Input }
func (v *wtViewer) DisplayName() string { return v.binding.Name }
func (v *wtViewer) Node() string        { return v.binding.Node }
func (v *wtViewer) Session() string     { return v.binding.Session }

func (v *wtViewer) SendDatagram(b []byte) error {
	return v.sess.SendDatagram(b)
}

func (v *wtViewer) SendControlJSON(b json.RawMessage) error {
	return v.ctrlWrite(b)
}

func (v *wtViewer) Close() { _ = v.sess.CloseWithError(0, "bye") }
