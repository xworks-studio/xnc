// rtv.go — RTV 中继服务器组装：三腿（host QUIC / 浏览器 WT / 浏览器 WS）
// + Hub + 鉴权/门控回调。TLS 证书经 TLSConfig 回调获取（ACME 证书管理
// 见 cert.go，热加载由回调天然支持——每次新监听/新连接取最新配置）。
package rtv

import (
	"crypto/tls"
)

// ViewerBinding 连接期鉴权产物：会话 token → 节点/会话绑定。
type ViewerBinding struct {
	Node    string // 节点 UUID 字符串（Hub 键）
	Session string // XNC 会话 id（input 门控键）
}

// AuthError 是 viewer 鉴权失败（HTTP 状态 + 文本，源自 session manager）。
type AuthError struct {
	Status  int
	Message string
}

// Server RTV 中继（host QUIC + WT + WS 三腿共享一个 Hub）。
type Server struct {
	Hub *Hub

	// TLSConfig 返回给定 ALPN 的 TLS 配置（证书热加载：每次调用取最新）。
	TLSConfig func(alpn []string) *tls.Config

	// AuthViewer 鉴权会话 token → 节点/会话绑定；失败回 HTTP 错误。
	// 由 api 层注入（session manager AttachClientRTV + 会话查表）。
	AuthViewer func(token string) (ViewerBinding, *AuthError)

	// WSOrigins WS 腿允许的 Origin 模式（空 = coder/websocket 同源校验，
	// 生产域名经 caddy 同源反代时即正确语义）。
	WSOrigins []string

	// Touch 会话活跃上报（viewer 控制帧 → session manager 的 idle/lease
	// TTL 判据）；可为 nil（无治理）。
	Touch func(session string)

	wtAddr  string // UDP（如 ":443"）
	hostAddr string // host 腿 QUIC（如 ":4433"）
	wsReady bool
}

// Options 构造参数（源自 server config；地址空 = 不启对应腿）。
type Options struct {
	HostAddr string // host 腿 QUIC 监听（如 ":4433"）
	WTAddr   string // 浏览器 WT 腿 UDP 监听（如 ":443"）
}

// New 组装 RTV 服务器（不监听；Start 才绑端口）。
func New(opts Options, tlsCfg func(alpn []string) *tls.Config,
	auth func(token string) (ViewerBinding, *AuthError), wsOrigins []string) *Server {
	s := &Server{Hub: NewHub(), TLSConfig: tlsCfg, AuthViewer: auth, WSOrigins: wsOrigins}
	s.wtAddr = opts.WTAddr
	s.hostAddr = opts.HostAddr
	return s
}

// Start 绑定并启动各腿（非阻塞；失败即返）。
func (s *Server) Start() error {
	if s.hostAddr != "" {
		if err := s.serveHostLeg(s.hostAddr); err != nil {
			return err
		}
	}
	if s.wtAddr != "" {
		if err := s.serveWTLeg(s.wtAddr); err != nil {
			return err
		}
	}
	s.wsReady = true
	return nil
}
