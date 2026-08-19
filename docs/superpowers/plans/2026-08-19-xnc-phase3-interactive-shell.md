# XNC Phase 3 — Interactive Shell 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `xnc shell web-01` 与 Web Terminal（xterm.js）获得真正交互式 PowerShell：ConPTY、VT 原样、Resize、Ctrl+C 不杀会话、状态保持（`$x=42; $x`→42）、断连清理无孤儿进程。

**Architecture:** REST 建 Shell Session → 客户端连 Shell WebSocket；Server 作为纯字节桥：客户端 binary frame ↔ agent 控制连接 binary frame（`[1B type][16B sessionId][payload]`），控制消息（RESIZE/CLOSE）走 text JSON。Agent 用 portable-pty（Windows=ConPTY）托管 pwsh/powershell。

**Tech Stack:** Go coder/websocket；Rust portable-pty、windows；CLI golang.org/x/term；Web xterm.js + @xterm/addon-fit。

**Spec:** `C:\Users\LABS\Desktop\XNC\spec.md` §11（binary frame）、§15-20（Shell 全模型）、§27/57（API/CLI）、§49（背压）、§50（限额）、§59（测试）

**前置:** Phase 1、Phase 2 计划完成（Hub Send/Handler、会话模式、detect_shell）。

## Global Constraints

* 继承 Phase 1/2 全部全局约束。
* Binary frame：`[1B frameType][16B sessionId(UUID 原始字节)][payload]`；`0x01=SHELL_INPUT`、`0x02=SHELL_OUTPUT`（spec §11）。
* Server 对 Shell 是**纯转发**：不解析 VT、不解析 prompt、不解析 PowerShell 输出（spec §17）。
* 生命周期（spec §20）：idle 30min 无 I/O 关闭、硬上限 8h、pwsh 退出→SHELL_CLOSE、客户端断开→SHELL_CLOSE、Node 离线→全部 Shell 关闭并杀进程。
* 限额：Shell per Node = 10（`XNC_SHELL_MAX_PER_NODE`）。
* 背压（spec §49）：Server 每方向有界队列（256 帧），客户端持续停滞 → 关会话，不 OOM。
* SessionToken：60s 单次，绑定 user+node+type=shell（spec §39）。
* Ctrl+C = 输入流 `0x03` 字节经 ConPTY 传递，**绝不 kill pwsh**（spec §19）。
* 审计：shell.open / shell.close（metadata: rows/cols、closeReason）。
* 权限：operator+；NODE_OFFLINE/NODE_DISABLED 同 Phase 2 语义。

---

### Task 1: Go binary frame 编解码 + Shell 消息

**Files:**
- Modify: `xnc-server/internal/proto/messages.go`（新增 frame.go 内容合并或新文件 `frame.go`）
- Test: `xnc-server/internal/proto/frame_test.go`

**Interfaces:**
- Produces: `type FrameType byte`；常量 `FRAME_INPUT FrameType = 0x01`、`FRAME_OUTPUT FrameType = 0x02`
- Produces: `proto.BuildFrame(t FrameType, sessionID uuid.UUID, payload []byte) []byte`；`proto.ParseFrame(b []byte) (FrameType, uuid.UUID, []byte, error)`（长度 <17 → error）
- Produces: JSON 消息常量与 payload：`MSG_SHELL_OPEN`（`ShellOpen{Cols, Rows int; Shell string \`json:"shell,omitempty"\`}`——Server→Agent，sessionId 在 Envelope）、`MSG_SHELL_OPENED`（`ShellOpened{Ok bool; ErrCode string}`）、`MSG_SHELL_RESIZE`（`ShellResize{Cols, Rows int}`）、`MSG_SHELL_CLOSE`（`ShellClose{Reason string}`，双向）

- [ ] **Step 1: 失败测试**

```go
func TestFrameRoundtrip(t *testing.T) {
	id := uuid.MustParse("6f9619ff-8b86-d011-b42d-00c04fc964ff")
	f := BuildFrame(FRAME_INPUT, id, []byte{0x03, 'a'})
	ft, sid, p, err := ParseFrame(f)
	if err != nil || ft != FRAME_INPUT || sid != id || string(p) != "\x03a" {
		t.Fatalf("ft=%v sid=%v p=%q err=%v", ft, sid, p, err)
	}
	if _, _, _, err := ParseFrame([]byte{0x01}); err == nil { t.Fatal("short frame must error") }
}
```

- [ ] **Step 2: 确认失败 → 实现；黄金样本同步 `proto/messages.md`**
- [ ] **Step 3: PASS → Commit** `feat(proto): binary frame codec and shell messages`

---

### Task 2: Shell 会话管理与 REST

