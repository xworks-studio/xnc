package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- fake agentctl pipe (net.Pipe 适配器，镜像 §6.3 单行 JSON 一问一答) ----

// fakeAgentctl 记录到达的请求并按预设函数应答（每连接一问一答）。
type fakeAgentctl struct {
	mu      sync.Mutex
	reqs    []agentctlReq
	respond func(req agentctlReq) agentctlResp
}

func (f *fakeAgentctl) record(req agentctlReq) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
}

func (f *fakeAgentctl) ops() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ops := make([]string, len(f.reqs))
	for i, r := range f.reqs {
		ops[i] = r.Op
	}
	return ops
}

func (f *fakeAgentctl) lastRegister() agentctlReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.reqs) - 1; i >= 0; i-- {
		if f.reqs[i].Op == "register" {
			return f.reqs[i]
		}
	}
	return agentctlReq{}
}

// dial 实现 agentctlDial 缝隙：返回 net.Pipe 一端，另一端起假服务端。
func (f *fakeAgentctl) dial(_ context.Context, _ string) (net.Conn, error) {
	f.mu.Lock()
	respond := f.respond
	f.mu.Unlock()
	c1, c2 := net.Pipe()
	go func() {
		defer c2.Close()
		line, err := bufio.NewReader(c2).ReadString('\n')
		if err != nil {
			return
		}
		var req agentctlReq
		if json.Unmarshal([]byte(strings.TrimSpace(line)), &req) != nil {
			return
		}
		f.record(req)
		b, err := json.Marshal(respond(req))
		if err != nil {
			return
		}
		_, _ = c2.Write(append(b, '\n'))
	}()
	return c1, nil
}

func (f *fakeAgentctl) install(t *testing.T) {
	t.Helper()
	old := agentctlDial
	agentctlDial = f.dial
	t.Cleanup(func() { agentctlDial = old })
}

func unreachablePipe(t *testing.T) {
	t.Helper()
	old := agentctlDial
	agentctlDial = func(context.Context, string) (net.Conn, error) {
		return nil, errors.New("pipe does not exist")
	}
	t.Cleanup(func() { agentctlDial = old })
}

// runCLICaptureBoth 同时捕获 stdout 与 stderr（json 模式的提示行断言用）。
func runCLICaptureBoth(t *testing.T, f func() int) (string, string, int) {
	t.Helper()
	var stdout, stderr string
	var code int
	stderr, code = captureStderr(t, func() int {
		stdout, code = captureStdout(t, f)
		return code
	})
	return stdout, stderr, code
}

// isolatedHome 隔离 HOME/USERPROFILE 与 XNC_* 环境变量并返回临时目录。
func isolatedHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOME", dir) // os.UserHomeDir fallback on unix
	t.Setenv("XNC_SERVER", "")
	t.Setenv("XNC_TOKEN", "")
	return dir
}

func writeConfig(t *testing.T, dir, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, ".xnc"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".xnc", "config.json"), []byte(body), 0o600))
}

const (
	regNodeID  = "11111111-2222-4333-8444-555555555555"
	onlineNode = `{"id":"` + regNodeID + `","name":"LAB-PC","cluster":"prod","hostname":"LAB",
		"os_version":"Windows 11","agent_version":"0.4.6","shell_type":"pwsh",
		"status":"online","last_seen_at":null}`
)

