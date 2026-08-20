package connect

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/agent/identity"
	"xnc/agent/machineinfo"
	"xnc/proto"
)

// fakeServer 实现最小控制面：CHALLENGE → 验签（测试内公钥）→ HELLO → HEARTBEAT echo。
func fakeServer(t *testing.T, pub ed25519.PublicKey, helloOK chan<- struct{}, beats chan<- int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := context.Background()
		write := func(typ string, p any) {
			m, _ := proto.NewMsg(typ, p)
			b, _ := json.Marshal(m)
			_ = c.Write(ctx, websocket.MessageText, b)
		}
		write(proto.TypeChallenge, proto.Challenge{Nonce: "test-nonce"})
		_, data, err := c.Read(ctx)
		require.NoError(t, err)
		var m proto.Message
		require.NoError(t, json.Unmarshal(data, &m))
		var cr proto.ChallengeResponse
		require.NoError(t, m.Decode(&cr))
		require.Equal(t, "node-x", cr.NodeID)
		require.True(t, ed25519.Verify(pub, []byte("test-nonce"), cr.Signature))
		write(proto.TypeHelloAck, struct{}{})
		helloOK <- struct{}{}
		n := 0
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var hm proto.Message
			require.NoError(t, json.Unmarshal(data, &hm))
			if hm.Type == proto.TypeHeartbeat {
				n++
				beats <- n
				write(proto.TypeHeartbeatAck, struct{}{})
			}
		}
	}))
}

func TestClientConnectHeartbeat(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	helloOK := make(chan struct{}, 1)
	beats := make(chan int, 8)
	srv := fakeServer(t, pub, helloOK, beats)

	k := &identity.Key{NodeID: "node-x", Priv: priv}
	c := NewClient("ws"+srv.URL[4:], k, machineinfo.Info{})
	c.Beat = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	select {
	case <-helloOK:
	case <-time.After(3 * time.Second):
		t.Fatal("no HELLO_ACK")
	}
	select {
	case n := <-beats:
		assert.GreaterOrEqual(t, n, 1)
	case <-time.After(3 * time.Second):
		t.Fatal("no heartbeat")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestRunOnce（Task 12）：单次生命周期——握手（dial → 认证 → HELLO_ACK）
// 成功即正常关闭并返回 nil，不进入心跳循环。
func TestRunOnce(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	helloOK := make(chan struct{}, 1)
	beats := make(chan int, 8)
	srv := fakeServer(t, pub, helloOK, beats)
	defer srv.Close()

	k := &identity.Key{NodeID: "node-x", Priv: priv}
	c := NewClient("ws"+srv.URL[4:], k, machineinfo.Info{})
	c.Beat = 50 * time.Millisecond

	done := make(chan error, 1)
	go func() { done <- c.RunOnce(context.Background()) }()

	select {
	case <-helloOK:
	case <-time.After(3 * time.Second):
		t.Fatal("no HELLO_ACK")
	}
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("RunOnce did not return after HELLO_ACK")
	}
}

// --- Fix round 1: drain reader, backoff reset, error diagnostics ---

// challenge 完成挑战-应答（复用 fakeServer 的流程），写 HELLO_ACK 后返回写帧助手。
func challenge(t *testing.T, c *websocket.Conn, pub ed25519.PublicKey) func(typ string, p any) {
	t.Helper()
	ctx := context.Background()
	write := func(typ string, p any) {
		m, _ := proto.NewMsg(typ, p)
		b, _ := json.Marshal(m)
		_ = c.Write(ctx, websocket.MessageText, b)
	}
	write(proto.TypeChallenge, proto.Challenge{Nonce: "test-nonce"})
	_, data, err := c.Read(ctx)
	require.NoError(t, err)
	var m proto.Message
	require.NoError(t, json.Unmarshal(data, &m))
	var cr proto.ChallengeResponse
	require.NoError(t, m.Decode(&cr))
	require.Equal(t, "node-x", cr.NodeID)
	require.True(t, ed25519.Verify(pub, []byte("test-nonce"), cr.Signature))
	write(proto.TypeHelloAck, struct{}{})
	return write
}

// TestDrainDetectsSilentPeer（Fix 1，单元）：对端发一帧后沉默（不关闭），
// drain 必须在读超时内判定死亡并返回——不早于 deadline，不晚于 ~2×deadline。
func TestDrainDetectsSilentPeer(t *testing.T) {
	release := make(chan struct{})
	ack, _ := proto.NewMsg(proto.TypeHeartbeatAck, struct{}{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		b, _ := json.Marshal(ack)
		_ = c.Write(context.Background(), websocket.MessageText, b)
		<-release // 持连接但不发数据：沉默而非关闭
	}))
	defer srv.Close()
	defer close(release)

	ws, _, err := websocket.Dial(context.Background(), "ws"+srv.URL[4:], nil)
	require.NoError(t, err)
	defer ws.CloseNow()

	c := NewClient("", nil, machineinfo.Info{})
	c.Beat = 100 * time.Millisecond // drain 读超时 = max(3×Beat, 1s) = 1s

	start := time.Now()
	dead := make(chan struct{})
	returned := make(chan struct{})
	go func() {
		c.drain(context.Background(), ws, func() { close(dead) })
		close(returned)
	}()

	// 首帧（HEARTBEAT_ACK）被丢弃后对端沉默：deadline 之前不得误判。
	select {
	case <-dead:
		t.Fatalf("drain fired before read deadline: %v", time.Since(start))
	case <-time.After(500 * time.Millisecond):
	}
	select {
	case <-dead:
	case <-time.After(2500 * time.Millisecond):
		t.Fatal("drain did not detect silent peer within ~deadline")
	}
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("drain did not return after declaring dead")
	}
}

