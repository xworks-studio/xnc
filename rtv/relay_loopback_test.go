// relay_loopback_test.go — RTV 中继回环集成：fake host（QUIC 腿）+ WT
// viewer 腿，覆盖移植语义的关键面：RelayTicket 注册校验（host 张）、
// config 广播、字节扇出、viewer 迁移（host 重启合成 frameLoss）、
// hostOffline 通知、input 门控（仲裁机）、控制权流转（last-take-wins +
// 冷却 + 能力拒绝）。TLS 用进程内自签（DevSelfSigned），客户端
// InsecureSkipVerify——仅验协议语义，Web PKI 由 ACME/文件路径承担。
package rtv

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
)

func tlsInsecure(alpn []string) *tls.Config {
	return &tls.Config{InsecureSkipVerify: true, NextProtos: alpn}
}

// newLoopSrv 起一个回环 Server（ephemeral 端口）+ 测试签发器；验签侧 =
// SimpleHost（与生产同构：relay-0 与 xnc-relay 都用它）。
func newLoopSrv(t *testing.T) (*Server, *Signer) {
	t.Helper()
	priv, err := GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	signer := &Signer{priv: priv, hostCache: map[string]string{}}
	verifier, err := NewVerifier([]string{signer.PublicKeyHex()})
	if err != nil {
		t.Fatal(err)
	}
	s := New(Options{HostAddr: "127.0.0.1:0", WTAddr: "127.0.0.1:0"},
		DevSelfSigned(), &SimpleHost{Verifier: verifier}, nil)
	if err := s.Start(); err != nil {
		t.Fatalf("rtv start: %v", err)
	}
	return s, signer
}

// viewerTok 便捷签发（会话粒度，能力可控）。
func viewerTok(t *testing.T, signer *Signer, sid, nid string, control, input bool) string {
	t.Helper()
	tok, err := signer.ViewerTicket(sid, nid, EmbeddedRelayID, "user-"+sid, control, input, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func dialHost(t *testing.T, addr string) (*quic.Conn, *quic.Stream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	c, err := quic.DialAddr(ctx, addr, tlsInsecure([]string{HostALPN}),
		&quic.Config{EnableDatagrams: true})
	if err != nil {
		t.Fatalf("host dial: %v", err)
	}
	st, err := c.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("host ctrl stream: %v", err)
	}
	return c, st
}

func dialWT(t *testing.T, url string) (*webtransport.Session, *webtransport.Stream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	d := &webtransport.Transport{
		TLSClientConfig: tlsInsecure([]string{"h3"}),
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true},
	}
	_, sess, err := d.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("wt dial: %v", err)
	}
	st, err := sess.OpenStreamSync(ctx)
	if err != nil {
		t.Fatalf("wt ctrl stream: %v", err)
	}
	return sess, st
}

// sendJSON 写 4B LE 长度前缀 JSON 控制帧。
func sendJSON(w io.Writer, v any) {
	b, _ := json.Marshal(v)
	_ = writeCtrlFrame(w, b)
}

// readJSON 阻塞读一帧控制 JSON。
func readJSON(r io.Reader) (map[string]any, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	body := make([]byte, binary.LittleEndian.Uint32(lenBuf[:]))
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	var m map[string]any
	return m, json.Unmarshal(body, &m)
}

// readJSONUntil 读到指定 type（跳过其他帧）或超时。
func readJSONUntil(r io.Reader, d time.Duration, wantType string) (map[string]any, error) {
	deadline := time.Now().Add(d)
	for {
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timeout waiting ctrl type %s", wantType)
		}
		m, err := readJSON(r)
		if err != nil {
			return nil, err
		}
		if m["type"] == wantType {
			return m, nil
		}
	}
}