// clustersServer 提供 /api/clusters（可选 /api/auth/login、/api/nodes/{id}）。
func clustersServer(t *testing.T, clusters string, extra http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/clusters":
			_, _ = w.Write([]byte(clusters))
		case extra != nil:
			extra(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// unregisteredThenRegister 是最常见的假管道：status=unregistered，register 成功。
func unregisteredThenRegister(nodeID string) func(agentctlReq) agentctlResp {
	return func(req agentctlReq) agentctlResp {
		switch req.Op {
		case "status":
			return agentctlResp{OK: true, State: "unregistered"}
		case "register":
			return agentctlResp{OK: true, NodeID: nodeID}
		}
		return agentctlResp{OK: false, Error: "bad_request: unknown op " + req.Op}
	}
}

// ---- register ----

// 单 cluster：列表即确认（--yes 跳过），管道 register 参数正确，轮询后打印
// 节点与面板入口。
func TestRegisterSingleClusterWithToken(t *testing.T) {
	isolatedHome(t)
	srv := clustersServer(t, `[{"id":"c1","name":"prod"}]`, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/nodes/"+regNodeID {
			_, _ = w.Write([]byte(onlineNode))
			return
		}
		http.NotFound(w, r)
	})
	pipe := &fakeAgentctl{respond: unregisteredThenRegister(regNodeID)}
	pipe.install(t)

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"register", "--yes",
			"--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 0, code)
	assert.Contains(t, out, "cluster: prod")
	assert.Contains(t, out, "LAB-PC")
	assert.Contains(t, out, regNodeID)
	assert.Contains(t, out, srv.URL, "panel URL (server) must be printed")

	assert.Equal(t, []string{"status", "register"}, pipe.ops())
	reg := pipe.lastRegister()
	assert.Equal(t, srv.URL, reg.Server)
	assert.Equal(t, "c1", reg.ClusterID)
	assert.Equal(t, "tk", reg.JWT)
}

// 已有会话（config 文件）：复用 JWT，打印当前账号。
func TestRegisterReusesSessionFromConfig(t *testing.T) {
	dir := isolatedHome(t)
	writeConfig(t, dir, `{"server":"", "token":"cfgtok", "remembered_email":"a@b.c"}`)
	srv := clustersServer(t, `[{"id":"c1","name":"prod"}]`, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/nodes/"+regNodeID {
			_, _ = w.Write([]byte(onlineNode))
		}
	})
	pipe := &fakeAgentctl{respond: unregisteredThenRegister(regNodeID)}
	pipe.install(t)

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"register", "--yes", "--server", srv.URL})
	})
	require.Equal(t, 0, code)
	assert.Contains(t, out, "using session a@b.c")
	assert.Equal(t, "cfgtok", pipe.lastRegister().JWT)
}

// 多 cluster：编号选择（stdin 喂入；非法输入重试）。
func TestRegisterMultiClusterSelectsByNumber(t *testing.T) {
	isolatedHome(t)
	srv := clustersServer(t, `[{"id":"c1","name":"prod"},{"id":"c2","name":"lab"}]`,
		func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/nodes/"+regNodeID {
				_, _ = w.Write([]byte(onlineNode))
			}
		})
	pipe := &fakeAgentctl{respond: unregisteredThenRegister(regNodeID)}
	pipe.install(t)

	out, code := runCLIWithStdin(t, "bogus\n2\n", []string{"register", "--yes",
		"--server", srv.URL, "--token", "tk"})
	require.Equal(t, 0, code)
	assert.Contains(t, out, "1) prod")
	assert.Contains(t, out, "2) lab")
	assert.Equal(t, "c2", pipe.lastRegister().ClusterID)
}