// TestClientStaysConnectedUnderAcksAndErrors（Fix 1，回归）：Beat=50ms 下
// 服务端持续 ACK 并每 3 拍注入一个非致命 ERROR 帧，客户端 ≥80 拍内零重连。
func TestClientStaysConnectedUnderAcksAndErrors(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	beats := make(chan int, 512)
	var dials atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dials.Add(1)
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		write := challenge(t, c, pub)
		n := 0
		for {
			_, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			var hm proto.Message
			require.NoError(t, json.Unmarshal(data, &hm))
			if hm.Type == proto.TypeHeartbeat {
				n++
				select {
				case beats <- n:
				default:
				}
				write(proto.TypeHeartbeatAck, struct{}{})
				if n%3 == 0 { // 非致命 ERROR 帧应被记录而非触发重连
					write(proto.TypeError, proto.ErrorPayload{
						Code: proto.CodeInternal, Message: "synthetic"})
				}
			}
		}
	}))
	defer srv.Close()

	k := &identity.Key{NodeID: "node-x", Priv: priv}
	c := NewClient("ws"+srv.URL[4:], k, machineinfo.Info{})
	c.Beat = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	require.Eventually(t, func() bool { return len(beats) >= 80 },
		6*time.Second, 50*time.Millisecond, "not enough heartbeats under ack+ERROR load")
	assert.Equal(t, int32(1), dials.Load(), "client must not reconnect under acks and ERROR frames")
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// TestBackoffResetAfterHealthyConnection（Fix 2）：前两次拨号快速失败（计数到 2），
// 第 3 次连接健康存活 500ms > BackoffReset(200ms) 后突然断开——计数应归零，
// 第 4 次拨号在 ~1s（backoff[0]）后到达；未重置则需等 backoff[2]=5s。
func TestBackoffResetAfterHealthyConnection(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)

	var mu sync.Mutex
	dialTimes := make([]time.Time, 0, 8)
	dial := make(chan int, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		mu.Lock()
		dialTimes = append(dialTimes, time.Now())
		seq := len(dialTimes)
		mu.Unlock()
		dial <- seq
		if seq != 3 {
			return // 接受后立即断开：让 once() 快速失败
		}
		// 第 3 次连接：完整握手，保持 500ms（周期发 ACK 喂 drain）后突然断开。
		write := challenge(t, c, pub)
		hold := time.After(500 * time.Millisecond)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-hold:
				return
			case <-tick.C:
				write(proto.TypeHeartbeatAck, struct{}{})
			}
		}
	}))
	defer srv.Close()

	k := &identity.Key{NodeID: "node-x", Priv: priv}
	c := NewClient("ws"+srv.URL[4:], k, machineinfo.Info{})
	c.Beat = 50 * time.Millisecond
	c.BackoffReset = 200 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	for seq := range dial {
		if seq >= 4 {
			break
		}
	}
	mu.Lock()
	gap := dialTimes[3].Sub(dialTimes[2])
	mu.Unlock()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	assert.GreaterOrEqual(t, gap, 800*time.Millisecond)
	assert.LessOrEqual(t, gap, 4*time.Second,
		"backoff not reset after healthy connection: expected ~1s retry, got slow retry")
}

