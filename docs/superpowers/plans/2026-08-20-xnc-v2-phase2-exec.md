# XNC v2 Phase 2（exec 会话）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付统一会话管理器（server）+ 会话引擎与 ExecManager（agent）+ `xnc exec` / `xnc run`（CLI），打通一次性命令与脚本执行全链路，E2E 覆盖 Scenario C/H。

**Architecture:** 会话建立走 v2 设计 §2.2：REST 创建 → 控制连接下发 SESSION_OPEN → agent 拨会话 WS → client 拨另一侧 → server 双向粘合；会话 WS 内 text=控制、binary=数据（exec 数据帧带 1 字节 stdout/stderr 前缀）；exec 超时计时只在 agent（kill 进程树 + EXEC_RESULT），client 断开即取消（server 经控制连接发 SESSION_CLOSE）。会话管理器是 Phase 3-6 复用的核心资产。

**Tech Stack:** 沿用 Phase 1 全栈（Go 1.26、chi、coder/websocket、pgx/sqlc、cobra、testcontainers-go、testify）；新增依赖仅 cli 模块的 coder/websocket。

**Spec:** `docs/superpowers/specs/2026-08-19-xnc-v2-unified-session-design.md` §2-§3 + 重写后的 `spec.md` §11-§14/§27/§58。执行者必须同时阅读两份。

## Global Constraints

- 模块路径与结构不变：`xnc/proto|server|agent|cli|mockagent`，go.work，replace ../proto；本机无 make/sqlc 需 `go install`（sqlc v1.31 已装）。
- `proto/` 仍是唯一协议定义点：SESSION_OPEN/REFUSED/CLOSE 的 payload struct、ExecParams/ExecResult、kind 常量只在此定义。
- 会话帧规则（设计 §2.4，绑定）：text frame = 会话级控制 JSON；binary frame = 不透明流字节；exec 的 binary = `[1 字节流标识][字节块]`，0x01=stdout、0x02=stderr。
- 超时责任（设计 §2.3，绑定）：exec 的 `timeoutSec` 只由 **agent** 计时（到点 kill 进程树 → `EXEC_RESULT{exitCode:null, timedOut:true}` → 关会话）；server 只负责 Opening TTL **60s**；client 断开 = 取消（server 发 SESSION_CLOSE，agent 清理）。
- 取消与清理：任一侧断开 → server 关闭对侧 + 经控制连接发 `SESSION_CLOSE{sessionId, reason}`；manager 清理必须幂等（sync.Once）。
- token 语义（设计 §2.2，绑定）：双侧 token 随机 32B、60s、单用途；client 经 `?token=` query；agent 的完整拨号 URL（含 agentToken）由 SESSION_OPEN.WsURL 携带（绝对 URL）。
- 绝对 URL 推导：`X-Forwarded-Proto` 头优先（caddy 注入 https），否则 r.TLS 判定；http→ws、https→wss。
- 审计（spec §7.6/§40，绑定）：exec.start 在 202 时写入、exec.finish 在会话关闭时写入（metadata 含 reason，**不含命令内容**）；日志同样不得记录命令/script 明文。
- 鉴权姿态：Phase 2 沿用 membership（任意成员可 exec，与 nodes API 一致）；viewer 角色拦截属 Phase 5。
- CLI 契约（cli.md，绑定）：`xnc exec <node> [--timeout 300] [--cwd PATH] [--] <command...>`；`xnc run <node> (--file x.ps1 | -) [--timeout 300]`；退出码透传，`timedOut`/未执行完 → **243**；--json envelope data={node, exitCode, stdout, stderr, durationMs, timedOut}；NODE_OFFLINE → 242。
- script ≤ 256 KB（agent 侧拒 + CLI 侧预检）；临时文件 `%TEMP%\xnc-<sessionId>.ps1`（跨平台 os.TempDir()），成功与失败路径都必须删除。
- 进程树 kill：windows 用 `taskkill /PID <pid> /T /F`，非 windows 用 Process.Kill()（标注 Linux 组杀为 Phase 8 议题）。
- 测试需 Docker（testcontainers）；每 Task 一 commit，conventional 风格；GOOS=linux+windows 双编译是 agent/cli 的验收门槛。

**本计划不含**：shell/file/screen/tunnel 会话（Phase 3-6）、RBAC viewer 拦截（Phase 5）、exec 并发限额（Phase 5 配额 pass）、Linux 进程组杀（Phase 8）。

---

## 文件结构总览

```text
proto/
├── session.go                 T1  会话 payload + exec 词汇 + kind 常量
└── session_test.go            T1
server/internal/session/
├── manager.go                 T2  生命周期核心（create/attach/expire/close，无 WS 细节）
├── pump.go                    T3  双向粘合（真实 WS 帧转发）
└── manager_test.go            T2/T3
server/internal/api/
├── sessionws.go               T3  /api/session/{id} 与 /api/agent/session 两个 WS 端点
├── exec_handlers.go           T4  POST /api/nodes/{id}/exec + 审计
├── sess_test_helpers_test.go  T3  全链路测试助手（假 agent 拨号协议）
├── sessionws_test.go          T3
└── exec_handlers_test.go      T4
server/internal/registry/registry.go   T3  NodeConn 增加 Send（修改）
server/internal/api/agentws.go         T3  控制连接写串行化 + SESSION_OPEN/CLOSE 通路（修改）
agent/connect/client.go               T5  drain → 分发循环（Handler 回调）（修改）
agent/connect/client_test.go          T5
agent/session/
├── engine.go                  T6  SESSION_OPEN 分发 + 会话表 + 拒绝路径
├── exec.go                    T6/T7  ExecManager（命令模式 T6，脚本模式 T7）
└── exec_test.go               T6/T7
agent/agent.go                 T8  装配 engine（修改）
mockagent/main.go              T8  注入 engine（修改）
cli/
├── nodeselect.go              T9  resolveNode 抽取（从 cmd_node.go）
├── ws.go                      T9  会话 WS 客户端拨号
├── cmd_exec.go                T9/T10  exec + run
├── cmd_exec_test.go           T9/T10
└── cmd_node.go                T9  改用 resolveNode（修改）
scripts/e2e_phase2.sh          T11
Makefile                       T11  e2e2 目标
```

---

### Task 1: proto — 会话 payload 与 exec 词汇

**Files:**
- Create: `proto/session.go`
- Test: `proto/session_test.go`

**Interfaces:**
- Consumes: 既有 `proto.Message`/`NewMsg`/`Decode` 与 TypeSessionOpen/TypeSessionRefused/TypeSessionClose 常量（Phase 1 已定义）
- Produces（后续所有任务依赖）:

```go
const KindExec = "exec"

type SessionOpen struct {
    SessionID  string          `json:"sessionId"`
    Kind       string          `json:"kind"`
    Params     json.RawMessage `json:"params"`
    AgentToken string          `json:"agentToken"`
    WsURL      string          `json:"wsUrl"`     // agent 拨号的绝对 URL，含 ?token=
    ExpiresAt  time.Time       `json:"expiresAt"`
}
type SessionRefused struct {
    SessionID string `json:"sessionId"`
    Code      string `json:"code"`
    Message   string `json:"message"`
}
type SessionClose struct {
    SessionID string `json:"sessionId"`
    Reason    string `json:"reason"`
}
type ExecParams struct {          // 会话 Params 的 exec 形态；command/script 二选一
    Command    string `json:"command,omitempty"`
    Script     string `json:"script,omitempty"`
    TimeoutSec int    `json:"timeoutSec,omitempty"`
    Cwd        string `json:"cwd,omitempty"`
}
type ExecResult struct {          // 会话 WS 的终态 text 帧
    ExitCode   *int   `json:"exitCode"`   // 超时/被杀时为 null
    TimedOut   bool   `json:"timedOut"`
    DurationMs int64  `json:"durationMs"`
}
func (m Message) IsText() bool    // 不需要——Message 不区分帧类型（WS 层职责），勿加
```

- [ ] **Step 1: 写失败测试**

`proto/session_test.go`：

```go
package proto

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionOpenRoundtrip(t *testing.T) {
	params, _ := json.Marshal(ExecParams{Command: "Get-Service", TimeoutSec: 300})
	m, err := NewMsg(TypeSessionOpen, SessionOpen{
		SessionID: "s-1", Kind: KindExec, Params: params,
		AgentToken: "at", WsURL: "wss://x/api/agent/session?token=at",
	})
	require.NoError(t, err)
	b, err := json.Marshal(m)
	require.NoError(t, err)
	assert.JSONEq(t, `{"type":"SESSION_OPEN","payload":{"sessionId":"s-1","kind":"exec",
		"params":{"command":"Get-Service","timeoutSec":300},"agentToken":"at",
		"wsUrl":"wss://x/api/agent/session?token=at","expiresAt":"0001-01-01T00:00:00Z"}}`, string(b))

	var got Message
	require.NoError(t, json.Unmarshal(b, &got))
	var so SessionOpen
	require.NoError(t, got.Decode(&so))
	assert.Equal(t, KindExec, so.Kind)
	var p ExecParams
	require.NoError(t, json.Unmarshal(so.Params, &p))
	assert.Equal(t, "Get-Service", p.Command)
}

func TestExecResultNullExitCode(t *testing.T) {
	b, err := json.Marshal(ExecResult{ExitCode: nil, TimedOut: true, DurationMs: 1001})
	require.NoError(t, err)
	assert.JSONEq(t, `{"exitCode":null,"timedOut":true,"durationMs":1001}`, string(b))

	var r ExecResult
	require.NoError(t, json.Unmarshal([]byte(`{"exitCode":7,"timedOut":false,"durationMs":9}`), &r))
	require.NotNil(t, r.ExitCode)
	assert.Equal(t, 7, *r.ExitCode)

	seven := 7
	b2, _ := json.Marshal(ExecResult{ExitCode: &seven})
	assert.Contains(t, string(b2), `"exitCode":7`)
}

func TestSessionCloseRefused(t *testing.T) {
	m, _ := NewMsg(TypeSessionClose, SessionClose{SessionID: "s-1", Reason: "client-gone"})
	b, _ := json.Marshal(m)
	assert.JSONEq(t, `{"type":"SESSION_CLOSE","payload":{"sessionId":"s-1","reason":"client-gone"}}`, string(b))
	m2, _ := NewMsg(TypeSessionRefused, SessionRefused{SessionID: "s-2", Code: CodeKindUnsupported, Message: "kind"})
	b2, _ := json.Marshal(m2)
	assert.Contains(t, string(b2), `"SESSION_REFUSED"`)
}
```

- [ ] **Step 2: 运行验证失败**

Run: `cd proto && go test ./...`
Expected: FAIL（session.go 未定义，编译错误）。

- [ ] **Step 3: 实现**

`proto/session.go`：

```go
package proto

import (
	"encoding/json"
	"time"
)

// 会话 kind。Phase 2 仅 exec；shell/file/screen/tunnel 由后续 Phase 注册。
const KindExec = "exec"

// SessionOpen 经控制连接下发：agent 按 WsURL（含 token 的绝对 URL）拨号。
type SessionOpen struct {
	SessionID  string          `json:"sessionId"`
	Kind       string          `json:"kind"`
	Params     json.RawMessage `json:"params"`
	AgentToken string          `json:"agentToken"`
	WsURL      string          `json:"wsUrl"`
	ExpiresAt  time.Time       `json:"expiresAt"`
}

// SessionRefused agent 不支持 kind 等能力协商失败时经控制连接回送。
type SessionRefused struct {
	SessionID string `json:"sessionId"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

// SessionClose server → agent 的会话清理通知（client 断开/超时/节点侧主动结束）。
type SessionClose struct {
	SessionID string `json:"sessionId"`
	Reason    string `json:"reason"`
}