// mediaPkt 最小合法媒体包头（单 SOF 数据 shard）。
func mediaPkt(frame uint32, payload byte) []byte {
	b := make([]byte, 34+8)
	copy(b[0:4], "MVP1")
	b[4] = 1                                   // SOF
	binary.LittleEndian.PutUint16(b[8:10], 1)  // dataShards k=1
	binary.LittleEndian.PutUint32(b[12:16], 1) // fecBlockTotal
	binary.LittleEndian.PutUint32(b[16:20], frame)
	b[20] = 1 // h264
	b[21] = 1 // keyframe
	binary.LittleEndian.PutUint64(b[24:32], uint64(time.Now().UnixMicro()))
	binary.LittleEndian.PutUint16(b[32:34], 8)
	b[34] = payload
	return b
}

// TestHostTicketValidation — host 张票据校验：空/垃圾/typ 错/节点不匹配/
// rid 不匹配全拒；(node,relay) 粒度跨会话字节等值；墓碑撤销。
func TestHostTicketValidation(t *testing.T) {
	s, signer := newLoopSrv(t)
	tok := signer.HostTicketFor("n1", EmbeddedRelayID)

	if s.validateHostHello("n1", "") || s.validateHostHello("", tok) ||
		s.validateHostHello("n1", "garbage") || s.validateHostHello("n2", tok) {
		t.Fatal("host hello validation must reject empty/garbage/mismatched pairs")
	}
	if !s.validateHostHello("n1", tok) {
		t.Fatal("valid host ticket rejected")
	}
	// viewer 张冒充 host 张 → 拒绝（typ 判据）。
	vt := viewerTok(t, signer, "sX", "n1", true, true)
	if s.validateHostHello("n1", vt) {
		t.Fatal("viewer ticket must not pass host hello validation")
	}
	// (node,relay) 粒度：跨会话字节等值（core cfg 等值复用的前提）。
	if signer.HostTicketFor("n1", EmbeddedRelayID) != tok {
		t.Fatal("host ticket must be byte-equal across sessions on same relay")
	}
	// 不同 relay 的 host 张 → rid 不匹配拒绝。
	tok2 := signer.HostTicketFor("n1", "rl-other")
	if s.validateHostHello("n1", tok2) {
		t.Fatal("ticket for another relay must be rejected")
	}
	// 验签侧错误分类：伪造 → ErrBadToken；墓碑 → ErrKilled。
	if _, err := s.Host.VerifyToken("aaaa..bbbb"); !errors.Is(err, ErrBadToken) {
		t.Fatalf("forged token must be ErrBadToken, got %v", err)
	}
	if k, ok := s.Host.(interface{ Kill(node, session string) }); ok {
		k.Kill("n1", "")
	} else {
		t.Fatal("SimpleHost must implement Kill")
	}
	if _, err := s.Host.VerifyToken(tok); !errors.Is(err, ErrKilled) {
		t.Fatalf("killed ticket must be ErrKilled, got %v", err)
	}
}

// TestTicketExpiry — exp ±leeway：刚签的过，过期+leeway 之外的拒。
func TestTicketExpiry(t *testing.T) {
	priv, err := GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	signer := &Signer{priv: priv, hostCache: map[string]string{}}
	v, err := NewVerifier([]string{signer.PublicKeyHex()})
	if err != nil {
		t.Fatal(err)
	}
	vt, err := signer.ViewerTicket("s1", "n1", EmbeddedRelayID, "u", true, true, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(vt); err != nil {
		t.Fatalf("fresh ticket must verify: %v", err)
	}
	et, _ := signer.ViewerTicket("s1", "n1", EmbeddedRelayID, "u", true, true, -2*ticketLeeway)
	if _, err := v.Verify(et); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired ticket must be ErrExpired, got %v", err)
	}
	lt, _ := signer.ViewerTicket("s1", "n1", EmbeddedRelayID, "u", true, true, -30*time.Second)
	if _, err := v.Verify(lt); err != nil {
		t.Fatalf("ticket inside leeway must verify: %v", err)
	}
}