**Files:**
- Create: `xnc-server/internal/shellmgr/shellmgr.go`
- Create: `xnc-server/internal/api/shell.go`
- Test: `xnc-server/internal/shellmgr/shellmgr_test.go`

**Interfaces:**
- Produces: `shellmgr.Manager`——`NewManager(pool, maxPerNode, idle, maxLifetime)`；字段 `map[sessionID]*Session`
- Produces: `POST /api/nodes/{id}/shell`（body `{"cols":120,"rows":30}` 可选）：
  * 校验链同 exec（operator+/online/非 disabled/限额 10）
  * `hub.Send(nodeID, SHELL_OPEN{cols,rows})` → 等 SHELL_OPENED（3s 超时；Ok=false → 502 SHELL_START_FAILED）
  * 返回 `{"ok":true,"data":{"sessionId","token","websocketUrl":"/api/shell/{sessionId}"}}`；audit shell.open
- Produces: `GET /api/shell/{sessionId}?token=`——升级 WS 后：
  * 客户端 binary → `ParseFrame` 校验 sessionId 匹配 → 重写 sessionId 后 `hub.SendBinary(nodeID, frame)`（Hub 新增 `SendBinary`；agent Conn 的二进制出帧通道与 JSON 通道分离但共用发送锁/队列）
  * agent binary（0x02）→ 按 sessionId 路由到客户端 WS 原样转发
  * 客户端 text JSON（RESIZE/CLOSE）→ 封 Envelope 转发 agent；agent SHELL_CLOSE/RESIZE → 转发客户端
  * 任一侧断开 → 双向 SHELL_CLOSE + 清理 + audit shell.close
  * 每 session 维护 idleTimer（任何方向 I/O 重置 30min）与 maxLifetime(8h)

- [ ] **Step 1: 失败测试**（假 agent + 假 WS 客户端：
  1. REST 400/403/409/429 矩阵；
  2. 正常流：REST → 假 agent 收 SHELL_OPEN → 回 OPENED → 客户端连 WS 发 INPUT 帧 → 假 agent 收到；假 agent 发 OUTPUT 帧 → 客户端收到；
  3. 客户端断开 → 假 agent 收 SHELL_CLOSE；
  4. 假 agent 断连（node offline）→ 客户端 WS 被关闭；
  5. 限额：第 11 个 → 429）
- [ ] **Step 2: 确认失败 → 实现**（Hub 扩展 `SendBinary(nodeID, []byte) error` 与二进制入站 handler 二元组：`SetBinaryHandler(fn func(nodeID string, frame []byte))`）
- [ ] **Step 3: PASS → Commit** `feat(server): shell session manager and ws bridge`

---

### Task 3: 空闲/生命周期清扫与背压关闭

**Files:**
- Modify: `xnc-server/internal/shellmgr/shellmgr.go`
- Test: `xnc-agent` 侧无关；`shellmgr_test.go` 追加

- [ ] **Step 1: 失败测试**（注入 idle=50ms、lifetime=150ms：无 I/O 会话 100ms 内被关（假 agent 收到 SHELL_CLOSE）；持续 I/O 的会话超过 idle 仍存活、超过 lifetime 被关；客户端停滞：假 agent 连发 1000 帧而客户端不读 → 会话最终被关闭而非无限缓冲）
- [ ] **Step 2: 实现**（per-session 有界 channel 256；`select` 写阻塞 5s → 强制关闭；后台 ticker 扫描 idle/lifetime）
- [ ] **Step 3: PASS → Commit** `feat(server): shell idle/lifetime sweep and backpressure close`

---

### Task 4: Rust proto 与 frame 镜像

**Files:**
- Modify: `xnc-agent/crates/xnc-proto/src/lib.rs`

**Interfaces:**
- Produces: `pub const FRAME_INPUT: u8 = 0x01; pub const FRAME_OUTPUT: u8 = 0x02;`
- Produces: `pub fn build_frame(t: u8, session_id: &str, payload: &[u8]) -> Vec<u8>`（session_id 为 UUID 字符串 → 解析为 16 字节；`pub fn parse_frame(b: &[u8]) -> Result<(u8, Uuid, &[u8])>`，uuid crate）
- Produces: `ShellOpen{cols,rows,shell}` / `ShellOpened{ok,err_code}` / `ShellResize{cols,rows}` / `ShellClose{reason}` serde 镜像 + 黄金测试（对齐 messages.md）

- [ ] **Step 1: 失败测试（含 Go 黄金样本字节级比对）→ Step 2: 实现 → Step 3: `cargo test -p xnc-proto` PASS**
- [ ] **Step 4: Commit** `feat(agent): shell proto and frame mirror`

---

### Task 5: Agent ShellManager（portable-pty）

