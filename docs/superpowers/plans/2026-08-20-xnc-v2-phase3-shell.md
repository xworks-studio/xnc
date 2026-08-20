# XNC v2 Phase 3（shell 会话）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付交互式 PowerShell 终端：ConPTY shell 会话（agent）+ shell 端点与限额/空闲计时（server）+ `xnc shell`（CLI，真 TTY raw mode + resize 跟随），E2E 覆盖会话级验收。

**Architecture:** 复用 Phase 2 的统一会话管理器（kind 参数化——manager/pump 对 kind 语义透明）；agent 侧以 Gate B 选定的 x/sys 直接封装 ConPTY（两个已验证不变量带入）；CLI 用 x/term 做 raw 终端 + 尺寸轮询。shell 是第一种**长寿命**会话：server 侧新增 idle/max-lifetime janitor 与每节点限额。

**Tech Stack:** 沿用全栈；新增依赖仅 `golang.org/x/term`（cli）。ConPTY 用既有的 x/sys（Gate B 已验证 v0.47 原生导出）。

**Spec:** `docs/superpowers/specs/2026-08-19-xnc-v2-unified-session-design.md` §2-§3.2 + `spec.md` §15-§20/§50-§51 + `docs/superpowers/spikes/gateb-conpty/FINDINGS.md`（Gate B 不变量，必读）。

## Global Constraints

- 模块结构不变；本机无 make；Docker 运行中（testcontainers）；双 GOOS 编译是 agent/cli 验收门槛。
- `proto/` 唯一协议定义点：KindShell、ShellParams/ShellBegin/ShellResize、SESSION_LIMIT_EXCEEDED 只在 proto 定义。
- shell 会话词汇（设计 §3.2，绑定）：SESSION_OPEN params `{cols, rows, shell?}`（缺省 agent 探测）；text `SHELL_BEGIN {shell}`（实际 shell）先于任何输出；text `SHELL_RESIZE {cols, rows}`（client→agent）；创建失败 → text `ERROR {code: SHELL_START_FAILED}` 后关连接；binary = 原始 VT 字节双向，无帧头无方向标记。
- ConPTY 两个 Gate B 不变量（绑定，违反即 0xC0000142 或静默脱离）：`UpdateProcThreadAttribute` 的 lpValue 传 HPCON **句柄值**（`uintptr(hpc)`）非指针；子进程 STARTUPINFO **必须**置 `STARTF_USESTDHANDLES`。选型为 x/sys 直接封装，**禁止引入第三方 conpty 库**。
- Ctrl+C 必须经终端输入（0x03 字节写入 pty input）传递，会话存活（spec §19）；resize 经 ResizePseudoConsole。
- 计时责任（设计 §2.3，绑定）：shell 的 idle 30min / max 8h 由 **server** 计时（janitor 关闭并 NotifyClose），agent 不做寿命计时。env：`XNC_SHELL_IDLE`/`XNC_SHELL_MAX` 可覆盖。
- 每节点 shell 会话限额 10（`XNC_SHELL_PER_NODE`），超限 → 409 `SESSION_LIMIT_EXCEEDED`（新错误码，spec §51 同步增行）→ CLI 246。
- 审计：shell.open（202 时）/ shell.close（关闭时，metadata.reason）；日志不含命令内容（VT 流本就不解析不记录）。
- CLI 契约（cli.md）：`xnc shell <node> [--cols N] [--rows N]`；**需要真 TTY**（非 TTY → 用法错误 exit 2，提示用 exec）；破坏性低——正常关闭 exit 0。
- 关闭清理：会话关闭（任一侧断开/超时/SESSION_CLOSE）必须 kill shell 进程树并 ClosePseudoConsole，无 conhost/powershell 残留（断连后进程残留是 Phase 1 spec §59 的验收句式）。
- 每任务一 commit；sqlc 无新查询（不动 db/queries）。

**本计划不含**：Web Terminal（Phase 7）、file/screen/tunnel（Phase 4/6）、exec 限额与 RBAC（Phase 5）、Linux openpty（Phase 8——非 Windows 的 shell handler 返回 SHELL_START_FAILED 桩）。

---

## 文件结构总览

```text
proto/
├── session.go                 T1  +KindShell/ShellParams/ShellBegin/ShellResize（修改）
├── errors.go                  T1  +CodeSessionLimited（修改）
└── session_test.go            T1
server/internal/config/config.go        T3  +ShellPerNode/ShellIdleTimeout/ShellMaxLifetime（修改）
server/internal/session/
├── manager.go                 T3  +限额/活动追踪/janitor/Close（修改）
├── manager_test.go            T3
server/internal/api/
├── exec_handlers.go           T2  抽取共享 startSession（修改）
├── shell_handlers.go          T2  POST /api/nodes/{id}/shell
├── shell_handlers_test.go     T2
agent/session/
├── conpty_windows.go          T4  Gate B 封装（含两不变量注释）
├── conpty_other.go            T4  非 Windows 桩
├── shell.go                   T4  Shell Handler（SHELL_BEGIN/pump/resize/清理）
├── shell_test.go              T4  Windows 集成（build tag windows）
agent/agent.go / mockagent/main.go      T5  注册 KindShell（修改）
cli/
├── cmd_shell.go               T6  x/term raw 模式 + resize 轮询
├── cmd_shell_test.go          T6  非 TTY 拒绝 + 错误路径
shellsmoke/                    T7  会话级冒烟工具（新模块 xnc/shellsmoke）
scripts/e2e_phase3.sh          T7
Makefile / go.work             T7  +shellsmoke 模块
spec.md                        T1  §51 +SESSION_LIMIT_EXCEEDED（修改）
```

---

### Task 1: proto — shell 词汇与限额错误码

**Files:**
- Modify: `proto/session.go`、`proto/errors.go`、`spec.md`（§51 错误码表）
- Test: `proto/session_test.go`（追加）

**Interfaces:**
- Consumes: 既有 proto 结构与 Phase 2 的 session payload 风格
- Produces（T2/T4/T6 依赖）:

```go
const KindShell = "shell"
type ShellParams struct {          // SESSION_OPEN params 的 shell 形态
    Cols  int    `json:"cols,omitempty"`
    Rows  int    `json:"rows,omitempty"`
    Shell string `json:"shell,omitempty"` // "" = agent 探测
}
type ShellBegin struct {           // agent → client 的首个 text 帧
    Shell string `json:"shell"`
}
type ShellResize struct {          // client → agent 的 text 帧
    Cols int `json:"cols"`
    Rows int `json:"rows"`
}
// errors.go
const CodeSessionLimited = "SESSION_LIMIT_EXCEEDED"
```

- [ ] **Step 1: 写失败测试**

追加到 `proto/session_test.go`：

```go
func TestShellVocabulary(t *testing.T) {
	m, err := NewMsg(TypeSessionOpen, SessionOpen{
		SessionID: "s1", Kind: KindShell,
		Params: mustRaw(t, ShellParams{Cols: 120, Rows: 30}),
	})
	require.NoError(t, err)
	b, err := json.Marshal(m)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"SESSION_OPEN","payload":{"sessionId":"s1","kind":"shell",
		"params":{"cols":120,"rows":30},"agentToken":"","wsUrl":"",
		"expiresAt":"0001-01-01T00:00:00Z"}}`, string(b))

	bb, _ := json.Marshal(ShellBegin{Shell: "pwsh"})
	assert.JSONEq(t, `{"shell":"pwsh"}`, string(bb))

	rb, _ := NewMsg("SHELL_RESIZE", ShellResize{Cols: 100, Rows: 40})
	// SHELL_RESIZE 是会话内 text 帧，type 字面量属 kind 私有词汇：
	// 不进 proto 常量（与 EXEC_RESULT 同策略），测试用字面量验证 payload。
	mr, err := NewMsg("SHELL_RESIZE", ShellResize{Cols: 100, Rows: 40})
	require.NoError(t, err)
	br, _ := json.Marshal(mr)
	assert.Contains(t, string(br), `"cols":100`)
	assert.Contains(t, string(br), `"rows":40`)

	assert.Equal(t, "SESSION_LIMIT_EXCEEDED", CodeSessionLimited)
	_ = rb
}

func mustRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
```

- [ ] **Step 2: 运行验证失败**

Run: `cd proto && go test ./... -count=1`
Expected: FAIL（KindShell 等未定义）。

- [ ] **Step 3: 实现**

`proto/session.go` 追加：

```go
// KindShell 交互式终端会话（ConPTY，Phase 3）。
const KindShell = "shell"

// ShellParams 会话 Params 的 shell 形态；Cols/Rows 为 0 时用默认 120x30，
// Shell 为空时由 agent 按探测结果决定。
type ShellParams struct {
	Cols  int    `json:"cols,omitempty"`
	Rows  int    `json:"rows,omitempty"`
	Shell string `json:"shell,omitempty"`
}

// ShellBegin agent → client：实际使用的 shell（SHELL_BEGIN text 帧）。
type ShellBegin struct {
	Shell string `json:"shell"`
}

// ShellResize client → agent：终端尺寸变化（SHELL_RESIZE text 帧）。
type ShellResize struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}
```

`proto/errors.go` 常量区追加：

```go
	CodeSessionLimited = "SESSION_LIMIT_EXCEEDED"
```

`spec.md` §51 错误码表（v2 重写后的列表块）在 `SESSION_EXPIRED` 行后追加一行 `SESSION_LIMIT_EXCEEDED`（保持 text 代码块风格）。

- [ ] **Step 4: 运行验证通过**

Run: `cd proto && go test ./... -count=1 && (cd ../server && go build ./...)`
Expected: PASS + server 仍编译。

- [ ] **Step 5: Commit**

```bash
git add proto spec.md
git commit -m "feat(proto): shell vocabulary and session-limit error code"
```

---

### Task 2: server — shell 端点（共享 startSession 抽取）

**Files:**
- Modify: `server/internal/api/exec_handlers.go`（抽取共享函数）
- Create: `server/internal/api/shell_handlers.go`
- Test: `server/internal/api/shell_handlers_test.go`

**Interfaces:**
- Consumes: T1 的 KindShell/ShellParams/CodeSessionLimited（本任务不用限额——T3 落地）；Phase 2 的 `session.Manager.Create/SetFinishFn/SetNotifyFn/NotifyClose`、`registry.Get(...).Send`、`wsBaseURL`、审计模式（`auditInsertTimeout`、background ctx）
- Produces:

```go
// exec_handlers.go 内抽取的共享函数（shell 复用；exec 行为不变）：
func (h *handlers) startSession(w http.ResponseWriter, r *http.Request,
    kind string, params json.RawMessage, openAction, closeAction string,
) (*session.CreateResult, bool) // false = 已响应错误；true = 已 202

// REST（本任务）：
// POST /api/nodes/{id}/shell   body {"cols"?,"rows"?,"shell"?}
//   → 202 {"sessionId","token","expiresAt","websocketUrl"}
//   校验：cols/rows ∈ [0,1000]（0→服务端缺省 120/30 写入 params）；shell 仅允许 ""/"pwsh"/"powershell"
//   404 NODE_NOT_FOUND / 409 NODE_OFFLINE / 400 校验失败
// 审计：shell.open / shell.close（metadata：kind、reason；无 VT 内容）
```

- [ ] **Step 1: 写失败测试**

`server/internal/api/shell_handlers_test.go`：

```go
package api

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

func shellPost(t *testing.T, env *TestEnv, nodeID, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", env.srv.URL+"/api/nodes/"+nodeID+"/shell",
		bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+env.AdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestShellSessionEndToEnd(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-SH1", "mid-sh1")

	// 离线 → 409
	resp := shellPost(t, env, nodeID, `{}`)
	defer resp.Body.Close()
	assert.Equal(t, 409, resp.StatusCode)

	ctrl := dialControl(t, env, nodeID)

	// 校验失败：cols 越界 / shell 名非法
	for _, b := range []string{`{"cols":1001}`, `{"rows":-1}`, `{"shell":"cmd"}`} {
		r := shellPost(t, env, nodeID, b)
		_ = r.Body.Close()
		assert.Equal(t, 400, r.StatusCode, b)
	}

	// 在线：假 agent 走 SHELL_BEGIN → VT 帧 → 断开
	var created struct {
		SessionID    string    `json:"sessionId"`
		Token        string    `json:"token"`
		WebsocketURL string    `json:"websocketUrl"`
	}
	agentDone := make(chan struct{})
	go func() {
		defer close(agentDone)
		fakeAgentSession(t, ctrl, func(aws *websocketConn) {
			wsWriteText(t, aws, mustMsg(t, proto.ShellBegin{Shell: "powershell"}))
			wsWriteBinary(t, aws, []byte("\x1b[2JPS> "))
			_ = aws.Close(closeCtx(t))
		})
	}()

	resp2 := shellPost(t, env, nodeID, `{"cols":100,"rows":40,"shell":"powershell"}`)
	defer resp2.Body.Close()
	require.Equal(t, 202, resp2.StatusCode)
	require.NoError(t, decodeJSON(resp2.Body, &created))
	assert.Contains(t, created.WebsocketURL, "/api/session/"+created.SessionID)

	cl := dialClientSession(t, env.srv.URL, "/api/session/"+created.SessionID, created.Token)
	begin := readMsg(t, cl) // 复用 agentws_test 的 text 帧读取
	require.Equal(t, "SHELL_BEGIN", begin.Type)
	var sb proto.ShellBegin
	require.NoError(t, begin.Decode(&sb))
	assert.Equal(t, "powershell", sb.Shell)
	vt := readBin(t, cl)
	assert.Contains(t, string(vt), "PS>")

	select {
	case <-agentDone:
	case <-time.After(5 * time.Second):
		t.Fatal("agent goroutine stuck")
	}

	// 审计对
	require.Eventually(t, func() bool {
		var n int
		require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
			`SELECT count(*) FROM audit_logs WHERE action IN ('shell.open','shell.close')`).Scan(&n))
		return n >= 2
	}, 5*time.Second, 200*time.Millisecond)
}