// ExecParams 会话 Params 的 exec 形态；command 与 script 二选一。
type ExecParams struct {
	Command    string `json:"command,omitempty"`
	Script     string `json:"script,omitempty"`
	TimeoutSec int    `json:"timeoutSec,omitempty"`
	Cwd        string `json:"cwd,omitempty"`
}

// ExecResult exec 会话 WS 的终态 text 帧；超时/被杀时 ExitCode 为 null。
type ExecResult struct {
	ExitCode   *int   `json:"exitCode"`
	TimedOut   bool   `json:"timedOut"`
	DurationMs int64  `json:"durationMs"`
}
```

- [ ] **Step 4: 运行验证通过**

Run: `cd proto && go test ./...`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add proto
git commit -m "feat(proto): session payloads and exec vocabulary"
```

---

### Task 2: server — 会话管理器核心（生命周期）

**Files:**
- Create: `server/internal/session/manager.go`
- Test: `server/internal/session/manager_test.go`

**Interfaces:**
- Consumes: `registry.Registry.Online(nodeID) bool`；`proto.APIError`/`proto.Err`/`proto.CodeInternal`
- Produces（T3/T4 依赖，签名精确）:

```go
type Manager struct{ ... }
func New(reg *registry.Registry, log *slog.Logger) *Manager

type Session struct { // 仅导出只读用途字段；连接与 token 为私有
    ID     string
    Kind   string
    NodeID uuid.UUID
    UserID uuid.UUID
    Params json.RawMessage
}
type CreateResult struct {
    Session      *Session
    AgentToken   string        // 仅进 SESSION_OPEN，绝不进 REST 响应
    ClientToken  string        // REST 响应的 token
    ExpiresAt    time.Time
    ClientPath   string        // "/api/session/<id>"
}

func (m *Manager) Create(nodeID, userID uuid.UUID, kind string, params json.RawMessage) (*CreateResult, *proto.APIError)
func (m *Manager) AttachAgent(sessionID, token string, ws *websocket.Conn) *proto.APIError
func (m *Manager) AttachClient(sessionID, token string, ws *websocket.Conn) *proto.APIError
func (m *Manager) NotifyClose(sessionID, reason string)   // api 层在控制连接发送 SESSION_CLOSE 失败时兜底清理
// 供 api 层注入的钩子（Create 返回后立即设置）：
//   res.Session.finish = func(reason string)      —— 审计 exec.finish（T4 设置）
//   res.Session.notifyAgent = func(so SessionClose) —— 控制连接发送器（T3/T4 设置）
// 钩子以未导出字段 + Manager 上的 setter 暴露：
func (m *Manager) SetFinishFn(s *Session, fn func(reason string))
func (m *Manager) SetNotifyFn(s *Session, fn func(sc proto.SessionClose) error)
```

- [ ] **Step 1: 写失败测试**

`server/internal/session/manager_test.go`：

```go
package session

import (
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
	"xnc/server/internal/registry"
)

func newMgr(t *testing.T) *Manager {
	t.Helper()
	return New(registry.New(), slog.Default())
}

func TestCreateAttachExpire(t *testing.T) {
	m := newMgr(t)
	res, apiErr := m.Create(uuid.New(), uuid.New(), proto.KindExec, []byte(`{}`))
	require.Nil(t, apiErr)
	require.NotEmpty(t, res.Session.ID)
	assert.NotEqual(t, res.AgentToken, res.ClientToken)
	assert.Equal(t, "/api/session/"+res.Session.ID, res.ClientPath)
	assert.WithinDuration(t, time.Now().Add(60*time.Second), res.ExpiresAt, 2*time.Second)

	// token 错误 → 401；会话不存在 → 404
	assert.Equal(t, 401, m.AttachAgent(res.Session.ID, "wrong", nil).Status)
	assert.Equal(t, 404, m.AttachAgent(uuid.NewString(), res.AgentToken, nil).Status)

	// 正确 attach（nil ws 允许：生命周期先行，粘合由 T3 的真实连接触发）
	assert.Nil(t, m.AttachAgent(res.Session.ID, res.AgentToken, nil))
	// token 单用途：二次使用 → 401
	assert.Equal(t, 401, m.AttachAgent(res.Session.ID, res.AgentToken, nil))
	// agent 侧重复 attach（另一 token 不存在）→ 404
	assert.Equal(t, 404, m.AttachAgent(res.Session.ID, "another", nil))
}

func TestOpeningTimeoutFiresFinish(t *testing.T) {
	m := newMgr(t)
	res, apiErr := m.Create(uuid.New(), uuid.New(), proto.KindExec, []byte(`{}`))
	require.Nil(t, apiErr)

	var reasons []string
	var notified []string
	m.SetFinishFn(res.Session, func(r string) { reasons = append(reasons, r) })
	m.SetNotifyFn(res.Session, func(sc proto.SessionClose) error {
		notified = append(notified, sc.Reason)
		return nil
	})
	m.shortenOpeningTTL(res.Session, 30*time.Millisecond) // 测试后门：缩短 TTL

	time.Sleep(120 * time.Millisecond)
	assert.Equal(t, []string{"opening-timeout"}, reasons)
	assert.Equal(t, []string{"opening-timeout"}, notified)
	// 过期后 token 失效
	assert.Equal(t, 410, m.AttachClient(res.Session.ID, res.ClientToken, nil).Status)
}

func TestCloseIdempotent(t *testing.T) {
	m := newMgr(t)
	res, _ := m.Create(uuid.New(), uuid.New(), proto.KindExec, []byte(`{}`))
	var n atomic.Int32
	m.SetFinishFn(res.Session, func(string) { n.Add(1) })
	m.SetNotifyFn(res.Session, func(proto.SessionClose) error { return nil })

	m.NotifyClose(res.Session.ID, "client-gone")
	m.NotifyClose(res.Session.ID, "client-gone") // 幂等
	assert.Equal(t, int32(1), n.Load())
}

func TestOfflineNodeRefused(t *testing.T) {
	// registry 无此节点连接 → Create 拒绝 NODE_OFFLINE（409）
	m := newMgr(t)
	_, apiErr := m.Create(uuid.New(), uuid.New(), proto.KindExec, []byte(`{}`))
	require.NotNil(t, apiErr)
	assert.Equal(t, 409, apiErr.Status)
	assert.Equal(t, proto.CodeNodeOffline, apiErr.Code)
}
```

（注：`Create` 内部检查 `reg.Online(nodeID)`——本测试用空 registry 制造离线；`TestCreateAttachExpire` 等用例因此需要 registry 中存在连接。用 `registry.NodeConn{NodeID: id, LastBeat: time.Now()}` 注入 `Add` 即可，`Cancel` 为 nil 时跳过调用——`Add` 需容忍 nil Cancel。测试助手改为：

```go
func onlineMgr(t *testing.T) (uuid.UUID, *Manager) {
	t.Helper()
	reg := registry.New()
	id := uuid.New()
	reg.Add(&registry.NodeConn{NodeID: id.String(), LastBeat: time.Now()})
	return id, New(reg, slog.Default())
}
```

`TestCreateAttachExpire`/`TestOpeningTimeoutFiresFinish`/`TestCloseIdempotent` 改用 `onlineMgr`。）

- [ ] **Step 2: 运行验证失败**

Run: `cd server && go test ./internal/session/`
Expected: FAIL（包不存在）。

- [ ] **Step 3: 实现**

`server/internal/session/manager.go`：

```go
// Package session 实现统一会话生命周期：创建（双侧 token）、双侧 attach、
// Opening TTL、关闭与清理。WS 帧粘合在 pump.go。
package session

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"xnc/proto"
	"xnc/server/internal/registry"
)

const openingTTL = 60 * time.Second

type Manager struct {
	reg *registry.Registry
	log *slog.Logger

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	id          string
	kind        string
	nodeID      uuid.UUID
	userID      uuid.UUID
	params      json.RawMessage
	agentToken  string
	clientToken string
	expiresAt   time.Time

	agentWS  *websocket.Conn
	clientWS *websocket.Conn
	glued    bool
	ttl      *time.Timer

	finish      func(reason string)
	notifyAgent func(sc proto.SessionClose) error
	closeOnce   sync.Once
}

// Session 是对外只读视图（ID/Kind/NodeID/UserID/Params）。
type Session session

type CreateResult struct {
	Session     *Session
	AgentToken  string
	ClientToken string
	ExpiresAt   time.Time
	ClientPath  string
}

func New(reg *registry.Registry, log *slog.Logger) *Manager {
	return &Manager{reg: reg, log: log, sessions: map[string]*session{}}
}

func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (m *Manager) Create(nodeID, userID uuid.UUID, kind string, params json.RawMessage) (*CreateResult, *proto.APIError) {
	if !m.reg.Online(nodeID.String()) {
		return nil, proto.Err(409, proto.CodeNodeOffline, "node is offline")
	}
	s := &session{
		id: uuid.NewString(), kind: kind, nodeID: nodeID, userID: userID,
		params: params, agentToken: newToken(), clientToken: newToken(),
		expiresAt: time.Now().Add(openingTTL),
	}
	m.mu.Lock()
	m.sessions[s.id] = s
	m.mu.Unlock()
	s.ttl = time.AfterFunc(openingTTL, func() { m.expire(s) })
	return &CreateResult{
		Session: (*Session)(s), AgentToken: s.agentToken, ClientToken: s.clientToken,
		ExpiresAt: s.expiresAt, ClientPath: "/api/session/" + s.id,
	}, nil
}

func (m *Manager) expire(s *session) {
	m.mu.Lock()
	if s.glued {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	m.close(s, "opening-timeout")
}

// shortenOpeningTTL 仅供测试缩短 Opening TTL。
func (m *Manager) shortenOpeningTTL(s *Session, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	(*session)(s).ttl.Reset(d)
}

func (m *Manager) attach(side, sessionID, token string, ws *websocket.Conn) *proto.APIError {
	m.mu.Lock()
	s, ok := m.sessions[sessionID]
	if !ok {
		m.mu.Unlock()
		return proto.Err(404, proto.CodeSessionNotFound, "session not found")
	}
	if time.Now().After(s.expiresAt) {
		m.mu.Unlock()
		return proto.Err(410, proto.CodeSessionExpired, "session expired")
	}
	var want string
	switch side {
	case "agent":
		want = s.agentToken
	case "client":
		want = s.clientToken
	}
	if token != want { // 常量时间比较对一次性随机 token 非必要（非常量 Secret 固定值）
		m.mu.Unlock()
		return proto.Err(401, proto.CodeUnauthorized, "invalid session token")
	}
	if side == "agent" {
		s.agentToken = "" // 单用途：用后即焚
		s.agentWS = ws
	} else {
		s.clientToken = ""
		s.clientWS = ws
	}
	both := s.agentWS != nil && s.clientWS != nil
	if both && !s.glued {
		s.glued = true
		if s.ttl != nil {
			s.ttl.Stop()
		}
	}
	m.mu.Unlock()
	if both {
		m.pump((*session)(s)) // T3 实现；T2 阶段 pump 为 no-op 也先留桩方法
	}
	return nil
}

func (m *Manager) AttachAgent(sessionID, token string, ws *websocket.Conn) *proto.APIError {
	return m.attach("agent", sessionID, token, ws)
}

func (m *Manager) AttachClient(sessionID, token string, ws *websocket.Conn) *proto.APIError {
	return m.attach("client", sessionID, token, ws)
}

// NotifyClose 幂等关闭：关双侧连接、清表、回调 finish、通知 agent。
func (m *Manager) NotifyClose(sessionID, reason string) {
	m.mu.Lock()
	s, ok := m.sessions[sessionID]
	m.mu.Unlock()
	if ok {
		m.close(s, reason)
	}
}

func (m *Manager) close(s *session, reason string) {
	s.closeOnce.Do(func() {
		if s.ttl != nil {
			s.ttl.Stop()
		}
		if s.agentWS != nil {
			_ = s.agentWS.CloseNow()
		}
		if s.clientWS != nil {
			_ = s.clientWS.CloseNow()
		}
		m.mu.Lock()
		delete(m.sessions, s.id)
		m.mu.Unlock()
		if s.notifyAgent != nil {
			_ = s.notifyAgent(proto.SessionClose{SessionID: s.id, Reason: reason})
		}
		if s.finish != nil {
			s.finish(reason)
		}
	})
}

func (m *Manager) SetFinishFn(s *Session, fn func(reason string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	(*session)(s).finish = fn
}

func (m *Manager) SetNotifyFn(s *Session, fn func(sc proto.SessionClose) error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	(*session)(s).notifyAgent = fn
}
```

