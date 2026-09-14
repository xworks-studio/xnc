// xnc-relay — RTV 外部中继（relay-plane，spec 2026-09-08 §4.2；2026-09-11
// Stage A 转正为唯一媒体路径——主站 XNC_RTV_EMBEDDED 默认 false）。
//
// 形态：单静态二进制 + systemd，裸跑（无 docker）。进程内组装 xnc/rtv 的
// host QUIC + viewer WT 两腿 + HTTP 腿（/ws 浏览器 WS 兜底 + /healthz，
// 生产由主机 caddy 前置终结 TLS——浏览器 WS 无法钉扎自签证书，CA 域名
// 是 WS 兜底的前置），经控制连接（wss → 主站 /api/relay/connect，经
// caddy TCP443）完成注册-挑战-应答准入、心跳、统计上报、RELAY_CONFIG
// （server 票据签名公钥，离线验票的信任根）、RELAY_SESSION_KILL（本地
// 墓碑 + 断连）、RELAY_SESSION_GRANT（被动首约下发）与 RELAY_RECONCILE
// （重连对账：上报在服会话集）。
//
// 纯 IP 自签模式：WT 腿用进程内自签证书，certSha256（DER 的 SHA-256）
// 随注册端点下发——浏览器 serverCertificateHashes 钉扎，免备案约束下的
// 唯一浏览器可用形态（spec §4.2）；host 腿同证书，server 侧据此对
// xnc-host 下发 certSha256 钉扎（DesktopParams.CertSHA256）。
//
// 凭据纪律：身份私钥只在 <data-dir>/identity.json（0600），绝不入 argv/
// 日志；日志剥离一切 query string 与票据。
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"xnc/proto"
	"xnc/rtv"
)

var log = slog.New(slog.NewJSONHandler(os.Stdout, nil))

