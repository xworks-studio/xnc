# M1-Slice3 输入闭环 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** viewer 可控制 XIAOXIN 桌面:键鼠/文本注入端到端(input/mouse DataChannel → pipe → SendInput)、简化 control lease、光标位置通道、卡键清理,配 session1 探针自动化验收;顺延三门(首帧<2s/20fps/PLI≤2s)尝试有线环境。

**Architecture:** 输入 = viewer(input 通道可靠/mouse 通道不可靠,固定二进制)→ agent 会话(lease 仲裁 + 预校验)→ pipe 0x0108 → xnc-desktop InputManager(SendInput + 扫描码/Unicode + lock 同步 + 卡键 janitor + dwExtraInfo 标记 + 桌面重绑重试)。光标 = desktop GetCursorInfo 8ms 轮询 → pipe 0x0109 → agent cursor 通道(不可靠)→ viewer 叠加圆点。lease = **agent 侧简化仲裁**(首请求者得、WS 断开释放、30s 无输入释放;server 侧仲裁 = M2,spec §11.1 偏差已裁决)。

**Tech Stack:** 既有栈不变;输入消息沿用固定二进制(protobuf codegen 顺延 M2——M0 计划注「M1 接入」按此偏差记录);web 端 KeyboardEvent.code→扫描码映射表(内嵌生成)。

**Spec:** docs/superpowers/specs/2026-08-22-xnc-agent-refactor-spec.md §11(输入)、§7.8(光标)、§7.3(桌面重绑)、§20 M1;承接:Slice2 results「Slice3 承接」+ ledger carries。

## Global Constraints

- 注入规则(spec §11.5):全部 SendInput;固定 dwExtraInfo 标记 0x584E4301;注入线程绑定 input desktop,失败→重绑→重试一次→上报 INPUT_DESKTOP_MISMATCH;MOVE_RELATIVE delta 钳 ±10000;坐标 = HOST_HELLO w/h 归一→虚拟桌面绝对(MOUSEEVENTF_ABSOLUTE|VIRTUALDESK)
- 键盘(spec §11.4):物理键 KEYEVENTF_SCANCODE(+extended)、文本 KEYEVENTF_UNICODE(代理对);修饰键=显式 down/up 序列,不做临时按压;LockState 同步(事件 vs GetKeyState 不一致先注入 CapsLock/NumLock);卡键 janitor:10s 扫描,>30s 未释放强制 KeyUp,退出时全释放
- 输入消息(pipe 0x0108,LE):`[u32 sub_id][u64 seq][u8 type][payload]`;type:1=MOVE `[s32 x][s32 y][u16 buttons]`、2=BUTTON `[u8 btn][u8 down]`、3=WHEEL `[s32 dx][s32 dy][u8 trackpad]`、4=KEY `[u16 scan][u8 down][u8 extended]`、5=TEXT `[u16 len][utf16le]`、6=LOCK `[u8 caps][u8 num]`。光标 0x0109(event):`[s32 x][s32 y][u8 visible]`
- lease(简化,agent 侧):control 通道 JSON `{"type":"lease_request"}`→`lease_granted{leaseId}`/`lease_denied{reason}`/`lease_revoked{reason}`;持有者断连或 30s 无输入→撤销;非持有者输入→desktop 侧丢弃+计数(不惩罚断连)
- agent 预校验(§11.7):seq 单调/lease 有效/按钮位合法/消息 ≤2KiB/move ≤500Hz(合并在 agent 侧)/总 ≤1000eps;不合法丢弃+计数
- e2eviewer keyframe-retry 承接:pending PLI 超时(1.5s)重发(frames>0 亦触发)
- 有线门:先探测 TB16G7/YOGAP7G11 网络类型(`xnc exec ... Get-NetAdapter`);有有线节点则三门在其上跑;全 WiFi 则记录分段证据(ICE+allocation/IDR 传输/组装)并维持顺延
- 不碰生产路径/legacy;C++ selftest 与全 Go 测试持续绿;凭据不入日志

---

### Task 1: xnc-desktop InputManager + 输入/光标消息

**Files:**
- Create: `native/desktop/input_manager.h/.cpp`(SendInput 封装 + 键钮状态表 + janitor + lock 同步)、`native/desktop/cursor_manager.h/.cpp`(GetCursorInfo 8ms 轮询线程,变化才发)
- Modify: `rt_pipe_server.h/.cpp`(0x0108 输入入站分发、0x0109 光标出站)、`xnc-desktop.cpp`、`desktop_selftest.cpp`、`build.bat`(user32 已有则无需)