// TestInputGating — forwardToHost 的 input 门控（仲裁机 + cap.input，
// in-package 直测）。
func TestInputGating(t *testing.T) {
	var mu sync.Mutex
	var sent []map[string]any
	h := &HostSession{NodeID: "smoke-node", viewers: map[uint64]Viewer{}}
	h.sendControl = func(v json.RawMessage) error {
		var m map[string]any
		_ = json.Unmarshal(v, &m)
		mu.Lock()
		sent = append(sent, m)
		mu.Unlock()
		return nil
	}
	hub := &Hub{arbiter: NewArbiter()}
	h.hub = hub
	v := &stubViewer{node: "smoke-node", sess: "sess-A", control: true, input: true}
	noCap := &stubViewer{node: "smoke-node", sess: "sess-B", control: true, input: false}

	inputMsg := mustJSON(map[string]any{"type": "input", "event": "mouse", "kind": "down"})
	h.forwardToHost(inputMsg, v)
	if len(sent) != 0 {
		t.Fatalf("input must be dropped without lease, got %v", sent)
	}
	hub.arbiter.Grant("smoke-node", "sess-A", "t")
	h.forwardToHost(inputMsg, v)
	if len(sent) != 1 || sent[0]["kind"] != "down" {
		t.Fatalf("gated input must pass, got %v", sent)
	}
	// 无 cap.input 的 viewer 即使持约也拒（策略在票上）。
	hub.arbiter.Grant("smoke-node", "sess-B", "t2") // 会覆盖失活约
	h.forwardToHost(inputMsg, noCap)
	if len(sent) != 1 {
		t.Fatalf("input without cap.input must be dropped even with lease, got %v", sent)
	}
	// 非 input 消息不受门控。
	h.forwardToHost(mustJSON(map[string]any{"type": "frameLoss", "reason": "x"}), v)
	if len(sent) != 2 {
		t.Fatalf("non-input ctrl must always forward, got %v", sent)
	}
}

type stubViewer struct {
	node           string
	sess           string
	control, input bool
}

func (v *stubViewer) ID() uint64                            { return 1 }
func (v *stubViewer) Kind() string                          { return "stub" }
func (v *stubViewer) Node() string                          { return v.node }
func (v *stubViewer) Session() string                       { return v.sess }
func (v *stubViewer) CanControl() bool                      { return v.control }
func (v *stubViewer) CanInput() bool                        { return v.input }
func (v *stubViewer) DisplayName() string                   { return "stub-" + v.sess }
func (v *stubViewer) SendDatagram(b []byte) error           { return nil }
func (v *stubViewer) SendControlJSON(json.RawMessage) error { return nil }
func (v *stubViewer) Close()                                {}