（`m.pump(s *session)` 在本 Task 先建空方法 `func (m *Manager) pump(s *session) {}` 并留注释 `// T3 实现帧粘合`；`TestOpeningTimeoutFiresFinish` 中 attach 用 nil ws 直接测 TTL，不触发 pump。）

同步修改 `server/internal/registry/registry.go` 的 `Add`：`if old, ok := r.conns[c.NodeID]; ok && old.Cancel != nil { old.Cancel() }` 已容忍 nil——若现实现已如此则不动，否则补 nil 判断（写进本 Task，测试 `TestRegistryNilCancel` 断言 nil Cancel 不 panic）。

- [ ] **Step 4: 运行验证通过**

Run: `cd server && go test ./internal/session/ ./internal/registry/`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add server/internal/session server/internal/registry
git commit -m "feat(server): session manager lifecycle with single-use tokens and opening ttl"
```

---

### Task 3: server — 会话 WS 端点、帧粘合与控制连接通路

**Files:**
- Create: `server/internal/session/pump.go`、`server/internal/api/sessionws.go`、`server/internal/api/sess_test_helpers_test.go`、`server/internal/api/sessionws_test.go`
- Modify: `server/internal/api/agentws.go`（写串行化 + NodeConn.Send）、`server/internal/registry/registry.go`（NodeConn.Send 字段）、`server/internal/api/router.go`（挂载端点 + Manager 构造）

**Interfaces:**
- Consumes: T2 Manager 全部；agentws 既有 `handlers{st,cfg,reg}`；TestEnv
- Produces:

```go
// registry.NodeConn 新增字段（agentws 赋值）：
type NodeConn struct { ...; Send func(m proto.Message) error }

// api.handlers 新增字段（router.go 构造）：
sess *session.Manager

// 端点：
// GET /api/session/{id}?token=     client 侧（AttachClient，失败即关）
// GET /api/agent/session?token=    agent 侧（AttachAgent）
// （两者均不需要 JWT：token 即凭证）

// session.pump（T3 实装）：
func (m *Manager) pump(s *session)  // 双 goroutine 双向转发 text+binary 帧，任一方向错误 → NotifyClose(reason)

// wsBaseURL 推导（sessionws.go）：
func wsBaseURL(r *http.Request) string   // X-Forwarded-Proto > r.TLS > 默认 ws
```

- [ ] **Step 1: 写失败测试（全链路）**

`server/internal/api/sess_test_helpers_test.go`——假 agent 协议助手（供 T3/T4 测试复用）：

```go
package api

import (
	"context"
	"encoding/json"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

// fakeAgentSession 在 control WS 上等 SESSION_OPEN，拨 agent 会话 WS，
// 随后按脚本 act 与 client 侧交互；返回收到的 SessionOpen。
func fakeAgentSession(t *testing.T, ctrlWS *websocket.Conn, act func(ws *websocket.Conn)) proto.SessionOpen {
	t.Helper()
	m := readMsg(t, ctrlWS)
	require.Equal(t, proto.TypeSessionOpen, m.Type)
	var so proto.SessionOpen
	require.NoError(t, m.Decode(&so))

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, so.WsURL, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ws.CloseNow() })
	act(ws)
	return so
}

// dialClientSession 以 CreateResult 的 token 拨 client 侧。
func dialClientSession(t *testing.T, base, path, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+base[4:]+path+"?token="+token, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ws.CloseNow() })
	return ws
}

// wsWriteText / wsWriteBinary：带 5s 超时的写助手。
func wsWriteText(t *testing.T, ws *websocket.Conn, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, ws.Write(ctx, websocket.MessageText, b))
}

func wsWriteBinary(t *testing.T, ws *websocket.Conn, data []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	require.NoError(t, ws.Write(ctx, websocket.MessageBinary, data))
}
```

（`readMsg`/`writeMsg` 已在 agentws_test.go——若为 `_test.go` 内私有则可直接复用同包；若重名则在 helpers 里别名。）

`server/internal/api/sessionws_test.go`：

```go
package api

import (
	"context"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

// 直接驱动 manager（绕过 REST，REST 属 T4）：Create → 两侧拨号 → 帧透传。
func TestSessionGlueRelaysFrames(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-S1", "mid-s1")
	ctrl := dialControl(t, env, nodeID) // 见下：完整挑战+HELLO 的控制连接助手

	res, apiErr := env.Sess.Create(mustUUID(nodeID), env.AdminUUID(t), proto.KindExec, []byte(`{}`))
	require.Nil(t, apiErr)

	fakeAgentSession(t, ctrl, func(aws *websocket.Conn) {
		wsWriteBinary(t, aws, append([]byte{0x01}, []byte("hello-stdout")...))
		wsWriteText(t, aws, mustMsg(t, proto.ExecResult{TimedOut: false, DurationMs: 1})) // 见 helpers：mustMsg 构造 {"type":"EXEC_RESULT","payload":...}
	})

	cl := dialClientSession(t, env.srv.URL, res.ClientPath, res.ClientToken)
	got := readBin(t, cl)
	assert.Equal(t, append([]byte{0x01}, []byte("hello-stdout")...), got)
}

// TestSessionClientDisconnectCloses：client 关闭 → agent 侧读错误 → NotifyClose →
// 控制连接收到 SESSION_CLOSE{reason:"peer-disconnect"}。
func TestSessionClientDisconnectCloses(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-S2", "mid-s2")
	ctrl := dialControl(t, env, nodeID)

	res, _ := env.Sess.Create(mustUUID(nodeID), env.AdminUUID(t), proto.KindExec, []byte(`{}`))
	agentGotClose := make(chan proto.SessionClose, 1)
	fakeAgentSession(t, ctrl, func(aws *websocket.Conn) {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		_, _, err := aws.Read(ctx) // 等 client 断开传导
		require.Error(t, err)
	})
	_ = agentGotClose

	cl := dialClientSession(t, env.srv.URL, res.ClientPath, res.ClientToken)
	_ = cl.CloseNow()

	// 控制连接上应收到 SESSION_CLOSE
	m := readMsg(t, ctrl)
	require.Equal(t, proto.TypeSessionClose, m.Type)
	var sc proto.SessionClose
	require.NoError(t, m.Decode(&sc))
	assert.Equal(t, res.Session.ID, sc.SessionID)
	assert.NotEmpty(t, sc.Reason)
}
```

实现者注意：`mustMsg(t, v any)` 在 helpers 里构造 EXEC_RESULT text 帧：`proto.NewMsg("EXEC_RESULT", v)`（server 不解析会话帧，"EXEC_RESULT" 字面量属 kind 私有词汇，允许出现在测试与 agent 实现中）；`readBin` 为读 binary 帧助手（`ws.Read` 后断言 `websocket.MessageBinary`）。需要的 TestEnv 扩展：`env.Sess *session.Manager`、`env.AdminUUID(t)`、`dialControl(t, env, nodeID)`（完整走挑战+HELLO 的真 WS，返回保持连接——复用 agentws_test.go 的既有流程封装成助手，注意该文件在同包）。

- [ ] **Step 2: 运行验证失败**

Run: `cd server && go test ./internal/api/ -run TestSession -count=1`
Expected: FAIL（Sess/助手不存在）。

- [ ] **Step 3: 实现**

`server/internal/session/pump.go`：

```go
package session

import (
	"context"
	"time"

	"github.com/coder/websocket"
)

const pumpWriteTimeout = 60 * time.Second

// pump 双向转发 text+binary 帧（server 不解析会话帧）；任一方向失败 → NotifyClose。
func (m *Manager) pump(s *session) {
	go func() {
		m.relay(s, s.clientWS, s.agentWS)
		m.NotifyClose(s.id, "peer-disconnect")
	}()
	go func() {
		m.relay(s, s.agentWS, s.clientWS)
		m.NotifyClose(s.id, "peer-disconnect")
	}()
}

func (m *Manager) relay(from, to *websocket.Conn) {
	if from == nil || to == nil {
		return
	}
	buf := make([]byte, 32*1024)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), pumpWriteTimeout)
		typ, r, err := from.Reader(ctx)
		if err != nil {
			cancel()
			return
		}
		w, err := to.Writer(ctx, typ)
		if err != nil {
			cancel()
			return
		}
		if _, err = copyBuf(w, r, buf); err != nil {
			cancel()
			return
		}
		if err = w.Close(); err != nil {
			cancel()
			return
		}
		cancel()
	}
}

func copyBuf(w *websocket.Writer, r *websocket.Reader, buf []byte) (int64, error) {
	var n int64
	for {
		nr, er := r.Read(buf)
		if nr > 0 {
			nw, ew := w.Write(buf[:nr])
			n += int64(nw)
			if ew != nil {
				return n, ew
			}
		}
		if er != nil {
			return n, nil // io.EOF 属正常
		}
	}
}
```

`server/internal/api/sessionws.go`：

```go
package api

import (
	"log/slog"
	"net/http"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"xnc/proto"
	"xnc/server/internal/session"
)

// wsBaseURL 推导绝对 WS 基址：X-Forwarded-Proto 优先（caddy 注入 https），
// 其次 r.TLS，默认 ws（本地 dev 明文）。
func wsBaseURL(r *http.Request) string {
	scheme := "ws"
	if p := r.Header.Get("X-Forwarded-Proto"); p == "https" {
		scheme = "wss"
	} else if r.TLS != nil {
		scheme = "wss"
	}
	return scheme + "://" + r.Host
}

func (h *handlers) clientSessionWS(w http.ResponseWriter, r *http.Request) {
	h.sessionWS(w, r, chi.URLParam(r, "id"), r.URL.Query().Get("token"), h.sess.AttachClient)
}

func (h *handlers) agentSessionWS(w http.ResponseWriter, r *http.Request) {
	h.sessionWS(w, r, "", r.URL.Query().Get("token"), h.sess.AttachAgent)
}

func (h *handlers) sessionWS(w http.ResponseWriter, r *http.Request, id, token string,
	attach func(string, string, *websocket.Conn) *proto.APIError) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	if apiErr := attach(id, token, c); apiErr != nil {
		slog.Warn("session attach rejected", "code", apiErr.Code)
		return // attach 失败：defer 关闭连接即可（无 REST 响应体可写）
	}
	// 连接交由 manager/pump 接管。此处不能读帧（会消费 pump 的数据），
	// 挂起直到请求 ctx 结束；manager 关闭连接（CloseNow）时 ctx 被 cancel，handler 退出。
	<-r.Context().Done()
}
```

（粘合触发前（对端未到）必须持续读吗？——不能读：pump 未启动，读了会消费帧。正确做法：attach 后阻塞在 `<-r.Context().Done()` 或由 manager 在关闭时关闭连接——handler 挂在 ctx 上即可，CloseNow 由 manager 触发后 handler 退出。将 for-read 循环替换为 `<-r.Context().Done()`，并注明原因。）

`registry.go` NodeConn 增加 `Send func(m proto.Message) error` 字段；`agentws.go`：

```go
// 控制连接写串行化：心跳 ACK 与 SESSION_OPEN/CLOSE 共用此发送器。
var wmu sync.Mutex
sendControl := func(m proto.Message) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	wmu.Lock()
	defer wmu.Unlock()
	return c.Write(ctx, websocket.MessageText, b)
}
conn.Send = sendControl
```

（心跳分支里的 `h.writeJSON(ctx, c, ...)` 改为 `sendControl(...)`，保持单一写路径。）

**控制连接读循环补 SESSION_REFUSED 分支**（agentws.go 心跳循环的 switch 增加）：

```go
case proto.TypeSessionRefused:
	var sr proto.SessionRefused
	if err := m.Decode(&sr); err == nil && h.sess != nil {
		h.sess.NotifyClose(sr.SessionID, "refused:"+sr.Code)
	}
