# XNC v2 Phase 6（桌面预览）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付桌面预览：screen 会话 kind + agent 侧 helper 进程（用户会话 GDI 捕获 → JPEG → named pipe）+ CLI `xnc screen --snapshot` + Web UI 预览面板，E2E 覆盖三态验收（capturing/locked/no_session）。

**Architecture:** Agent（Session 0 服务）通过 WTSQueryUserToken + CreateProcessAsUser 拉起 xnc-screen-helper.exe（用户 console 会话），helper 用 GDI BitBlt 捕获主显示器 → 缩放 → JPEG → 经 named pipe 回传给 screen 会话引擎 → 会话 WS binary 帧到客户端。Server 纯转发不解析不落盘。

**Tech Stack:** 沿用全栈；GDI 捕获用 `x/sys/windows` API（GetDC/BitBlt/GdipCreateBitmapFromHBITMAP 或纯 GDI+ JPEG 编码）；named pipe 用 `winio` 或 `x/sys/windows` CreateNamedPipe。

**Spec:** `spec.md` §64（桌面预览完整定义）+ 设计 §3.2 screen 词汇。

## Global Constraints

- **协议**（设计 §3.2，绑定）：SESSION_OPEN params `{fps=1, quality=60, maxWidth=1280}`；text `SCREEN_BEGIN {width, height, state}` 先行；text `SCREEN_STATE {state}` 变化时；binary = 单帧完整 JPEG（WS message 即一帧）。
- **状态三态**（spec §64）：`capturing`（正常出帧）/ `locked`（锁屏）/ `no_session`（未登录）。状态而非黑屏。
- **边界**（spec §64）：只读无键鼠注入；默认 1 fps 上限 5 fps；JPEG 质量 ~60 宽度 ≤1280 单帧 ≤300KB；预览会话存在才捕获关闭即停。
- **Session 0 问题**（spec §64）：Agent 是 Session 0 服务无法直接访问用户桌面 → WTSQueryUserToken + CreateProcessAsUser 拉起 helper（用户 console 会话）→ GDI 捕获 → named pipe 回传。
- **REST**：`POST /api/nodes/{id}/screen {fps?, quality?, maxWidth?}` → 202 统一响应；RBAC operator+（viewer 403）。
- **审计**：screen.open / screen.close；帧数据不落盘不进日志。
- **CLI**：`xnc screen <node> --snapshot out.jpg`（--json 返回元数据）；`xnc screen <node> --open`（浏览器打开实时预览——生成本地 HTML 打开默认浏览器连接 WS）。
- **Web UI 预览面板**：NodeDetail 页新增 [Screen Preview] 按钮 → 弹出面板消费 screen 会话 WS 显示 JPEG 帧。
- **清理**：预览 WS 关闭 → helper 进程退出无残留；Node 离线 → 预览会话关闭。
- **性能**：1 fps 下单帧 capture+encode < 100ms。
- **权限**：operator+（viewer 不可见）。
- 每任务一 commit；双 GOOS 编译门槛（agent 侧 Windows-only 代码 behind build tags）。

**不含**：Windows.Graphics.Capture（后续优化）、多显示器（monitor 参数预留）、键鼠控制（控制走 RDP tunnel）。

---

## 文件结构总览

```text
proto/
├── session.go                 T1  +KindScreen/ScreenParams/ScreenBegin/ScreenState
└── session_test.go            T1
server/internal/api/
├── screen_handlers.go         T2  POST /api/nodes/{id}/screen
├── screen_handlers_test.go    T2
agent/session/
├── screen.go                  T3  Screen Handler（helper 拉起 + pipe 读取 + 帧推送）
├── screen_test.go             T3
agent/screen-helper/
├── main.go                    T4  xnc-screen-helper.exe（GDI 捕获 + JPEG 编码 + named pipe 服务端）
├── capture_windows.go         T4  GDI BitBlt + GDI+ JPEG
├── pipe_windows.go            T4  named pipe 服务端
├── capture_other.go           T4  非 Windows 桩
agent/agent.go / mockagent     T5  注册 KindScreen
cli/
├── cmd_screen.go              T6  xnc screen --snapshot / --open
├── cmd_screen_test.go         T6
web/src/pages/
├── ScreenPreview.tsx          T7  Web UI 预览面板
web/src/pages/NodeDetail.tsx   T7  + [Screen Preview] 按钮
scripts/e2e_phase6.sh          T8  三态验收 + 无残留断言
```

---

### Task 1: proto — screen 词汇

**Files:**
- Modify: `proto/session.go`
- Test: `proto/session_test.go`（追加）

**Interfaces:**
- Produces:

```go
const KindScreen = "screen"

type ScreenParams struct {
    Fps      int `json:"fps,omitempty"`      // 默认 1，上限 5
    Quality  int `json:"quality,omitempty"`  // JPEG 质量，默认 60
    MaxWidth int `json:"maxWidth,omitempty"` // 默认 1280
}

type ScreenBegin struct {
    Width  int    `json:"width"`
    Height int    `json:"height"`
    State  string `json:"state"` // capturing / locked / no_session
}

type ScreenState struct {
    State string `json:"state"`
}
```

TDD 步骤同前几个 Phase——测试 JSON 序列化 → 实现 → 绿 → commit `feat(proto): screen session vocabulary`。

---

### Task 2: server — screen 端点

**Files:**
- Create: `server/internal/api/screen_handlers.go`
- Modify: `server/internal/api/router.go`
- Test: `server/internal/api/screen_handlers_test.go`

**Interfaces:**
- Consumes: T1 词汇；Phase 3 `startSession` 共享路径
- Produces:

```text
POST /api/nodes/{id}/screen {"fps"?:1,"quality"?:60,"maxWidth"?:1280} → 202
校验：fps ∈ [0,5]（0→1 默认）；quality ∈ [0,100]（0→60）；maxWidth ∈ [0,1920]（0→1280）
RBAC：requireMinRole("operator")
审计：screen.open / screen.close
```

Handler 模式与 shell 完全同构（共享 startSession），只是 params 校验和 action 名不同。

---

### Task 3: agent — Screen Handler（helper 拉起 + pipe 读取）

**Files:**
- Create: `agent/session/screen.go`、`agent/session/screen_test.go`

**Interfaces:**
- Consumes: T1 词汇；既有 Handler 接口；`execWriteTimeout`
- Produces:

```go
type Screen struct{ Log *slog.Logger; HelperPath string } // HelperPath 默认同目录 xnc-screen-helper.exe
func NewScreen(log *slog.Logger) *Screen
func (s *Screen) Handle(ctx context.Context, ws *websocket.Conn, sessionID string, params json.RawMessage)
```

Handle 核心流程：
1. 解析 ScreenParams，应用默认值
2. **探测用户会话状态**：`WTSGetActiveConsoleSessionId()` → 若 0xFFFFFFFF → `no_session`，发 SCREEN_BEGIN {state:"no_session"} 后关连接
3. 拉起 helper：`WTSQueryUserToken(sessionId)` → `CreateProcessAsUser` 启动 `xnc-screen-helper.exe --pipe xnc-screen-<sessionID> --fps N --quality N --max-width N`
4. 连接 named pipe `\\.\pipe\xnc-screen-<sessionID>`（client 端）
5. 发 SCREEN_BEGIN {width, height, state:"capturing"}（width/height 从 helper 首条 pipe 消息获取）
6. 循环读 pipe：JPEG 字节 → WS binary 帧；pipe 消息含状态变化 → WS text SCREEN_STATE
7. ctx done / WS close → 杀 helper 进程 + 关 pipe

pipe 协议（自定义二进制，简单帧）：
```
[1B 类型][4B 长度][payload]
类型：0x01=JPEG 帧（payload=JPEG 字节）
      0x02=状态（payload=ASCII state 字符串）
      0x03=尺寸（payload=8B: width int32 + height int32）
```

**锁屏检测**（简化实现）：helper 每次捕获前检查 `OpenInputDesktop()` 是否成功——失败 = locked。发 0x02 帧。

测试（Windows 集成，`//go:build windows`）：需要一个 mock helper（不依赖真实 GDI）：
- mock helper 写 fake JPEG 字节到 pipe → Screen Handler 推 WS binary 帧
- mock helper 发状态 no_session → SCREEN_BEGIN 含 no_session
- WS close → helper 进程被杀（检查进程退出）

---

### Task 4: agent — xnc-screen-helper.exe（GDI 捕获）

**Files:**
- Create: `agent/screen-helper/main.go`、`agent/screen-helper/capture_windows.go`、`agent/screen-helper/pipe_windows.go`、`agent/screen-helper/capture_other.go`

**Interfaces:**
- Consumes: Windows GDI + GDI+ API
- Produces: `xnc-screen-helper.exe` 可执行文件

Helper 是独立可执行文件（不是 agent 的子包——必须独立进程在用户会话中运行）：

```
用法：xnc-screen-helper.exe --pipe <name> --fps <N> --quality <N> --max-width <N>
```

capture_windows.go 核心逻辑：
```go
func captureScreen(maxWidth, quality int) ([]byte, int, int, error) {
    // 1. GetDC(0) 获取屏幕 DC
    // 2. GetSystemMetrics(SM_CXSCREEN/SM_CYSCREEN) 获取分辨率
    // 3. CreateCompatibleDC + CreateCompatibleBitmap
    // 4. BitBlt 拷贝屏幕到 bitmap
    // 5. 计算缩放比例（width > maxWidth 时等比缩小）
    // 6. GDI+ GdipCreateBitmapFromHBITMAP + GdipSaveImageToStream（JPEG 编码）
    // 7. 返回 JPEG 字节 + 实际 width/height
}
```