func main() {
	var (
		serverURL   = flag.String("server", "", "控制连接地址（wss://xnc.app/api/relay/connect，经 caddy TCP443）")
		dataDir     = flag.String("data-dir", "/var/lib/xnc-relay", "状态目录（identity/relay id，0600）")
		publicHost  = flag.String("public-host", "", "注册端点的对外地址（纯 IP，如 47.96.83.132）")
		hostAddr    = flag.String("host-addr", ":4433", "host 腿 QUIC 监听地址")
		wtAddr      = flag.String("wt-addr", ":443", "viewer WT 腿监听地址")
		hostPort    = flag.Int("host-port", 4433, "注册端点：host 腿对外端口")
		wtPort      = flag.Int("wt-port", 443, "注册端点：WT 腿对外端口")
		region      = flag.String("region", "", "区域标签（分配打分的区域权重）")
		allowOrigin = flag.String("allow-origin", "", "WT/WS 腿放行的页面 Origin（逗号分隔，如 https://xnc.app）——跨 host 外部 relay 必配：同 host 校验对 主站页面→relay 地址 的连接必拒")
		maxSessions = flag.Int("max-sessions", 100, "容量声明：并发会话上限（打分用）")
		maxMbpsOut  = flag.Int("max-mbps-out", 500, "容量声明：出带宽上限 Mbps")
		// HTTP 腿（2026-09-11 Stage A）：/ws 浏览器 WS 兜底 + /healthz。
		// 生产形态 = 本地明文 + 主机 caddy 前置（ACME/HTTPS，同主站模式）；
		// 无 caddy 部署可用 --http-cert/--http-key 直启 TLS（需 CA 证书——
		// 浏览器 WS 无法钉扎自签证书）。空串关闭 HTTP 腿。
		httpAddr = flag.String("http-addr", "127.0.0.1:8080", "HTTP 腿监听地址（/ws + /healthz + 会话数据腿；空 = 关闭）")
		wsPort   = flag.Int("ws-port", 443, "注册端点：WS 兜底腿对外端口（caddy 前置 = 443）")
		httpCert = flag.String("http-cert", "", "HTTP 腿直启 TLS 证书（PEM；空 = 明文，由 caddy 前置终结 TLS）")
		httpKey  = flag.String("http-key", "", "HTTP 腿直启 TLS 私钥（PEM）")
		// 会话数据面（2026-09-11 Stage B）：exec/shell/file/tunnel 的会话
		// WS 腿挂本 relay。sessionHost = 浏览器/agent 可达的对外域名（经
		// caddy CA 证书）——非空才宣告 sdata 端点（server 见到才会把会话
		// 数据路由到本 relay）；纯 IP 未配域名时不宣告，会话走主站旧路径。
		sessionHost = flag.String("session-host", "", "会话数据腿对外域名（如 r1.xnc.app；空 = 不宣告会话数据面）")
		sessionPort = flag.Int("session-port", 443, "注册端点：会话数据腿对外端口（caddy 前置 = 443）")
	)
	flag.Parse()
	if *serverURL == "" || *publicHost == "" {
		fmt.Fprintln(os.Stderr, "xnc-relay: --server and --public-host are required")
		os.Exit(2)
	}
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		log.Error("data dir", "err", err)
		os.Exit(1)
	}

	// 身份：ed25519 密钥对，load-or-create（0600，绝不入 argv/日志）。
	priv := loadOrCreateIdentity(*dataDir)
	pubHex := hex.EncodeToString(priv.Public().(ed25519.PublicKey))

	// relay id：首注册由 server 分配（挑战回传），此后持久化复用。
	relayID := loadRelayID(*dataDir)

	// 自签证书 + 钉扎指纹（纯 IP 模式的浏览器可用性来源）。有效期 7 天
	// 持久化：WebTransport serverCertificateHashes 钉扎规范要求证书有效
	// 期 ≤14 天（825 天的通用自签会被浏览器拒绝——真机验收踩坑）；剩余
	// <24h 自动重签，指纹随注册端点自动流转到新会话。
	tlsProv, certSHA := loadOrCreateCert(*dataDir)

	// 验票器：bootstrap 态空钥（RELAY_CONFIG 到达前全拒——无会话无害）。
	verifier, err := rtv.NewVerifier(nil)
	if err != nil {
		log.Error("verifier", "err", err)
		os.Exit(1)
	}
	host := &rtv.SimpleHost{Verifier: verifier}

	var allowOrigins []string
	for _, o := range strings.Split(*allowOrigin, ",") {
		if o = strings.TrimSpace(o); o != "" {
			allowOrigins = append(allowOrigins, o)
		}
	}
	srv := rtv.New(rtv.Options{
		HostAddr: *hostAddr, WTAddr: *wtAddr,
		RelayID:        relayID, // 空则首个合法 host hello 之前由控制连接回填
		WTAllowOrigins: allowOrigins,
	}, tlsProv, host, allowOrigins) // WS 兜底腿 Origin 白名单同 WT（跨 host 页面 → relay /ws）
	if err := srv.Start(); err != nil {
		log.Error("legs start", "err", err)
		os.Exit(1)
	}

	// 会话数据路由器（Stage B）：sdata 票据验签 + 双腿粘合泵。rid 经
	// 闭包取（控制连接回填后才可用；空 = 拒绝接入）。Origin 白名单同
	// WT/WS 腿（2026-09-14 修复：此前漏传，浏览器跨 host 腿恒 403）。
	sessRouter := newSessionRouter(verifier, func() string { return srv.RelayID }, allowOrigins)

	endpoints := []proto.EndpointDesc{
		{Transport: "wt", Host: *publicHost, Port: *wtPort, Path: "/wt", CertSHA256: certSHA},
		{Transport: "quic", Host: *publicHost, Port: *hostPort, ALPN: rtv.HostALPN},
	}
	// HTTP 腿（/ws 浏览器兜底 + 会话数据腿 + /healthz）：开启即注册 ws
	// 候选（browser 按 wt→ws 顺序尝试；无 CA 证书的部署 ws 握手必败，
	// viewer 自然回落，无害）。会话数据端点（sdata）仅在 --session-host
	// 给出对外域名时宣告（浏览器/agent WS 均无法钉扎自签——域名 + caddy
	// CA 是数据腿的硬前置）。
	if *httpAddr != "" {
		startHTTPLeg(*httpAddr, *httpCert, *httpKey, srv, sessRouter)
		endpoints = append(endpoints, proto.EndpointDesc{
			Transport: "ws", Host: *publicHost, Port: *wsPort, Path: "/ws",
		})
		if *sessionHost != "" {
			endpoints = append(endpoints, proto.EndpointDesc{
				Transport: "sdata", Host: *sessionHost, Port: *sessionPort,
			})
			log.Info("session data plane advertised", "host", *sessionHost, "port", *sessionPort)
		}
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go controlLoop(ctx, *serverURL, relayID, pubHex, priv, endpoints,
		*region, *maxSessions, *maxMbpsOut, srv, verifier, *dataDir, sessRouter)

	<-ctx.Done()
	log.Info("xnc-relay shutting down")
}