**Interfaces:** `InputManager::Inject(const InputMsg&)`(0x0108 解码后);`InputManager::ReleaseAll()`(drain/断连调用);`CursorManager::Start(pipe sink)/Stop`。selftest(无桌面部分):消息解码表驱动、按钮位/坐标钳制、键状态表 janitor 逻辑(时间注入)、lock diff 计算、dwExtraInfo 常量;SendInput 真路径留 T6 探针验证。

### Task 2: BuildChildCommandLine must-fix(独立小任务)

**Files:** `native/core/spawn.cpp`(+拒绝内嵌引号与尾反斜杠,错误信息带字符位置)、`native/core/selftest.cpp`(矩阵:含 `"`、`\"`、尾 `\`、空参、合法路径)。Commit: `fix(native/core): reject quotes and trailing backslash in child args (slice3 must-fix)`。

### Task 3: agent 输入路径 + lease + 预校验

**Files:**
- Create: `agent/desktop/input.go`(两 DataChannel 建立:input 可靠/mouse 不可靠 + cursor 不可靠出站;固定二进制编解码;预校验;lease 注册表)
- Modify: `agent/desktop/session.go`(通道创建、lease 消息接入 control 词汇、cursor 0x0109→cursor 通道转发、断连 ReleaseAll 语义经 desktop pipe 广播 DRAIN?——简化:断连时对持有 lease 者发 DETACH 前 ReleaseAll 消息)

**Interfaces(供 T4/T5):** control 词汇 + `lease_request/granted/denied/revoked`;mouse 通道 payload=0x0108 MOVE 变体`[u64 seq][s32 x][s32 y][u16 buttons]`(无 sub_id,agent 填);input 通道同理其余 type;cursor 通道 `[s32 x][s32 y][u8 visible]`。loopback 测试扩展:双 PC + 输入回环断言(fake host 记录收到的 Inject 序列)。

### Task 4: web DesktopLive 输入采集 + 光标叠加

**Files:**
- Create: `web/src/pages/desktop/keymap.ts`(KeyboardEvent.code→scan/extended 表,从 USB HID/Win 扫描码公开对照生成,~110 键)+ `cursor.ts`(叠加圆点)
- Modify: `DesktopLive.tsx`(pointer 事件→归一坐标 MOVE/BUTTON、wheel、keydown/keyup→scan、文本框→TEXT、lock 状态同步、lease 按钮/状态、断连释放提示)

**验收:** 手动 + tsc/lint 绿;owner 目检合并入 T6 报告。

### Task 5: e2eviewer 输入自动化 + XIAOXIN 探针

**Files:**
- Modify: `tools/e2eviewer/main.go`(`--input-script <json>`:序列化 [move/keyup/keydown/text/wait/lease];keyframe-retry pending-PLI 1.5s 超时)
- Create: `scripts/input-probe.ps1`(session1 运行:GetCursorPos 100ms 采样 + GetAsyncKeyState 标记键 + 写 CSV 日志)、`scripts/notepad-type-test.ps1`(session1 开 notepad,等保存信号文件?——改为探针一体化:探针自建 TextBox 窗口?PS 无窗体——用 notepad:探针只记录光标/键态;打字验证 = notepad 已由脚本打开,viewer 输入文本+Ctrl+S+文件名,`xnc get` 文件比对)

### Task 6: XIAOXIN E2E 验收门

**Files:** `scripts/e2e-slice3.sh`(部署 dev 栈 + input-probe 启动 + 场景驱动);results 文档。

**验收门:** ①注入生效:move 序列→probe CSV 坐标轨迹匹配(±5px);TEXT→notepad 保存文件内容匹配;WHEEL/KEY 探针键态翻转 ②lease:第二 viewer 无 lease 输入被拒(agent 计数断言),请求后获得并可移交 ③卡键清理:viewer 按住键断连→30s 内 probe 键态回弹 ④光标通道:probe 光标移动→viewer cursor 事件时延 <200ms(相对 pipe 时间戳) ⑤keyframe-retry:mid-stream PLI 丢失场景重试生效 ⑥有线门尝试(探测三节点网型,有有线则跑三门,否则分段证据+维持顺延)。全数字入 results。

## Self-Review 记录
- spec §11 全链落地(除 map/translate 模式=DD-08 维持);lease 为裁决简化(server 侧=M2);protobuf codegen 顺延 M2(偏差记录);UAC/锁屏输入属 M2(DesktopSupervisor)
- T2 独立先行(20 行);T1/T3 契约靠 0x0108 布局钉死;T5 探针是 ⑥ 前置
