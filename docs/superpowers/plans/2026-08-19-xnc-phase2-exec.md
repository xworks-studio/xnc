# XNC Phase 2 — Exec 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 实现一次性命令与脚本执行：`xnc exec web-01 -- hostname` 与 `xnc run web-01 --file t.ps1`，流式输出 + 退出码透传 + 超时/取消杀进程树 + 临时文件即清。

**Architecture:** 复用 Phase 1 的 Hub 长连接：REST 建 Exec Session → CLI 连 Exec WebSocket → Server 经 Agent 控制连接转发 EXEC_REQUEST/OUTPUT/RESULT/CANCEL。Agent 以子进程运行 pwsh（UTF-8 包装），脚本落临时文件执行后 finally 删除。

**Tech Stack:** 同 Phase 1（Go coder/websocket、Rust tokio/reqwest）；新增 `golang.org/x/term` 之外的依赖不引入。

**Spec:** `C:\Users\LABS\Desktop\XNC\spec.md` §14（Exec 模型）、§58（数据流/回调）、§27（REST）、§57（CLI）、§59（测试）

**前置:** Phase 1 计划已全部完成（Hub、proto、CLI 骨架、agent 连接在线）。

## Global Constraints

* 继承 Phase 1 计划的全部全局约束（envelope、退出码、TLS、命名、Conventional Commits）。
* Exec 权限：operator+；节点须 online 且非 disabled（`NODE_DISABLED`）。
* 并发限制：Exec concurrent per Node = 10（可配置 `XNC_EXEC_MAX_PER_NODE`）。
* 脚本 ≤ 256 KB，超出 → `FILE_TOO_LARGE`（HTTP 400）。
* EXEC_RESULT.exitCode 为可空（超时/取消 = null + timedOut=true）。
* Agent 临时文件：`%TEMP%\xnc-<requestId>.ps1`，finally 删除（失败路径同样删除）。
* 输出编码：Agent 强制 UTF-8（包装 `[Console]::OutputEncoding`），chunk base64 ≤ 32 KB。
* 审计：`exec.start`（REST 创建）/ `exec.finish`（终态，metadata 记 timedOut 与 durationMs，不记命令内容——命令内容是否记录由部署配置决定，MVP 不记）。

---

### Task 1: Go proto 扩展 Exec 消息

**Files:**
- Modify: `xnc-server/internal/proto/messages.go`
- Test: `xnc-server/internal/proto/messages_test.go`

**Interfaces:**
- Produces: 常量 `MSG_EXEC_REQUEST/EXEC_OUTPUT/EXEC_RESULT/EXEC_CANCEL`
- Produces:

```go
type ExecRequest struct {
	Command string `json:"command,omitempty"` // 与 script 二选一
	Script  string `json:"script,omitempty"`
	Timeout int    `json:"timeout,omitempty"` // 秒，默认 300
}
type ExecOutput struct {
	Stream string `json:"stream"` // "stdout" | "stderr"
	Data   string `json:"data"`   // base64
}
type ExecResult struct {
	ExitCode  *int  `json:"exitCode"`
	TimedOut  bool  `json:"timedOut"`
	DurationMs int64 `json:"durationMs"`
}
```

- [ ] **Step 1: 失败测试（黄金样本）**

```go
func TestExecGolden(t *testing.T) {
	e := Envelope{Type: MSG_EXEC_OUTPUT, RequestID: "r1",
		Payload: mustJSON(ExecOutput{Stream: "stdout", Data: "QUJD"})}
	b, _ := json.Marshal(e)
	want := `{"type":"EXEC_OUTPUT","requestId":"r1","payload":{"stream":"stdout","data":"QUJD"}}`
	if string(b) != want { t.Fatalf("got %s", b) }
	ec := 0
	e2 := Envelope{Type: MSG_EXEC_RESULT, RequestID: "r1", Payload: mustJSON(ExecResult{ExitCode: &ec})}
	b2, _ := json.Marshal(e2)
	if !strings.Contains(string(b2), `"exitCode":0`) { t.Fatalf("got %s", b2) }
}
```

- [ ] **Step 2: 确认失败 → 实现四个类型与常量**
- [ ] **Step 3: `go test ./internal/proto/` PASS；把黄金样本同步进 `proto/messages.md`**
- [ ] **Step 4: Commit** `feat(proto): exec request/output/result/cancel messages`

---

### Task 2: Hub 发送与消息路由扩展