// startHTTPLeg 起 HTTP 腿（/ws 兜底 + 会话数据腿 + /healthz）：默认明文
// 本地口（生产由 caddy 前置终结 TLS）；--http-cert/--http-key 给出则直启
// TLS。会话数据腿挂 /api/agent/session 与 /api/session/{sid}（sdata 票据
// 即凭证，路径与主站同形——agent/CLI 对 URL 形态无假设）。
func startHTTPLeg(addr, certFile, keyFile string, srv *rtv.Server, sessRouter *sessionRouter) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.WSHandler())
	mux.HandleFunc("/api/agent/session", sessRouter.AgentLegHandler())
	mux.HandleFunc("/api/session/{sid}", sessRouter.ClientLegHandler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srvHTTP := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		var err error
		if certFile != "" && keyFile != "" {
			log.Info("http leg (tls)", "addr", addr)
			err = srvHTTP.ListenAndServeTLS(certFile, keyFile)
		} else {
			log.Info("http leg (plain; front with caddy for browser TLS)", "addr", addr)
			err = srvHTTP.ListenAndServe()
		}
		if err != nil && err != http.ErrServerClosed {
			log.Error("http leg failed", "err", err)
		}
	}()
}

// controlLoop 控制连接：注册 → 挑战-应答 → 服务（心跳/统计/配置/击杀）。
// 断线指数退避重连（2s → 30s），server 重启即重连重注册（软状态重建）。
func controlLoop(ctx context.Context, serverURL, relayID, pubHex string, priv ed25519.PrivateKey,
	endpoints []proto.EndpointDesc, region string, maxSessions, maxMbpsOut int,
	srv *rtv.Server, verifier *rtv.Verifier, dataDir string, sessRouter *sessionRouter,
) {
	backoff := 2 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := controlSession(ctx, serverURL, relayID, pubHex, priv, endpoints,
			region, maxSessions, maxMbpsOut, srv, verifier, dataDir, sessRouter); err != nil {
			log.Warn("control session ended", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func controlSession(ctx context.Context, serverURL, relayID, pubHex string, priv ed25519.PrivateKey,
	endpoints []proto.EndpointDesc, region string, maxSessions, maxMbpsOut int,
	srv *rtv.Server, verifier *rtv.Verifier, dataDir string, sessRouter *sessionRouter,
) error {
	dialCtx, dialCancel := context.WithTimeout(ctx, 15*time.Second)
	c, _, err := websocket.Dial(dialCtx, serverURL, nil)
	dialCancel()
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer c.CloseNow()
	c.SetReadLimit(64 * 1024)

	sendMu := sync.Mutex{}
	send := func(m proto.Message) error {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		sendMu.Lock()
		defer sendMu.Unlock()
		wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
		defer wcancel()
		return c.Write(wctx, websocket.MessageText, b)
	}

	// ① 注册。
	reg, _ := proto.NewMsg(proto.TypeRelayRegister, proto.RelayRegister{
		RelayID: relayID, PublicKey: pubHex, Region: region,
		Endpoints: endpoints, MaxSessions: maxSessions, MaxMbpsOut: maxMbpsOut,
		Version: "p1", ClockUnix: time.Now().Unix(),
	})
	if err := send(reg); err != nil {
		return err
	}

	// ② 挑战-应答（对 nonce b64 串签名——与 agentws 同构）。
	rctx, rcancel := context.WithTimeout(ctx, 60*time.Second)
	_, data, err := c.Read(rctx)
	rcancel()
	if err != nil {
		return err
	}
	var msg proto.Message
	if json.Unmarshal(data, &msg) != nil || msg.Type != proto.TypeChallenge {
		return fmt.Errorf("expected challenge, got %s", msg.Type)
	}
	var ch proto.Challenge
	if msg.Decode(&ch) != nil {
		return fmt.Errorf("bad challenge")
	}
	if ch.RelayID != "" && ch.RelayID != relayID {
		if relayID != "" {
			return fmt.Errorf("server assigned different relay id")
		}
		relayID = ch.RelayID // 首注册：持久化分配身份
		saveRelayID(dataDir, relayID)
		srv.RelayID = relayID
		log.Info("relay id assigned", "relayId", relayID)
	}
	sig := ed25519.Sign(priv, []byte(ch.Nonce))
	cr, _ := proto.NewMsg(proto.TypeRelayChallengeResponse, proto.RelayChallengeResponse{
		RelayID: relayID, Signature: sig,
	})
	if err := send(cr); err != nil {
		return err
	}
	log.Info("control session authenticated", "relayId", relayID)

	// 认证通过即对账（RELAY_RECONCILE）：上报在服会话集——server 重启后
	// 重建 sticky/最小会话记录（孤儿收敛的 relay 侧事实源；server 回的
	// Alive 差集墓碑由 P2 接续，本向先通）。
	live := srv.Hub.LiveSessions()
	rl := make([]proto.RelayLiveSession, 0, len(live))
	for _, e := range live {
		rl = append(rl, proto.RelayLiveSession{SessionID: e.SessionID, NodeID: e.NodeID, Viewers: e.Viewers})
	}
	_ = send(proto.Message{Type: proto.TypeRelayReconcile,
		Payload: mustJSON(proto.RelayReconcile{Live: rl})})

	// ③ 服务循环 + 心跳/统计上报。会话数据腿的终局上报（Stage B）与
	// 活跃 sid 集均经本连接；断线即摘回调（控制连接是终局上报的唯一
	// 通道，断线期终局丢弃——server 的 Opening/idle 兜底收）。
	sessRouter.closed = func(cl proto.RelaySessionClosed) {
		_ = send(proto.Message{Type: proto.TypeRelaySessionClosed, Payload: mustJSON(cl)})
	}
	defer func() { sessRouter.closed = nil }()
	stats := newStatsReporter(srv, sessRouter, func(st proto.RelayStats) {
		_ = send(proto.Message{Type: proto.TypeRelayStats, Payload: mustJSON(st)})
	})
	defer stats.stop()
	// 心跳独立 goroutine（2026-09-11 修）：原实现把 ticker 检查挂在读循环
	// 尾部——server 不主动发消息时读阻塞、心跳永不发出，控制连接每 90s
	// 读超时断连重连（journal 周期性 flap 的根因，分配资格随之振荡）。
	hbStop := make(chan struct{})
	defer close(hbStop)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-hbStop:
				return
			case <-t.C:
				_ = send(proto.Message{Type: proto.TypeRelayHeartbeat,
					Payload: mustJSON(proto.RelayHeartbeat{ClockUnix: time.Now().Unix()})})
			}
		}
	}()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return err
		}
		if typ != websocket.MessageText {
			continue
		}
		var msg proto.Message
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.Type {
		case proto.TypeRelayConfig:
			var cfg proto.RelayConfig
			if msg.Decode(&cfg) == nil && len(cfg.SigningPubkeys) > 0 {
				if err := verifier.SetKeys(cfg.SigningPubkeys); err != nil {
					log.Error("set signing keys failed", "err", err)
				} else {
					log.Info("signing pubkeys installed", "count", len(cfg.SigningPubkeys))
				}
			}
		case proto.TypeRelaySessionKill:
			var k proto.RelaySessionKill
			if msg.Decode(&k) == nil {
				srv.KillSession(k.NodeID, k.SessionID, k.Reason)
				sessRouter.Kill(k.SessionID, k.Reason) // 会话数据腿同步终局（Stage B）
				log.Info("session killed", "node", k.NodeID, "reason", k.Reason)
			}
		case proto.TypeRelaySessionGrant:
			// 被动首约（2026-09-11 Stage A）：本地仲裁机空闲时授予——
			// 外部会话与内嵌 relay-0 的首 viewer UX 对齐。
			var g proto.RelaySessionGrant
			if msg.Decode(&g) == nil {
				srv.Hub.Arbiter().Grant(g.NodeID, g.SessionID, g.Holder)
			}
		case proto.TypeRelayHeartbeatAck:
			// 无载荷。
		case proto.TypeError:
			log.Warn("server error frame", "payload", string(msg.Payload))
		}
	}
}

