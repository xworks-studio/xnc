//go:build windows

package agentctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var pipeSeq atomic.Int64

// testPipe 生成进程内唯一的测试管道名（并行测试/重跑不冲突）。
func testPipe(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf(`\\.\pipe\xnc-agentctl-test-%d-%d`, os.Getpid(), pipeSeq.Add(1))
}

// fakeDeps 记录调用并返回预设结果的 Deps 桩。
type fakeDeps struct {
	mu           sync.Mutex
	regArgs      []struct{ server, clusterID, jwt string }
	regNodeID    string
	regErr       error
	deregCalls   int
	deregErr     error
	statusVal    Status
	upgChannels  []string
	upgTriggered bool
	upgErr       error
	dspActions   []string
	dspErr       error
}

func (f *fakeDeps) Register(_ context.Context, server, clusterID, jwt string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regArgs = append(f.regArgs, struct{ server, clusterID, jwt string }{server, clusterID, jwt})
	return f.regNodeID, f.regErr
}

func (f *fakeDeps) Deregister(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deregCalls++
	return f.deregErr
}

func (f *fakeDeps) Status() Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statusVal
}

func (f *fakeDeps) Upgrade(_ context.Context, channel string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upgChannels = append(f.upgChannels, channel)
	return f.upgTriggered, f.upgErr
}

func (f *fakeDeps) Display(_ context.Context, action string) (*DisplayInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dspActions = append(f.dspActions, action)
	return &DisplayInfo{DriverInstalled: true, VirtualActive: action == "on"}, f.dspErr
}

func (f *fakeDeps) upgradeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.upgChannels)
}

// serve 起真实管道服务端（真机 Windows 管道），等就绪后返回。
func serve(t *testing.T, deps Deps) *Server {
	t.Helper()
	return serveServer(t, &Server{
		Name: testPipe(t), Deps: deps,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
}

// serveServer 启动调用方预构造的服务端（字段须在 Listen 前定形——Listen 后
// dispatch 与测试并发读写字段会构成数据竞争）。
func serveServer(t *testing.T, s *Server) *Server {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- s.Listen(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Logf("listen exited: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Listen did not return after ctx cancel")
		}
	})
	require.Eventually(t, func() bool {
		c, err := winio.DialPipe(s.name(), nil)
		if err == nil {
			_ = c.Close()
			return true
		}
		return false
	}, 5*time.Second, 50*time.Millisecond, "pipe listener must become ready")
	return s
}

// roundTrip 走真实管道完成一次单行 JSON 请求/应答。
func roundTrip(t *testing.T, s *Server, req Request) Response {
	t.Helper()
	resp, err := RoundTrip(t.Context(), s.name(), req)
	require.NoError(t, err)
	return resp
}

// roundTripRaw 写原始行（坏输入路径）。
func roundTripRaw(t *testing.T, s *Server, line string) Response {
	t.Helper()
	conn, err := winio.DialPipe(s.name(), nil)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))
	_, err = conn.Write([]byte(line + "\n"))
	require.NoError(t, err)
	buf := make([]byte, 8192)
	n, err := conn.Read(buf)
	require.NoError(t, err)
	var resp Response
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(string(buf[:n]))), &resp))
	return resp
}

// status：真实管道往返，全字段。
func TestPipeStatusRoundTrip(t *testing.T) {
	deps := &fakeDeps{statusVal: Status{
		State: StateOnline, NodeID: "node-1", Server: "https://xnc.example",
		ClusterID: "c-1", Version: "0.4.6",
	}}
	s := serve(t, deps)
	resp := roundTrip(t, s, Request{Op: OpStatus})
	assert.True(t, resp.OK)
	assert.Equal(t, StateOnline, resp.State)
	assert.Equal(t, "node-1", resp.NodeID)
	assert.Equal(t, "https://xnc.example", resp.Server)
	assert.Equal(t, "c-1", resp.ClusterID)
	assert.Equal(t, "0.4.6", resp.Version)
	assert.Empty(t, resp.Error)
}

// register：参数原样到达编排层，nodeId 原样返回。
func TestPipeRegisterRoundTrip(t *testing.T) {
	deps := &fakeDeps{regNodeID: "node-9"}
	s := serve(t, deps)
	resp := roundTrip(t, s, Request{
		Op: OpRegister, Server: "https://xnc.example", ClusterID: "c-2", JWT: "jwt-secret",
	})
	assert.True(t, resp.OK)
	assert.Equal(t, "node-9", resp.NodeID)
	require.Len(t, deps.regArgs, 1)
	assert.Equal(t, "https://xnc.example", deps.regArgs[0].server)
	assert.Equal(t, "c-2", deps.regArgs[0].clusterID)
	assert.Equal(t, "jwt-secret", deps.regArgs[0].jwt)
}