锁屏检测：
```go
func isLocked() bool {
    // OpenInputDesktop(0, false, DESKTOP_READOBJECTS) 失败 = 锁屏
    h, err := windows.OpenInputDesktop(0, false, windows.DESKTOP_READOBJECTS)
    if err != nil { return true }
    windows.CloseDesktop(h)
    return false
}
```

pipe_windows.go：named pipe 服务端（CreateNamedPipe + ConnectNamedPipe + 循环写帧）。

主循环：每 1/fps 秒检查锁屏 → 捕获 → 写 pipe 帧 → 检查 pipe 断开（客户端断开 → 退出）。

非 Windows（`//go:build !windows`）：main 输出错误退出。

**无单测**（GDI 需要真桌面）——验证通过 T3 的 mock helper 测试 + T8 E2E 真机验收。

---

### Task 5: agent/mockagent 注册

两行 Register：`engine.Register(proto.KindScreen, session.NewScreen(<logger>))`。

Helper 部署：agent 二进制同目录下放 `xnc-screen-helper.exe`。agent/screen-helper 的构建产物放 `bin/` 下，与 agent 一起部署。

---

### Task 6: CLI — xnc screen

**Files:**
- Create: `cli/cmd_screen.go`、`cli/cmd_screen_test.go`

```text
xnc screen <node> --snapshot out.jpg    # POST → WS → 收 1 帧 → 写文件 → exit 0
                                        # --json 返回 {node, width, height, state, size, durationMs}
xnc screen <node> --open                # POST → 生成本地 HTML（内嵌 WS URL + token）
                                        # → 打开默认浏览器 → 持续收帧显示
```

--snapshot 逻辑：
1. resolveNode → POST /screen {fps:1} → 202
2. 拨 WS → 等 SCREEN_BEGIN（拿 width/height/state）
3. state==capturing → 等第一帧 binary → 写文件
4. state!=capturing → 输出状态 + exit 0（不是错误）
5. --json：envelope {node, width, height, state, size, durationMs}

--open 逻辑：生成本地 HTML 文件（`<img>` 标签 + JS 定时替换 src 为 WS blob URL）→ `start` / `open` 打开。

---

### Task 7: Web UI — Screen Preview 面板

**Files:**
- Create: `web/src/pages/ScreenPreview.tsx`
- Modify: `web/src/pages/NodeDetail.tsx`（加 [Screen Preview] 按钮）、`web/src/main.tsx`（路由）

ScreenPreview.tsx 核心逻辑：
```tsx
// 1. POST /screen → 202 {websocketUrl}
// 2. WebSocket → binaryType = "arraybuffer"
// 3. ws.onmessage: text → SCREEN_BEGIN/SCREEN_STATE 更新状态显示
//                   binary → URL.createObjectURL(new Blob([data], {type:"image/jpeg"}))
//                            → 设置 <img src>
// 4. 定时清理 blob URL（防内存泄漏）
// 5. ws.onclose → 显示 "Preview ended"
// 6. 组件卸载 → ws.close()
```

UI：全屏面板（或对话框），顶部状态栏（node 名 + state 徽章），中间 `<img>` 自适应显示。

NodeDetail 加按钮：`[Screen Preview]` → Link to `/screen/{nodeId}`。

---

### Task 8: E2E — 三态验收

**Files:**
- Create: `scripts/e2e_phase6.sh`
- Modify: `Makefile`（e2e6 目标）

脚本流程：
1. Dev 栈 + mockagent 上线
2. `xnc screen MOCK-xxx --snapshot out.jpg` → 验证 JPEG 文件非空 + `--json` envelope 含 state/width/height
3. 验证 helper 无残留：`tasklist /FI "IMAGENAME eq xnc-screen-helper.exe"` → 空
4. viewer 权限测试（如有 viewer 用户配置）
5. 清理

真机验收（合并后手工）：TB16G7 三种状态——正常登录 / 锁屏 / 注销。

---

## 完成定义

```text
全模块测试绿 + 双 GOOS 编译
bash scripts/e2e_phase6.sh 全绿
agent/screen-helper 构建产出 xnc-screen-helper.exe（与 agent 同部署）
真机 TB16G7：
  正常桌面 → --snapshot 返回可查看 JPEG
  锁屏 → state=locked（非黑屏）
  注销 → state=no_session
  关闭后 tasklist 无 helper 进程残留
Web UI NodeDetail → [Screen Preview] → 1fps JPEG 帧显示
```