```

`registry.go` 本 Task 同步补 `Get`：

```go
func (r *Registry) Get(nodeID string) *NodeConn {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.conns[nodeID]
}
```

（附单测：Add 后 Get 返回同指针；不存在返回 nil。T4 的 exec handler 直接使用。）

`router.go`：构造 `sess := session.New(reg, slog.Default())`，`h.sess = sess`，挂载：

```go
r.Get("/api/session/{id}", h.clientSessionWS)
r.Get("/api/agent/session", h.agentSessionWS)
```

TestEnv 增加 `Sess` 字段（NewTestEnv 里从 router 侧暴露——handlers 私有，测试同包可直接构造后传入 NewRouter？NewRouter 内部建 manager 则测试拿不到。方案：NewRouter 增加可变参数或导出 `NewRouterWithSession(st, cfg, reg, sess)`；取简单者——`NewRouter` 签名不变，新增导出字段不可行，故新增：`func NewRouterWithSession(st *db.Store, cfg config.Config, reg *registry.Registry, sess *session.Manager) http.Handler`，`NewRouter` 变为调用它（sess=nil 时自建）。测试用 NewRouterWithSession 传入自建 manager 存 env.Sess。）

- [ ] **Step 4: 运行验证通过**

Run: `cd server && go test ./internal/api/ -run TestSession -count=1 && go test ./... -count=1`
Expected: PASS（全量回归含 Phase 1 用例）。

- [ ] **Step 5: Commit**

```bash
git add server/internal/session server/internal/api server/internal/registry
git commit -m "feat(server): session ws endpoints, frame pump, serialized control-channel send"
```

---

### Task 4: server — exec REST 端点与审计

**Files:**
- Create: `server/internal/api/exec_handlers.go`
- Modify: `server/internal/api/router.go`（挂载 POST /api/nodes/{id}/exec）
- Test: `server/internal/api/exec_handlers_test.go`

**Interfaces:**
- Consumes: T2/T3 全部；`auth.UserFrom`；`GetNodeForUser`；`InsertAuditLog`（pgtype.UUID 可空 + Metadata []byte）
- Produces:

```text
POST /api/nodes/{id}/exec   （Bearer，membership）
  body {"command"?:string, "script"?:string, "timeoutSec"?:int, "cwd"?:string}
  → 202 {"sessionId","token","expiresAt","websocketUrl"}
  错误：404 NODE_NOT_FOUND / 409 NODE_OFFLINE / 400（command/script 二选一、script>256KB、timeoutSec 越界 [1,86400]）
审计：exec.start（202 时）；exec.finish（会话关闭时，metadata.reason，无命令内容）
SESSION_OPEN.WsURL = wsBaseURL(r) + "/api/agent/session?token=" + AgentToken
SESSION_OPEN 经 NodeConn.Send 下发；Send 失败 → 409 NODE_OFFLINE + 会话清理
```

- [ ] **Step 1: 写失败测试**

`server/internal/api/exec_handlers_test.go`：

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

func execPost(t *testing.T, env *TestEnv, nodeID, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", env.srv.URL+"/api/nodes/"+nodeID+"/exec",
		bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+env.AdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestExecSessionEndToEnd(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-E1", "mid-e1")
	ctrl := dialControl(t, env, nodeID)

	// 离线 → 409 NODE_OFFLINE（节点注册但未连控制连接）
	resp := execPost(t, env, nodeID, `{"command":"hostname"}`)
	defer resp.Body.Close()
	assert.Equal(t, 409, resp.StatusCode)
	var e1 struct {
		Error proto.APIError `json:"error"`
	}
	_ = decodeJSON(resp.Body, &e1)
	assert.Equal(t, proto.CodeNodeOffline, e1.Error.Code)

	// 在线：控制连接建立后创建 exec 会话
	ctrlReady := dialControl(t, env, nodeID) // dialControl 内部已等待 HELLO_ACK
	var created struct {
		SessionID    string    `json:"sessionId"`
		Token        string    `json:"token"`
		ExpiresAt    time.Time `json:"expiresAt"`
		WebsocketURL string    `json:"websocketUrl"`
	}
	resultCh := make(chan struct{})
	go func() {
		defer close(resultCh)
		fakeAgentSession(t, ctrlReady, func(aws *websocket.Conn) {
			wsWriteBinary(t, aws, append([]byte{0x01}, []byte("WEB-E1-OUT")...))
			wsWriteBinary(t, aws, append([]byte{0x02}, []byte("some-err")...))
			ec := 7
			wsWriteText(t, aws, mustMsg(t, proto.ExecResult{ExitCode: &ec, DurationMs: 12}))
			_ = aws.Close(nowCtx(t))
		})
	}()

	resp2 := execPost(t, env, nodeID, `{"command":"hostname","timeoutSec":60}`)
	defer resp2.Body.Close()
	require.Equal(t, 202, resp2.StatusCode)
	require.NoError(t, decodeJSON(resp2.Body, &created))
	require.NotEmpty(t, created.Token)
	assert.Contains(t, created.WebsocketURL, "/api/session/"+created.SessionID)
	<-resultCh

	cl := dialClientSession(t, env.srv.URL, "/api/session/"+created.SessionID, created.Token)
	ctx := nowCtx(t)
	// 两条 binary 帧按序到达（stdout 前缀 0x01 / stderr 前缀 0x02）
	d1 := readBin(t, cl)
	require.Equal(t, byte(0x01), d1[0])
	assert.Equal(t, "WEB-E1-OUT", string(d1[1:]))
	d2 := readBin(t, cl)
	require.Equal(t, byte(0x02), d2[0])
	// 终态 text：EXEC_RESULT
	typ, data := readRaw(t, cl)
	require.Equal(t, "text", typ)
	var m proto.Message
	require.NoError(t, jsonUnmarshal(data, &m))
	var res proto.ExecResult
	require.NoError(t, m.Decode(&res))
	require.NotNil(t, res.ExitCode)
	assert.Equal(t, 7, *res.ExitCode)
	_ = ctx

	// 审计：exec.start 存在；等待 finish（会话关闭路径）
	require.Eventually(t, func() bool {
		var n int
		require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
			`SELECT count(*) FROM audit_logs WHERE action IN ('exec.start','exec.finish')`).Scan(&n))
		return n >= 2
	}, 5*time.Second, 200*time.Millisecond)
	// 审计 metadata 无命令内容
	var meta string
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT metadata::text FROM audit_logs WHERE action='exec.start'`).Scan(&meta))
	assert.NotContains(t, meta, "hostname")
}

func TestExecValidation(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-E2", "mid-e2")
	_ = dialControl(t, env, nodeID)

	cases := []struct {
		body string
		code int
	}{
		{`{}`, 400},                                    // command/script 均空
		{`{"command":"a","script":"b"}`, 400},          // 同时给
		{`{"command":"a","timeoutSec":0}`, 400},        // 越界
		{`{"command":"a","timeoutSec":100000}`, 400},   // 越界
		{`{"script":"` + string(make([]byte, 256*1024+1)) + `"}`, 400}, // 超限（\x00 字节 JSON 合法）
	}
	for _, c := range cases {
		resp := execPost(t, env, nodeID, c.body)
		_ = resp.Body.Close()
		assert.Equal(t, c.code, resp.StatusCode, c.body[:20])
	}
}
```

（助手补充：`mustMsg` = `proto.NewMsg` + require；`nowCtx` = 5s ctx；`readRaw` 返回 ("text"|"binary", data)；`jsonUnmarshal` = `json.Unmarshal` 别名。这些放 sess_test_helpers_test.go。）

- [ ] **Step 2: 运行验证失败**

Run: `cd server && go test ./internal/api/ -run TestExec -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现**

`server/internal/api/exec_handlers.go`：

```go
package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"

	"xnc/proto"
	"xnc/server/internal/auth"
	"xnc/server/internal/db/sqlc"
	"xnc/server/internal/session"
)

type execReq struct {
	Command    string `json:"command"`
	Script     string `json:"script"`
	TimeoutSec int    `json:"timeoutSec"`
	Cwd        string `json:"cwd"`
}

func (h *handlers) execStart(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	nodeID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "node not found"))
		return
	}
	if _, err := h.st.Q().GetNodeForUser(r.Context(), sqlc.GetNodeForUserParams{
		UserID: u.ID, ID: nodeID}); err != nil {
		respondError(w, proto.Err(404, proto.CodeNodeNotFound, "node not found"))
		return
	}

	var req execReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "bad request"))
		return
	}
	switch {
	case req.Command == "" && req.Script == "":
		respondError(w, proto.Err(400, proto.CodeInternal, "command or script required"))
		return
	case req.Command != "" && req.Script != "":
		respondError(w, proto.Err(400, proto.CodeInternal, "command and script are exclusive"))
		return
	case len(req.Script) > 256*1024:
		respondError(w, proto.Err(400, proto.CodeFileTooLarge, "script exceeds 256KB"))
		return
	case req.TimeoutSec < 0 || req.TimeoutSec > 86400:
		respondError(w, proto.Err(400, proto.CodeInternal, "timeoutSec out of range"))
		return
	}
	if req.TimeoutSec == 0 {
		req.TimeoutSec = 300
	}

	params, _ := json.Marshal(proto.ExecParams{
		Command: req.Command, Script: req.Script, TimeoutSec: req.TimeoutSec, Cwd: req.Cwd,
	})
	res, apiErr := h.sess.Create(nodeID, u.ID, proto.KindExec, params)
	if apiErr != nil {
		respondError(w, apiErr)
		return
	}

	// 审计钩子：finish 无命令内容，仅 reason。
	h.sess.SetFinishFn(res.Session, func(reason string) {
		_ = h.st.Q().InsertAuditLog(r.Context(), sqlc.InsertAuditLogParams{
			UserID: pgUUID(u.ID), NodeID: pgUUID(nodeID),
			Action: "exec.finish",
			Metadata: mustJSON(map[string]string{"reason": reason, "kind": proto.KindExec}),
		})
	})
	// 控制连接通知 + 兜底清理。
	nodeConn := h.reg.Get(nodeID.String()) // registry.Get 为 T3 提供
	if nodeConn == nil || nodeConn.Send == nil {
		h.sess.NotifyClose(res.Session.ID, "node-offline")
		respondError(w, proto.Err(409, proto.CodeNodeOffline, "node is offline"))
		return
	}
	openMsg, _ := proto.NewMsg(proto.TypeSessionOpen, proto.SessionOpen{
		SessionID: res.Session.ID, Kind: proto.KindExec, Params: params,
		AgentToken: res.AgentToken,
		WsURL:      wsBaseURL(r) + "/api/agent/session?token=" + res.AgentToken,
		ExpiresAt:  res.ExpiresAt,
	})
	if err := nodeConn.Send(openMsg); err != nil {
		h.sess.NotifyClose(res.Session.ID, "node-offline")
		respondError(w, proto.Err(409, proto.CodeNodeOffline, "node is offline"))
		return
	}
	h.sess.SetNotifyFn(res.Session, func(sc proto.SessionClose) error {
		if nc := h.reg.Get(nodeID.String()); nc != nil && nc.Send != nil {
			msg, _ := proto.NewMsg(proto.TypeSessionClose, sc)
			return nc.Send(msg)
		}
		return nil // 连接已失：manager 已在 close 路径清理
	})

	_ = h.st.Q().InsertAuditLog(r.Context(), sqlc.InsertAuditLogParams{
		UserID: pgUUID(u.ID), NodeID: pgUUID(nodeID), Action: "exec.start",
		Metadata: mustJSON(map[string]string{"kind": proto.KindExec}),
	})
	respondJSON(w, 202, map[string]any{
		"sessionId": res.Session.ID, "token": res.ClientToken,
		"expiresAt": res.ExpiresAt,
		"websocketUrl": "/api/session/" + res.Session.ID + "?token=" + res.ClientToken,
	})
}
```