// register：编排层错误原样透传（<code>: <message> 由编排层格式化）。
func TestPipeRegisterError(t *testing.T) {
	deps := &fakeDeps{regErr: errors.New("MACHINE_ID_CONFLICT: machineId already registered in cluster \"other\"")}
	s := serve(t, deps)
	resp := roundTrip(t, s, Request{Op: OpRegister, Server: "s", ClusterID: "c", JWT: "j"})
	assert.False(t, resp.OK)
	assert.Equal(t, `MACHINE_ID_CONFLICT: machineId already registered in cluster "other"`, resp.Error)
}

// register：缺参 → bad_request（不触达编排层）。
func TestPipeRegisterValidation(t *testing.T) {
	deps := &fakeDeps{regNodeID: "n"}
	s := serve(t, deps)
	for name, req := range map[string]Request{
		"no server":    {Op: OpRegister, ClusterID: "c", JWT: "j"},
		"no clusterId": {Op: OpRegister, Server: "s", JWT: "j"},
		"no jwt":       {Op: OpRegister, Server: "s", ClusterID: "c"},
	} {
		resp := roundTrip(t, s, req)
		assert.False(t, resp.OK, name)
		assert.Contains(t, resp.Error, "bad_request", name)
	}
	assert.Empty(t, deps.regArgs)
}

// deregister：非管理员连接被拒（forbidden: admin required），不触达编排层；
// 管理员连接放行。IsAdminConn 在 Listen 前注入（避免与 dispatch 并发写）。
func TestPipeDeregisterAdminGate(t *testing.T) {
	deps := &fakeDeps{}
	var admin atomic.Bool
	s := serveServer(t, &Server{
		Name: testPipe(t), Deps: deps,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		IsAdminConn: func(net.Conn) (bool, error) { return admin.Load(), nil },
	})

	resp := roundTrip(t, s, Request{Op: OpDeregister})
	assert.False(t, resp.OK)
	assert.Equal(t, "forbidden: admin required", resp.Error)
	assert.Equal(t, 0, deps.deregCalls, "deregister must not run for non-admin client")

	admin.Store(true)
	resp = roundTrip(t, s, Request{Op: OpDeregister})
	assert.True(t, resp.OK)
	assert.Equal(t, 1, deps.deregCalls)
}

// 坏输入：未知 op / 非 JSON 行 / 缺 op。
func TestPipeBadRequests(t *testing.T) {
	deps := &fakeDeps{}
	s := serve(t, deps)
	resp := roundTrip(t, s, Request{Op: "reboot"})
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "bad_request")

	resp = roundTripRaw(t, s, "not json at all")
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "bad_request")

	resp = roundTripRaw(t, s, `{}`)
	assert.False(t, resp.OK)
	assert.Contains(t, resp.Error, "bad_request")
}

// 同一服务端顺序服务多条连接（一次一个连接模型）。
func TestPipeSequentialConnections(t *testing.T) {
	deps := &fakeDeps{statusVal: Status{State: StateRegistered, NodeID: "n", Version: "v"}}
	s := serve(t, deps)
	for i := 0; i < 3; i++ {
		resp := roundTrip(t, s, Request{Op: OpStatus})
		assert.True(t, resp.OK)
		assert.Equal(t, StateRegistered, resp.State)
	}
}

// ctx 取消 → Listen 返回（优雅停机）。
func TestListenClosesOnCtxCancel(t *testing.T) {
	deps := &fakeDeps{}
	s := &Server{Name: testPipe(t), Deps: deps, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Listen(ctx) }()
	require.Eventually(t, func() bool {
		c, err := winio.DialPipe(s.name(), nil)
		if err == nil {
			_ = c.Close()
			return true
		}
		return false
	}, 5*time.Second, 50*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Listen returned %v, want ctx error or nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("Listen did not return after ctx cancel")
	}
}

// ---- upgrade op（§9.1 手动触发 / §9.5 跨频道切换；无 admin 门） ----