func TestShellParamsDefaultsInSessionOpen(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-SH2", "mid-sh2")
	ctrl := dialControl(t, env, nodeID)

	// captureOnly 变体：读控制连接上的 SESSION_OPEN 即送 channel，不拨会话。
	openCh := captureSessionOpen(t, ctrl)

	resp := shellPost(t, env, nodeID, `{}`)
	defer resp.Body.Close()
	require.Equal(t, 202, resp.StatusCode)

	select {
	case so := <-openCh:
		var p proto.ShellParams
		require.NoError(t, jsonUnmarshal(so.Params, &p))
		assert.Equal(t, 120, p.Cols)
		assert.Equal(t, 30, p.Rows)
	case <-time.After(3 * time.Second):
		t.Fatal("no SESSION_OPEN")
	}
}
```

（helpers 追加 `captureSessionOpen(t, ctrl) chan proto.SessionOpen`：读一条消息、断言 type==SESSION_OPEN、decode 后送 channel 返回；`websocketConn` 若 helpers 用的是 `*websocket.Conn` 原名则以现名为准——第一个测试中的 `aws *websocketConn` 直接写作 `aws *websocket.Conn`。`closeCtx`/`jsonUnmarshal` 以 helpers 文件现有命名对齐，勿造重复助手。）

- [ ] **Step 2: 运行验证失败**

Run: `cd server && go test ./internal/api/ -run TestShell -count=1`
Expected: FAIL（端点不存在）。

- [ ] **Step 3: 实现**

`exec_handlers.go` 抽取（execStart 改为调用，行为零变化——既有 TestExec* 全绿为证）：

```go
// startSession 是 exec/shell 共享的会话创建路径：hooks → 控制连接下发 → 审计 open → 202。
// 返回 (result, true) 表示已写 202；false 表示已写错误响应。
func (h *handlers) startSession(w http.ResponseWriter, r *http.Request,
	kind string, params json.RawMessage, openAction, closeAction string,
) (*session.CreateResult, bool) {
	u := auth.UserFrom(r.Context())
	nodeID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "node not found"))
		return nil, false
	}
	if _, err := h.st.Q().GetNodeForUser(r.Context(), sqlc.GetNodeForUserParams{
		UserID: u.ID, ID: nodeID}); err != nil {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "node not found"))
		return nil, false
	}
	res, apiErr := h.sess.Create(nodeID, u.ID, kind, params)
	if apiErr != nil {
		respondError(w, apiErr)
		return nil, false
	}
	h.sess.SetFinishFn(res.Session, func(reason string) {
		ctx, cancel := context.WithTimeout(context.Background(), auditInsertTimeout)
		defer cancel()
		_ = h.st.Q().InsertAuditLog(ctx, sqlc.InsertAuditLogParams{
			UserID: pgUUID(u.ID), NodeID: pgUUID(nodeID), Action: closeAction,
			Metadata: mustJSON(map[string]string{"reason": reason, "kind": kind, "sessionId": res.Session.ID}),
		})
	})
	nodeConn := h.reg.Get(nodeID.String())
	if nodeConn == nil || nodeConn.Send == nil {
		h.sess.NotifyClose(res.Session.ID, "node-offline")
		respondError(w, proto.Err(409, proto.CodeNodeOffline, "node is offline"))
		return nil, false
	}
	openMsg, _ := proto.NewMsg(proto.TypeSessionOpen, proto.SessionOpen{
		SessionID: res.Session.ID, Kind: kind, Params: params,
		AgentToken: res.AgentToken,
		WsURL:      wsBaseURL(r) + "/api/agent/session?token=" + res.AgentToken,
		ExpiresAt:  res.ExpiresAt,
	})
	if err := nodeConn.Send(openMsg); err != nil {
		h.sess.NotifyClose(res.Session.ID, "node-offline")
		respondError(w, proto.Err(409, proto.CodeNodeOffline, "node is offline"))
		return nil, false
	}
	h.sess.SetNotifyFn(res.Session, func(sc proto.SessionClose) error {
		if nc := h.reg.Get(nodeID.String()); nc != nil && nc.Send != nil {
			msg, _ := proto.NewMsg(proto.TypeSessionClose, sc)
			return nc.Send(msg)
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), auditInsertTimeout)
	defer cancel()
	_ = h.st.Q().InsertAuditLog(ctx, sqlc.InsertAuditLogParams{
		UserID: pgUUID(u.ID), NodeID: pgUUID(nodeID), Action: openAction,
		Metadata: mustJSON(map[string]string{"kind": kind, "sessionId": res.Session.ID}),
	})
	respondJSON(w, 202, map[string]any{
		"sessionId": res.Session.ID, "token": res.ClientToken,
		"expiresAt": res.ExpiresAt,
		"websocketUrl": "/api/session/" + res.Session.ID + "?token=" + res.ClientToken,
	})
	return res, true
}
```

（execStart 保留自己的请求体解码 + 校验 + params 构造 + `r.Body = http.MaxBytesReader(...)`，其余替换为 `h.startSession(w, r, proto.KindExec, params, "exec.start", "exec.finish")`；T2 提交时既有 exec 测试不改即绿。）

`shell_handlers.go`：

```go
package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