**Files:**
- Create: `xnc-agent/crates/xnc-agent/src/shell.rs`
- Modify: `xnc-agent/crates/xnc-agent/src/run.rs`（分发 SHELL_OPEN / binary 帧 / SHELL_RESIZE / SHELL_CLOSE；控制连接断开 → `ShellManager::close_all()`）
- Test: `xnc-agent/crates/xnc-agent/tests/shell.rs`

**Interfaces:**
- Produces: `ShellManager`——`handle_open(ws, env)`：

```rust
let shell = detect_shell(); // Phase 2 T8
let pair = native_pty_system().openpty(PtySize { rows, cols, pixel_width: 0, pixel_height: 0 })?;
let mut cmd = CommandBuilder::new(&shell.exe);
cmd.args(["-NoLogo", "-WorkingDirectory", r"C:\Users\Public"]); // 交互式不加 -NonInteractive
let child = pair.slave.spawn_command(cmd)?;
let reader = pair.master.try_clone_reader()?;
let writer = pair.master.take_writer()?;
```

  * 读循环：`reader → 32KB 块 → build_frame(FRAME_OUTPUT, session, chunk) → ws 发送`
  * 写路径：INPUT 帧 → `writer.write_all(payload)`
  * RESIZE → `pair.master.resize(PtySize{..})`
  * pwsh 退出（read 返回 0 / child.try_wait 完成）→ 发 JSON SHELL_CLOSE{reason:"process-exit"} → 清理
  * SHELL_CLOSE / 控制连接断开 → `child.kill()` + drop master → 清理
  * 本地限额 10（防御性）；`close_all()` 在连接管理器重连前调用（旧连接的 Shell 一律终止，spec §41）

- [ ] **Step 1: 失败测试**（in-test 假 server + 内存 WS 通道，直接驱动 ShellManager：
  1. open(cols 80,rows 24) → 回 SHELL_OPENED{ok:true}；写入 INPUT 帧 `"echo xnc-ok\r"` → 2s 内 OUTPUT 帧流中含 `xnc-ok`（UTF-8 子串匹配，忽略 VT 控制符）；
  2. 状态保持：`$x=42\r` 后 `$x\r` → 输出含 "42"；
  3. RESIZE 不报错且后续输出仍流式；
  4. `exit\r` → 收到 SHELL_CLOSE{reason:"process-exit"}；
  5. 强制 close_all → 子进程退出（child.try_wait Some 且临时 spawned pwsh 计数归零——用进程名采样断言无 pwsh 残留）；
  6. Ctrl+C：`ping -t 127.0.0.1\r` 跑 1s → INPUT 帧 `\x03` → 2s 内收到新的 prompt 输出且会话未关（Spec §19 验收））
- [ ] **Step 2: 确认失败 → 实现**（依赖 `portable-pty`；shell 选型：detect_shell().exe；测试环境无 pwsh 时走 powershell.exe——探测逻辑天然覆盖）
- [ ] **Step 3: `cargo test -p xnc-agent --test shell` PASS（Windows only，`#![cfg(windows)]`）**
- [ ] **Step 4: Commit** `feat(agent): shell manager on portable-pty with full lifecycle`

---

### Task 6: xnc shell CLI

**Files:**
- Modify: `xnc-cli/cmd/xnc/main.go`、`xnc-cli/internal/client/client.go`
- Test: `xnc-cli/internal/client/client_test.go`

**Interfaces:**
- Produces: `client.APIClient.OpenShell(node string, cols, rows int) (*ShellSession, error)`（REST）；`(*ShellSession) WsURL() string`
- Produces: `xnc shell <node>` 实现：
  1. `term.MakeRaw(os.Stdin.Fd())`，defer 恢复（含 panic path）
  2. 初始尺寸 `terminal.GetSize` → REST cols/rows
  3. 双向 pump：stdin 读 → `BuildFrame(FRAME_INPUT, session, b)` → WS binary；WS binary → ParseFrame（校验 0x02+sessionId）→ stdout 原样写
  4. 每 500ms 轮询 `terminal.GetSize`，变化 → text JSON SHELL_RESIZE（Windows 无 SIGWINCH，轮询是务实方案；注释注明）
  5. 退出条件：WS 关闭（打印 `\r\n[session closed]\r\n`，原因来自关闭前 ERROR/CLOSE 消息）或 stdin EOF（发 SHELL_CLOSE）
  6. Ctrl+C：raw 模式下自然成为输入字节 0x03 转发，**不注册 interrupt 信号处理器**（注释说明这是 spec §19 要求）
- 退出码：正常关闭 0；NODE_OFFLINE → 242；其他错误按 Phase 1 映射

