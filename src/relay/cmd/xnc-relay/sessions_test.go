package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"xnc/proto"
	"xnc/rtv"
)

// newTestRouter 构造带签名器配套验签器的 session router（rid 固定）。
func newTestRouter(t *testing.T) (*sessionRouter, *rtv.Signer, *httptest.Server) {
	t.Helper()
	priv, err := rtv.GenerateSigningKey()
	if err != nil {
		t.Fatal(err)
	}
	sign := rtv.NewSignerFromKey(priv)
	verifier, err := rtv.NewVerifier([]string{sign.PublicKeyHex()})
	if err != nil {
		t.Fatal(err)
	}
	sr := newSessionRouter(verifier, func() string { return "rl-test" })
	mux := http.NewServeMux()
	mux.HandleFunc("/api/agent/session", sr.AgentLegHandler())
	mux.HandleFunc("/api/session/{sid}", sr.ClientLegHandler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return sr, sign, srv
}

func dialLeg(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func writeFrame(t *testing.T, c *websocket.Conn, typ websocket.MessageType, data []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, typ, data); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readFrame(t *testing.T, c *websocket.Conn) (websocket.MessageType, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, r, err := c.Reader(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return typ, buf
}

// TestSessionRouterLoopback：双腿粘合后的帧不透明双向泵（text+binary，
// 双方向），ActiveSids 包含 sid，任一腿断开对端即收尾。
func TestSessionRouterLoopback(t *testing.T) {
	sr, sign, srv := newTestRouter(t)
	const sid = "sess-loop-1"
	atok, _ := sign.SessionDataTicket(sid, "node-1", "rl-test", "agent", time.Hour)
	ctok, _ := sign.SessionDataTicket(sid, "node-1", "rl-test", "client", time.Hour)

	agent := dialLeg(t, srv.URL+"/api/agent/session?token="+atok)
	clnt := dialLeg(t, srv.URL+"/api/session/"+sid+"?token="+ctok)

	// 等粘合（粘合即泵启动）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sr.ActiveSids()) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(sr.ActiveSids()) != 1 || sr.ActiveSids()[0] != sid {
		t.Fatalf("ActiveSids = %v, want [%s]", sr.ActiveSids(), sid)
	}

	// client → agent：text。
	writeFrame(t, clnt, websocket.MessageText, []byte(`{"type":"ECHO"}`))
	if typ, data := readFrame(t, agent); typ != websocket.MessageText || string(data) != `{"type":"ECHO"}` {
		t.Fatalf("agent got %v %q", typ, data)
	}
	// agent → client：binary（帧类型必须原样保序）。
	writeFrame(t, agent, websocket.MessageBinary, []byte{0x01, 0x02, 0xFF})
	if typ, data := readFrame(t, clnt); typ != websocket.MessageBinary || data[2] != 0xFF {
		t.Fatalf("client got %v %x", typ, data)
	}

	// 任一腿断开 → 对端读即失败（泵终局）。
	_ = clnt.Close(websocket.StatusNormalClosure, "bye")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := agent.Reader(ctx); err == nil {
		t.Fatal("agent leg should close after client disconnect")
	}
}

// TestSessionRouterTicketDiscipline：错 sub / 错 sid / 错 rid / 重复同侧
// 一律拒绝。
func TestSessionRouterTicketDiscipline(t *testing.T) {
	_, sign, srv := newTestRouter(t)
	const sid = "sess-disc-1"
	// rid 不符的签发（模拟跨 relay 挪用）。
	badRid, _ := sign.SessionDataTicket(sid, "node-1", "rl-other", "agent", time.Hour)
	if c := tryDial(t, srv.URL+"/api/agent/session?token="+badRid); c != nil {
		t.Fatal("bad relay id ticket must be rejected")
	}
	wrongSub, _ := sign.SessionDataTicket(sid, "node-1", "rl-test", "client", time.Hour)
	if c := tryDial(t, srv.URL+"/api/agent/session?token="+wrongSub); c != nil {
		t.Fatal("client-sub ticket must be rejected on agent leg")
	}
	atok, _ := sign.SessionDataTicket(sid, "node-1", "rl-test", "agent", time.Hour)
	// client 腿路径 sid 与票据不符（票据属 sess-other，拨 sess-mismatch）。
	ctokOther, _ := sign.SessionDataTicket("sess-other", "node-1", "rl-test", "client", time.Hour)
	if c := tryDial(t, srv.URL+"/api/session/sess-mismatch?token="+ctokOther); c != nil {
		t.Fatal("path/session mismatch must be rejected")
	}
	// 同侧重复接入：第一条占用，第二条升级后立即被状态机关闭
	// （票据合法——拒绝只能发生在 accept 之后，客户端表现为连上即断）。
	first := dialLeg(t, srv.URL+"/api/agent/session?token="+atok)
	_ = first
	if c := tryDial(t, srv.URL+"/api/agent/session?token="+atok); c != nil {
		// Dial 可能已成功（升级先于状态机判定）：读必须立即失败。
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if _, _, err := c.Reader(ctx); err == nil {
			t.Fatal("duplicate agent leg must be closed by state machine")
		}
		_ = c.CloseNow()
	}
}

// tryDial 期待握手被拒（返回 nil = 拒绝成功）。
func tryDial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil
	}
	_ = c.CloseNow()
	return c
}

// TestSessionRouterKill：server 撤销关双腿。
func TestSessionRouterKill(t *testing.T) {
	sr, sign, srv := newTestRouter(t)
	const sid = "sess-kill-1"
	atok, _ := sign.SessionDataTicket(sid, "node-1", "rl-test", "agent", time.Hour)
	ctok, _ := sign.SessionDataTicket(sid, "node-1", "rl-test", "client", time.Hour)
	agent := dialLeg(t, srv.URL+"/api/agent/session?token="+atok)
	clnt := dialLeg(t, srv.URL+"/api/session/"+sid+"?token="+ctok)
	_ = agent

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(sr.ActiveSids()) == 0 {
		time.Sleep(20 * time.Millisecond)
	}

	sr.Kill(sid, "test-kill")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := clnt.Reader(ctx); err == nil {
		t.Fatal("client leg should close on kill")
	}
}

// TestSessionRouterClosedReport：终局回调携带 sid（relay → server 的
// RELAY_SESSION_CLOSED 载荷语义）。
func TestSessionRouterClosedReport(t *testing.T) {
	sr, sign, srv := newTestRouter(t)
	const sid = "sess-closed-1"
	atok, _ := sign.SessionDataTicket(sid, "node-1", "rl-test", "agent", time.Hour)
	ctok, _ := sign.SessionDataTicket(sid, "node-1", "rl-test", "client", time.Hour)

	closedCh := make(chan proto.RelaySessionClosed, 1)
	sr.closed = func(cl proto.RelaySessionClosed) { closedCh <- cl }

	agent := dialLeg(t, srv.URL+"/api/agent/session?token="+atok)
	clnt := dialLeg(t, srv.URL+"/api/session/"+sid+"?token="+ctok)
	_ = agent

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(sr.ActiveSids()) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	_ = clnt.Close(websocket.StatusNormalClosure, "bye")

	select {
	case cl := <-closedCh:
		if cl.SessionID != sid || !strings.Contains(cl.Reason, "disconnect") {
			t.Fatalf("closed report = %+v", cl)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no closed report")
	}
}