配套小件（同文件或 respond.go）：

```go
func pgUUID(id uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: id, Valid: true} }
func mustJSON(v any) []byte           { b, _ := json.Marshal(v); return b }
```

（cluster_id 留空直接传零值 `pgtype.UUID{}`。）router.go 挂载：

```go
r.Route("/api/nodes/{id}/exec", func(nr chi.Router) {
	nr.Use(auth.Middleware(cfg.JWTSecret, st))
	nr.Post("/", h.execStart)
})
```

- [ ] **Step 4: 运行验证通过**

Run: `cd server && go test ./internal/api/ -run TestExec -count=1 && go test ./... -count=1`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add server
git commit -m "feat(server): exec session endpoint with audit and offline handling"
```

---

### Task 5: agent — 控制连接读分发重构

**Files:**
- Modify: `agent/connect/client.go`、`agent/connect/client_test.go`

**Interfaces:**
- Consumes: 既有 Client（Run/once/handshake/drain/readDeadline）
- Produces（T6 依赖）:

```go
// Handler 处理 server → agent 的控制消息。nil 安全：Client.Handler 为 nil 时
// 维持 Phase 1 行为（ack 丢弃、ERROR 记日志）。
type Handler interface {
    HandleSessionOpen(ctx context.Context, so proto.SessionOpen)
    HandleSessionClose(ctx context.Context, sc proto.SessionClose)
}
// Client 新增字段：
Handler Handler
```

- [ ] **Step 1: 写失败测试**

在 `agent/connect/client_test.go` 追加：

```go
func TestDispatchesSessionMessages(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	openCh := make(chan proto.SessionOpen, 1)
	closeCh := make(chan proto.SessionClose, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _, err := websocket.Accept(w, r, nil) // 简化握手：直接 HELLO_ACK 流程见 fakeServer；本测试复用 fakeServer 并在其 HELLO_ACK 后注入两条消息——实现时把 fakeServer 加 hooks 参数或新起精简 server
		_ = err
		_ = c
	}))
	_ = srv
	_ = pub
	_ = priv
	_ = openCh
	_ = closeCh
	// 见下方"实现注记"：以 fakeServer 变体完成——HELLO_ACK 后：
	//   write SESSION_OPEN{sessionId:"s1", kind:"exec"}
	//   write SESSION_CLOSE{sessionId:"s1"}
	// Client.Handler 捕获两条；断言 channel 收到且顺序无关。
}
```

**实现注记（执行者按此写完整测试，不留占位）**：新增 `fakeServerWithHooks(t, pub, afterHello func(write func(typ string, payload any)))`——在 fakeServer 基础上抽出 `write` 闭包；本测试 `afterHello` 里依次 `write(proto.TypeSessionOpen, proto.SessionOpen{SessionID:"s1", Kind: proto.KindExec, Params: []byte(`{}`)})` 与 `write(proto.TypeSessionClose, proto.SessionClose{SessionID:"s1", Reason:"test"})`。断言：

```go
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
```

`stubHandler`：

```go
type stubHandler struct {
	open  chan proto.SessionOpen
	close chan proto.SessionClose
}
func (s stubHandler) HandleSessionOpen(_ context.Context, so proto.SessionOpen) { s.open <- so }
func (s stubHandler) HandleSessionClose(_ context.Context, sc proto.SessionClose) { s.close <- sc }
```

- [ ] **Step 2: 运行验证失败**

Run: `cd agent && go test ./internal/../connect/ -run TestDispatches -count=1`（即 `go test ./connect/ -run TestDispatches -count=1`）
Expected: FAIL（Handler 未定义）。

- [ ] **Step 3: 实现**

`client.go` 修改（保持既有测试全绿）：

```go
// Handler 处理 server → agent 的控制消息；nil 时维持仅丢弃行为。
type Handler interface {
	HandleSessionOpen(ctx context.Context, so proto.SessionOpen)
	HandleSessionClose(ctx context.Context, sc proto.SessionClose)
}

type Client struct {
	ServerURL string
	Key       *identity.Key
	Info      machineinfo.Info
	Beat      time.Duration
	Log       *slog.Logger
	Handler   Handler // nil 安全
	// BackoffReset 等既有字段不变
}
```

`once()` 内 drain 改造为分发循环 `readLoop(pctx, ws, dead)`：解码 Message 后 switch：

```go
switch m.Type {
case proto.TypeHeartbeatAck:
	// 丢弃
case proto.TypeError:
	var ep proto.ErrorPayload
	if err := m.Decode(&ep); err == nil {
		c.Log.Warn("server error", "code", ep.Code, "message", ep.Message)
	}
case proto.TypeSessionOpen:
	var so proto.SessionOpen
	if err := m.Decode(&so); err == nil && c.Handler != nil {
		h := c.Handler
		go h.HandleSessionOpen(pctx, so) // 会话处理不得阻塞心跳/读取
	}
case proto.TypeSessionClose:
	var sc proto.SessionClose
	if err := m.Decode(&sc); err == nil && c.Handler != nil {
		h := c.Handler
		go h.HandleSessionClose(pctx, sc)
	}
default:
	// 前向兼容：未知类型忽略
}
```

（`m.Decode` 的解码目标即本分支的 `sc`；liveness deadline、binary 帧即死、`dead()` 触发重连等既有语义全部保留；既有 `TestDrainDetectsSilentPeer`/`TestClientStaysConnectedUnderAcksAndErrors` 必须不改即绿——ack 丢弃分支覆盖。）

- [ ] **Step 4: 运行验证通过**

Run: `cd agent && go test ./connect/ -count=1 && GOOS=linux go build ./... && GOOS=windows go build ./...`
Expected: PASS + 双编译过。

- [ ] **Step 5: Commit**

```bash
git add agent/connect
git commit -m "feat(agent): control-connection dispatch for session messages"
```

---

### Task 6: agent — 会话引擎与 ExecManager（命令模式）

**Files:**
- Create: `agent/session/engine.go`、`agent/session/exec.go`、`agent/session/exec_test.go`

**Interfaces:**
- Consumes: T5 `connect.Handler`；proto SessionOpen/Close/ExecParams/ExecResult；identity/machineinfo
- Produces（T8 依赖）:

```go
type Handler interface { // 会话 kind 处理器（与会话 WS 一一对应）
    Handle(ctx context.Context, ws *websocket.Conn, sessionID string, params json.RawMessage)
}
type Engine struct{ ... }
func NewEngine(log *slog.Logger, sendControl func(m proto.Message) error) *Engine
func (e *Engine) Register(kind string, h Handler)
// Engine 实现 connect.Handler：
func (e *Engine) HandleSessionOpen(ctx context.Context, so proto.SessionOpen)
func (e *Engine) HandleSessionClose(ctx context.Context, sc proto.SessionClose)

// Exec（命令模式）：
func NewExec(log *slog.Logger) *Exec
type Exec struct{ Log *slog.Logger; TmpDir string } // TmpDir 空 = os.TempDir()（T7 用）
func (ex *Exec) Handle(ctx context.Context, ws *websocket.Conn, sessionID string, params json.RawMessage)
```

- [ ] **Step 1: 写失败测试**

`agent/session/exec_test.go`：

```go
package session

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

// runExec 起 httptest WS 服务端并让 Exec 作为客户端拨入；返回服务侧连接。
// 注意：dial 错误经 errCh 回报（goroutine 内禁用 require）。
func runExec(t *testing.T, params proto.ExecParams) *websocket.Conn {
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
		NewExec(testLogger()).Handle(context.Background(), c, "sess-test", raw)
		errCh <- nil
	}()
	select {
	case c := <-up:
		return c
	case err := <-errCh:
		t.Fatalf("exec dial/handle failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("no ws connection")
	}
	return nil
}

func readFrame(t *testing.T, ws *websocket.Conn) (string, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	typ, data, err := ws.Read(ctx)
	require.NoError(t, err)
	if typ == websocket.MessageText {
		return "text", data
	}
	return "binary", data
}

func TestExecCommandStreamsAndExits(t *testing.T) {
	if isWindows() {
		ws := runExec(t, proto.ExecParams{Command: `Write-Output hello; Write-Error boom; exit 7`, TimeoutSec: 30})
		var stdout, stderr bytes.Buffer
		var res *proto.ExecResult
		for res == nil {
			kind, data := readFrame(t, ws)
			switch kind {
			case "binary":
				if data[0] == 0x01 {
					stdout.Write(data[1:])
				} else {
					stderr.Write(data[1:])
				}
			case "text":
				var m proto.Message
				require.NoError(t, json.Unmarshal(data, &m))
				require.Equal(t, "EXEC_RESULT", m.Type)
				var r proto.ExecResult
				require.NoError(t, m.Decode(&r))
				res = &r
			}
		}
		assert.Contains(t, stdout.String(), "hello")
		assert.Contains(t, stderr.String(), "boom")
		require.NotNil(t, res.ExitCode)
		assert.Equal(t, 7, *res.ExitCode)
		assert.False(t, res.TimedOut)
		assert.True(t, res.DurationMs >= 0)
	} else {
		ws := runExec(t, proto.ExecParams{Command: `echo hello; echo boom 1>&2; exit 7`, TimeoutSec: 30})
		_ = ws
		// 同构断言（sh 路径）……与 windows 分支相同逻辑，抽 helper 断言避免重复
	}
}

func TestExecTimeoutKillsAndReports(t *testing.T) {
	cmd := `Start-Sleep 60`
	if !isWindows() {
		cmd = `sleep 60`
	}
	start := time.Now()
	ws := runExec(t, proto.ExecParams{Command: cmd, TimeoutSec: 1})
	var res *proto.ExecResult
	for res == nil {
		kind, data := readFrame(t, ws)
		if kind == "text" {
			var m proto.Message
			require.NoError(t, json.Unmarshal(data, &m))
			var r proto.ExecResult
			require.NoError(t, m.Decode(&r))
			res = &r
		}
	}
	assert.True(t, res.TimedOut)
	assert.Nil(t, res.ExitCode)
	assert.Less(t, time.Since(start), 15*time.Second) // 秒级杀掉，非等满 60s
}

func isWindows() bool { return runtime.GOOS == "windows" }
```

（实现者把 windows/非 windows 两分支的断言抽成一个 `collectExec(t, ws)` helper 消除重复；`testLogger()` = `slog.New(slog.NewTextHandler(io.Discard, nil))`。）

- [ ] **Step 2: 运行验证失败**

Run: `cd agent && go test ./session/ -count=1`
Expected: FAIL（包不存在）。

- [ ] **Step 3: 实现**

`agent/session/engine.go`：

```go
// Package session 实现 agent 侧会话引擎：SESSION_OPEN → 拨号 → kind 分发。
package session