**Files:**
- Modify: `xnc-server/internal/agenthub/hub.go`
- Test: `xnc-server/internal/agenthub/hub_test.go`

**Interfaces:**
- Produces: `(*Hub) Send(nodeID string, env proto.Envelope) error`（无连接 → `ErrNodeOffline`）
- Produces: `(*Hub) SetMessageHandler(fn func(nodeID string, env proto.Envelope))`——agent 读循环中 HEARTBEAT 之外的消息回调给 fn（nil 则忽略）
- Consumes: Phase 1 的 Conn/Register/Unregister

- [ ] **Step 1: 失败测试**（httptest + 假 agent 拨号（复用 Phase 1 全握手测试辅助）：server `Send(nodeID, EXEC_REQUEST)` → 假 agent 收到该 envelope；假 agent 发 EXEC_RESULT → handler 收到 `(nodeID, env)`）
- [ ] **Step 2: 确认失败 → 实现**（Conn 增加 `send chan proto.Envelope`（缓冲 256，满则关连接——背压兜底）；读循环 default 分支调用 handler）
- [ ] **Step 3: 测试 PASS**
- [ ] **Step 4: Commit** `feat(server): hub send and message handler routing`

---

### Task 3: Exec REST 与会话编排

**Files:**
- Create: `xnc-server/internal/execmgr/execmgr.go`
- Create: `xnc-server/internal/api/exec.go`
- Test: `xnc-server/internal/execmgr/execmgr_test.go`

**Interfaces:**
- Consumes: Task 1/2、Phase 1 RequireAuth/Hub
- Produces: `execmgr.Manager`——`NewManager(hub *agenthub.Hub, pool *pgxpool.Pool, maxPerNode int)`；`(*Manager) HandleAgentMessage(nodeID string, env proto.Envelope)`（EXEC_OUTPUT/EXEC_RESULT → 转发给对应客户端 WS；RESULT 后清理 + audit exec.finish）
- Produces: `POST /api/nodes/{id}/exec`（body `{"command":"..."} `或 `{"script":"...","timeout":300}`）：
  - 403 FORBIDDEN（非 operator+）/ 404 NODE_NOT_FOUND / 409 NODE_OFFLINE / 409 NODE_DISABLED
  - 并发超限 → 429 `SESSION_LIMIT`（CLI 246）
  - 成功 → `{"ok":true,"data":{"requestId","token","websocketUrl":"/api/exec/{requestId}"}}`；audit `exec.start`
- Produces: `GET /api/exec/{requestId}?token=`（Exec WebSocket，token 单次 60s，绑定 user+node）：
  - 服务端→客户端 text JSON：`{"type":"OUTPUT","stream","data"}` / `{"type":"RESULT","exitCode","timedOut","durationMs"}` / `{"type":"ERROR","code","message"}`
  - 客户端→服务端：`{"type":"CANCEL"}` 或直接断开 → Server 下发 EXEC_CANCEL
  - 服务端超时守护：payload.timeout + 10s 宽限仍无 RESULT → 下发 EXEC_CANCEL 并发 `{"type":"RESULT","exitCode":null,"timedOut":true}`
  - 节点中途离线（Hub Unregister）→ 关闭该节点全部 Exec 会话，客户端收 `{"type":"ERROR","code":"NODE_OFFLINE"}`

- [ ] **Step 1: 失败测试**（假 agent 全握手 + 真 REST/WS：
  1. viewer 403、offline 409、disabled 409、超限 429；
  2. 正常流：REST → 假 agent 收到 EXEC_REQUEST(script) → 回 EXEC_OUTPUT×2 + EXEC_RESULT(0) → 客户端 WS 依次收到 OUTPUT/OUTPUT/RESULT；
  3. 客户端发 CANCEL → 假 agent 收到 EXEC_CANCEL；
  4. 假 agent 断开 → 客户端收 ERROR NODE_OFFLINE）
- [ ] **Step 2: 确认失败 → 实现 execmgr.go + exec.go**（会话 map[requestID]*session{nodeID,userID,clientWS,cancelOnce}；router 挂两路由）
- [ ] **Step 3: 测试 PASS**
- [ ] **Step 4: Commit** `feat(server): exec session api with streaming and cancel`

---

### Task 4: Rust proto 镜像

**Files:**
- Modify: `xnc-agent/crates/xnc-proto/src/lib.rs`

**Interfaces:**
- Consumes: Task 1 黄金样本（同步进 messages.md 后照抄）
- Produces:

```rust
#[derive(Serialize, Deserialize)]
pub struct ExecRequest { pub command: Option<String>, pub script: Option<String>, pub timeout: Option<u64> }
#[derive(Serialize, Deserialize)]
pub struct ExecOutput { pub stream: String, pub data: String }
#[derive(Serialize, Deserialize)]
pub struct ExecResult { pub exit_code: Option<i32>, pub timed_out: bool, pub duration_ms: i64 }
```

- [ ] **Step 1: 失败测试**（黄金 JSON 字符串反序列化+再序列化相等，含 exitCode:null 样本）
- [ ] **Step 2: 实现 → `cargo test -p xnc-proto` PASS**
- [ ] **Step 3: Commit** `feat(agent): exec proto mirror`

---

### Task 5: Agent ExecManager——command 模式

**Files:**
- Create: `xnc-agent/crates/xnc-agent/src/exec.rs`（含 shell 探测 `detect_shell() -> Shell { Exe: PathBuf, Type: String }`：PATH 找 pwsh.exe → "pwsh"，否则 powershell.exe → "windows-powershell"）
- Modify: `xnc-agent/crates/xnc-agent/src/run.rs`（读循环分发 EXEC_REQUEST → ExecManager）
- Test: `xnc-agent/crates/xnc-agent/tests/exec.rs`

**Interfaces:**
- Produces: `ExecManager::handle(ws: &mut WsTx, env: Envelope)`——构造命令并 spawn：

```rust
// UTF-8 输出包装：避免中文系统 OEM 代码页乱码
let cmdline = format!(
    "[Console]::OutputEncoding=[Text.Encoding]::UTF8; {}",
    req.command.unwrap_or_default()
);
CommandBuilder: shell.exe -NoLogo -NonInteractive -Command <cmdline>
```

  - stdout/stderr 两路 `tokio::io::AsyncReadExt` 循环读 32KB → base64 → EXEC_OUTPUT{stream,data}
  - 结束 → EXEC_RESULT{exit_code, duration_ms}
  - timeout（默认 300s）到点 → `taskkill /PID <pid> /T /F` → RESULT{exit_code:null, timed_out:true}
  - 收到 EXEC_CANCEL（读循环通知，tokio::sync::Notify）→ 同上 kill 流程
- Produces: `detect_shell()` 缓存于进程全局（OnceLock）

- [ ] **Step 1: 失败测试**（不依赖网络：直接构造 Envelope 调 handle，用内存 channel 代替 WsTx：
  1. `command: "Write-Output hello"` → OUTPUT(stdout,"aGVsbG8=") + RESULT(0)；
  2. `command: "exit 42"` → RESULT(42)；
  3. `command: "Start-Sleep 30", timeout:1` → 2s 内 RESULT(null, timedOut=true)；
  4. CANCEL 通知后 1s 内 RESULT(null)（Start-Sleep 30 场景）；
  5. stderr 流：`command: "Write-Error boom"` → OUTPUT(stream=stderr) 存在）
- [ ] **Step 2: 确认失败 → 实现**（base64/chrono 依赖已在 workspace）
- [ ] **Step 3: `cargo test -p xnc-agent` PASS**
- [ ] **Step 4: Commit** `feat(agent): exec manager with utf8 wrapper, timeout and cancel`

---

### Task 6: Agent 脚本模式

**Files:**
- Modify: `xnc-agent/crates/xnc-agent/src/exec.rs`
- Test: `xnc-agent/crates/xnc-agent/tests/exec.rs`（追加）

**Interfaces:**
- Consumes: Task 5 handle 流程
- Produces: script 分支——`%TEMP%\xnc-<requestId>.ps1` 写入（校验 ≤ 256KB 由 Server 侧完成，Agent 侧防御性再查）→ 命令 `-Command "[Console]::OutputEncoding=...; & 'C:\...\xnc-<id>.ps1'"` → `finally`（Rust：defer 模式 = 显式 drop 前删除 + panic-safe catch_unwind 或 RAII guard struct）删除临时文件

- [ ] **Step 1: 失败测试**（script: `"Write-Output ok; exit 7"` → RESULT(7) 且 `%TEMP%` 下 `xnc-*` 测试前后的文件集合不变；脚本内 `Start-Sleep 30` + CANCEL → RESULT(null) 且临时文件已删）
- [ ] **Step 2: 实现 TempScriptGuard（RAII 删除）**
- [ ] **Step 3: 测试 PASS → Commit** `feat(agent): script exec with temp file lifecycle`

---

### Task 7: xnc exec / xnc run CLI