// --- Task 5: drain 重构为消息分发 ---

// fakeServerWithHooks 在 fakeServer 的握手流程上增加 afterHello 钩子：
// HELLO_ACK 写出后立刻以 write 注入额外 server → agent 帧，随后照常 echo 心跳。
func fakeServerWithHooks(t *testing.T, pub ed25519.PublicKey, afterHello func(write func(typ string, p any))) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		write := challenge(t, c, pub)
		afterHello(write)
		for { // 心跳 echo：维持连接存活，验证分发不阻塞读取循环
			_, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			var hm proto.Message
			require.NoError(t, json.Unmarshal(data, &hm))
			if hm.Type == proto.TypeHeartbeat {
				write(proto.TypeHeartbeatAck, struct{}{})
			}
		}
	}))
}

// stubHandler 把分发的会话消息送入 channel 供测试断言。
type stubHandler struct {
	open  chan proto.SessionOpen
	close chan proto.SessionClose
}

func (s stubHandler) HandleSessionOpen(_ context.Context, so proto.SessionOpen) { s.open <- so }
func (s stubHandler) HandleSessionClose(_ context.Context, sc proto.SessionClose) {
	s.close <- sc
}

// TestDispatchesSessionMessages（Task 5）：HELLO_ACK 后 server 依次下发
// SESSION_OPEN 与 SESSION_CLOSE，两者必须经 Client.Handler 分发到独立
// goroutine（不阻塞心跳/读取循环）并完整到达测试桩。
func TestDispatchesSessionMessages(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	openCh := make(chan proto.SessionOpen, 1)
	closeCh := make(chan proto.SessionClose, 1)

	srv := fakeServerWithHooks(t, pub, func(write func(typ string, p any)) {
		write(proto.TypeSessionOpen, proto.SessionOpen{
			SessionID: "s1", Kind: proto.KindExec, Params: []byte(`{}`)})
		write(proto.TypeSessionClose, proto.SessionClose{SessionID: "s1", Reason: "test"})
	})
	defer srv.Close()

	k := &identity.Key{NodeID: "node-x", Priv: priv}
	c := NewClient("ws"+srv.URL[4:], k, machineinfo.Info{})
	c.Beat = 50 * time.Millisecond
	c.Handler = stubHandler{open: openCh, close: closeCh}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)

	select {
	case so := <-openCh:
		assert.Equal(t, "s1", so.SessionID)
		assert.Equal(t, proto.KindExec, so.Kind)
	case <-time.After(3 * time.Second):
		t.Fatal("no SESSION_OPEN dispatched")
	}
	select {
	case sc := <-closeCh:
		assert.Equal(t, "s1", sc.SessionID)
	case <-time.After(3 * time.Second):
		t.Fatal("no SESSION_CLOSE dispatched")
	}
}