import (
	"context"
	"log/slog"
	"sync"

	"github.com/coder/websocket"

	"xnc/proto"
)

type Handler interface {
	Handle(ctx context.Context, ws *websocket.Conn, sessionID string, params json.RawMessage)
}

type Engine struct {
	log         *slog.Logger
	sendControl func(m proto.Message) error
	handlers    map[string]Handler

	mu       sync.Mutex
	active   map[string]context.CancelFunc
}

func NewEngine(log *slog.Logger, sendControl func(m proto.Message) error) *Engine {
	return &Engine{log: log, sendControl: sendControl,
		handlers: map[string]Handler{}, active: map[string]context.CancelFunc{}}
}

func (e *Engine) Register(kind string, h Handler) {
	e.handlers[kind] = h
}

func (e *Engine) HandleSessionOpen(_ context.Context, so proto.SessionOpen) {
	h, ok := e.handlers[so.Kind]
	if !ok {
		e.log.Warn("unsupported session kind", "kind", so.Kind, "session", so.SessionID)
		if e.sendControl != nil {
			msg, _ := proto.NewMsg(proto.TypeSessionRefused, proto.SessionRefused{
				SessionID: so.SessionID, Code: proto.CodeKindUnsupported,
				Message: "kind not supported by agent",
			})
			_ = e.sendControl(msg)
		}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.mu.Lock()
	e.active[so.SessionID] = cancel
	e.mu.Unlock()

	go func() {
		defer func() {
			e.mu.Lock()
			delete(e.active, so.SessionID)
			e.mu.Unlock()
			cancel()
		}()
		ws, _, err := websocket.Dial(ctx, so.WsURL, nil)
		if err != nil {
			e.log.Warn("session dial failed", "session", so.SessionID, "err", err)
			return // Opening TTL 兜底
		}
		defer ws.CloseNow()
		h.Handle(ctx, ws, so.SessionID, so.Params)
	}()
}

func (e *Engine) HandleSessionClose(_ context.Context, sc proto.SessionClose) {
	e.mu.Lock()
	cancel, ok := e.active[sc.SessionID]
	e.mu.Unlock()
	if ok {
		cancel() // 触发 ExecManager 的 ctx 取消路径：杀进程树、清理、退出
	}
}
```

`agent/session/exec.go`：

```go
package session

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/coder/websocket"

	"xnc/proto"
)

const (
	stdoutPrefix = byte(0x01)
	stderrPrefix = byte(0x02)
	execWriteTimeout = 10 * time.Second
)

type Exec struct {
	Log    *slog.Logger
	TmpDir string // 空 = os.TempDir()；脚本模式临时文件目录（测试注入）
}

func NewExec(log *slog.Logger) *Exec { return &Exec{Log: log} }

func (ex *Exec) Handle(ctx context.Context, ws *websocket.Conn, sessionID string, params json.RawMessage) {
	var p proto.ExecParams
	if err := json.Unmarshal(params, &p); err != nil {
		ex.result(ctx, ws, nil, false, 0)
		return
	}
	deadline := time.Duration(p.TimeoutSec) * time.Second
	if deadline <= 0 {
		deadline = 300 * time.Second
	}
	start := time.Now()

	var cleanup func()
	if p.Script != "" {
		tmp, err := ex.writeScript(sessionID, p.Script)
		if err != nil {
			ex.result(ctx, ws, nil, false, 0)
			return
		}
		cleanup = func() { _ = os.Remove(tmp) }
		defer cleanup()
	}
	cmd, err := ex.buildCommand(p, sessionID)
	if err != nil {
		ex.result(ctx, ws, nil, false, time.Since(start).Milliseconds())
		return
	}
	if p.Cwd != "" {
		cmd.Dir = p.Cwd
	}
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		ex.result(ctx, ws, nil, false, time.Since(start).Milliseconds())
		return
	}

	timedOut := make(chan struct{}, 1)
	timer := time.AfterFunc(deadline, func() {
		timedOut <- struct{}{}
		killTree(cmd.Process.Pid)
	})
	defer timer.Stop()

	ex.pump(ctx, ws, stdout, stdoutPrefix)
	ex.pump(ctx, ws, stderr, stderrPrefix)

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	var exitCode *int
	timed := false
	select {
	case <-waitCh:
		if code := cmd.ProcessState.ExitCode(); code >= 0 {
			exitCode = &code
		}
	case <-ctx.Done():
		killTree(cmd.Process.Pid)
		<-waitCh
	case <-timedOut:
		timed = true
		<-waitCh
	}
	ex.result(ctx, ws, exitCode, timed, time.Since(start).Milliseconds())
}

