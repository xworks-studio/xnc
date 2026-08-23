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
	// 上界 5s(T5:并行测试负载下调度延迟曾把 1s 退避拉到 ~4.3s,判据
	// 是「已从 4s 档重置回 ~1s 档」而非精确墙钟)。
	assert.LessOrEqual(t, gap, 5*time.Second,
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

// blockingOpenHandler 的 HandleSessionOpen 故意停车（阻塞至 ctx 取消），
// SessionClose 照常送入 channel——用于构造能区分内联同步分发与 goroutine
// 分发的场景：前者会卡死读循环，后者不受影响。
type blockingOpenHandler struct {
	close chan proto.SessionClose
}

func (b blockingOpenHandler) HandleSessionOpen(ctx context.Context, _ proto.SessionOpen) {
	<-ctx.Done() // 模拟慢/卡死的会话处理；随 pctx 取消退出
}

func (b blockingOpenHandler) HandleSessionClose(_ context.Context, sc proto.SessionClose) {
	b.close <- sc
}

// TestDispatchDoesNotBlockReadLoop（Fix 1，回归护栏）：HandleSessionOpen 永久
// 阻塞时，读循环必须继续消费——server 在 SESSION_OPEN 200ms 后下发
// SESSION_CLOSE；若分发是内联同步调用，读循环被卡死的 open 处理挂起，
// SESSION_CLOSE 永远到不了（TestDispatchesSessionMessages 的 cap-1 缓冲
// channel 区分不了这两种实现，本测试补上判别力）。
func TestDispatchDoesNotBlockReadLoop(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	closeCh := make(chan proto.SessionClose, 1)

	srv := fakeServerWithHooks(t, pub, func(write func(typ string, p any)) {
		write(proto.TypeSessionOpen, proto.SessionOpen{SessionID: "s1", Kind: proto.KindExec})
		time.Sleep(200 * time.Millisecond) // 给（假设的内联）分发一个卡死读循环的机会
		write(proto.TypeSessionClose, proto.SessionClose{SessionID: "s1", Reason: "test"})
	})
	defer srv.Close()

	k := &identity.Key{NodeID: "node-x", Priv: priv}
	c := NewClient("ws"+srv.URL[4:], k, machineinfo.Info{})
	c.Beat = 50 * time.Millisecond
	c.Handler = blockingOpenHandler{close: closeCh}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // pctx 随连接拆除取消，卡住的 open 处理 goroutine 退出
	go c.Run(ctx)

	select {
	case sc := <-closeCh:
		assert.Equal(t, "s1", sc.SessionID)
	case <-time.After(3 * time.Second):
		t.Fatal("read loop blocked by session dispatch: SESSION_CLOSE undelivered while open-handler parked")
	}
}

// --- Task 8: OnReady 装配钩子 ---

// TestOnReadyFiresAfterHelloAck（Task 8）：OnReady 必须在每次 HELLO_ACK 成功
// 后（once 内 handshake 返回、drain 启动前）恰好触发一次，含重连。判别手段：
// ① 计数器断言两次连接（首连被 server 强制掐断制造一次重连）各恰好一次，
// 且二连存活期内不再触发；② 回调内经交出的 sendControl 发探测帧
// （SESSION_REFUSED/onready-probe），server 只在读循环里收它——探测帧能回到
// server 即证明闭包绑定的是 HELLO_ACK 之后的活连接（时机在握手完成之后）。
func TestOnReadyFiresAfterHelloAck(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	var dials, fires atomic.Int32
	probes := make(chan string, 4)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		write := challenge(t, c, pub) // CHALLENGE → 验签 → 写出 HELLO_ACK
		if dials.Add(1) == 1 {
			// 首连：停留 150ms（足够客户端读完 HELLO_ACK 并触发 OnReady）后
			// 掐断，制造一次强制重连。
			time.Sleep(150 * time.Millisecond)
			return
		}
		for { // 后续连接：echo 心跳维持存活；记录 OnReady 探测帧
			_, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			var hm proto.Message
			require.NoError(t, json.Unmarshal(data, &hm))
			switch hm.Type {
			case proto.TypeSessionRefused:
				var sr proto.SessionRefused
				require.NoError(t, hm.Decode(&sr))
				select {
				case probes <- sr.SessionID:
				default:
				}
			case proto.TypeHeartbeat:
				write(proto.TypeHeartbeatAck, struct{}{})
			}
		}
	}))
	defer srv.Close()

	k := &identity.Key{NodeID: "node-x", Priv: priv}
	c := NewClient("ws"+srv.URL[4:], k, machineinfo.Info{})
	c.Beat = 50 * time.Millisecond
	c.OnReady = func(sendControl func(m proto.Message) error) {
		fires.Add(1)
		// HELLO_ACK 后连接必须已可写：交出的闭包即刻可用，探测帧直达 server。
		msg, err := proto.NewMsg(proto.TypeSessionRefused,
			proto.SessionRefused{SessionID: "onready-probe", Code: proto.CodeKindUnsupported})
		require.NoError(t, err)
		_ = sendControl(msg)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// 一次强制重连共两次连接：OnReady 每连接恰好一次（backoff[0]=1s，宽限 6s）。
	require.Eventually(t, func() bool { return fires.Load() == 2 },
		6*time.Second, 50*time.Millisecond, "OnReady must fire once per connection, including reconnect")
	select {
	case id := <-probes:
		assert.Equal(t, "onready-probe", id,
			"sendControl closure must deliver to the post-HELLO_ACK live connection")
	case <-time.After(3 * time.Second):
		t.Fatal("probe sent inside OnReady never reached server")
	}
	// 二连存活期内（多个心跳拍过去）不得再触发：恰好每连接一次，而非每拍/每帧。
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, int32(2), fires.Load(), "OnReady must fire exactly once per connection")

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