// upgrade：channel 原样到达编排层，应答 {ok:true,triggered:true}。
func TestPipeUpgradeRoundTrip(t *testing.T) {
	deps := &fakeDeps{upgTriggered: true}
	s := serve(t, deps)
	resp := roundTrip(t, s, Request{Op: OpUpgrade, Channel: "dev"})
	assert.True(t, resp.OK)
	assert.True(t, resp.Triggered)
	assert.Empty(t, resp.Note)
	assert.Empty(t, resp.Error)
	require.Equal(t, []string{"dev"}, deps.upgChannels)

	// 不带 channel（当前频道即时检查）同样放行。
	resp = roundTrip(t, s, Request{Op: OpUpgrade})
	assert.True(t, resp.OK)
	assert.True(t, resp.Triggered)
	assert.Equal(t, []string{"dev", ""}, deps.upgChannels)
}

// upgrade：更新已在途（pending/进行中）→ 非错误应答
// {ok:true,triggered:false,note:"update already in progress"}（CLI 渲染
// note 后转入轮询）。
func TestPipeUpgradeAlreadyInProgress(t *testing.T) {
	deps := &fakeDeps{upgTriggered: false}
	s := serve(t, deps)
	resp := roundTrip(t, s, Request{Op: OpUpgrade})
	assert.True(t, resp.OK, "in-flight must not be an error")
	assert.False(t, resp.Triggered)
	assert.Equal(t, "update already in progress", resp.Note)
	assert.Empty(t, resp.Error)
}

// upgrade：编排层错误原样透传（如 not_registered）。
func TestPipeUpgradeError(t *testing.T) {
	deps := &fakeDeps{upgErr: errors.New("not_registered: no binding")}
	s := serve(t, deps)
	resp := roundTrip(t, s, Request{Op: OpUpgrade})
	assert.False(t, resp.OK)
	assert.Equal(t, "not_registered: no binding", resp.Error)
}

// upgrade：channel 白名单校验先于编排层（stable|dev 之外的值 → bad_request，
// 不触碰绑定）。
func TestPipeUpgradeChannelValidation(t *testing.T) {
	deps := &fakeDeps{upgTriggered: true}
	s := serve(t, deps)
	for _, ch := range []string{"beta", "STABLE", "stable ", "production"} {
		resp := roundTrip(t, s, Request{Op: OpUpgrade, Channel: ch})
		assert.False(t, resp.OK, "channel %q", ch)
		assert.Contains(t, resp.Error, "bad_request", "channel %q", ch)
	}
	assert.Zero(t, deps.upgradeCalls(), "invalid channel must not reach Deps.Upgrade")
}

// upgrade：不设 admin 门（任何交互用户可触发，与 register 同级；§9.5 跨频道
// 切换经管道 + binding 原子写实现，无需提权）。回归 pin：IsAdminConn 即使
// 注入也绝不被 upgrade 分支调用。
func TestPipeUpgradeNoAdminGate(t *testing.T) {
	deps := &fakeDeps{upgTriggered: true}
	var adminChecks atomic.Int64
	s := serveServer(t, &Server{
		Name: testPipe(t), Deps: deps,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		IsAdminConn: func(net.Conn) (bool, error) { adminChecks.Add(1); return false, nil },
	})
	resp := roundTrip(t, s, Request{Op: OpUpgrade, Channel: "dev"})
	assert.True(t, resp.OK, "upgrade must not require admin")
	assert.Zero(t, adminChecks.Load())
}

// connIsAdmin 真实链路冒烟：裸管道 accept → 同进程 dial → 句柄取 pid →
// 进程令牌查组。取值取决于本进程提权状态（不可断言），但链路必须无错。
func TestConnIsAdminSmoke(t *testing.T) {
	ln, err := winio.ListenPipe(testPipe(t), nil)
	require.NoError(t, err)
	defer ln.Close()
	type res struct {
		ok  bool
		err error
	}
	out := make(chan res, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			out <- res{err: err}
			return
		}
		defer conn.Close()
		ok, err := connIsAdmin(conn)
		out <- res{ok: ok, err: err}
	}()
	c, err := winio.DialPipe(ln.Addr().String(), nil)
	require.NoError(t, err)
	defer c.Close()
	select {
	case r := <-out:
		require.NoError(t, r.err)
		t.Logf("connIsAdmin=%v (elevation of test process)", r.ok)
	case <-time.After(5 * time.Second):
		t.Fatal("accept/isAdmin did not complete")
	}
}