// TestRelayLoopback — host 注册 → viewer 绑定 → config 广播 → 媒体字节
// 扇出 → host 重启迁移（frameLoss host-restart）→ hostOffline 通知。
func TestRelayLoopback(t *testing.T) {
	s, signer := newLoopSrv(t)
	hostAddr, wtAddr := s.ActualAddrs()
	if hostAddr == "" || wtAddr == "" {
		t.Fatalf("leg addrs not captured: %q %q", hostAddr, wtAddr)
	}
	wtURL := "https://" + wtAddr + "/wt?token=" + viewerTok(t, signer, "sess-A", "smoke-node", true, true)
	token := signer.HostTicketFor("smoke-node", EmbeddedRelayID)

	// host 注册（读侧独立 goroutine 排空控制回读，避免流控阻塞）。
	conn, st := dialHost(t, hostAddr)
	defer func() { _ = conn.CloseWithError(0, "bye") }()
	sendJSON(st, map[string]any{"type": "hello", "role": "host",
		"nodeId": "smoke-node", "token": token})
	waitFor(t, "host registered", func() bool { return s.Hub.Host("smoke-node") != nil })

	// WT viewer 绑定。
	wtSess, wtSt := dialWT(t, wtURL)
	sendJSON(wtSt, map[string]any{"type": "hello", "role": "viewer"})
	waitFor(t, "viewer bound", func() bool {
		h := s.Hub.Host("smoke-node")
		return h != nil && h.viewerCount() == 1
	})

	// config 广播 → viewer 收到。
	sendJSON(st, map[string]any{"type": "config", "width": 1280, "height": 720,
		"fps": 30, "encoder": "test-enc", "encoderHw": false, "fecPercentage": 20})
	if m, err := readJSONUntil(wtSt, 3*time.Second, "config"); err != nil {
		t.Fatalf("viewer config: %v", err)
	} else if m["encoder"] != "test-enc" {
		t.Fatalf("config mismatch: %v", m)
	}

	// 媒体字节扇出。
	pkt := mediaPkt(1, 0xAB)
	if err := conn.SendDatagram(pkt); err != nil {
		t.Fatalf("send datagram: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got, err := wtSess.ReceiveDatagram(ctx)
	if err != nil {
		t.Fatalf("wt datagram: %v", err)
	}
	if string(got) != string(pkt) {
		t.Fatalf("fanout bytes mismatch: %x != %x", got, pkt)
	}

	// host 重启（同 token 再注册）：viewer 迁移；新 host 收合成
	// frameLoss(host-restart)。
	conn2, st2 := dialHost(t, hostAddr)
	defer func() { _ = conn2.CloseWithError(0, "bye") }()
	sendJSON(st2, map[string]any{"type": "hello", "role": "host",
		"nodeId": "smoke-node", "token": token})
	// RegisterHost 的合成顺序：先 viewers（host 感知迁移后的观众数），
	// 后 frameLoss(host-restart)（触发 IDR）。
	if m, err := readJSONUntil(st2, 3*time.Second, "viewers"); err != nil {
		t.Fatalf("viewers notify: %v", err)
	} else if m["count"] != float64(1) {
		t.Fatalf("migrated viewer count mismatch: %v", m)
	}
	if m, err := readJSONUntil(st2, 3*time.Second, "frameLoss"); err != nil {
		t.Fatalf("host-restart frameLoss: %v", err)
	} else if m["reason"] != "host-restart" {
		t.Fatalf("frameLoss reason mismatch: %v", m)
	}
	// 旧连接关闭（被替换）。
	_ = conn.CloseWithError(0, "replaced")

	// config 重广播 → 迁移后的 viewer 仍在线可收。
	sendJSON(st2, map[string]any{"type": "config", "width": 640, "height": 360,
		"fps": 30, "encoder": "test-enc2", "encoderHw": false, "fecPercentage": 10})
	if m, err := readJSONUntil(wtSt, 3*time.Second, "config"); err != nil {
		t.Fatalf("viewer config after migration: %v", err)
	} else if m["encoder"] != "test-enc2" {
		t.Fatalf("post-migration config mismatch: %v", m)
	}

	// host 真消失 → viewer 收 hostOffline。
	_ = conn2.CloseWithError(0, "bye")
	if m, err := readJSONUntil(wtSt, 5*time.Second, "hostOffline"); err != nil {
		t.Fatalf("hostOffline notify: %v", err)
	} else if m["node"] != "smoke-node" {
		t.Fatalf("hostOffline node mismatch: %v", m)
	}
}

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// recViewer 记录控制下行的 stub（控制权流转测试用）。
type recViewer struct {
	stubViewer
	id2  uint64
	mu   sync.Mutex
	sent []map[string]any
}

func (v *recViewer) ID() uint64 { return v.id2 }
func (v *recViewer) SendControlJSON(b json.RawMessage) error {
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	v.mu.Lock()
	v.sent = append(v.sent, m)
	v.mu.Unlock()
	return nil
}
func (v *recViewer) last() map[string]any {
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.sent) == 0 {
		return nil
	}
	return v.sent[len(v.sent)-1]
}

// TestControlFlow — 控制权流转消息在 relay 终结（本地仲裁机）：
// takeControl/releaseControl 不进 host；成功广播 controlState、冷却拒绝
// 回 controlResult、无能力拒绝回 capability；新 viewer AddViewer 时单发
// 当前归属。
func TestControlFlow(t *testing.T) {
	h := &HostSession{NodeID: "n1", viewers: map[uint64]Viewer{}}
	var hostCtrl []map[string]any
	h.sendControl = func(v json.RawMessage) error {
		var m map[string]any
		_ = json.Unmarshal(v, &m)
		hostCtrl = append(hostCtrl, m)
		return nil
	}
	a := &recViewer{stubViewer: stubViewer{node: "n1", sess: "sess-A", control: true, input: true}, id2: 1}
	b := &recViewer{stubViewer: stubViewer{node: "n1", sess: "sess-B", control: true, input: true}, id2: 2}
	noCap := &recViewer{stubViewer: stubViewer{node: "n1", sess: "sess-C", control: false}, id2: 3}
	hub := &Hub{arbiter: NewArbiter(), hosts: map[string]*HostSession{"n1": h}}
	h.hub = hub
	h.mu.Lock()
	h.viewers[1] = a
	h.viewers[2] = b
	h.mu.Unlock()

	takeMsg := mustJSON(map[string]any{"type": "takeControl"})
	// A 接管 → A/B 均收 controlState(holder=sess-A)，host 不收 takeControl。
	h.forwardToHost(takeMsg, a)
	for _, v := range []*recViewer{a, b} {
		m := v.last()
		if m == nil || m["type"] != "controlState" || m["holderSession"] != "sess-A" {
			t.Fatalf("A take: viewer must get controlState holder=sess-A, got %v", m)
		}
	}
	// A 重取 = 幂等 held。
	h.forwardToHost(takeMsg, a)
	if m := a.last(); m["holderSession"] != "sess-A" {
		t.Fatalf("A re-take must stay holder, got %v", m)
	}
	// B 接管（首个抢占，无冷却）→ holder=sess-B。
	h.forwardToHost(takeMsg, b)
	if m := a.last(); m["holderSession"] != "sess-B" {
		t.Fatalf("B take: A must see holder=sess-B, got %v", m)
	}
	// A 立即抢回 → 冷却拒绝：controlResult + 状态同步，holder 不变。
	h.forwardToHost(takeMsg, a)
	if m := a.last(); m == nil || m["type"] != "controlState" || m["holderSession"] != "sess-B" {
		t.Fatalf("cooldown: A last msg must be controlState holder=sess-B, got %v", m)
	}
	var denied bool
	a.mu.Lock()
	for _, m := range a.sent {
		if m["type"] == "controlResult" && m["ok"] == false && m["reason"] == "cooldown" {
			denied = true
		}
	}
	a.mu.Unlock()
	if !denied {
		t.Fatal("cooldown denial must send controlResult{ok:false,reason:cooldown}")
	}
	// 无 cap.control → capability 拒绝。
	h.forwardToHost(takeMsg, noCap)
	var capDenied bool
	noCap.mu.Lock()
	for _, m := range noCap.sent {
		if m["type"] == "controlResult" && m["ok"] == false && m["reason"] == "capability" {
			capDenied = true
		}
	}
	noCap.mu.Unlock()
	if !capDenied {
		t.Fatal("capability denial must send controlResult{ok:false,reason:capability}")
	}
	// B 释放 → 广播空闲。
	h.forwardToHost(mustJSON(map[string]any{"type": "releaseControl"}), b)
	if m := a.last(); m["holderSession"] != "" {
		t.Fatalf("release: holder must be empty, got %v", m)
	}
	// takeControl/releaseControl 绝不进 host。
	for _, m := range hostCtrl {
		if m["type"] == "takeControl" || m["type"] == "releaseControl" {
			t.Fatalf("control messages must not reach host, got %v", m)
		}
	}
	// AddViewer 单发当前归属（新加入者立即知道 view-only/谁在控）。
	hub.AddViewer("n1", a)
	if m := a.last(); m == nil || m["type"] != "controlState" {
		t.Fatalf("AddViewer must unicast controlState, got %v", m)
	}
}