// ---------------- 统计上报（10s 差分） ----------------

type statsReporter struct {
	srv         *rtv.Server
	sessRouter  *sessionRouter
	send        func(proto.RelayStats)
	stopF       chan struct{}
	once        sync.Once
}

func newStatsReporter(srv *rtv.Server, sessRouter *sessionRouter, send func(proto.RelayStats)) *statsReporter {
	r := &statsReporter{srv: srv, sessRouter: sessRouter, send: send, stopF: make(chan struct{})}
	go r.loop()
	return r
}

func (r *statsReporter) loop() {
	var lastRx, lastTx uint64
	last := time.Now()
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-r.stopF:
			return
		case now := <-t.C:
			snap := r.srv.Hub.Snapshot()
			sessions, viewers, rx, tx := 0, 0, uint64(0), uint64(0)
			if hosts, ok := snap["hosts"].([]map[string]any); ok {
				sessions = len(hosts)
				for _, h := range hosts {
					if v, ok := h["viewers"].(int); ok {
						viewers += v
					}
					rx += toU64(h["rxBytes"])
					tx += toU64(h["txBytes"])
				}
			}
			secs := now.Sub(last).Seconds()
			if secs <= 0 {
				secs = 10
			}
			// 活跃 sid 集：RTV viewer 会话 + 会话数据腿（Stage B）——
			// server 侧统一代 Touch（粘合/idle 旁路，见 pool 注释）。
			active := make([]string, 0)
			for sid := range r.srv.Hub.ActiveSessions() {
				active = append(active, sid)
			}
			if r.sessRouter != nil {
				active = append(active, r.sessRouter.ActiveSids()...)
			}
			r.send(proto.RelayStats{
				Sessions: sessions, Viewers: viewers,
				MbpsIn:     float64(rx-lastRx) * 8 / secs / 1e6,
				MbpsOut:    float64(tx-lastTx) * 8 / secs / 1e6,
				ActiveSids: active,
			})
			lastRx, lastTx, last = rx, tx, now
		}
	}
}