// pump 为一条输出流起一个转发 goroutine（stdout 与 stderr 各调一次）：
func (ex *Exec) pump(ctx context.Context, ws *websocket.Conn, r io.Reader, prefix byte) {
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				frame := append([]byte{prefix}, buf[:n]...)
				wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
				if werr := ws.Write(wctx, websocket.MessageBinary, frame); werr != nil {
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
}

func (ex *Exec) result(ctx context.Context, ws *websocket.Conn, code *int, timed bool, ms int64) {
	msg, _ := proto.NewMsg("EXEC_RESULT", proto.ExecResult{ExitCode: code, TimedOut: timed, DurationMs: ms})
	b, _ := json.Marshal(msg)
	wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
	defer cancel()
	_ = ws.Write(wctx, websocket.MessageText, b)
	_ = ws.Close(wctx, websocket.StatusNormalClosure, "")
}
```

`buildCommand` / `writeScript` / `killTree`：

```go
func (ex *Exec) buildCommand(p proto.ExecParams, sessionID string) (*exec.Cmd, error) {
	if runtime.GOOS == "windows" {
		shell := "powershell"
		if _, err := exec.LookPath("pwsh"); err == nil {
			shell = "pwsh"
		}
		if p.Script != "" {
			return exec.Command(shell, "-NoLogo", "-NonInteractive", "-File", ex.scriptPath(sessionID)), nil
		}
		return exec.Command(shell, "-NoLogo", "-NonInteractive", "-Command", p.Command), nil
	}
	// 非 Windows（测试/Linux 演进）：sh -c
	src := p.Command
	if p.Script != "" {
		src = ex.scriptPath(sessionID)
		return exec.Command("sh", src), nil
	}
	return exec.Command("sh", "-c", src), nil
}

func (ex *Exec) writeScript(sessionID, script string) (string, error) {
	dir := ex.TmpDir
	if dir == "" {
		dir = os.TempDir()
	}
	path := filepath.Join(dir, "xnc-"+sessionID+".ps1")
	return path, os.WriteFile(path, []byte(script), 0o600)
}

func (ex *Exec) scriptPath(sessionID string) string {
	dir := ex.TmpDir
	if dir == "" {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "xnc-"+sessionID+".ps1")
}

func killTree(pid int) {
	if pid <= 0 {
		return
	}
	if runtime.GOOS == "windows" {
		_ = exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F").Run()
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL) // Linux 进程组杀为 Phase 8 议题
}
```

（"EXEC_RESULT" 字面量收敛：在 exec.go 顶部 `const typeExecResult = "EXEC_RESULT"`，proto 不加常量——会话级词汇属 kind 私有，设计 §3.2 的会话面 text 类型按 kind 归属。导入补 `io`、`path/filepath`。）

- [ ] **Step 4: 运行验证通过**

Run: `cd agent && go test ./session/ -count=1 && GOOS=linux go build ./... && GOOS=windows go build ./...`
Expected: PASS + 双编译（timeout 用例 ~1-2s 完成）。

- [ ] **Step 5: Commit**

```bash
git add agent/session
git commit -m "feat(agent): session engine and exec manager with tree-kill timeout"
```

---

### Task 7: agent — exec 脚本模式与临时文件生命周期

**Files:**
- Modify: `agent/session/exec.go`（脚本路径细节已在 T6 铺垫，本 Task 收口）
- Test: `agent/session/exec_test.go`（追加）

**Interfaces:**
- Consumes: T6 Exec（TmpDir 注入点）
- Produces: 脚本模式行为契约——`%TEMP%\xnc-<sessionId>.ps1` 创建、执行、**所有路径删除**；256KB 上限由 server 侧校验（agent 信任 params，但 >1MB 的 script 直接拒绝执行返回 result 不落盘）

- [ ] **Step 1: 写失败测试**

追加到 `exec_test.go`：

```go
func TestExecScriptLifecycle(t *testing.T) {
	dir := t.TempDir()
	ex := &Exec{Log: testLogger(), TmpDir: dir}

	srvUp := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		require.NoError(t, err)
		srvUp <- c
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	script := "Write-Output from-script\nexit 3"
	if !isWindows() {
		script = "echo from-script\nexit 3"
	}
	raw, _ := json.Marshal(proto.ExecParams{Script: script, TimeoutSec: 30})
	go func() {
		c, _, err := websocket.Dial(context.Background(), "ws"+srv.URL[4:], nil)
		require.NoError(t, err)
		ex.Handle(context.Background(), c, "sess-script", raw)
	}()
	ws := <-srvUp

	var res *proto.ExecResult
	var out string
	for res == nil {
		kind, data := readFrame(t, ws)
		if kind == "binary" && data[0] == 0x01 {
			out += string(data[1:])
		} else if kind == "text" {
			var m proto.Message
			require.NoError(t, json.Unmarshal(data, &m))
			var r proto.ExecResult
			require.NoError(t, m.Decode(&r))
			res = &r
		}
	}
	assert.Contains(t, out, "from-script")
	require.NotNil(t, res.ExitCode)
	assert.Equal(t, 3, *res.ExitCode)

	// 临时文件已删除（成功路径）
	entries, _ := os.ReadDir(dir)
	assert.Empty(t, entries)
}

func TestExecScriptCleanupOnFailure(t *testing.T) {
	dir := t.TempDir()
	ex := &Exec{Log: testLogger(), TmpDir: dir}
	script := "exit 9"
	if !isWindows() {
		script = "exit 9"
	}
	raw, _ := json.Marshal(proto.ExecParams{Script: script, TimeoutSec: 30})
	up := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		require.NoError(t, err)
		up <- c
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	go func() {
		c, _, _ := websocket.Dial(context.Background(), "ws"+srv.URL[4:], nil)
		ex.Handle(context.Background(), c, "sess-fail", raw)
	}()
	ws := <-up
	for {
		kind, data := readFrame(t, ws)
		if kind == "text" {
			var m proto.Message
			_ = json.Unmarshal(data, &m)
			var r proto.ExecResult
			_ = m.Decode(&r)
			require.NotNil(t, r.ExitCode)
			require.Equal(t, 9, *r.ExitCode)
			break
		}
	}
	entries, _ := os.ReadDir(dir)
	assert.Empty(t, entries, "temp script must be removed on failure path too")
}

func TestExecOversizeScriptRefused(t *testing.T) {
	dir := t.TempDir()
	ex := &Exec{Log: testLogger(), TmpDir: dir}
	huge := strings.Repeat("a", 1024*1024+1)
	raw, _ := json.Marshal(proto.ExecParams{Script: huge, TimeoutSec: 5})
	up := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		require.NoError(t, err)
		up <- c
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	go func() {
		c, _, _ := websocket.Dial(context.Background(), "ws"+srv.URL[4:], nil)
		ex.Handle(context.Background(), c, "sess-huge", raw)
	}()
	ws := <-up
	kind, data := readFrame(t, ws)
	require.Equal(t, "text", kind)
	var m proto.Message
	require.NoError(t, json.Unmarshal(data, &m))
	var r proto.ExecResult
	require.NoError(t, m.Decode(&r))
	assert.Nil(t, r.ExitCode)
	entries, _ := os.ReadDir(dir)
	assert.Empty(t, entries, "no temp file written for oversize script")
}
```

- [ ] **Step 2: 运行验证失败**

Run: `cd agent && go test ./session/ -run TestExecScript -count=1`
Expected: FAIL（oversize 拒绝未实现——前两个用例可能已绿，第三个红）。

- [ ] **Step 3: 实现**

`exec.go` 的 Handle 开头（decode 后）追加：

```go
	if len(p.Script) > 1024*1024 {
		// server 已拦 256KB；agent 兜底 1MB，防篡改路径直接落盘超大文件
		ex.result(ctx, ws, nil, false, 0)
		return
	}
```

（其余脚本路径 T6 已实现：writeScript → defer Remove 覆盖所有 return。）

- [ ] **Step 4: 运行验证通过**

Run: `cd agent && go test ./session/ -count=1`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add agent/session
git commit -m "test(agent): exec script lifecycle including failure and oversize paths"
```

---

### Task 8: mockagent 与 agent 装配

**Files:**
- Modify: `agent/agent.go`（Agent 装配 Engine + Exec）、`mockagent/main.go`

**Interfaces:**
- Consumes: T5 `connect.Client.Handler`、T6 `session.Engine/NewExec`
- Produces: 真实 agent 与 mockagent 都具备 exec 会话能力；`agent.Agent.Run` 不变签名

- [ ] **Step 1: 实现**

`agent/agent.go` 的 `Run` 中，构造 connect.Client 后装配：

```go
func (a *Agent) Run(ctx context.Context) error {
	k, info, err := a.EnsureEnrolled(ctx)
	if err != nil {
		return err
	}
	c := connect.NewClient(a.ServerURL, k, info)
	// 每次连接就绪（含重连）重建 engine：旧 engine 的 sendControl 绑定旧连接，
	// 其 active 会话已随断连作废，重建即正确语义。
	c.OnReady = func(sendControl func(m proto.Message) error) {
		engine := session.NewEngine(slog.Default(), sendControl)
		engine.Register(proto.KindExec, session.NewExec(slog.Default()))
		c.Handler = engine
	}
	return c.Run(ctx)
}
```

为此 `connect.Client` 增加 `OnReady func(sendControl func(m proto.Message) error)` 字段：`once()` 在 HELLO_ACK 成功后、启动读循环前调用一次（把控制连接的写闭包交出；`once()` 内若无独立写函数则抽 `writeControl(m proto.Message) error` 闭包复用）。

`mockagent/main.go` 的 runOne 同样装配（与 agent.go 相同的"每次就绪重建"语义）：

```go
	c := connect.NewClient(server, k, mockInfo(i))
	c.Beat = beat
	c.Log = slog.With("node", i)
	c.OnReady = func(send func(m proto.Message) error) {
		engine := session.NewEngine(c.Log, send)
		engine.Register(proto.KindExec, session.NewExec(c.Log))
		c.Handler = engine
	}
```

- [ ] **Step 2: 联动验证**

Run: `cd agent && go test ./... -count=1 && cd ../mockagent && go build ./... && go vet ./...`
Expected: PASS（connect 的 OnReady 无新测试——由 T5 既有测试守护行为，OnReady 仅在 HELLO_ACK 后回调，追加一个断言回调时机的用例：

```go
func TestOnReadyFiresAfterHelloAck(t *testing.T) { /* fakeServer + OnReady channel 断言恰好一次/每次连接一次 */ }
```

写进 client_test.go，先红后绿。）

- [ ] **Step 3: Commit**

```bash
git add agent mockagent
git commit -m "feat(agent,mockagent): wire session engine into control client"
```

---

### Task 9: cli — 节点选择器抽取与 exec 命令

**Files:**
- Create: `cli/nodeselect.go`、`cli/ws.go`、`cli/cmd_exec.go`
- Modify: `cli/cmd_node.go`（改用共享 resolveNode）、`cli/main.go`（注册命令）
- Test: `cli/cmd_exec_test.go`

**Interfaces:**
- Consumes: `Client.Do`、`PrintJSON`、`ExitCode`、既有 node 列表解析逻辑
- Produces（T10 复用）:

```go
// nodeselect.go
func resolveNode(cl *Client, arg string) (nodeDTO-ish, *proto.APIError)
// 返回 {ID, Name, Cluster}；歧义时错误 message 列出候选（保持 Phase 1 行为）
// cmd_node.go 的 node show 改调此函数；exec/run 也用

// ws.go
func dialSession(server, wsPath string) (*websocket.Conn, error) // https→wss
func readWS(ctx, ws) (kind string, data []byte, err error)       // "text"|"binary"

// cmd_exec.go
// xnc exec <node> [--timeout N] [--cwd PATH] [--output] [--] <command...>
// 无 -- 时剩余 args 以空格 join 为命令（v1 §28 形态）
```

- [ ] **Step 1: 写失败测试**

`cli/cmd_exec_test.go`：

```go
package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeExecServer: POST /api/nodes/{id}/exec → 202 + 会话；WS 上假 agent 流。
func fakeExecServer(t *testing.T, exitCode int, stdout, stderr string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/nodes/n1/exec" && r.Method == "POST" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"sessionId":"s1","token":"ct","expiresAt":"2026-01-01T00:00:00Z",
				"websocketUrl":"/api/session/s1?token=ct"}`))
			return
		}
		if r.URL.Path == "/api/session/s1" {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			go func() {
				defer c.CloseNow()
				ctx := r.Context()
				_ = c.Write(ctx, websocket.MessageBinary, append([]byte{0x01}, stdout...))
				_ = c.Write(ctx, websocket.MessageBinary, append([]byte{0x02}, stderr...))
				ec := exitCode
				res := mustJSONStr(map[string]any{"exitCode": ec, "timedOut": false, "durationMs": 5})
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"EXEC_RESULT","payload":`+res+`}`))
			}()
			return
		}
		http.NotFound(w, r)
	}))
	return srv
}

func TestExecStreamsAndPassthroughExit(t *testing.T) {
	srv := fakeExecServer(t, 7, "out-line\n", "err-line\n")
	defer srv.Close()

	out, code := captureStdout2(t, func() int {
		return runCLI(t.Context(), []string{"exec", "n1", "--json",
			"--server", srv.URL, "--token", "tk", "--", "hostname"})
	})
	require.Equal(t, 7, code) // 退出码透传
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Node      string `json:"node"`
			ExitCode  int    `json:"exitCode"`
			Stdout    string `json:"stdout"`
			Stderr    string `json:"stderr"`
			TimedOut  bool   `json:"timedOut"`
		} `json:"data"`
	}
	require.NoError(t, jsonUnmarshalStr(out, &env))
	assert.True(t, env.OK)
	assert.Equal(t, 7, env.Data.ExitCode)
	assert.Equal(t, "out-line\n", env.Data.Stdout)
	assert.Equal(t, "err-line\n", env.Data.Stderr)
}

func TestExecTimedOutExits243(t *testing.T) {
	srv := fakeExecTimedServer(t) // 同上但 payload {"exitCode":null,"timedOut":true,...}
	defer srv.Close()
	_, code := captureStdout2(t, func() int {
		return runCLI(t.Context(), []string{"exec", "n1", "--json",
			"--server", srv.URL, "--token", "tk", "--", "slow"})
	})
	assert.Equal(t, 243, code)
}
```

（`captureStdout2`/`jsonUnmarshalStr`/`mustJSONStr` 为测试助手——`captureStdout2` 即既有 captureStdout（返回 (string, int)），如已存在直接用；命名以现有 main_test.go 为准，勿建重复助手。fakeExecTimedServer 与 fakeExecServer 参数化即可，不必单独函数。）

- [ ] **Step 2: 运行验证失败**

Run: `cd cli && go get github.com/coder/websocket && go mod tidy && go test ./... -count=1`
Expected: FAIL（exec 命令不存在）。

- [ ] **Step 3: 实现**

`cli/nodeselect.go`——从 cmd_node.go 抽取列表+匹配+歧义候选逻辑为 `resolveNode(cl *Client, arg string) (nodeRef, *proto.APIError)`（`type nodeRef struct{ ID, Name, Cluster string }`）；`cmd_node.go` 的 show 改调用（行为不变，golden 不动）。

`cli/ws.go`：

```go
package main

import (
	"context"
	"strings"
	"time"

	"github.com/coder/websocket"
)

func dialSession(server, wsPath string) (*websocket.Conn, error) {
	url := strings.Replace(strings.Replace(server, "https://", "wss://", 1), "http://", "ws://", 1) + wsPath
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, url, nil)
	return c, err
}

func readWS(ctx context.Context, ws *websocket.Conn) (string, []byte, error) {
	typ, data, err := ws.Read(ctx)
	if err != nil {
		return "", nil, err
	}
	if typ == websocket.MessageText {
		return "text", data, nil
	}
	return "binary", data, nil
}
```

`cli/cmd_exec.go`：

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"xnc/proto"
)

type execOutcome struct {
	node      string
	exitCode  *int
	timedOut  bool
	duration  int64
	stdout    strings.Builder
	stderr    strings.Builder
}

func runExecCommand(cmd *cobra.Command, args []string) int {
	cfg, _ := LoadConfig()
	cl := NewClient(resolveServer(cmd, cfg), resolveToken(cmd, cfg))

	node, apiErr := resolveNode(cl, args[0])
	if apiErr != nil {
		return cliFail(cmd, apiErr)
	}
	command := strings.Join(args[1:], " ")

	body := map[string]any{"command": command, "timeoutSec": intTimeoutFlag(cmd)}
	if cwd := strFlag(cmd, "cwd"); cwd != "" {
		body["cwd"] = cwd
	}
	var created struct {
		SessionID    string    `json:"sessionId"`
		Token        string    `json:"token"`
		WebsocketURL string    `json:"websocketUrl"`
	}
	if e := cl.Do("POST", "/api/nodes/"+node.ID+"/exec", body, &created); e != nil {
		return cliFail(cmd, e)
	}

	ws, err := dialSession(cl.Base, created.WebsocketURL)
	if err != nil {
		return cliFail(cmd, proto.Err(0, "NETWORK", err.Error()))
	}
	defer ws.CloseNow()

	out := execOutcome{node: node.Name}
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	for {
		kind, data, err := readWS(ctx, ws)
		if err != nil {
			break // 连接关闭（正常路径：EXEC_RESULT 后 agent 关）
		}
		switch kind {
		case "binary":
			if len(data) == 0 {
				continue
			}
			if data[0] == 0x01 {
				os.Stdout.Write(data[1:])
				out.stdout.Write(data[1:])
			} else {
				os.Stderr.Write(data[1:])
				out.stderr.Write(data[1:])
			}
		case "text":
			var m proto.Message
			if json.Unmarshal(data, &m) != nil || m.Type != "EXEC_RESULT" {
				continue
			}
			var r proto.ExecResult
			if m.Decode(&r) == nil {
				out.exitCode, out.timedOut, out.duration = r.ExitCode, r.TimedOut, r.DurationMs
			}
		}
	}

	if jsonOut(cmd) {
		ec := any(nil)
		if out.exitCode != nil {
			ec = *out.exitCode
		}
		PrintJSON(true, map[string]any{
			"node": out.node, "exitCode": ec, "stdout": out.stdout.String(),
			"stderr": out.stderr.String(), "durationMs": out.duration, "timedOut": out.timedOut,
		}, nil)
	}
	switch {
	case out.exitCode == nil:
		return 243 // 未拿到退出码（超时/中断）
	default:
		return *out.exitCode
	}
}
```

（`intTimeoutFlag/strFlag/cliFail/jsonOut/resolveServer/resolveToken` 均为既有或以薄封装落在 cmd_exec.go；`--timeout` 默认 300。cobra RunE 里 `os.Exit(runExecCommand(cmd, args))`——与既有命令的退出模式保持一致。`--json` 下 stdout 实时打印与最终 envelope 会交错——接受：envelope 在流之后输出，jsonl 消费者按行解析不受影响；此行为写入命令注释。）

`main.go` 注册 execCmd（Use: `exec <node> [flags] [--] <command...>`，Args: cobra.MinimumNArgs(2)，flags: --timeout/--cwd）。

- [ ] **Step 4: 运行验证通过**

Run: `cd cli && go test ./... -count=1`
Expected: PASS（含既有 golden 回归）。

- [ ] **Step 5: Commit**

```bash
git add cli
git commit -m "feat(cli): xnc exec with streaming output and exit-code passthrough"
```

---

### Task 10: cli — run 命令（脚本）

**Files:**
- Modify: `cli/cmd_exec.go`（追加 runCmd）
- Test: `cli/cmd_exec_test.go`（追加）

**Interfaces:**
- Consumes: T9 全部
- Produces: `xnc run <node> (--file x.ps1 | -) [--timeout N] [--output json|table]`

- [ ] **Step 1: 写失败测试**

追加：

```go
func TestRunScriptFromStdin(t *testing.T) {
	var gotBody []byte
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/nodes/n1/exec" {
			gotBody, _ = io.ReadAll(r.Body)
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"sessionId":"s1","token":"ct","websocketUrl":"/api/session/s1?token=ct",
				"expiresAt":"2026-01-01T00:00:00Z"}`))
			return
		}
		if r.URL.Path == "/api/session/s1" {
			c, _ := websocket.Accept(w, r, nil)
			go func() {
				defer c.CloseNow()
				_ = c.Write(r.Context(), websocket.MessageBinary, append([]byte{0x01}, []byte("done")...))
				_ = c.Write(r.Context(), websocket.MessageText,
					[]byte(`{"type":"EXEC_RESULT","payload":{"exitCode":0,"timedOut":false,"durationMs":3}}`))
			}()
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	// stdin 注入脚本（runCLI 助手需支持 stdin —— captureStdout 家族加 stdin 参数变体，
	// 或用 os.Stdin = tmp file 的方式；以现有测试助手风格实现 stdinPipe 变体）
	out, code := runCLIWithStdin(t, "Write-Output ok\n", []string{
		"run", "n1", "-", "--json", "--server", srv.URL, "--token", "tk"})
	require.Equal(t, 0, code)
	assert.Contains(t, out, `"stdout":"done"`)
	assert.Contains(t, string(gotBody), `"script":"Write-Output ok`)
}