// 零 cluster：报错并提示找管理员。
func TestRegisterZeroClustersHint(t *testing.T) {
	isolatedHome(t)
	srv := clustersServer(t, `[]`, nil)
	pipe := &fakeAgentctl{respond: unregisteredThenRegister(regNodeID)}
	pipe.install(t)

	stderr, code := captureStderr(t, func() int {
		return runCLI(t.Context(), []string{"register", "--yes",
			"--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, exitMissing, code)
	assert.Contains(t, stderr, "no clusters")
	assert.NotContains(t, strings.Join(pipe.ops(), ","), "register")
}

// 单 cluster 确认提示被拒绝：中止，不发 register。
func TestRegisterConfirmDeclined(t *testing.T) {
	isolatedHome(t)
	srv := clustersServer(t, `[{"id":"c1","name":"prod"}]`, nil)
	pipe := &fakeAgentctl{respond: unregisteredThenRegister(regNodeID)}
	pipe.install(t)

	_, code := runCLIWithStdin(t, "n\n", []string{"register",
		"--server", srv.URL, "--token", "tk"})
	require.Equal(t, exitUsage, code)
	assert.Equal(t, []string{"status"}, pipe.ops())
}

// 已注册：提示当前绑定并指向 --force，不触达 register。
func TestRegisterAlreadyRegisteredNeedsForce(t *testing.T) {
	isolatedHome(t)
	srv := clustersServer(t, `[{"id":"c1","name":"prod"}]`, nil)
	pipe := &fakeAgentctl{respond: func(req agentctlReq) agentctlResp {
		if req.Op == "status" {
			return agentctlResp{OK: true, State: "registered",
				NodeID: "n0", ClusterID: "c0", Server: "https://old.example"}
		}
		return agentctlResp{OK: true}
	}}
	pipe.install(t)

	stderr, code := captureStderr(t, func() int {
		return runCLI(t.Context(), []string{"register", "--yes",
			"--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, exitMissing, code)
	assert.Contains(t, stderr, "c0")
	assert.Contains(t, stderr, "--force")
	assert.Equal(t, []string{"status"}, pipe.ops())
}

// --force：先 deregister 再 register（改绑）。
func TestRegisterForceRebinds(t *testing.T) {
	isolatedHome(t)
	srv := clustersServer(t, `[{"id":"c1","name":"prod"}]`, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/nodes/"+regNodeID {
			_, _ = w.Write([]byte(onlineNode))
		}
	})
	pipe := &fakeAgentctl{respond: func(req agentctlReq) agentctlResp {
		switch req.Op {
		case "status":
			return agentctlResp{OK: true, State: "online", NodeID: "n0", ClusterID: "c0"}
		case "deregister":
			return agentctlResp{OK: true}
		case "register":
			return agentctlResp{OK: true, NodeID: regNodeID}
		}
		return agentctlResp{OK: false, Error: "bad_request: " + req.Op}
	}}
	pipe.install(t)

	_, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"register", "--yes", "--force",
			"--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 0, code)
	assert.Equal(t, []string{"status", "deregister", "register"}, pipe.ops())
}

// --force 且未注册：deregister 的 not_registered 错误被忽略。
func TestRegisterForceIgnoresNotRegistered(t *testing.T) {
	isolatedHome(t)
	srv := clustersServer(t, `[{"id":"c1","name":"prod"}]`, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/nodes/"+regNodeID {
			_, _ = w.Write([]byte(onlineNode))
		}
	})
	pipe := &fakeAgentctl{respond: func(req agentctlReq) agentctlResp {
		switch req.Op {
		case "status":
			return agentctlResp{OK: true, State: "unregistered"}
		case "deregister":
			return agentctlResp{OK: false, Error: "not_registered: no binding"}
		case "register":
			return agentctlResp{OK: true, NodeID: regNodeID}
		}
		return agentctlResp{OK: false, Error: "bad_request: " + req.Op}
	}}
	pipe.install(t)

	_, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"register", "--yes", "--force",
			"--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 0, code)
	assert.Equal(t, []string{"status", "deregister", "register"}, pipe.ops())
}

// 管道 register 返回 409 冲突（message 含冲突 cluster 名）：原样透传并提示 --force。
func TestRegisterConflictHintsForce(t *testing.T) {
	isolatedHome(t)
	srv := clustersServer(t, `[{"id":"c1","name":"prod"}]`, nil)
	pipe := &fakeAgentctl{respond: func(req agentctlReq) agentctlResp {
		switch req.Op {
		case "status":
			return agentctlResp{OK: true, State: "unregistered"}
		case "register":
			return agentctlResp{OK: false,
				Error: `MACHINE_ID_CONFLICT: machineId already registered in cluster "other"`}
		}
		return agentctlResp{OK: false, Error: "bad_request: " + req.Op}
	}}
	pipe.install(t)

	stderr, code := captureStderr(t, func() int {
		return runCLI(t.Context(), []string{"register", "--yes",
			"--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, exitMissing, code) // MACHINE_ID_CONFLICT maps alongside NODE_ALREADY_ENROLLED
	assert.Contains(t, stderr, `MACHINE_ID_CONFLICT: machineId already registered in cluster "other"`)
	assert.Contains(t, stderr, "--force")
}

// 管道不可达：register 快速失败并提示 agent 服务。
func TestRegisterPipeUnreachable(t *testing.T) {
	isolatedHome(t)
	srv := clustersServer(t, `[{"id":"c1","name":"prod"}]`, nil)
	unreachablePipe(t)

	stderr, code := captureStderr(t, func() int {
		return runCLI(t.Context(), []string{"register", "--yes",
			"--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, exitNet, code)
	assert.Contains(t, stderr, "agent service not reachable")
}

// 无 JWT：内联 login（非交互：--email + stdin 密码），JWT 存 config 并传入管道。
func TestRegisterInlineLoginNonInteractive(t *testing.T) {
	dir := isolatedHome(t)
	var loginSeen bool
	srv := clustersServer(t, `[{"id":"c1","name":"prod"}]`, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			loginSeen = true
			b := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(b)
			assert.Contains(t, string(b), `"email":"a@b.c"`)
			assert.Contains(t, string(b), `"password":"pw"`)
			_, _ = w.Write([]byte(`{"token":"jwt-2","user":{"id":"u1","email":"a@b.c","display_name":"A"}}`))
		case "/api/nodes/" + regNodeID:
			_, _ = w.Write([]byte(onlineNode))
		default:
			http.NotFound(w, r)
		}
	})
	pipe := &fakeAgentctl{respond: unregisteredThenRegister(regNodeID)}
	pipe.install(t)

	out, code := runCLIWithStdin(t, "pw\n", []string{"register", "--yes",
		"--server", srv.URL, "--email", "a@b.c"})
	require.Equal(t, 0, code)
	assert.True(t, loginSeen)
	assert.Equal(t, "jwt-2", pipe.lastRegister().JWT)
	cfg, err := os.ReadFile(filepath.Join(dir, ".xnc", "config.json"))
	require.NoError(t, err)
	assert.Contains(t, string(cfg), `"token": "jwt-2"`)
	assert.Contains(t, out, "LAB-PC")
}

// 轮询至 online：首次 offline、第二次 online（缩短轮询间隔）。
func TestRegisterPollsUntilOnline(t *testing.T) {
	isolatedHome(t)
	var nodeFetches int
	srv := clustersServer(t, `[{"id":"c1","name":"prod"}]`, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/nodes/"+regNodeID {
			http.NotFound(w, r)
			return
		}
		nodeFetches++
		if nodeFetches == 1 {
			_, _ = w.Write([]byte(strings.Replace(onlineNode, `"status":"online"`, `"status":"offline"`, 1)))
			return
		}
		_, _ = w.Write([]byte(onlineNode))
	})
	pipe := &fakeAgentctl{respond: unregisteredThenRegister(regNodeID)}
	pipe.install(t)

	oldI, oldT := registerPollInterval, registerPollTimeout
	registerPollInterval, registerPollTimeout = 5*time.Millisecond, 2*time.Second
	t.Cleanup(func() { registerPollInterval, registerPollTimeout = oldI, oldT })

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"register", "--yes",
			"--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 0, code)
	assert.GreaterOrEqual(t, nodeFetches, 2)
	assert.Contains(t, out, "LAB-PC")
	assert.NotContains(t, out, "not online")
}

// 轮询超时：注册已成功，仅告警不失败。
func TestRegisterPollTimeoutWarns(t *testing.T) {
	isolatedHome(t)
	srv := clustersServer(t, `[{"id":"c1","name":"prod"}]`, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/nodes/"+regNodeID {
			_, _ = w.Write([]byte(strings.Replace(onlineNode, `"status":"online"`, `"status":"offline"`, 1)))
			return
		}
		http.NotFound(w, r)
	})
	pipe := &fakeAgentctl{respond: unregisteredThenRegister(regNodeID)}
	pipe.install(t)

	oldI, oldT := registerPollInterval, registerPollTimeout
	registerPollInterval, registerPollTimeout = 5*time.Millisecond, 30*time.Millisecond
	t.Cleanup(func() { registerPollInterval, registerPollTimeout = oldI, oldT })

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"register", "--yes",
			"--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 0, code)
	assert.Contains(t, out, "not online")
}

// ---- deregister ----

func TestDeregisterAdminHint(t *testing.T) {
	isolatedHome(t)
	pipe := &fakeAgentctl{respond: func(agentctlReq) agentctlResp {
		return agentctlResp{OK: false, Error: "forbidden: admin required"}
	}}
	pipe.install(t)

	stderr, code := captureStderr(t, func() int {
		return runCLI(t.Context(), []string{"deregister"})
	})
	require.Equal(t, exitForbid, code)
	assert.Contains(t, stderr, "forbidden: admin required")
	assert.Contains(t, stderr, "administrator")
}

func TestDeregisterSuccess(t *testing.T) {
	isolatedHome(t)
	pipe := &fakeAgentctl{respond: func(agentctlReq) agentctlResp {
		return agentctlResp{OK: true}
	}}
	pipe.install(t)

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"deregister"})
	})
	require.Equal(t, 0, code)
	assert.Contains(t, out, "deregistered")

	outJSON, code2 := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"deregister", "--json"})
	})
	require.Equal(t, 0, code2)
	assert.JSONEq(t, `{"ok":true,"data":{"deregistered":true},"error":null}`, outJSON)
}

