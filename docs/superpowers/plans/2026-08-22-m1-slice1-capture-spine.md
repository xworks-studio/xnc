# M1-Slice1 捕获脊柱 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 LABS-XIAOXIN 上跑通诊断闭环:xnc-core 以正确令牌把 xnc-desktop 拉进 console 会话,DXGI 采集 → FrameCache → MF 软编 → `--diag-dump` 落盘 H.264,取回后用 nalcheck 证明「无关键帧风暴、静止近零输出、变化持续编码」。

**Architecture:** 三个 native 二进制共享 `native/common`(M0 的 frame/handshake/log 下移);xnc-desktop 走 CPU 路径(DDA BGRA 读回 → BT.601 NV12 → CMSH264EncoderMFT),GPU 直编留 M4;诊断模式进程内直写文件,不经 pipe(agent↔desktop 管道 M1-Slice3 接)。

**Tech Stack:** C++17 / MSVC /MT(SDK + bcrypt,无新依赖);Go 1.26(tools/nalcheck);部署经现有 `xnc put/exec`(exec 为 SYSTEM@session0,PS5.1 语法,`&&` 不可用)。

**Spec:** docs/superpowers/specs/2026-08-22-xnc-agent-refactor-spec.md(§4.2 TokenManager、§7 xnc-desktop、§9 IPC、§7.10 编码契约、§20 M1)

## Global Constraints

- E2 教训为硬契约(spec §7.10):`ForceNextIdr` 一次性消费(输入提交即清除,禁止以「未见关键帧输出」为由重发);编码 fps 与采集节奏对齐(媒体类型帧率=目标 fps,墙钟 PTS);IDR 前必带 SPS/PPS、统一 4 字节起始码、丢弃 AUD
- 帧状态机(spec §7.5):`INIT → WAIT_BASE_FRAME → HAVE_BASE → INCREMENTAL`;首有效帧全量 CopyResource,禁止只应用 dirty rect;WAIT_TIMEOUT 不编码不发包;静止自然 0fps
- 令牌(spec §4.2):xnc-desktop 令牌 = SYSTEM 主令牌复制 + `SetTokenInformation(TokenSessionId=N)` + `CreateProcessAsUserW(lpDesktop="winsta0\\default")`;命令行无敏感字段
- 不修改 legacy:`agent/screen-helper/**`、`agent/session/screen*.go` 只读参考,一行不改;不碰 proto/ipc 语义
- native 构建全部 build.bat + vcvars64(`/MT /std:c++17 /utf-8 /W3`);bin/ 产物不入库
- clean-room:参考 = Windows SDK 文档 + 本仓库已有代码(dda.c、screen-helper Go 实现)
- 调试目标节点 LABS-XIAOXIN(console 会话 1 = LABS,Active);LABS-DEV 侧只构建与验证,不运行采集(本机在 RDP 中,会话扰动)
- 每个 C++ 任务的可测部分以 `--selftest` 扩展或 diag 输出断言承载;任务验收命令必须真实可跑

## 参考实现索引(执行者按需读,不复制粘贴式移植——按契约重写)