func TestRunOversizeScriptRejectedLocally(t *testing.T) {
	dir := t.TempDir()
	big := strings.Repeat("a", 256*1024+1)
	path := filepath.Join(dir, "big.ps1")
	require.NoError(t, os.WriteFile(path, []byte(big), 0o600))
	_, code := runCLIWithStdin(t, "", []string{
		"run", "n1", "--file", path, "--json", "--server", "http://127.0.0.1:1", "--token", "tk"})
	assert.Equal(t, 2, code) // 用法错误（本地预检，不发请求）
}
```

- [ ] **Step 2: 运行验证失败**

Run: `cd cli && go test ./... -count=1`
Expected: FAIL（run 未实现 / stdin 助手未定义）。

- [ ] **Step 3: 实现**

`cmd_exec.go` 追加（复用 runExecCommand 的会话循环——抽出 `runSession(cl, node, body, cmd) int` 共享）：

```go
func runRunCommand(cmd *cobra.Command, args []string) int {
	cfg, _ := LoadConfig()
	cl := NewClient(resolveServer(cmd, cfg), resolveToken(cmd, cfg))
	node, apiErr := resolveNode(cl, args[0])
	if apiErr != nil {
		return cliFail(cmd, apiErr)
	}

	fileArg, _ := cmd.Flags().GetString("file")
	var script []byte
	var err error
	switch {
	case fileArg == "-":
		script, err = io.ReadAll(os.Stdin)
	case fileArg != "":
		script, err = os.ReadFile(fileArg)
	default:
		err = fmt.Errorf("--file or - required")
	}
	if err != nil {
		return cliFail(cmd, proto.Err(2, "USAGE", err.Error()))
	}
	if len(script) > 256*1024 {
		return cliFail(cmd, proto.Err(2, "USAGE",
			"script exceeds 256KB; upload+exec arrives in Phase 4"))
	}
	return runSession(cl, node, map[string]any{
		"script": string(script), "timeoutSec": intTimeoutFlag(cmd)}, cmd)
}
```

（`runSession` = T9 的 runExecCommand 主体去掉 body 构造；`proto.Err(2,"USAGE",...)` 与既有 failUsage 路径一致。`runCLIWithStdin` 助手加进 main_test.go：os.Pipe 替换 os.Stdin + 既有 captureStdout 组合。）

- [ ] **Step 4: 运行验证通过**

Run: `cd cli && go test ./... -count=1`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add cli
git commit -m "feat(cli): xnc run with script payload from file or stdin"
```

---

### Task 11: E2E Scenario C/H 与文档核对

**Files:**
- Create: `scripts/e2e_phase2.sh`
- Modify: `Makefile`（e2e2 目标）、`skills/xnc/SKILL.md` 与 `skills/xnc/references/cli.md`（仅当行为与文档不符时修正）

**Interfaces:**
- Consumes: T1-T10 全部、dev compose 栈、e2e_phase1.sh 的模式
- Produces: `bash scripts/e2e_phase2.sh` 一键验收 exec/run（Scenario C + H 的 run 部分 + 超时矩阵）

- [ ] **Step 1: 写脚本**

`scripts/e2e_phase2.sh`：

```bash
#!/usr/bin/env bash
# Phase 2 E2E：Scenario C（exec）+ H 的 run 部分 + 超时/退出码矩阵。
set -euo pipefail
cd "$(dirname "$0")/.."

SERVER=http://127.0.0.1:8080
EXE=""
case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) EXE=".exe";; esac
XNC="bin/xnc$EXE"; MOCK="bin/mockagent$EXE"
export XNC_SERVER=$SERVER

echo "== build =="
(cd cli && go build -o "../$XNC" .)
(cd mockagent && go build -o "../$MOCK" .)

echo "== dev stack up =="
COMPOSE="docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.dev.yml"
[ -f deploy/.env ] || cp deploy/.env.example deploy/.env
$COMPOSE up -d --build
trap '$COMPOSE down -v' EXIT
for i in $(seq 1 30); do curl -sf "$SERVER/api/health" >/dev/null && break; sleep 1; done

echo "== login =="
TOKEN_JSON=$(printf 'change-me' | "$XNC" login --server "$SERVER" --email admin@example.com --json)
export XNC_TOKEN=$(echo "$TOKEN_JSON" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
[ -n "$XNC_TOKEN" ] || { echo "login failed"; exit 1; }

echo "== node up =="
ETOK=$("$XNC" token create default --json | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
IDDIR=$(mktemp -d)
"$MOCK" --server "$SERVER" --token "$ETOK" --identity-dir "$IDDIR" --beat 2s >"$IDDIR/mock.log" 2>&1 &
MPID=$!
NODE=$("$XNC" node list --json | sed -n 's/.*"name":"\([^"]*\)".*/\1/p' | head -1)
sleep 3

echo "== C: exec hostname =="
OUT=$("$XNC" exec "$NODE" --json -- hostname); CODE=$?
[ $CODE -eq 0 ] || { echo "exec failed: $OUT"; kill $MPID; exit 1; }
echo "$OUT" | grep -q '"exitCode":0' || { echo "no exitCode 0: $OUT"; kill $MPID; exit 1; }
echo "$OUT" | grep -q '"stdout":"' || { echo "no stdout: $OUT"; kill $MPID; exit 1; }
echo "C: OK"

echo "== H: run script exit code 7 =="
cat >"$IDDIR/t.ps1" <<'PSEOF'
Write-Output script-ran
exit 7
PSEOF
OUT2=$("$XNC" run "$NODE" --file "$IDDIR/t.ps1" --json); CODE2=$?
[ $CODE2 -eq 7 ] || { echo "run exit=$CODE2 (want 7): $OUT2"; kill $MPID; exit 1; }
echo "$OUT2" | grep -q 'script-ran' || { echo "no script output"; kill $MPID; exit 1; }
echo "H: OK (exit passthrough + output)"

echo "== timeout matrix: sleep 30 with timeoutSec 2 → 243 =="
OUT3=$("$XNC" exec "$NODE" --timeout 2 --json -- "Start-Sleep 30"); CODE3=$?
[ $CODE3 -eq 243 ] || { echo "timeout exit=$CODE3 (want 243): $OUT3"; kill $MPID; exit 1; }
echo "$OUT3" | grep -q '"timedOut":true' || { echo "not timedOut: $OUT3"; kill $MPID; exit 1; }
echo "T: OK"

kill $MPID; wait $MPID 2>/dev/null || true
echo "== ALL PHASE2 E2E PASSED =="
```

（注：`Start-Sleep` 仅 Windows PowerShell 存在——mockagent 跑在本机 Windows，成立；若未来 CI 在 Linux 跑此脚本需按 uname 分支切换 `sleep 30`，脚本头部加注释说明。`NODE` 提取依赖单行 JSON envelope 的首个 name。）

Makefile 追加：

```make
.PHONY: e2e2
e2e2:
	bash scripts/e2e_phase2.sh
```

`chmod +x`（`git update-index --chmod=+x` 提交执行位）。

- [ ] **Step 2: 运行 E2E**

Run: `bash scripts/e2e_phase2.sh`
Expected: `C: OK`、`H: OK`、`T: OK`、`ALL PHASE2 E2E PASSED`，exit 0。若失败按 e2e_phase1 的调试方法（compose logs xnc-server、mock 日志）定位——产品缺陷报 BLOCKED，脚本缺陷就地修。

- [ ] **Step 3: 文档核对**

对照 `skills/xnc/references/cli.md`：exec/run 的 flag、退出码（243 超时已列）、`--` 约定、`-` stdin——逐项与实现行为比对；不一致处**修文档使其匹配实现**（实现以本计划 Global Constraints 为准）。`skills/xnc/SKILL.md` 的 exec/run 段落同查。

- [ ] **Step 4: 真机抽验（可选但推荐）**

部署到生产并真机验证（SRV + LABS-TB16G7）：

```bash
py deploy/deploy_srv.py push && py deploy/deploy_srv.py up && py deploy/deploy_srv.py verify
powershell -NoProfile -File deploy/agent_tb16g7.ps1 -Server https://control.xnc.app -Token refresh
bin/xnc.exe exec LABS-TB16G7 -- hostname   # 期望输出 LABS-TB16G7，exit 0
bin/xnc.exe run LABS-TB16G7 --file <(echo 'exit 5') 2>/dev/null || echo "exit=$?"  # 期望 5（Git Bash 进程替换可用则直接跑）
```

- [ ] **Step 5: Commit**

```bash
git add scripts Makefile skills
git commit -m "test(e2e): phase 2 exec/run scenarios against dev compose stack"
```

---

## 完成定义（Phase 2 DoD）

```text
五模块 go test 全绿；GOOS=linux+windows 双编译过；bash scripts/e2e_phase2.sh 全绿
Scenario C：xnc exec <node> -- hostname → exitCode 0 + stdout
Scenario H（run）：脚本 exit 7 透传 + 输出回流 + 临时文件清理（单测覆盖）
超时矩阵：timeoutSec 2 × sleep 30 → CLI 243 + timedOut:true，进程秒杀
审计：exec.start / exec.finish 落库，metadata 无命令内容
skills/xnc 文档与实现一致
```

后续 Phase 3（shell 会话）另起计划——**先做 roadmap Gate B 的 ConPTY spike**。