// ---- status（本机块 + 会话块） ----

func TestStatusLocalAndSession(t *testing.T) {
	dir := isolatedHome(t)
	writeConfig(t, dir, `{"server":"https://cfg.example","token":"t","remembered_email":"a@b.c"}`)
	pipe := &fakeAgentctl{respond: func(req agentctlReq) agentctlResp {
		require.Equal(t, "status", req.Op)
		return agentctlResp{OK: true, State: "online", NodeID: "n1",
			Server: "https://xnc.example", ClusterID: "c1", Version: "0.4.6"}
	}}
	pipe.install(t)

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"status"})
	})
	require.Equal(t, 0, code)
	assert.Contains(t, out, "local:")
	assert.Contains(t, out, "online")
	assert.Contains(t, out, "n1")
	assert.Contains(t, out, "c1")
	assert.Contains(t, out, "0.4.6")
	assert.Contains(t, out, "session:")
	assert.Contains(t, out, "https://cfg.example")
	assert.Contains(t, out, "a@b.c")
	assert.Contains(t, out, "present")

	outJSON, code2 := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"status", "--json"})
	})
	require.Equal(t, 0, code2)
	assert.JSONEq(t, `{"ok":true,"data":{
		"local":{"reachable":true,"state":"online","nodeId":"n1",
			"server":"https://xnc.example","clusterId":"c1","version":"0.4.6"},
		"session":{"server":"https://cfg.example","email":"a@b.c","token_present":true}},
		"error":null}`, outJSON)
}