| 参考 | 位置 | 用途 |
|---|---|---|
| DXGI COM 全流程(C) | agent/screen-helper/dda/dda.c + dda.h | D3D11 设备/输出枚举/DuplicateOutput/AcquireNextFrame/光标/ACCESS_LOST 重建 |
| MF H.264 编码(Go syscall) | agent/screen-helper/encode_windows.go | MFT 初始化参数、ICodecAPI、码流整形(vclNALUs/extractNALs 语义)、输出循环 |
| 采集主循环/GOP 记账(Go) | agent/screen-helper/capture_windows.go | 帧节奏、静止自愈、时间基 GOP——**其 force-key 逻辑是 E2 bug 本体,只取教训不取逻辑** |
| BGRA→NV12 BT.601(Go) | agent/screen-helper/pixel_windows.go | 转换公式与行距处理 |
| M0 native 代码 | native/core/* | frame/handshake/log/pipe 复用来源 |

---

### Task 1: native/common 提取(共享 frame/handshake/log)

**Files:**
- Create: `native/common/`(移动 frame.h/.cpp、handshake.h/.cpp、log.h;pipe_server 留在 core)
- Modify: `native/core/build.bat`(源路径指 ../common)、`native/core/pipe_server.cpp` 等 include 路径、`native/core/xnc-core.cpp`
- Test: `native/core/build.bat selftest && bin/xnc-core-selftest.exe`(不变绿不算完)

**Interfaces(不变):** `xnc::Frame/EncodeFrame/DecodeFrame/ReadFrame/WriteFrame`、handshake 全套、`XNC_LOG`——namespace 保持 `xnc`,仅物理位置移动;Task 3/5 以 `#include "common/frame.h"` 方式引用(相对路径 `../common/`)。

- [ ] **Step 1** 移动文件 + 更新两处 include/build.bat;`native/core/build.bat` 与 `native/core/build.bat selftest` 均编译通过
- [ ] **Step 2** `bin/xnc-core-selftest.exe` → `selftest ok`;`bin\xnc-core.exe --console --smoke-secret <hex>` 手动 smoke:日志正常、Ctrl+C 退出 0
- [ ] **Step 3** Commit: `refactor(native): extract shared frame/handshake/log to native/common`

### Task 2: xnc-desktop 骨架 + `--diag-dump` 模式(无采集)

**Files:**
- Create: `native/desktop/xnc-desktop.cpp`(main/arg 解析)、`native/desktop/desktop_selftest.cpp`、`native/desktop/build.bat`
- Create: `native/desktop/capture.h`(Task 3 实现):`class ICapture { virtual bool Acquire(FrameBlob&) = 0; ... }`、`struct FrameBlob { std::vector<uint8_t> bgra; uint32_t w, h; uint64_t mono_us; }`

**Interfaces:**
- `xnc-desktop.exe [--console-diag --duration <sec> --out <file.h264> --fps <n>] | --selftest`
- `--console-diag` 无捕获器时:循环写结构化日志(心跳+`diag_no_capture`),到 duration 退出 0;退出码:参数错 2、内部错 1
- selftest 断言:arg 解析(默认 fps 30、duration 10)、FrameBlob 布局

- [ ] **Step 1** selftest(RED)→ **Step 2** 实现 → `native/desktop/build.bat && bin/xnc-desktop.exe --selftest` → `selftest ok`
- [ ] **Step 3** `bin/xnc-desktop.exe --console-diag --duration 2 --out %TEMP%\t.h264` → 日志两拍心跳、退出 0
- [ ] **Step 4** Commit: `feat(native/desktop): process skeleton with console-diag mode`

### Task 3: DXGI 采集模块(DXgiBackend)

**Files:**
- Create: `native/desktop/dxgi_capture.h/.cpp`(实现 Task 2 的 ICapture)
- Modify: `native/desktop/xnc-desktop.cpp`(接線:console-diag 构造 DxgiCapture)
- Test: `native/desktop/desktop_selftest.cpp` 扩展

**Interfaces:**
- `std::unique_ptr<ICapture> TryCreateDxgiCapture(std::string* err)`;`Acquire(FrameBlob&)` 语义:成功 true;超时(静止)`err_timeout` 静默复返;ACCESS_LOST/DEVICE_REMOVED 内部重建一次,连续失败返回 false+err
- 实现要点(参照 dda.c,按 spec §7.4):`DuplicateOutput1(B8G8R8A8)` 失败降 `DuplicateOutput`;首帧/重建后第一帧 **CopyResource→staging→Map 全量读回**;`LastPresentTime==0`/WAIT_TIMEOUT = 无变化;ReleaseFrame 立即归还;输出紧凑 BGRA(FrameBlob)
- 诊断接線:`--console-diag` 每秒日志 `captured=N timeouts=M rebuilds=K w=W h=H`;首帧把 BGRA 头 64 字节哈希(非全黑判定:采样 256 点不全等)

- [ ] **Step 1** selftest 扩展:FrameBlob 尺寸/哈希工具单测(合成数据)→ GREEN
- [ ] **Step 2** DxgiCapture 实现 + 接線;**LABS-DEV 上只编译不跑采集**;部署 XIAOXIN 跑(见 Task 7 部署器,此任务先手动:`xnc put` 两 exe → `xnc exec LABS-XIAOXIN "bin\xnc-desktop.exe --console-diag --duration 10 --out C:\xnc-diag\cap.h264"` → exec 读回日志),验收:captured≥1、w/h=真实分辨率、非全黑
- [ ] **Step 3** Commit: `feat(native/desktop): DXGI capture backend (CPU readback)`

### Task 4: MF 软件编码器(MfSoftEncoder,合成输入自测)

**Files:**
- Create: `native/desktop/mf_encoder.h/.cpp`、`native/desktop/nv12.h/.cpp`(BT.601 转换)
- Test: `desktop_selftest.cpp` 扩展

**Interfaces:**
```cpp
class MfSoftEncoder {
 public:
  bool Init(uint32_t w, uint32_t h, uint32_t fps, uint32_t bitrate_bps, std::string* err);
  // 输入紧凑 BGRA,内部转 NV12 后编码;返回 0..N 个 Annex-B AU
  bool Encode(const uint8_t* bgra, size_t len, std::vector<std::vector<uint8_t>>& aus, std::string* err);
  void ForceNextIdr(const char* reason);   // 一次性:下一次 Encode 输入即清除(硬契约)
  bool LastWasKey() const; const std::vector<uint8_t>& SpsPps() const;
  void Drain(std::vector<std::vector<uint8_t>>& aus);  // 冷启动缓冲排空,不重复 force-key
};
```
- selftest(合成彩条 NV12/BGRA 输入,无需桌面):(a) 编码 60 帧 → ≥1 输出、首输出含 SPS/PPS+IDR;(b) **force-key 契约**:冷启动缓冲期对连续 5 帧只调用一次 ForceNextIdr + Drain 排空 → 断言输出中 IDR 恰 1 个(防 E2 风暴的回归测试);(c) 稳态第 30 帧再 ForceNextIdr → 下一输出为 IDR;(d) LastWasKey/SpsPps 一致
- Init 参数对齐 spec §7.10:B 帧 0、低延迟、无 lookahead(可设则设,失败容忍并记日志);媒体类型 fps=调用方 fps

- [ ] **Step 1** selftest(RED:类不存在)→ **Step 2** 实现(参照 encode_windows.go 的 MF 流程与 vclNALUs 语义,C++ 重写;ICodecAPI GOP/ForceKey 同款调用)→ **Step 3** `bin/xnc-desktop.exe --selftest` 绿
- [ ] **Step 4** Commit: `feat(native/desktop): MF software H.264 encoder with one-shot force-key contract`

### Task 5: FrameCache + 主循环串联(diag-dump 产出真码流)

**Files:**
- Create: `native/desktop/frame_cache.h`(状态机+记账)、`native/desktop/pipeline.h/.cpp`
- Modify: `xnc-desktop.cpp`、`desktop_selftest.cpp`

**Interfaces:**
- `Pipeline::Run(ICapture&, MfSoftEncoder&, FILE* out, uint32_t duration_s)`:Acquire→(全量/增量语义记状态机)→Encode→码流整形(SPS/PPS 前置+4B 起始码+去 AUD)→fwrite;每秒日志 `captured/encoded/keyframes/timeouts/dropped`;结束写 `stats.json`(同字段+时长+分辨率)
- FrameCache 状态机单测(合成 capture:首帧全量、后续 timeout 不编码、重建后回 WAIT_BASE_FRAME 且 ForceNextIdr)
- 整链验收(XIAOXIN):静止桌面 60s → delta 帧数 ≤2、无 IDR 风暴;`xnc exec` 制造变化(如 `ping -n 30 127.0.0.11` 刷屏)→ 帧持续输出

- [ ] **Step 1** 状态机 selftest(RED)→ **Step 2** 实现 → selftest GREEN → **Step 3** XIAOXIN 双场景实测(手动 xnc put/exec/get,产物 ≥1 帧 h264 + stats.json 回传)→ **Step 4** Commit: `feat(native/desktop): frame pipeline with base-frame state machine and diag dump`

### Task 6: xnc-core TokenManager + StartCapture(最小集)

**Files:**
- Create: `native/core/token_manager.h/.cpp`、`native/core/spawn.h/.cpp`
- Modify: `native/core/xnc-core.cpp`(`--diag-spawn <exe> <args...>` console 直通)、`native/core/pipe_server.cpp`(新 MsgType `MSG_START_CAPTURE=0x0100`、`MSG_STOP_CAPTURE=0x0101`,payload M1-Slice3 换 protobuf,现为 `[wts_session u32][ascii exe-rel-path][ascii args, \x1f 分隔]` 定长头简单布局;StartCaptureResponse `[pid u32][exit_semantics]`)
- Test: `native/core/selftest.cpp` 扩展(token 布局断言);真实验证走 XIAOXIN

**Interfaces:**
- `TokenManager::SessionSystemToken(DWORD session_id, HANDLE* out)`:DuplicateSelf(SYSTEM 主令牌)→ SetTokenInformation(TokenSessionId)→ 返回;`SpawnInSession(HANDLE token, const wchar_t* exe, const wchar_t* cmdline, STARTUPINFO 含 winsta0\default)` → pid
- 校验:exe 相对路径仅允许同目录 `xnc-desktop.exe`(白名单,spec §6.4 精神);session_id 必须 = `WTSGetActiveConsoleSessionId()`(锁屏合法)
- `--diag-spawn`:console 模式下直接 SpawnInSession 拉起 desktop 并等待、转发其退出码(不经 RPC,便于远程 exec 一条命令完成端到端)

- [ ] **Step 1** selftest(token/参数校验路径)→ **Step 2** 实现 → **Step 3** XIAOXIN 验证:`xnc put` core+desktop → `xnc exec LABS-XIAOXIN "...\xnc-core.exe --console --diag-spawn xnc-desktop.exe --console-diag --duration 15 --out C:\xnc-diag\e2e.h264"` → 验证 desktop 进程确实落在 session 1(exec `query session` + `tasklist /FI "IMAGENAME eq xnc-desktop.exe" /V` 看 SESSIONNAME 列)→ h264 产出
- [ ] **Step 4** Commit: `feat(native/core): session token manager and diag-spawn for xnc-desktop`

### Task 7: nalcheck 工具 + 自动化部署脚本 + XIAOXIN 验收门

**Files:**
- Create: `tools/nalcheck/main.go`(Annex-B 解析:总帧数/IDR 数与序号/SPS+PPS 完整性/字节数/平均码率;`--max-idr-ratio` 断言) + `nalcheck_test.go`(合成流含 3 IDR → 计数正确)
- Create: `scripts/diag-deploy.sh`(LABS-DEV 侧:native 两 build.bat → `xnc put` XIAOXIN `C:\xnc-diag\` → exec 静止 60s 场景 → 变化场景(内联 `ping` 刷屏 30s)→ `xnc get` 回 h264+stats.json → 本机 `go run tools/nalcheck` 两遍)

**Interfaces:** `nalcheck <file.h264> [--json] [--assert-idr-interval ≥N 帧]`(退出码 0/1)

- [ ] **Step 1** nalcheck TDD(合成流:手写 SPS/PPS/IDR/P NAL 序列)→ GREEN → Commit: `feat(tools): nalcheck Annex-B stream analyzer`
- [ ] **Step 2** diag-deploy.sh + 全流程在 XIAOXIN 真跑:**验收门 = ①静止 60s:帧 ≤5、无 IDR>2;②变化 30s:帧 ≥60、IDR ≤5、码率 0.5–8Mbps;③stats.json 与 nalcheck 双证**;结果写 `docs/superpowers/plans/…-results.md`
- [ ] **Step 3** Commit: `feat(scripts): XIAOXIN diag deploy loop + M1-Slice1 acceptance evidence`

## Self-Review 记录
- Spec 覆盖:M1 的「DXGI 采集/FrameCache/软编先行」= Task 3/4/5;「xnc-core StartCapture + TokenManager 最小集」= Task 6;硬契约(force-key/首帧全量/静止 0fps)入 Global Constraints + 各 selftest;M1 其余(Pion/TURN/web/coturn/agent 侧)= M1-Slice2/3,不在此计划
- 交叉引用:Task 2 定 ICapture/FrameBlob 供 3/5;Task 4 定 MfSoftEncoder 供 5;Task 6 消费 spawn 白名单;nalcheck 消费 5 的产物——签名已对齐
- 部署注意:exec 是 PS5.1(`&&` 不可用);远程路径统一 `C:\xnc-diag\`(脚本内创建);XIAOXIN console=LABS@session1(2026-08-22 实测)