**Files:**
- Modify: `xnc-cli/cmd/xnc/main.go`、`xnc-cli/internal/client/client.go`
- Test: `xnc-cli/internal/client/client_test.go`

**Interfaces:**
- Produces: `client.APIClient.Exec(node, req ExecBody) (*ExecSession, error)`（POST + 返回 WS URL）；`(*ExecSession) Stream() <-chan ExecEvent`（`ExecEvent{Type, Stream, Data []byte, ExitCode *int, TimedOut bool, ErrCode string}`）；`(*ExecSession) Cancel()`
- Produces: 命令 `xnc exec <node> [--timeout N] [--cwd PATH] -- <command...>`（cwd 实现为命令前缀 `Set-Location '<cwd>'; `）与 `xnc run <node> (--file f.ps1 | -) [--timeout N]`（stdin 读脚本；>256KB 报错退出 2）
- Produces: 输出行为——终端模式：OUTPUT 事件按 stream 打 stdout/stderr（实时）；RESULT 后：JSON 模式打印 envelope（data: node/exitCode/stdout/stderr/timedOut/durationMs，stdout/stderr 为累积文本）；退出码 = exitCode 透传（null→243 超时 / ErrCode→240/244/246 映射）

- [ ] **Step 1: 失败测试**（httptest + 内存 WS 假 server：OUTPUT×2+RESULT(0) → 累积 stdout 正确、exit 0；RESULT(42) → exit 42；无 RESULT 收 ERROR NODE_OFFLINE → exit 240 之外按映射（NODE_OFFLINE→242）；--json golden 输出字符串比对）
- [ ] **Step 2: 确认失败 → 实现**（WS 客户端 coder/websocket/golang）
- [ ] **Step 3: `go test ./...` PASS；本地对 server+agent 冒烟：`xnc exec web-01 -- hostname`**
- [ ] **Step 4: Commit** `feat(cli): xnc exec and xnc run with streaming and exit codes`

---

### Task 8: shell_type 探测上报与展示

**Files:**
- Modify: `xnc-agent/crates/xnc-agent/src/connect.rs`（HELLO payload 增加 `shellType: detect_shell().type`）
- Modify: `xnc-server/internal/api/agent_connect.go`（握手时 `UPDATE nodes SET shell_type=$1`）
- Modify: `xnc-cli/internal/output/output.go`（node show 表格加 Shell 列）
- Test: `xnc-server/internal/agenthub/hub_test.go`（追加：HELLO 带 shellType → 库中更新）；`xnc-agent` connect 测试（追加断言 Hello 序列化含 shellType）

- [ ] **Step 1: 两侧失败测试 → Step 2: 实现 → Step 3: PASS**
- [ ] **Step 4: Commit** `feat: report and display agent shell type`

---

### Task 9: Phase 2 E2E 验收

**Files:**
- Create: `scripts/e2e-phase2.ps1`（NODE_MAIN 真 机验收脚本，读 config.env）

- [ ] **Step 1: 本地 E2E**（server + NODE_MAIN agent 在线）

```powershell
xnc exec web-01 -- hostname                 # 期望输出主机名，exit 0
xnc exec web-01 -- "exit 42"; echo "code=$LASTEXITCODE"   # code=42
$x = xnc exec web-01 --timeout 2 -- "Start-Sleep 30"      # 2-4s 返回，退出码 243
Set-Content t.ps1 'Write-Output ok; exit 7'
xnc run web-01 --file t.ps1                  # 输出 ok，退出码 7
Get-ChildItem $env:TEMP\xnc-*.ps1            # 无残留
```

- [ ] **Step 2: 公网 SRV E2E**（同上对 https://control.xnc.app；含 viewer 账号 403 一例）
- [ ] **Step 3: 全部符合预期 → Commit** `test: phase2 e2e script`

---

## Self-Review 结论

* Spec 覆盖：§53 Phase 2 五项（exec API→T3、EXEC_* 消息→T1/T4、超时取消→T3/T5、xnc run→T6/T7、验收→T9）✓；§58 数据流逐条（流式/终态/取消/退出码）✓；§59 矩阵 exec 行（退出码透传 T7、超时 kill T5、大输出背压——Hub send 缓冲 256 T2 兜底、cwd T7）✓
* 无占位符；类型一致（ExecRequest/Output/Result 字段名 Go↔Rust 黄金样本对齐；Manager.HandleAgentMessage 与 Hub.SetMessageHandler 签名闭合）✓