// 管道不可达：提示后继续输出会话块，退出码 0。
func TestStatusPipeUnreachableShowsSession(t *testing.T) {
	dir := isolatedHome(t)
	writeConfig(t, dir, `{"server":"https://cfg.example","token":"t","remembered_email":"a@b.c"}`)
	unreachablePipe(t)

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"status"})
	})
	require.Equal(t, 0, code)
	assert.Contains(t, out, "agent service not reachable")
	assert.Contains(t, out, "session:")
	assert.Contains(t, out, "https://cfg.example")

	outJSON, code2 := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"status", "--json"})
	})
	require.Equal(t, 0, code2)
	assert.JSONEq(t, `{"ok":true,"data":{
		"local":{"reachable":false},
		"session":{"server":"https://cfg.example","email":"a@b.c","token_present":true}},
		"error":null}`, outJSON)
}

// status 渲染新增的 channel / update 字段（Task 8 wire 扩展；可缺省）。
func TestStatusRendersChannelAndUpdate(t *testing.T) {
	isolatedHome(t)
	pipe := &fakeAgentctl{respond: func(req agentctlReq) agentctlResp {
		return agentctlResp{OK: true, State: "online", NodeID: "n1", Server: "https://s",
			ClusterID: "c1", Channel: "dev", Version: "0.6.2",
			Update: &agentctlUpdate{Phase: "applying", From: "0.6.1", To: "0.6.2"}}
	}}
	pipe.install(t)

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"status"})
	})
	require.Equal(t, 0, code)
	assert.Contains(t, out, "dev")
	assert.Contains(t, out, "applying")
	assert.Contains(t, out, "0.6.1")
	assert.Contains(t, out, "0.6.2")

	outJSON, code2 := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"status", "--json"})
	})
	require.Equal(t, 0, code2)
	assert.JSONEq(t, `{"ok":true,"data":{
		"local":{"reachable":true,"state":"online","nodeId":"n1",
			"server":"https://s","clusterId":"c1","channel":"dev","version":"0.6.2",
			"update":{"phase":"applying","from":"0.6.1","to":"0.6.2"}},
		"session":{"server":"","email":"","token_present":false}},
		"error":null}`, outJSON)
}

// ---- login 尾行提示（§10） ----

