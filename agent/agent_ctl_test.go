package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/agent/binding"
	"xnc/agent/connect"
	"xnc/agent/identity"
	"xnc/proto"
)

// testCtlPipe 进程内唯一的测试管道名（避免占用生产 \\.\pipe\xnc-agentctl）。
func testCtlPipe(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf(`\\.\pipe\xnc-agentctl-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
}

// fakeRegisterServer 实现 Task 2 端点形状（POST /api/clusters/{id}/nodes/register），
// 记录最近一次请求（Authorization 头与 body），按 status/code 回应。
type fakeRegisterServer struct {
	mu       sync.Mutex
	srv      *httptest.Server
	auth     string
	body     map[string]string
	respCode int
	respBody string
}

func newFakeRegisterServer(t *testing.T) *fakeRegisterServer {
	t.Helper()
	f := &fakeRegisterServer{respCode: http.StatusCreated,
		respBody: `{"nodeId":"n-1","clusterId":"c-1","name":"h"}`}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/clusters/c-1/nodes/register", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&f.body)
		code, body := f.respCode, f.respBody
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRegisterServer) lastAuth() string { f.mu.Lock(); defer f.mu.Unlock(); return f.auth }
func (f *fakeRegisterServer) lastBody() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.body
}

// register 全链路：identity 生成（agent 进程内）→ Task 2 端点（JWT 进头）→
// NodeID 回写 identity → binding 落盘 → 返回 nodeId。
func TestCtlRegister(t *testing.T) {
	f := newFakeRegisterServer(t)
	dir := t.TempDir()
	a := &Agent{StateDir: dir}

	nodeID, err := a.Register(t.Context(), f.srv.URL, "c-1", "jwt-abc")
	require.NoError(t, err)
	assert.Equal(t, "n-1", nodeID)

	assert.Equal(t, "Bearer jwt-abc", f.lastAuth())
	body := f.lastBody()
	for _, k := range []string{"hostname", "machineId", "osVersion", "agentVersion", "publicKey"} {
		assert.NotEmpty(t, body[k], "field %s", k)
	}
	pub, err := base64.StdEncoding.DecodeString(body["publicKey"])
	require.NoError(t, err)
	assert.Len(t, pub, ed25519.PublicKeySize)

	// identity：公钥与请求一致，NodeID 已回写。
	k, err := identity.Load(filepath.Join(dir, "identity.json"))
	require.NoError(t, err)
	assert.Equal(t, "n-1", k.NodeID)
	assert.Equal(t, body["publicKey"], k.PublicKeyB64())

	// binding：server/cluster/node 齐备。
	b, ok, err := binding.Load(dir)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, f.srv.URL, b.Server)
	assert.Equal(t, "c-1", b.ClusterID)
	assert.Equal(t, "n-1", b.NodeID)
}

// register 复用既有 identity（重复注册同一 machine：公钥不变）。
func TestCtlRegisterReusesIdentity(t *testing.T) {
	f := newFakeRegisterServer(t)
	a := &Agent{StateDir: t.TempDir()}

	_, err := a.Register(t.Context(), f.srv.URL, "c-1", "j")
	require.NoError(t, err)
	first := f.lastBody()["publicKey"]

	_, err = a.Register(t.Context(), f.srv.URL, "c-1", "j2")
	require.NoError(t, err)
	assert.Equal(t, first, f.lastBody()["publicKey"])
}

// register 失败（409 冲突）：错误串 "<code>: <message>" 直通，不落 binding，
// 但已生成的 identity 保留（重注册复用）。
func TestCtlRegisterServerError(t *testing.T) {
	f := newFakeRegisterServer(t)
	f.mu.Lock()
	f.respCode = http.StatusConflict
	f.respBody = `{"error":{"code":"MACHINE_ID_CONFLICT","message":"machineId already registered in cluster \"other\""}}`
	f.mu.Unlock()
	dir := t.TempDir()
	a := &Agent{StateDir: dir}

	_, err := a.Register(t.Context(), f.srv.URL, "c-1", "jwt-x")
	require.Error(t, err)
	assert.Equal(t, `MACHINE_ID_CONFLICT: machineId already registered in cluster "other"`, err.Error())

	_, ok, err := binding.Load(dir)
	require.NoError(t, err)
	assert.False(t, ok, "no binding on failed register")

	_, kerr := os.Stat(filepath.Join(dir, "identity.json"))
	assert.NoError(t, kerr, "generated identity must survive a failed register")
}

// jwt 不落任何日志（spec §6.2 用后即弃；实现纪律）。
func TestCtlRegisterJWTNeverLogged(t *testing.T) {
	f := newFakeRegisterServer(t)
	logs := captureLogs(t)
	a := &Agent{StateDir: t.TempDir()}
	_, err := a.Register(t.Context(), f.srv.URL, "c-1", "jwt-super-secret")
	require.NoError(t, err)
	assert.NotContains(t, logs.String(), "jwt-super-secret")
}

// register 唤醒空转（wake-not-poll）：默认 5s 轮询间隔下，管道注册写入
// binding 后 idleAwaitRegistration 必须 <1s 返回。
func TestCtlRegisterWakesIdleLoop(t *testing.T) {
	f := newFakeRegisterServer(t)
	a := &Agent{StateDir: t.TempDir()} // 无 server/token：直接进入空转

	got := make(chan *binding.Binding, 1)
	go func() {
		b, err := a.awaitBinding(t.Context())
		if err == nil {
			got <- b
		}
	}()
	time.Sleep(100 * time.Millisecond) // 让空转就位

	start := time.Now()
	_, err := a.Register(t.Context(), f.srv.URL, "c-1", "j")
	require.NoError(t, err)
	select {
	case b := <-got:
		assert.Less(t, time.Since(start), time.Second, "wake must beat the 5s poll")
		assert.Equal(t, f.srv.URL, b.Server)
	case <-time.After(3 * time.Second):
		t.Fatal("idle loop did not wake on register")
	}
}

// fakeControlServer 最小控制面：挑战认证 → HELLO_ACK → 心跳应答；
// 收到 NODE_DELETE 时按 mode 行为（正常关闭 / ERROR 帧后关闭）。
type fakeControlServer struct {
	mu       sync.Mutex
	srv      *httptest.Server
	nodeIDs  map[string]bool // 已见 nodeID（认证到达即记）
	deletes  []string
	errOnDel bool
	auths    int // 完成挑战认证的连接数（重连回归断言用）
}

func newFakeControlServer(t *testing.T) *fakeControlServer {
	t.Helper()
	f := &fakeControlServer{nodeIDs: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeControlServer) handle(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := r.Context()
	write := func(typ string, p any) {
		m, _ := proto.NewMsg(typ, p)
		b, _ := json.Marshal(m)
		_ = c.Write(ctx, websocket.MessageText, b)
	}
	write(proto.TypeChallenge, proto.Challenge{Nonce: "n"})
	// 认证不验签（假面）：读 CHALLENGE_RESPONSE 记 nodeID 即认。
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var m proto.Message
		if json.Unmarshal(data, &m) != nil {
			return
		}
		switch m.Type {
		case proto.TypeChallengeResponse:
			var cr proto.ChallengeResponse
			_ = m.Decode(&cr)
			f.mu.Lock()
			f.nodeIDs[cr.NodeID] = true
			f.auths++
			f.mu.Unlock()
		case proto.TypeHello:
			write(proto.TypeHelloAck, struct{}{})
		case proto.TypeHeartbeat:
			write(proto.TypeHeartbeatAck, struct{}{})
		case proto.TypeNodeDelete:
			f.mu.Lock()
			errOnDel := f.errOnDel
			f.deletes = append(f.deletes, m.Type)
			f.mu.Unlock()
			if errOnDel {
				write(proto.TypeError, proto.ErrorPayload{Code: "INTERNAL", Message: "db down"})
			}
			_ = c.Close(websocket.StatusNormalClosure, "node deleted")
			return
		}
	}
}

func (f *fakeControlServer) deletesSeen() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deletes)
}

// authCount 统计完成挑战认证的连接数（在线周期 + 注销一次性连接 + 任何
// 意外的重连）。
func (f *fakeControlServer) authCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.auths
}

// deregister：NODE_DELETE 走 WS（机器身份），binding 删除，identity 保留。
func TestCtlDeregister(t *testing.T) {
	fc := newFakeControlServer(t)
	dir := t.TempDir()
	require.NoError(t, binding.Save(dir, &binding.Binding{
		Server: fc.srv.URL, ClusterID: "c-1", NodeID: "n-del"}))
	k := identity.Generate()
	k.NodeID = "n-del"
	require.NoError(t, identity.Save(k, filepath.Join(dir, "identity.json")))
	a := &Agent{StateDir: dir}

	require.NoError(t, a.Deregister(t.Context()))
	assert.Equal(t, 1, fc.deletesSeen(), "server must receive NODE_DELETE")

	_, ok, err := binding.Load(dir)
	require.NoError(t, err)
	assert.False(t, ok, "binding.json must be removed")

	_, ierr := os.Stat(filepath.Join(dir, "identity.json"))
	assert.NoError(t, ierr, "identity.json must be kept for re-register reuse")

	st := a.Status()
	assert.Equal(t, "unregistered", st.State)
}

// deregister 前置校验：未注册 → not_registered。
func TestCtlDeregisterNotRegistered(t *testing.T) {
	a := &Agent{StateDir: t.TempDir()}
	err := a.Deregister(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not_registered")
}

// deregister 服务端失败（ERROR 帧）：错误直通（<code>: <message>），binding 保留。
func TestCtlDeregisterServerError(t *testing.T) {
	fc := newFakeControlServer(t)
	fc.mu.Lock()
	fc.errOnDel = true
	fc.mu.Unlock()
	dir := t.TempDir()
	require.NoError(t, binding.Save(dir, &binding.Binding{
		Server: fc.srv.URL, ClusterID: "c-1", NodeID: "n-x"}))
	k := identity.Generate()
	k.NodeID = "n-x"
	require.NoError(t, identity.Save(k, filepath.Join(dir, "identity.json")))
	a := &Agent{StateDir: dir}

	err := a.Deregister(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "INTERNAL")
	assert.Contains(t, err.Error(), "db down")
	_, ok, _ := binding.Load(dir)
	assert.True(t, ok, "binding must be kept when server rejects deletion")
}

// status：无 binding → unregistered（带 version）；有 binding 无连接 →
// registered；控制连接就绪 → online。
func TestCtlStatus(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{StateDir: dir}

	st := a.Status()
	assert.Equal(t, "unregistered", st.State)
	assert.Empty(t, st.NodeID)
	assert.NotEmpty(t, st.Version, "version always reported")

	require.NoError(t, binding.Save(dir, &binding.Binding{
		Server: "https://s", ClusterID: "c-9", NodeID: "n-9"}))
	st = a.Status()
	assert.Equal(t, "registered", st.State)
	assert.Equal(t, "n-9", st.NodeID)
	assert.Equal(t, "https://s", st.Server)
	assert.Equal(t, "c-9", st.ClusterID)

	// online：真 connect.Client 对假控制面。
	fc := newFakeControlServer(t)
	k := identity.Generate()
	k.NodeID = "n-9"
	c := connect.NewClient(fc.srv.URL, k, a.info())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = c.Run(ctx) }()
	require.Eventually(t, func() bool { return c.Connected() }, 5*time.Second, 50*time.Millisecond)
	a.setConn(c)
	assert.Equal(t, "online", a.Status().State)
	cancel()
	require.Eventually(t, func() bool { return a.Status().State != "online" }, 5*time.Second, 50*time.Millisecond)
}

// Run 周期 + rebind 集成：绑定后上线（HELLO 到达假控制面）→ deregister 拆线
// 回空转 → Run 不退出 → ctx 取消正常收线。
func TestRunCycleDeregister(t *testing.T) {
	fc := newFakeControlServer(t)
	dir := t.TempDir()
	require.NoError(t, binding.Save(dir, &binding.Binding{
		Server: fc.srv.URL, ClusterID: "c-1", NodeID: "n-run"}))
	k := identity.Generate()
	k.NodeID = "n-run"
	require.NoError(t, identity.Save(k, filepath.Join(dir, "identity.json")))
	a := &Agent{StateDir: dir, CtlPipeName: testCtlPipe(t)}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- a.Run(ctx) }()

	require.Eventually(t, func() bool { return a.Status().State == "online" },
		10*time.Second, 100*time.Millisecond, "agent must go online after Run")

	require.NoError(t, a.Deregister(t.Context()))
	assert.Equal(t, 1, fc.deletesSeen())
	require.Eventually(t, func() bool { return a.Status().State == "unregistered" },
		5*time.Second, 100*time.Millisecond, "agent must fall back to idle after deregister")

	// 顺序回归陷阱（rebind 必须晚于 binding 删除）：若先发信号，run 循环可能
	// 抢在删除落盘前消费它、重读到仍在的 binding，为已注销节点立刻发起一条
	// 新的认证连接（在线周期 + 注销一次性连接之外的第 3+ 条）。观察窗口内
	// 认证连接数不得增加。
	auths := fc.authCount()
	assert.GreaterOrEqual(t, auths, 2, "online cycle + one-shot deregister connections")
	time.Sleep(700 * time.Millisecond)
	assert.Equal(t, auths, fc.authCount(),
		"no new authenticated connection may appear after deregister")

	// Run 仍在运行（空转等待，不因 rebind 退出）。
	select {
	case err := <-runErr:
		t.Fatalf("Run must not exit after rebind, got %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-runErr:
		assert.True(t, errors.Is(err, context.Canceled), "Run exits with ctx error, got %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}