func (r *statsReporter) stop() { r.once.Do(func() { close(r.stopF) }) }

func toU64(v any) uint64 {
	switch n := v.(type) {
	case uint64:
		return n
	case int:
		if n < 0 {
			return 0
		}
		return uint64(n)
	}
	return 0
}

// ---------------- 身份 / relay id / 自签证书 ----------------

func loadOrCreateIdentity(dir string) ed25519.PrivateKey {
	path := filepath.Join(dir, "identity.json")
	if b, err := os.ReadFile(path); err == nil {
		var id struct {
			PrivateKey string `json:"privateKey"`
		}
		if json.Unmarshal(b, &id) == nil {
			if raw, err := hex.DecodeString(id.PrivateKey); err == nil && len(raw) == ed25519.PrivateKeySize {
				return ed25519.PrivateKey(raw)
			}
		}
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		log.Error("identity keygen", "err", err)
		os.Exit(1)
	}
	b, _ := json.Marshal(map[string]string{
		"privateKey": hex.EncodeToString(priv),
		"publicKey":  hex.EncodeToString(priv.Public().(ed25519.PublicKey)),
	})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		log.Error("identity persist", "err", err)
		os.Exit(1)
	}
	log.Info("identity generated", "path", path)
	return priv
}

func loadRelayID(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, "relay.json"))
	if err != nil {
		return ""
	}
	var v struct {
		RelayID string `json:"relayId"`
	}
	_ = json.Unmarshal(b, &v)
	return v.RelayID
}

func saveRelayID(dir, id string) {
	b, _ := json.Marshal(map[string]string{"relayId": id})
	_ = os.WriteFile(filepath.Join(dir, "relay.json"), b, 0o600)
}

// loadOrCreateCert 自签证书（纯 IP 模式）：有效期 7 天（WebTransport
// serverCertificateHashes 钉扎要求证书有效期 ≤14 天）、落盘复用（剩余
// >24h）、到期重签。指纹随注册端点下发（浏览器 serverCertificateHashes /
// host cfg certSha256 钉扎）。
func loadOrCreateCert(dir string) (func([]string) *tls.Config, string) {
	certPath := filepath.Join(dir, "wt-cert.pem")
	keyPath := filepath.Join(dir, "wt-key.pem")
	if cert, err := tls.LoadX509KeyPair(certPath, keyPath); err == nil {
		if leaf, err := x509.ParseCertificate(cert.Certificate[0]); err == nil &&
			time.Until(leaf.NotAfter) > 24*time.Hour {
			sum := sha256.Sum256(cert.Certificate[0])
			log.Info("wt cert loaded", "notAfter", leaf.NotAfter.Format(time.RFC3339))
			return tlsProvider(cert), hex.EncodeToString(sum[:])
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Error("cert keygen", "err", err)
		os.Exit(1)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().Unix()),
		Subject:      pkix.Name{CommonName: "xnc-relay"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(7 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		log.Error("cert create", "err", err)
		os.Exit(1)
	}
	// 落盘（0600）；失败不致命——进程内证书仍可用，重启换指纹而已。
	keyDER, _ := x509.MarshalECPrivateKey(key)
	_ = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_ = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600)
	sum := sha256.Sum256(der)
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	log.Info("wt cert generated", "notAfter", tpl.NotAfter.Format(time.RFC3339))
	return tlsProvider(cert), hex.EncodeToString(sum[:])
}

func tlsProvider(cert tls.Certificate) func([]string) *tls.Config {
	return func(alpn []string) *tls.Config {
		return &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   alpn,
			MinVersion:   tls.VersionTLS13,
		}
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