- [ ] **Step 1: 失败测试**（内存假 WS server：收到 INPUT 帧回固定 OUTPUT 帧 → 断言 pump 转发与 RESIZE 发送（模拟 stdin 尺寸变化的注入接口）；不测真实 TTY）
- [ ] **Step 2: 确认失败 → 实现**
- [ ] **Step 3: `go test ./...` PASS；真机冒烟 `xnc shell web-01` 手工交互（cd/$x/Get-Process/resize/Ctrl+C）**
- [ ] **Step 4: Commit** `feat(cli): xnc shell interactive terminal`

---

### Task 7: Web Terminal（xterm.js）

**Files:**
- Modify: `web/src/App.tsx`（路由 `/nodes/:id/terminal`）
- Create: `web/src/pages/Terminal.tsx`、`web/src/shell.ts`（WS + frame 帮助函数，与 api.ts 同层）

**Interfaces:**
- Consumes: Task 2 的 REST + Shell WS 协议（binary 帧 + text JSON 控制）
- Produces:

```typescript
// web/src/shell.ts 核心
export function connectShell(nodeId: string): Promise<{ term: Terminal; ws: WebSocket }> {
  // POST /api/nodes/{id}/shell {cols, rows}（用 term 尺寸）→ 连 websocketUrl?token=
  // ws.binaryType = "arraybuffer"
  // ws.onmessage: ArrayBuffer 且首字节 0x02 → term.write(payload.subarray(17))
  // term.onData(b) → ws.send(帧: [0x01, ...uuidBytes(sessionId), ...encoder.encode(b)])
  // fit.onResize → ws.send(JSON {type:"SHELL_RESIZE", payload:{cols, rows}})
  // ws.onclose → term.write("\r\n\x1b[31m[session closed]\x1b[0m\r\n")
}
```

  * UUID 字符串→16 字节：`crypto.randomUUID()` 建会话时由 server 返回 sessionId，前端用一次性解析函数（去连字符 hex → Uint8Array）
  * fit addon 随窗口 resize 自动触发；组件卸载 → 发 SHELL_CLOSE + ws.close + term.dispose

- [ ] **Step 1: 实现 Terminal 页**（黑底 xterm 容器全屏、fit、loading/错误态：NODE_OFFLINE 显示徽标）
- [ ] **Step 2: 手工验证**（server+agent 真机：打开页面敲 `hostname`、`$x=42`→`$x`、拉伸窗口列宽生效、Ctrl+C 会话存活、关标签页后 agent 无 pwsh 残留）
- [ ] **Step 3: Commit** `feat(web): xterm.js terminal page`

---

### Task 8: Phase 3 E2E 验收

**Files:**
- Create: `scripts/e2e-phase3.ps1`

- [ ] **Step 1: 真机脚本验收（Scenario D 全量 + 边界）**

```powershell
# 交互链（自动注入式，管道喂命令到 xnc shell -- （CLI 增加 --feed <file> 测试后门：
#   逐行写入 stdin 模拟人工，专用于 E2E，不影响交互模式）
"cd C:\Windows", "`$x = 123", "echo `$x" | xnc shell web-01 --feed -
# 期望输出流包含 123
1..3 | % { "echo ok$_" } | xnc shell web-01 --feed -
# Get-Process 大输出不丢字节：比对行数
"Get-Process | Measure-Object | % Count" | xnc shell web-01 --feed -
# Ctrl+C：ping -t + \x03 注入后会话存活
# 断连：sc stop XNCAgent → 客户端 5s 内 [session closed] → sc start 后可重新开 shell
# 残留检查：agent 侧 pwsh 进程数前后一致
```

- [ ] **Step 2: 公网 SRV 重复关键用例（Web Terminal 一例 + CLI 一例）**
- [ ] **Step 3: Commit** `test: phase3 e2e script`

---

## Self-Review 结论

* Spec 覆盖：§53 Phase 3 六项（ConPTY→T5、降级→T5 复用 detect_shell、Shell WebSocket→T2、Resize→T2/T5/T6/T7、Ctrl+C→T5/T6、双端并行交付→T6/T7）✓；§17 VT 原样/纯转发→T2 明示 ✓；§18 Resize→T2/T5 ✓；§19 Ctrl+C→T5 测 6 + T6 设计 ✓；§20 生命周期六种关闭→T2/T3/T5（timeout 在 T3，node offline 在 T2/T5，admin 强制=Phase 5 复用 close API）✓；§49 背压→T3 ✓；§50 限额→T2/T5 ✓
* 无占位符；类型一致：frame 头 17 字节两语言字节级对齐（T1↔T4）；sessionId 在 REST 为字符串、在帧内为 16 字节 UUID，两侧解析函数签名闭合；ShellOpen/Opened/Resize/Close 字段 Go↔Rust 黄金样本对齐 ✓
* 顺序依赖：T5 依赖 Phase 2 的 detect_shell；T6 依赖 T1 的 Go frame codec；T7 依赖 T2 协议 ✓