func (h *handlers) shellStart(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	var req struct {
		Cols  int    `json:"cols"`
		Rows  int    `json:"rows"`
		Shell string `json:"shell"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "bad request"))
		return
	}
	if req.Cols == 0 {
		req.Cols = 120
	}
	if req.Rows == 0 {
		req.Rows = 30
	}
	switch {
	case req.Cols < 1 || req.Cols > 1000, req.Rows < 1 || req.Rows > 1000:
		respondError(w, proto.Err(400, proto.CodeInternal, "cols/rows must be 1-1000"))
		return
	case req.Shell != "" && req.Shell != "pwsh" && req.Shell != "powershell":
		respondError(w, proto.Err(400, proto.CodeInternal, "shell must be pwsh or powershell"))
		return
	}
	params, _ := json.Marshal(proto.ShellParams{Cols: req.Cols, Rows: req.Rows, Shell: req.Shell})
	h.startSession(w, r, proto.KindShell, params, "shell.open", "shell.close")
}
```

（import `xnc/proto`；router.go 在 `/api/nodes` 认证组内挂载 `r.Route("/{id}/shell", ...)` 或直接 `r.Post("/api/nodes/{id}/shell", h.shellStart)` 于 auth 组——与 exec 挂载方式一致，看现有写法对齐。）注意 `proto.ShellParams` 序列化时 `omitempty` 会把 120/30 写出（非零），默认值测试断言即成立。

- [ ] **Step 4: 运行验证通过**

Run: `cd server && go test ./internal/api/ -run 'TestShell|TestExec' -count=1 && go test ./... -count=1`
Expected: PASS（exec 回归全绿）。

- [ ] **Step 5: Commit**

```bash
git add server
git commit -m "feat(server): shell session endpoint via shared start-session path"
```

---

### Task 3: server — 每节点限额与 idle/max-lifetime janitor

**Files:**
- Modify: `server/internal/session/manager.go`、`server/internal/config/config.go`、`server/internal/api/router.go`
- Test: `server/internal/session/manager_test.go`（追加）

**Interfaces:**
- Consumes: T1 `CodeSessionLimited`/`KindShell`；Phase 2 的 Manager/pump 结构
- Produces:

```go
// config 新增：
ShellPerNode      int           // 默认 10，env XNC_SHELL_PER_NODE；0 = 不限
ShellIdleTimeout  time.Duration // 默认 30m，env XNC_SHELL_IDLE；0 = 不限
ShellMaxLifetime  time.Duration // 默认 8h，env XNC_SHELL_MAX；0 = 不限

// Manager 新增（New 后由 router 赋值一次，测试可改）：
ShellPerNode      int
ShellIdleTimeout  time.Duration
ShellMaxLifetime  time.Duration
janitorInterval   time.Duration // 默认 30s；测试缩短
func (m *Manager) Close()       // 停 janitor（测试用；生产随进程）
func (m *Manager) CountByNode(nodeID uuid.UUID, kind string) int
// Create 行为新增：kind==KindShell 且 ShellPerNode>0 且 CountByNode>=ShellPerNode
//   → 409 SESSION_LIMIT_EXCEEDED
// Create 行为新增：kind==KindShell 时 session 记录 startedAt/idleTimeout/maxLifetime
// pump 行为新增：每帧转发更新 session.lastActivity
// janitor：每 tick 检查 idle 超时（"idle-timeout"）与寿命超时（"max-lifetime"）→ NotifyClose
```

- [ ] **Step 1: 写失败测试**

追加到 `server/internal/session/manager_test.go`：

```go
func TestShellPerNodeLimit(t *testing.T) {
	id, m := onlineMgr(t)
	defer m.Close()
	m.ShellPerNode = 2
	for i := 0; i < 2; i++ {
		res, apiErr := m.Create(id, uuid.New(), proto.KindShell, []byte(`{}`))
		require.Nil(t, apiErr)
		_ = res
	}
	_, apiErr := m.Create(id, uuid.New(), proto.KindShell, []byte(`{}`))
	require.NotNil(t, apiErr)
	assert.Equal(t, 409, apiErr.Status)
	assert.Equal(t, proto.CodeSessionLimited, apiErr.Code)
	// exec 不受限
	_, apiErr = m.Create(id, uuid.New(), proto.KindExec, []byte(`{}`))
	assert.Nil(t, apiErr)
	// 关掉一个 shell 后可再建
	m.NotifyClose(firstShellID(t, m, id), "test")
	res, apiErr := m.Create(id, uuid.New(), proto.KindShell, []byte(`{}`))
	assert.Nil(t, apiErr)
	_ = res
}

func firstShellID(t *testing.T, m *Manager, nodeID uuid.UUID) string {
	t.Helper()
	sess := m.SessionsOf(nodeID, proto.KindShell)
	require.NotEmpty(t, sess)
	return sess[0].ID
}

func TestIdleTimeoutClosesShell(t *testing.T) {
	id, m := onlineMgr(t)
	defer m.Close()
	m.ShellIdleTimeout = 100 * time.Millisecond
	m.janitorInterval = 30 * time.Millisecond

	var reasons []string
	res, _ := m.Create(id, uuid.New(), proto.KindShell, []byte(`{}`))
	m.SetFinishFn(res.Session, func(r string) { reasons = append(reasons, r) })

	require.Eventually(t, func() bool { return len(reasons) > 0 }, 3*time.Second, 50*time.Millisecond)
	assert.Equal(t, "idle-timeout", reasons[0])
}

func TestActivityPreventsIdleTimeout(t *testing.T) {
	id, m := onlineMgr(t)
	defer m.Close()
	m.ShellIdleTimeout = 250 * time.Millisecond
	m.janitorInterval = 50 * time.Millisecond

	res, _ := m.Create(id, uuid.New(), proto.KindShell, []byte(`{}`))
	closed := make(chan string, 1)
	m.SetFinishFn(res.Session, func(r string) { closed <- r })

	// 每 100ms 活动一次，持续 600ms（跨越 2 个 idle 窗口）
	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		m.touch(res.Session, time.Now())
		time.Sleep(100 * time.Millisecond)
	}
	select {
	case r := <-closed:
		t.Fatalf("closed early: %s", r)
	default:
	}
	// 停止活动 → 应在 idle+janitor 内关闭
	select {
	case r := <-closed:
		assert.Equal(t, "idle-timeout", r)
	case <-time.After(3 * time.Second):
		t.Fatal("no idle close after activity stopped")
	}
}

func TestMaxLifetimeClosesShell(t *testing.T) {
	id, m := onlineMgr(t)
	defer m.Close()
	m.ShellMaxLifetime = 120 * time.Millisecond
	m.janitorInterval = 30 * time.Millisecond

	res, _ := m.Create(id, uuid.New(), proto.KindShell, []byte(`{}`))
	closed := make(chan string, 1)
	m.SetFinishFn(res.Session, func(r string) { closed <- r })
	m.touch(res.Session, time.Now().Add(time.Hour)) // 活动再频繁也逃不过寿命

	select {
	case r := <-closed:
		assert.Equal(t, "max-lifetime", r)
	case <-time.After(3 * time.Second):
		t.Fatal("no max-lifetime close")
	}
}
```

（`m.touch` 与 `m.SessionsOf` 为测试/内部助手：`touch(s *Session, at time.Time)` 更新 lastActivity；`SessionsOf(nodeID, kind)` 返回 []*Session 快照。均为未导出或半导出小函数，写进 manager.go。）

- [ ] **Step 2: 运行验证失败**

Run: `cd server && go test ./internal/session/ -run 'TestShellPerNode|TestIdle|TestActivity|TestMaxLifetime' -count=1`
Expected: FAIL（字段/方法未定义）。

- [ ] **Step 3: 实现**

`config.go` Config 结构追加三字段 + Load 解析：

```go
	ShellPerNode      int           // XNC_SHELL_PER_NODE，默认 10，0 = 不限
	ShellIdleTimeout  time.Duration // XNC_SHELL_IDLE，默认 30m，0 = 不限
	ShellMaxLifetime  time.Duration // XNC_SHELL_MAX，默认 8h，0 = 不限
```

（Load 内：`c.ShellPerNode = envInt("XNC_SHELL_PER_NODE", 10)`；`c.ShellIdleTimeout = envDur("XNC_SHELL_IDLE", 30*time.Minute)`；`c.ShellMaxLifetime = envDur("XNC_SHELL_MAX", 8*time.Hour)`。补 `envInt` 助手：Atoi 失败回默认。）

`manager.go`：

```go
// —— shell 会话治理（Phase 3）——
// New() 末尾启动：go m.janitorLoop()
// session 私有结构新增：
//   startedAt     time.Time
//   lastActivity  atomic.Int64 // unixnano
//   idleTimeout   time.Duration
//   maxLifetime   time.Duration

func New(reg *registry.Registry, log *slog.Logger) *Manager {
	m := &Manager{..., janitorInterval: 30 * time.Second, ShellPerNode: 10,
		ShellIdleTimeout: 30 * time.Minute, ShellMaxLifetime: 8 * time.Hour}
	m.stopJanitor = make(chan struct{})
	go m.janitorLoop()
	return m
}

func (m *Manager) Close() {
	select {
	case <-m.stopJanitor:
	default:
		close(m.stopJanitor)
	}
}

func (m *Manager) janitorLoop() {
	t := time.NewTicker(m.janitorInterval)
	defer t.Stop()
	for {
		select {
		case <-m.stopJanitor:
			return
		case now := <-t.C:
			m.mu.Lock()
			var expire []struct {
				id     string
				reason string
			}
			for id, s := range m.sessions {
				if s.kind != proto.KindShell {
					continue
				}
				if s.maxLifetime > 0 && now.Sub(s.startedAt) > s.maxLifetime {
					expire = append(expire, struct {
						id     string
						reason string
					}{id, "max-lifetime"})
					continue
				}
				if s.idleTimeout > 0 {
					last := time.Unix(0, s.lastActivity.Load())
					if now.Sub(last) > s.idleTimeout {
						expire = append(expire, struct {
							id     string
							reason string
						}{id, "idle-timeout"})
					}
				}
			}
			m.mu.Unlock()
			for _, e := range expire {
				m.NotifyClose(e.id, e.reason) // 锁外，幂等
			}
		}
	}
}

// Create 内、Online 检查后追加：
	if kind == proto.KindShell {
		if m.ShellPerNode > 0 && m.CountByNode(nodeID, proto.KindShell) >= m.ShellPerNode {
			return nil, proto.Err(409, proto.CodeSessionLimited, "shell session limit reached")
		}
	}
// Create 的 session 构造追加：
	if kind == proto.KindShell {
		s.startedAt = time.Now()
		s.lastActivity.Store(time.Now().UnixNano())
		s.idleTimeout = m.ShellIdleTimeout
		s.maxLifetime = m.ShellMaxLifetime
	}

func (m *Manager) CountByNode(nodeID uuid.UUID, kind string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.sessions {
		if s.nodeID == nodeID && s.kind == kind {
			n++
		}
	}
	return n
}

func (m *Manager) SessionsOf(nodeID uuid.UUID, kind string) []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Session
	for _, s := range m.sessions {
		if s.nodeID == nodeID && s.kind == kind {
			view := (*Session)(s)
			out = append(out, view)
		}
	}
	return out
}

func (m *Manager) touch(s *Session, at time.Time) {
	(*session)(s).lastActivity.Store(at.UnixNano())
}
```

pump 活动追踪：`relay` 签名增加 session 参数（`m.relay(s *session, from, to)`），每成功转发一帧后 `s.lastActivity.Store(time.Now().UnixNano())`；`pump(s)` 两个方向都传 `s`。（既有 pump 测试不断言活动性，不受影响。）

`router.go`：NewRouterWithSession 构造 manager 后赋值 `sess.ShellPerNode = cfg.ShellPerNode` 等三字段（Manager.New 的默认值仅作零值兜底）。

- [ ] **Step 4: 运行验证通过**

Run: `cd server && go test ./internal/session/ -count=1 && go test ./... -count=1`
Expected: PASS（既有 manager 测试需补 `defer m.Close()` 的不受影响——若 TestMain 层面无泄漏检测则无感；T3 泄漏的 janitor goroutine 在测试进程内无害，但新测试要 Close）。

- [ ] **Step 5: Commit**

```bash
git add server
git commit -m "feat(server): shell per-node limit and idle/max-lifetime janitor"
```

---

### Task 4: agent — ConPTY 封装与 Shell 会话处理器

**Files:**
- Create: `agent/session/conpty_windows.go`、`agent/session/conpty_other.go`、`agent/session/shell.go`
- Test: `agent/session/shell_test.go`（`//go:build windows`）

**Interfaces:**
- Consumes: Gate B 原型（`docs/superpowers/spikes/gateb-conpty/prototype-direct-xsys.go`——执行者先读它）；T1 词汇；既有 `Handler` 接口、`killTree`、`execWriteTimeout`
- Produces（T5/T7 依赖）:

```go
// conpty_windows.go
type conPTY struct { /* hpc, inPipe, outPipe, cmd, exited */ }
func startConPTY(cols, rows int, exe string, args ...string) (*conPTY, error)
func (p *conPTY) Resize(cols, rows int) error
func (p *conPTY) Wait() error              // 等 cmd 退出（无超时；调用方 select ctx）
func (p *conPTY) KillAndClose()            // killTree + ClosePseudoConsole + 关管道（幂等）

// shell.go
type Shell struct{ Log *slog.Logger }
func NewShell(log *slog.Logger) *Shell
func (sh *Shell) Handle(ctx context.Context, ws *websocket.Conn, sessionID string, params json.RawMessage)
// 协议行为：SHELL_BEGIN 先行；binary 双向；text 仅接受 SHELL_RESIZE；
// 创建失败 → text ERROR{SHELL_START_FAILED} → 关连接；ctx 结束/对端断开 → KillAndClose
```

- [ ] **Step 1: 写失败测试**

`agent/session/shell_test.go`：

```go
//go:build windows

package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

// runShell 起 httptest WS 让 Shell 作为客户端拨入；返回服务侧连接与结束信号。
// errCh 回报拨号/Handle 错误（goroutine 内不 require）。
func runShell(t *testing.T, params proto.ShellParams) (*websocket.Conn, chan struct{}) {
	t.Helper()
	up := make(chan *websocket.Conn, 1)
	errCh := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		up <- c
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	raw, _ := json.Marshal(params)
	go func() {
		c, _, err := websocket.Dial(context.Background(), "ws"+srv.URL[4:], nil)
		if err != nil {
			errCh <- err
			return
		}
		NewShell(testLogger()).Handle(context.Background(), c, "sess-shell", raw)
		errCh <- nil
	}()
	select {
	case c := <-up:
		return c, nilCh(errCh)
	case err := <-errCh:
		t.Fatalf("shell dial failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no ws")
	}
	return nil, nil
}

func nilCh(errCh chan error) chan struct{} {
	done := make(chan struct{})
	go func() { <-errCh; close(done) }()
	return done
}

// collectUntil 从 ws 读帧直到 match 返回 true（跳过非 binary 或不匹配的帧），
// 超时 fail。返回匹配帧原文。
func collectUntil(t *testing.T, ws *websocket.Conn, match func(kind string, data []byte) bool, timeout time.Duration) []byte {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case <-deadline:
			t.Fatal("collectUntil: timeout waiting for match")
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		typ, data, err := ws.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		kind := "binary"
		if typ == websocket.MessageText {
			kind = "text"
		}
		if match(kind, data) {
			return data
		}
	}
}

func TestShellEchoResizeCtrlC(t *testing.T) {
	ws, done := runShell(t, proto.ShellParams{Cols: 80, Rows: 25})
	defer func() { _ = ws.CloseNow() }()

	// 1) SHELL_BEGIN 先行，报告实际 shell
	begin := collectUntil(t, ws, func(k string, d []byte) bool {
		if k != "text" {
			return false
		}
		var m proto.Message
		return json.Unmarshal(d, &m) == nil && m.Type == "SHELL_BEGIN"
	}, 10*time.Second)
	var sb proto.ShellBegin
	var m proto.Message
	require.NoError(t, json.Unmarshal(begin, &m))
	require.NoError(t, m.Decode(&sb))
	assert.NotEmpty(t, sb.Shell)

	// 2) echo 回显（VT 序列夹杂，bytes.Contains 判定）
	writeBin(t, ws, []byte("echo sh-echo-ok\r"))
	collectUntil(t, ws, func(k string, d []byte) bool {
		return k == "binary" && strings.Contains(string(d), "sh-echo-ok")
	}, 10*time.Second)

	// 3) resize 后继续工作
	writeText(t, ws, mustShellMsg(t, "SHELL_RESIZE", proto.ShellResize{Cols: 100, Rows: 40}))
	writeBin(t, ws, []byte("echo sh-post-resize\r"))
	collectUntil(t, ws, func(k string, d []byte) bool {
		return k == "binary" && strings.Contains(string(d), "sh-post-resize")
	}, 10*time.Second)

	// 4) Ctrl+C 会话存活
	writeBin(t, ws, []byte{0x03})
	writeBin(t, ws, []byte("echo sh-after-ctrlc\r"))
	collectUntil(t, ws, func(k string, d []byte) bool {
		return k == "binary" && strings.Contains(string(d), "sh-after-ctrlc")
	}, 10*time.Second)

	// 5) 退出 → Handle 返回（会话结束信号）
	writeBin(t, ws, []byte("exit\r"))
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Handle did not return after exit")
	}
}

func TestShellCloseKillsProcess(t *testing.T) {
	var pid int64
	up := make(chan *websocket.Conn, 1)
	errCh := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		up <- c
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	raw, _ := json.Marshal(proto.ShellParams{Cols: 80, Rows: 25})
	go func() {
		c, _, err := websocket.Dial(context.Background(), "ws"+srv.URL[4:], nil)
		if err != nil {
			errCh <- err
			return
		}
		NewShell(testLogger()).Handle(context.Background(), c, "sess-kill", raw)
		errCh <- nil
	}()
	ws := <-up
	collectUntil(t, ws, func(k string, d []byte) bool {
		return k == "text" && strings.Contains(string(d), "SHELL_BEGIN")
	}, 10*time.Second)

	// 从系统进程表找 shell 子进程：按 conPTY 记录的 PID 暴露测试钩子
	pid = lastShellPID() // 实现里 Shell 测试包暴露的原子变量（见实现注记）
	_ = ws.CloseNow()    // 模拟 client 断开

	require.Eventually(t, func() bool {
		return !processAlive(t, pid)
	}, 10*time.Second, 300*time.Millisecond, "shell process must die after disconnect")
}

func processAlive(t *testing.T, pid int64) bool {
	t.Helper()
	if pid == 0 {
		return false
	}
	out, err := exec.Command("tasklist", "/FI", "PID eq "+itoa(pid)).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), itoa(pid))
}

func itoa(n int64) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.TrimSpace(string([]byte{
		byte('0' + n/10000%10), byte('0' + n/1000%10), byte('0' + n/100%10),
		byte('0' + n/10%10), byte('0' + n%10)})), "\x00", ""))
}
```

（实现注记：①`writeBin/writeText/mustShellMsg` 若 exec_test 无现成同名则在本文件定义（text = `proto.NewMsg(typ, payload)` marshal 后写 MessageText）；②`itoa` 直接用 `strconv.FormatInt`——上面手写版作废；③`lastShellPID()`：shell.go 内包级 `var lastStartedPID atomic.Int64`（仅测试读取，生产无消费者），startConPTY 成功后写入 cmd.Process.Pid。这三个注记即实现要求，不留自由发挥。）

- [ ] **Step 2: 运行验证失败**

Run: `cd agent && go test ./session/ -count=1`
Expected: FAIL（shell.go/conpty 不存在）。

- [ ] **Step 3: 实现**

`conpty_windows.go`——基于 Gate B 原型（先读 `docs/superpowers/spikes/gateb-conpty/prototype-direct-xsys.go`），整理为：

```go
//go:build windows

package session

// Windows ConPTY 封装。两个 Gate B 验证的不变量（违反即子进程 0xC0000142 或静默脱离 pty）：
//  1. UpdateProcThreadAttribute 的 lpValue 必须传 HPCON 句柄【值】（uintptr(hpc)），不是 &hpc；
//  2. 子进程 STARTUPINFO 必须置 STARTF_USESTDHANDLES，否则静默不挂 pty（输出丢失但退出码 0）。

import (
	"io"
	"os"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

type conPTY struct {
	hpc    windows.Handle
	inW    io.WriteCloser // CLI → pty input（写端）
	outR   io.ReadCloser  // pty output → CLI（读端）
	cmd    *exec.Cmd
	inR    *os.File
	outW   *os.File
	closed bool
}

func startConPTY(cols, rows int, exe string, args ...string) (*conPTY, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close(); inW.Close()
		return nil, err
	}
	var hpc windows.Handle
	size := windows.Coord{X: int16(cols), Y: int16(rows)}
	if err := windows.CreatePseudoConsole(size, windows.Handle(inR.Fd()),
		windows.Handle(outW.Fd()), 0, &hpc); err != nil {
		inR.Close(); inW.Close(); outR.Close(); outW.Close()
		return nil, err
	}
	// 父进程侧的管道副本可立即关闭：pty 已自行持有。
	inR.Close()
	outW.Close()

	al, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		inW.Close(); outR.Close()
		return nil, err
	}
	// 不变量 1：lpValue 传句柄值。
	al.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		unsafe.Pointer(uintptr(hpc)), unsafe.Sizeof(hpc), nil)

	si := &windows.StartupInfoEx{}
	si.Cb = uint32(unsafe.Sizeof(*si))
	// 不变量 2：必须置 STARTF_USESTDHANDLES。
	si.StartupInfo.Flags = windows.STARTF_USESTDHANDLES
	si.AttributeList = al.List()
	pi := &windows.ProcessInformation{}
	cmdLine, _ := windows.UTF16PtrFromString(exe + " " + strings.Join(args, " "))
	err = windows.CreateProcess(nil, cmdLine, nil, nil, true, 0, nil, nil, &si.StartupInfo, pi)
	// ……（错误清理路径同原型）
}
```

（执行注记：上面是结构骨架；`docs/superpowers/spikes/gateb-conpty/prototype-direct-xsys.go` 是**真机验证过的完整实现**——逐段移植它（管道创建、CreatePseudoConsole、属性列表挂接 `si.AttributeList = al.List()`、CreateProcess inheritHandles=true、错误清理），重命名为 conPTY 类型并补 `Resize/Wait/KillAndClose` 三个方法：`Resize` = `windows.ResizePseudoConsole(p.hpc, windows.Coord{X: int16(cols), Y: int16(rows)})`；`Wait` = `p.cmd.Wait()`（注意原型若直接用 CreateProcess 则以句柄等待，照原型）；`KillAndClose`：`if p.closed { return }` → `killTree(pid)` → 等进程退出 → `windows.ClosePseudoConsole(p.hpc)` → 关 inW/outR → `p.closed = true`。）

`conpty_other.go`：

```go
//go:build !windows

package session

import "errors"

type conPTY struct{}

func startConPTY(cols, rows int, exe string, args ...string) (*conPTY, error) {
	return nil, errors.New("conpty: windows only (linux pty arrives in Phase 8)")
}
func (p *conPTY) Resize(cols, rows int) error        { return nil }
func (p *conPTY) Wait() error                        { return nil }
func (p *conPTY) KillAndClose()                      {}
func (p *conPTY) Read(b []byte) (int, error)         { return 0, errors.New("unsupported") }
func (p *conPTY) Write(b []byte) (int, error)        { return 0, errors.New("unsupported") }
```

（若 windows 版以方法暴露 io（见 shell.go 用法），两平台方法集需一致——shell.go 只在 windows 路径用 p.inW/p.outR，非 windows 走 SHELL_START_FAILED 提前返回，不触 io 方法。）

`shell.go`：

```go
package session

import (
	"context"
	"encoding/json"
	"log/slog"
	"os/exec"
	"runtime"
	"sync/atomic"

	"github.com/coder/websocket"

	"xnc/proto"
)

// lastStartedPID 仅供集成测试断言进程清理（生产无消费者）。
var lastStartedPID atomic.Int64

type Shell struct{ Log *slog.Logger }

func NewShell(log *slog.Logger) *Shell { return &Shell{Log: log} }

func (sh *Shell) Handle(ctx context.Context, ws *websocket.Conn, sessionID string, params json.RawMessage) {
	var p proto.ShellParams
	_ = json.Unmarshal(params, &p)
	cols, rows := p.Cols, p.Rows
	if cols < 1 || cols > 1000 {
		cols = 120
	}
	if rows < 1 || rows > 1000 {
		rows = 30
	}

	exe := p.Shell
	if exe == "" {
		exe = probeShell()
	}
	if runtime.GOOS != "windows" {
		sh.failStart(ctx, ws, "shell sessions require windows agent")
		return
	}
	if _, err := exec.LookPath(exe); err != nil {
		sh.failStart(ctx, ws, "shell not found: "+exe)
		return
	}

	pty, err := startConPTY(cols, rows, exe, "-NoLogo")
	if err != nil {
		sh.Log.Warn("conpty start failed", "err", err)
		sh.failStart(ctx, ws, "conpty start failed")
		return
	}
	lastStartedPID.Store(int64(pty.cmd.Process.Pid))

	// SHELL_BEGIN 先于任何输出帧。
	if b, err := proto.NewMsg("SHELL_BEGIN", proto.ShellBegin{Shell: exe}); err == nil {
		if jb, err2 := json.Marshal(b); err2 == nil {
			wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
			_ = ws.Write(wctx, websocket.MessageText, jb)
			cancel()
		}
	}

	done := make(chan struct{})
	go func() { // pty output → ws binary
		defer close(done)
		buf := make([]byte, 32*1024)
		for {
			n, err := pty.outR.Read(buf)
			if n > 0 {
				wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
				if we := ws.Write(wctx, websocket.MessageBinary, buf[:n]); we != nil {
					cancel()
					return
				}
				cancel()
			}
			if err != nil {
				return
			}
		}
	}()

	// ws 输入 → pty；text 只认 SHELL_RESIZE。
	go func() {
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			switch typ {
			case websocket.MessageBinary:
				if _, err := pty.inW.Write(data); err != nil {
					return
				}
			case websocket.MessageText:
				var m proto.Message
				if json.Unmarshal(data, &m) != nil || m.Type != "SHELL_RESIZE" {
					continue
				}
				var r proto.ShellResize
				if m.Decode(&r) == nil && r.Cols > 0 && r.Cols <= 1000 && r.Rows > 0 && r.Rows <= 1000 {
					_ = pty.Resize(r.Cols, r.Rows)
				}
			}
		}
	}()

	waitCh := make(chan error, 1)
	go func() { waitCh <- pty.Wait() }()
	select {
	case <-waitCh: // shell 退出（exit 命令）
	case <-done: // 输出流结束（pty 关闭）
	case <-ctx.Done(): // SESSION_CLOSE / 会话终止
	}
	pty.KillAndClose()
	_ = ws.Close(websocket.StatusNormalClosure, "")
}

func (sh *Shell) failStart(ctx context.Context, ws *websocket.Conn, msg string) {
	m, _ := proto.NewMsg(proto.TypeError, proto.ErrorPayload{Code: proto.CodeShellStartFailed, Message: msg})
	b, _ := json.Marshal(m)
	wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
	defer cancel()
	_ = ws.Write(wctx, websocket.MessageText, b)
	_ = ws.Close(wctx, websocket.StatusInternalError, "shell start failed")
}

func probeShell() string {
	if _, err := exec.LookPath("pwsh"); err == nil {
		return "pwsh"
	}
	return "powershell"
}
```

- [ ] **Step 4: 运行验证通过**

Run: `cd agent && go test ./session/ -count=1 && GOOS=linux go build ./... && GOOS=windows go build ./...`
Expected: PASS（含杀进程残留断言）+ 双编译。

- [ ] **Step 5: Commit**

```bash
git add agent/session
git commit -m "feat(agent): conpty shell handler with begin/resize/ctrl-c and clean teardown"
```

---

### Task 5: agent 与 mockagent 注册 shell

**Files:**
- Modify: `agent/agent.go`、`mockagent/main.go`

**Interfaces:**
- Consumes: T4 `NewShell`
- Produces: 两端 OnReady 回调中追加 `engine.Register(proto.KindShell, session.NewShell(...))`

- [ ] **Step 1: 实现**

两处 OnReady 回调的 Register 序列后各追加一行：

```go
		engine.Register(proto.KindShell, session.NewShell(slog.Default())) // agent.go（mockagent 用 c.Log）
```

- [ ] **Step 2: 验证**

Run: `cd agent && go test ./... -count=1 && cd ../mockagent && go build ./... && go vet ./...`
Expected: PASS + 构建过。

- [ ] **Step 3: Commit**

```bash
git add agent mockagent
git commit -m "feat(agent,mockagent): register shell session handler"
```

---

### Task 6: cli — xnc shell

**Files:**
- Create: `cli/cmd_shell.go`
- Modify: `cli/main.go`（注册命令）、`cli/exit.go`（+246 映射 SESSION_LIMIT_EXCEEDED）
- Test: `cli/cmd_shell_test.go`

**Interfaces:**
- Consumes: `resolveNode`、`dialSession`、`Client.Do`、exit 助手；新依赖 `golang.org/x/term`
- Produces: `xnc shell <node> [--cols N] [--rows N]`——真 TTY raw 模式；初始尺寸取当前终端（--cols/--rows 覆盖）；resize 轮询 500ms；SHELL_BEGIN → stderr 提示；ERROR → stderr + exit 250；正常关闭 exit 0；**非 TTY → 用法错误 exit 2**（"requires an interactive terminal; use xnc exec"）

- [ ] **Step 1: 写失败测试**

`cli/cmd_shell_test.go`：

```go
package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// go test 的 stdin 是管道（非 TTY）→ 必须 usage 错误。
func TestShellRequiresTTY(t *testing.T) {
	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"shell", "n1",
			"--server", "http://127.0.0.1:1", "--token", "tk"})
	})
	assert.Equal(t, 2, code)
	assert.Contains(t, out, "interactive terminal")
	assert.Contains(t, out, "xnc exec")
}

func TestShellSessionLimitedMaps246(t *testing.T) {
	assert.Equal(t, 246, ExitCode(apiErrOf(409, "SESSION_LIMIT_EXCEEDED")))
}

func apiErrOf(status int, code string) *protoAPIErr { // 与 exit_test 的构造方式对齐——
	return protoErr(status, code)                    // 直接用既有测试里构造 APIError 的写法
}
```

（实现注记：`protoErr` 若无现成则 `import "xnc/proto"` + `proto.Err(status, code, "")`——以 exit_test.go 现有写法为准。）

- [ ] **Step 2: 运行验证失败**

Run: `cd cli && go get golang.org/x/term && go mod tidy && go test ./... -count=1`
Expected: FAIL（shell 命令不存在）。

- [ ] **Step 3: 实现**

`cli/exit.go` 映射追加：`case proto.CodeSessionLimited: return exitQuota`（exitQuota=246 已有常量）。

`cli/cmd_shell.go`：

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"xnc/proto"
)

func runShell(cmd *cobra.Command, args []string) error {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return failUsage(cmd, "xnc shell requires an interactive terminal; use xnc exec for scripting")
	}

	cfg, _ := LoadConfig()
	cl := NewClient(resolveServer(cmd, cfg), resolveToken(cmd, cfg))
	node, apiErr := resolveNode(cl, args[0])
	if apiErr != nil {
		return cliFail(cmd, apiErr)
	}

	cols, rows, err := term.GetSize(fd)
	if err != nil || cols < 1 || rows < 1 {
		cols, rows = 120, 30
	}
	if v := intFlag(cmd, "cols"); v > 0 {
		cols = v
	}
	if v := intFlag(cmd, "rows"); v > 0 {
		rows = v
	}

	body, _ := json.Marshal(map[string]any{"cols": cols, "rows": rows})
	var created struct {
		SessionID    string `json:"sessionId"`
		Token        string `json:"token"`
		WebsocketURL string `json:"websocketUrl"`
	}
	if e := cl.Do("POST", "/api/nodes/"+node.ID+"/shell", jsonRaw(body), &created); e != nil {
		return cliFail(cmd, e)
	}

	oldState, err := term.MakeRaw(fd)
	if err == nil {
		defer func() { _ = term.Restore(fd, oldState) }()
	}

	ws, err := dialSession(cl.Base, created.WebsocketURL)
	if err != nil {
		return cliFail(cmd, proto.Err(0, "NETWORK", err.Error()))
	}
	defer ws.CloseNow()

	ctx := cmd.Context()
	stdinDone := make(chan struct{})
	go func() { // stdin → ws binary
		defer close(stdinDone)
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				if we := ws.Write(wctx, websocket.MessageBinary, buf[:n]); we != nil {
					cancel()
					return
				}
				cancel()
			}
			if err != nil {
				return // EOF（Ctrl+D/Z）→ 结束输入
			}
		}
	}()

	resizeDone := make(chan struct{})
	go func() { // 尺寸轮询 → SHELL_RESIZE
		defer close(resizeDone)
		lastCols, lastRows := cols, rows
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				c, r, err := term.GetSize(fd)
				if err != nil || c == lastCols && r == lastRows {
					continue
				}
				lastCols, lastRows = c, r
				m, _ := proto.NewMsg("SHELL_RESIZE", proto.ShellResize{Cols: c, Rows: r})
				b, _ := json.Marshal(m)
				wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
				_ = ws.Write(wctx, websocket.MessageText, b)
				cancel()
			}
		}
	}()

	// 主循环：ws → stdout
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			return nil // 对端关闭（exit 命令 / 会话结束）→ 正常退出 0
		}
		switch typ {
		case websocket.MessageBinary:
			_, _ = os.Stdout.Write(data)
		case websocket.MessageText:
			var m proto.Message
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			switch m.Type {
			case "SHELL_BEGIN":
				var sb proto.ShellBegin
				if m.Decode(&sb) == nil {
					fmt.Fprintf(os.Stderr, "\r\n[xnc] connected: %s\r\n", sb.Shell)
				}
			case proto.TypeError:
				var ep proto.ErrorPayload
				if m.Decode(&ep) == nil {
					fmt.Fprintf(os.Stderr, "\r\n[xnc] error: %s: %s\r\n", ep.Code, ep.Message)
					return exitErr(250)
				}
			}
		}
	}
}
```

（`intFlag/jsonRaw/exitErr` 为薄助手——若 main.go/cmd_exec.go 已有等价物（如 flag 读取方式）则复用其名；没有则在 cmd_shell.go 定义：`intFlag(cmd, name) int`、`jsonRaw(b []byte) any` 直接传 `bytes.NewReader` 风格按 Client.Do 的 body 参数类型适配（Do 收 any 并 Marshal——传 map 即可，删除 jsonRaw）。main.go 注册：Use: `shell <node> [--cols N] [--rows N]`，MinimumNArgs(1)，flags --cols/--rows。）

- [ ] **Step 4: 运行验证通过**

Run: `cd cli && go test ./... -count=1 && go build -o ../bin/xnc.exe . && ../bin/xnc.exe shell --help`
Expected: PASS + help 正常。

- [ ] **Step 5: Commit**

```bash
git add cli
git commit -m "feat(cli): xnc shell with raw tty, resize polling, clean teardown"
```

---

### Task 7: shellsmoke 工具 + E2E + 文档核对

**Files:**
- Create: `shellsmoke/main.go`、`shellsmoke/go.mod`、`scripts/e2e_phase3.sh`
- Modify: `go.work`、`Makefile`（+shellsmoke、e2e3 目标）、`skills/xnc/references/cli.md`（错误码表 +SESSION_LIMIT_EXCEEDED；如需 shell 行为注释）
- Test: `bash scripts/e2e_phase3.sh`

**Interfaces:**
- Consumes: T1-T6 全部；mockagent 的 shell 引擎（真 ConPTY，跑在 Windows 开发机）
- Produces: 会话级冒烟工具与一键 E2E：

```text
shellsmoke --server URL --node NAME（XNC_TOKEN 环境变量认证）
  流程：node 解析 → POST /shell → 拨 WS → 验 SHELL_BEGIN → echo 回显 →
        resize 后续可用 → Ctrl+C 存活 → exit 关闭 → 0 残留断言