func TestLoginTailRegisterHint(t *testing.T) {
	loginSrv := func(t *testing.T) *httptest.Server {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"jwt-1","user":{"id":"u1","email":"a@b.c","display_name":"A"}}`))
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	t.Run("unregistered prints hint", func(t *testing.T) {
		isolatedHome(t)
		pipe := &fakeAgentctl{respond: unregisteredThenRegister("")}
		pipe.install(t)
		srv := loginSrv(t)

		out, code := runCLIWithStdin(t, "pw\n", []string{"login", "--server", srv.URL, "--email", "a@b.c"})
		require.Equal(t, 0, code)
		assert.Contains(t, out, "Logged in")
		assert.Contains(t, out, "node not registered - run: xnc register")
	})

	t.Run("pipe unreachable stays silent", func(t *testing.T) {
		isolatedHome(t)
		unreachablePipe(t)
		srv := loginSrv(t)

		out, code := runCLIWithStdin(t, "pw\n", []string{"login", "--server", srv.URL, "--email", "a@b.c"})
		require.Equal(t, 0, code)
		assert.Contains(t, out, "Logged in")
		assert.NotContains(t, out, "not registered")
	})

	t.Run("registered prints no hint", func(t *testing.T) {
		isolatedHome(t)
		pipe := &fakeAgentctl{respond: func(agentctlReq) agentctlResp {
			return agentctlResp{OK: true, State: "online", NodeID: "n1"}
		}}
		pipe.install(t)
		srv := loginSrv(t)

		out, code := runCLIWithStdin(t, "pw\n", []string{"login", "--server", srv.URL, "--email", "a@b.c"})
		require.Equal(t, 0, code)
		assert.NotContains(t, out, "not registered")
	})

	t.Run("json mode hint goes to stderr", func(t *testing.T) {
		isolatedHome(t)
		pipe := &fakeAgentctl{respond: unregisteredThenRegister("")}
		pipe.install(t)
		srv := loginSrv(t)

		r, w, err := os.Pipe()
		require.NoError(t, err)
		_, _ = w.WriteString("pw\n")
		_ = w.Close()
		oldStdin := os.Stdin
		os.Stdin = r
		t.Cleanup(func() { os.Stdin = oldStdin })

		stdout, stderr, code := runCLICaptureBoth(t, func() int {
			return runCLI(t.Context(), []string{"login", "--json", "--server", srv.URL, "--email", "a@b.c"})
		})
		require.Equal(t, 0, code)
		assert.Contains(t, stdout, `"token":"jwt-1"`)
		assert.NotContains(t, stdout, "not registered", "stdout must stay a single envelope line")
		assert.Contains(t, stderr, "node not registered - run: xnc register")
	})
}

// ---- agentctlRoundTrip 协议往返（wire 镜像字段 tag） ----

func TestAgentctlRoundTripWire(t *testing.T) {
	pipe := &fakeAgentctl{respond: func(agentctlReq) agentctlResp {
		return agentctlResp{OK: true, NodeID: "nx", State: "online"}
	}}
	pipe.install(t)

	resp, err := agentctlRoundTrip(t.Context(), agentctlReq{
		Op: "register", Server: "https://s", ClusterID: "c", JWT: "j"})
	require.NoError(t, err)
	assert.True(t, resp.OK)
	assert.Equal(t, "nx", resp.NodeID)
	assert.Equal(t, "online", resp.State)
	require.Len(t, pipe.reqs, 1)

	// wire 字段名与 agent/agentctl.Request/Response 一致（op/server/clusterId/jwt/channel）。
	b, err := json.Marshal(agentctlReq{Op: "register", Server: "s", ClusterID: "c", JWT: "j"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"op":"register","server":"s","clusterId":"c","jwt":"j"}`, string(b))

	b3, err := json.Marshal(agentctlReq{Op: "upgrade", Channel: "dev"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"op":"upgrade","channel":"dev"}`, string(b3))

	// triggered 恒输出（upgrade 在途 = {ok:true,triggered:false,note:...}）。
	b2, err := json.Marshal(agentctlResp{OK: false, Error: "forbidden: admin required"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"ok":false,"triggered":false,"error":"forbidden: admin required"}`, string(b2))

	b4, err := json.Marshal(agentctlResp{OK: true, Triggered: false, Note: "update already in progress"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"ok":true,"triggered":false,"note":"update already in progress"}`, string(b4))
}
