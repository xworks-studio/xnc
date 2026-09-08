// xnc-relay — RTV 外部中继（relay-plane P1，spec 2026-09-08 §4.2）。
//
// 形态：单静态二进制 + systemd，裸跑（无 docker）。进程内组装 xnc/rtv 的
// 三腿（host QUIC + viewer WT；纯 IP 模式不开 WS 腿——浏览器无法对裸 IP
// 钉扎，兜底由主站 relay-0 承担），经控制连接（wss → 主站 /api/relay/connect，
// 经 caddy TCP443）完成注册-挑战-应答准入、心跳、统计上报、
// RELAY_CONFIG（server 票据签名公钥，离线验票的信任根）与
// RELAY_SESSION_KILL（本地墓碑 + 断连）。
//
// 纯 IP 自签模式：WT 腿用进程内自签证书，certSha256（DER 的 SHA-256）
// 随注册端点下发——浏览器 serverCertificateHashes 钉扎，免备案约束下的
// 唯一浏览器可用形态（spec §4.2）。
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
	"flag"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"os/signal"
	"path/filepath"
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
		maxSessions = flag.Int("max-sessions", 100, "容量声明：并发会话上限（打分用）")
		maxMbpsOut  = flag.Int("max-mbps-out", 500, "容量声明：出带宽上限 Mbps")
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

	// 自签证书 + 钉扎指纹（纯 IP 模式的浏览器可用性来源）。
	tlsProv, certSHA := selfSignedTLS()

	// 验票器：bootstrap 态空钥（RELAY_CONFIG 到达前全拒——无会话无害）。
	verifier, err := rtv.NewVerifier(nil)
	if err != nil {
		log.Error("verifier", "err", err)
		os.Exit(1)
	}
	host := &rtv.SimpleHost{Verifier: verifier}

	srv := rtv.New(rtv.Options{
		HostAddr: *hostAddr, WTAddr: *wtAddr,
		RelayID: relayID, // 空则首个合法 host hello 之前由控制连接回填
	}, tlsProv, host, nil)
	if err := srv.Start(); err != nil {
		log.Error("legs start", "err", err)
		os.Exit(1)
	}

	endpoints := []proto.EndpointDesc{
		{Transport: "wt", Host: *publicHost, Port: *wtPort, Path: "/wt", CertSHA256: certSHA},
		{Transport: "quic", Host: *publicHost, Port: *hostPort, ALPN: rtv.HostALPN},
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go controlLoop(ctx, *serverURL, relayID, pubHex, priv, endpoints,
		*region, *maxSessions, *maxMbpsOut, srv, verifier, *dataDir)

	<-ctx.Done()
	log.Info("xnc-relay shutting down")
}

// controlLoop 控制连接：注册 → 挑战-应答 → 服务（心跳/统计/配置/击杀）。
// 断线指数退避重连（2s → 30s），server 重启即重连重注册（软状态重建）。
func controlLoop(ctx context.Context, serverURL, relayID, pubHex string, priv ed25519.PrivateKey,
	endpoints []proto.EndpointDesc, region string, maxSessions, maxMbpsOut int,
	srv *rtv.Server, verifier *rtv.Verifier, dataDir string,
) {
	backoff := 2 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		if err := controlSession(ctx, serverURL, relayID, pubHex, priv, endpoints,
			region, maxSessions, maxMbpsOut, srv, verifier, dataDir); err != nil {
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
	srv *rtv.Server, verifier *rtv.Verifier, dataDir string,
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

	// ③ 服务循环 + 心跳/统计上报。
	stats := newStatsReporter(srv, func(st proto.RelayStats) {
		_ = send(proto.Message{Type: proto.TypeRelayStats, Payload: mustJSON(st)})
	})
	defer stats.stop()
	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()
	for {
		readCtx, readCancel := context.WithTimeout(ctx, 90*time.Second)
		typ, data, err := c.Read(readCtx)
		readCancel()
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
				log.Info("session killed", "node", k.NodeID, "reason", k.Reason)
			}
		case proto.TypeRelayHeartbeatAck:
			// 无载荷。
		case proto.TypeError:
			log.Warn("server error frame", "payload", string(msg.Payload))
		}
		select {
		case <-heartbeat.C:
			_ = send(proto.Message{Type: proto.TypeRelayHeartbeat,
				Payload: mustJSON(proto.RelayHeartbeat{ClockUnix: time.Now().Unix()})})
		default:
		}
	}
}

// ---------------- 统计上报（10s 差分） ----------------

type statsReporter struct {
	srv   *rtv.Server
	send  func(proto.RelayStats)
	stopF chan struct{}
	once  sync.Once
}

func newStatsReporter(srv *rtv.Server, send func(proto.RelayStats)) *statsReporter {
	r := &statsReporter{srv: srv, send: send, stopF: make(chan struct{})}
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
			// 活跃 sid 集：server 侧代触碰（粘合/idle 旁路，见 pool 注释）。
			active := make([]string, 0)
			for sid := range r.srv.Hub.ActiveSessions() {
				active = append(active, sid)
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

// selfSignedTLS 自签证书（纯 IP 模式）：进程生命周期内固定，指纹随注册
// 端点下发（浏览器 serverCertificateHashes / host cfg certSha256 钉扎）。
func selfSignedTLS() (func([]string) *tls.Config, string) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Error("cert keygen", "err", err)
		os.Exit(1)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "xnc-relay"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(825 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		log.Error("cert create", "err", err)
		os.Exit(1)
	}
	sum := sha256.Sum256(der)
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	return func(alpn []string) *tls.Config {
		return &tls.Config{
			Certificates: []tls.Certificate{cert},
			NextProtos:   alpn,
			MinVersion:   tls.VersionTLS13,
		}
	}, hex.EncodeToString(sum[:])
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