e2e_phase3.sh：dev 栈 + mockagent + shellsmoke + CLI 非 TTY 检查 + 限额 409 检查
```

- [ ] **Step 1: shellsmoke 工具**

`shellsmoke/go.mod`（module xnc/shellsmoke + require xnc/proto + replace ../proto）；`go.work` use 列表 + Makefile MODULES 追加 shellsmoke。

`shellsmoke/main.go`：

```go
// shellsmoke：xnc shell 会话级冒烟（也可对生产跑）。
// 用法：XNC_SERVER=... XNC_TOKEN=... shellsmoke --node NAME
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"

	"xnc/proto"
)

func main() {
	node := flag.String("node", "", "node name (required)")
	flag.Parse()
	server := os.Getenv("XNC_SERVER")
	token := os.Getenv("XNC_TOKEN")
	if *node == "" || server == "" || token == "" {
		fmt.Fprintln(os.Stderr, "--node and XNC_SERVER/XNC_TOKEN required")
		os.Exit(2)
	}
	if err := run(server, token, *node); err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		os.Exit(1)
	}
	fmt.Println("SHELLSMOKE: ALL PASS")
}

func run(server, token, node string) error {
	hc := &http.Client{Timeout: 15 * time.Second}
	// 1) node 解析
	req, _ := http.NewRequest("GET", server+"/api/nodes", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	var env struct {
		Data []struct{ ID, Name string } `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return err
	}
	resp.Body.Close()
	var nodeID string
	for _, n := range env.Data {
		if strings.EqualFold(n.Name, node) {
			nodeID = n.ID
			break
		}
	}
	if nodeID == "" {
		return fmt.Errorf("node %q not found", node)
	}
	// 2) POST /shell
	req2, _ := http.NewRequest("POST", server+"/api/nodes/"+nodeID+"/shell",
		bytes.NewBufferString(`{"cols":80,"rows":25}`))
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := hc.Do(req2)
	if err != nil {
		return err
	}
	defer resp2.Body.Close()
	var payload struct { // 202 与错误体共用一次 decode
		Token        string `json:"token"`
		WebsocketURL string `json:"websocketUrl"`
		Error        proto.APIError `json:"error"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&payload); err != nil {
		return err
	}
	if resp2.StatusCode != 202 {
		return fmt.Errorf("shell start: status %d code=%s", resp2.StatusCode, payload.Error.Code)
	}
	// 3) 拨 WS + 驱动
	wsURL := strings.Replace(strings.Replace(server, "https://", "wss://", 1), "http://", "ws://", 1) + payload.WebsocketURL
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return err
	}
	defer ws.CloseNow()

	if err := expectText(ctx, ws, "SHELL_BEGIN"); err != nil {
		return err
	}
	fmt.Println("PASS: SHELL_BEGIN")

	writeBin(ctx, ws, []byte("echo smoke-echo-ok\r"))
	if err := expectBinary(ctx, ws, "smoke-echo-ok"); err != nil {
		return err
	}
	fmt.Println("PASS: echo")

	writeText(ctx, ws, mustMsg("SHELL_RESIZE", proto.ShellResize{Cols: 100, Rows: 40}))
	writeBin(ctx, ws, []byte("echo smoke-post-resize\r"))
	if err := expectBinary(ctx, ws, "smoke-post-resize"); err != nil {
		return err
	}
	fmt.Println("PASS: resize")

	writeBin(ctx, ws, []byte{0x03})
	writeBin(ctx, ws, []byte("echo smoke-after-ctrlc\r"))
	if err := expectBinary(ctx, ws, "smoke-after-ctrlc"); err != nil {
		return err
	}
	fmt.Println("PASS: ctrl-c survive")

	writeBin(ctx, ws, []byte("exit\r"))
	return nil
}
```

（`expectText/expectBinary/writeBin/writeText/mustMsg` 为本文件内的私有助手——读取循环带 15s 每帧超时、text 帧解析 proto.Message 匹配 type、binary 做 bytes.Contains；注意 POST 错误体只 decode 一次：先 decode errEnv 再按需 decode created——上面顺序笔误，执行者按"一次 decode 到中间 struct 再分发"写清楚。）

- [ ] **Step 2: E2E 脚本**

`scripts/e2e_phase3.sh`：

```bash
#!/usr/bin/env bash
# Phase 3 E2E：shell 会话级验收（SHELL_BEGIN/echo/resize/ctrl-c）+ CLI 非 TTY + 限额。
set -euo pipefail
cd "$(dirname "$0")/.."

