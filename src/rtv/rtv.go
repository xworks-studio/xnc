// rtv.go — RTV 中继组装：三腿（host QUIC / 浏览器 WT / 浏览器 WS）
// + Hub + PlaneHost 宿主缝。TLS 证书经 TLSConfig 回调获取（ACME 证书管理
// 见 cert.go，热加载由回调天然支持——每次新监听/新连接取最新配置）。
// relay-plane：嵌入 relay-0（server 进程内）与独立 xnc-relay 进程共用本
// 组装，差异只在 PlaneHost 实现与 RelayID。
package rtv

import (
	"crypto/tls"
	"sync"
)

// EmbeddedRelayID 内嵌中继（relay-0）的 rid：server 进程内装配时票据
// rid claim 与 Options.RelayID 缺省都取此值。
const EmbeddedRelayID = "rl-0"

// PlaneHost 媒体面宿主缝（relay-plane spec §4.1，取代原 router 的五处
// 注入：AuthViewer/Touch/HostTokenOf/InputGate/ControlHooks）：
//   - VerifyToken：票据离线验签（SimpleHost = Verifier；server 进程与
//     relay 进程同构，签发侧私钥绝不出宿主）；
//   - Touch：会话活跃上报（server = session manager idle/lease 判据；
//     relay = no-op 或控制连接节流上报，间隔须 < 租约 TTL）；
//   - SessionEvent：生命周期事件上行（审计/观测）。
type PlaneHost interface {
	VerifyToken(token string) (Ticket, error)
	Touch(session string)
	SessionEvent(typ, node, session string)
}

// ViewerBinding 连接期验签产物：viewer 票据 claims → 节点/会话绑定 +
// 能力（仲裁与门控的准入判据，relay 不做任何角色计算）。
type ViewerBinding struct {
	Node    string // 节点 UUID 字符串（Hub 键）
	Session string // XNC 会话 id（input 门控键）
	Name    string // 显示名（controlState 广播）
	Control bool   // cap.control
	Input   bool   // cap.input
}

// AuthError 是 viewer 鉴权失败（HTTP 状态 + 文本）。
type AuthError struct {
	Status  int
	Message string
}

// Server RTV 中继（host QUIC + WT + WS 三腿共享一个 Hub）。
type Server struct {
	Hub *Hub
	// Host 宿主缝（鉴权/活跃/事件）。
	Host PlaneHost

	// TLSConfig 返回给定 ALPN 的 TLS 配置（证书热加载：每次调用取最新）。
	TLSConfig func(alpn []string) *tls.Config

	// WSOrigins WS 腿允许的 Origin 模式（空 = coder/websocket 同源校验，
	// 生产域名经 caddy 同源反代时即正确语义）。
	WSOrigins []string

	// RelayID 本中继的 rid：票据 rid claim 必须匹配（跨 relay 连接被
	// 拒绝/触发 redirect 的判据）。
	RelayID string

	wtAddr   string // UDP（如 ":443"）
	hostAddr string // host 腿 QUIC（如 ":4433"）

	// wtAllowOrigins 额外放行 Origin 精确匹配表（见 Options.WTAllowOrigins）。
	wtAllowOrigins map[string]bool

	// 实际监听地址（":0" → ephemeral 后回填；测试读取）。
	addrMu       sync.Mutex
	hostAddrInfo string
	wtAddrInfo   string
}

// ActualAddrs 返回两腿的实际监听地址（Start 后可用；测试与观测用）。
func (s *Server) ActualAddrs() (host, wt string) {
	s.addrMu.Lock()
	defer s.addrMu.Unlock()
	return s.hostAddrInfo, s.wtAddrInfo
}

// Options 构造参数（源自宿主 config；地址空 = 不启对应腿）。
type Options struct {
	HostAddr string // host 腿 QUIC 监听（如 ":4433"）
	WTAddr   string // 浏览器 WT 腿 UDP 监听（如 ":443"）
	RelayID  string // 本中继 rid（空 = EmbeddedRelayID）
	// WTAllowOrigins WT 腿放行的额外 Origin（形如 https://xnc.app，与
	// Origin 头精确匹配；空 = 仅同 host——嵌入 relay-0 的现状语义）。
	// 跨 host 外部 relay 必配：页面 Origin 是主站域而请求 Host 是
	// relay 地址，同 host 校验必拒；会话票据才是本腿真实凭据，Origin
	// 仅防跨站冒用。
	WTAllowOrigins []string
}

// New 组装 RTV 服务器（不监听；Start 才绑端口）。
func New(opts Options, tlsCfg func(alpn []string) *tls.Config,
	host PlaneHost, wsOrigins []string) *Server {
	if opts.RelayID == "" {
		opts.RelayID = EmbeddedRelayID
	}
	origins := map[string]bool{}
	for _, o := range opts.WTAllowOrigins {
		if o != "" {
			origins[o] = true
		}
	}
	s := &Server{Hub: NewHub(), Host: host, TLSConfig: tlsCfg,
		WSOrigins: wsOrigins, RelayID: opts.RelayID, wtAllowOrigins: origins}
	s.Hub.events = host.SessionEvent
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
	return nil
}

// validateHostHello host 腿注册校验（hello{nodeId, token}）：票据必须是
// host 张、绑定该节点且 rid 匹配本机。取代原 Hub 内存 minted 表 +
// 活跃会话回查（无状态验签，server 重启不再需要恢复路径）。
func (s *Server) validateHostHello(nodeID, token string) bool {
	if nodeID == "" || token == "" {
		return false
	}
	tk, err := s.Host.VerifyToken(token)
	return err == nil && tk.Typ == TicketHost &&
		tk.NodeID == nodeID && tk.RelayID == s.RelayID
}

// authViewer viewer 腿连接期鉴权（?token=）：viewer 张 + rid 匹配 → 绑定。
func (s *Server) authViewer(token string) (ViewerBinding, *AuthError) {
	tk, err := s.Host.VerifyToken(token)
	if err != nil || tk.Typ != TicketViewer || tk.RelayID != s.RelayID {
		return ViewerBinding{}, &AuthError{Status: 401, Message: "invalid viewer ticket"}
	}
	return ViewerBinding{
		Node: tk.NodeID, Session: tk.SessionID, Name: tk.Name,
		Control: tk.Control, Input: tk.Input,
	}, nil
}

// KillSession 撤销（relay-plane spec §3.4）：宿主墓碑（SimpleHost 实现
// Kill）+ 断开该键全部连接。session 为空 = 节点级撤销（host + 全 viewer）。
func (s *Server) KillSession(node, session, reason string) {
	if k, ok := s.Host.(interface{ Kill(node, session string) }); ok {
		k.Kill(node, session)
	}
	s.Hub.dropForKill(node, session)
	s.Host.SessionEvent("session.kill", node, session)
}