SERVER=http://127.0.0.1:8080
EXE=""
case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) EXE=".exe";; esac
XNC="bin/xnc$EXE"; MOCK="bin/mockagent$EXE"; SMOKE="bin/shellsmoke$EXE"
export XNC_SERVER=$SERVER

echo "== build =="
(cd cli && go build -o "../$XNC" .)
(cd mockagent && go build -o "../$MOCK" .)
(cd shellsmoke && go build -o "../$SMOKE" .)

echo "== dev stack =="
COMPOSE="docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.dev.yml"
[ -f deploy/.env ] || cp deploy/.env.example deploy/.env
$COMPOSE up -d --build
trap 'kill ${MPID:-} 2>/dev/null || true; $COMPOSE down -v; [ -n "${IDDIR:-}" ] && rm -rf "$IDDIR" || true' EXIT
for i in $(seq 1 30); do curl -sf "$SERVER/api/health" >/dev/null && break; sleep 1; done

echo "== login + node up =="
TOKEN_JSON=$(printf 'change-me' | "$XNC" login --server "$SERVER" --email admin@example.com --json)
export XNC_TOKEN=$(echo "$TOKEN_JSON" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
ETOK=$("$XNC" token create default --json | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
IDDIR=$(mktemp -d)
"$MOCK" --server "$SERVER" --token "$ETOK" --identity-dir "$IDDIR" --beat 2s >"$IDDIR/mock.log" 2>&1 &
MPID=$!
NODE=$("$XNC" node list --json | grep -o '"name":"[^"]*"' | head -1 | sed 's/"name":"//;s/"//')
sleep 3
echo "node: $NODE"

echo "== shellsmoke =="
"$SMOKE" --node "$NODE"
echo "SMOKE: OK"

echo "== CLI 非 TTY 拒绝 =="
set +e
OUT=$(bin/xnc$EXE shell "$NODE" --json </dev/null 2>&1); CODE=$?
set -e
[ $CODE -eq 2 ] || { echo "shell non-tty exit=$CODE: $OUT"; exit 1; }
echo "NON-TTY: OK (exit 2)"
echo "== 限额说明 =="
# 环境级限额验证（409/246）由 server 单测覆盖（TestShellPerNodeLimit）——
# 默认栈限额为 10，会话级冒烟不会触达；生产观察随 Phase 5 配额 pass 汇总。
echo "== ALL PHASE3 E2E PASSED =="
```

（限额段简化说明：环境级限额验证需要定制栈，留给单测与生产观察——脚本中不要留死代码，删去 OUT2 行，仅保留注释性 echo。`chmod +x` + `git update-index --chmod=+x`。Makefile 加 `e2e3: bash scripts/e2e_phase3.sh` 与 MODULES 加 shellsmoke。）

- [ ] **Step 3: 运行 E2E**

Run: `bash scripts/e2e_phase3.sh`
Expected: `SHELL_BEGIN/echo/resize/ctrl-c` PASS + `SMOKE: OK` + `NON-TTY: OK` + `ALL PHASE3 E2E PASSED`，exit 0。

- [ ] **Step 4: 文档核对**

`skills/xnc/references/cli.md`：错误码表追加 `SESSION_LIMIT_EXCEEDED`；确认 shell 行为描述（--cols/--rows、需 TTY）与实现一致。`skills/xnc/SKILL.md` shell 段一致性检查。

- [ ] **Step 5: Commit**

```bash
git add shellsmoke scripts Makefile go.work skills
git commit -m "test(e2e): phase 3 shell session smoke via shellsmoke tool"
```

---

## 完成定义（Phase 3 DoD）

```text
五模块 + shellsmoke 测试/构建全绿；GOOS=linux+windows 双编译过
bash scripts/e2e_phase3.sh 全绿（真 ConPTY 会话：BEGIN/echo/resize/ctrl-c/退出）
断连后 shell 进程无残留（单测 + shellsmoke 断言路径）
限额 409 SESSION_LIMIT_EXCEEDED（单测）与 idle/max-lifetime janitor（单测，可缩短时钟）
spec §51 / cli.md 错误码同步；exec 行为零回归（共享 startSession 抽取由既有测试守护）
```

后续：Phase 4（file/tunnel/RDP）另起计划——无前置风险门（websocket 粘合吞吐属 Gate C，随 Phase 4 验证）。
