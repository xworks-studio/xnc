# 采集兜底加速 + 版本单一来源 + 日志布局治理 设计规范

> 2026-09-15。来源:Live Desktop 全链路实测(点击→首帧冷启动 31.2s,其中 28.5s
> 为 DXGI 无帧 30s 兜底计时器)+ 五进制版本号审计(仅 agent 正确)+ 日志位置
> 审计(三份日志违规住安装目录、`logs\` 规范目录成孤儿、零轮转)。
> 本文与 AGENTS.md 冲突处以本文为准(实施后同步回写 AGENTS.md,见实施方案 T6)。

## 0. 背景与实测基线

2026-09-15 在 LABS-XIAOXIN(agent 0.10.25,盒盖屏休眠)实测:

| 场景 | click→首帧 | 分解 |
|---|---|---|
| 冷启动·屏休眠 | **31.2s** | 控制面+网络+节点启动仅 2.7s;其余 28.5s = DXGI 0 帧→30s 兜底计时器→GDI 产帧 300ms |
| 冷启动·推断健康屏 | ~3.0-3.2s | 绑定 2.7s(含 2s hello 盲轮询 ~1.5s)+ IDR 链 ~0.3-0.5s |
| warm(host 已注册) | **807ms** | POST 74ms + 传输 83ms + join→IDR 650ms(含 QSV 编码器重建 226ms、IDR 870KB 令牌桶整流) |

根因链(`src/host/src/capture.rs`):

- 新 viewer 加入触发 `force_frame()`,但 force 产帧路径要求
  `!last_raw.is_empty()`(capture.rs:244)——屏休眠下 DXGI 从未交付过像素,
  `last_raw` 为空,force 永远产不出帧,**viewer 的首 IDR 无限等待**;
- 唯一恢复路径是 WouldBlock 连续 30s 的 GDI 兜底计时器(capture.rs:157-171)。
  30s 是刻意值:防"健康静止桌面误降级"(GDI 全屏 BitBlt 慢,CAPTUREBLT 把
  物理光标烤进帧),**不能全局改成 5s**;
- 结论:必须把"有产帧需求但 DXGI 给不出"与"稳态静止"两个条件拆开。

版本审计结论:`build.ps1` 仅对 xnc-agent 注入 ldflags(`-X
xnc/agent/machineinfo.Version`);CLI 是 `const cliVersion`(main.go:20,
ldflags 无法覆盖),host 用 `env!("CARGO_PKG_VERSION")`(Cargo.toml 恒
0.1.0),core/shellhost 无版本概念。发版校验(build.ps1:127-131)只查 agent。

日志审计结论:spec(2026-09-03 安装器设计 §3)规定运行日志归
`C:\ProgramData\XNC\logs\`;实际 agent-service.log 在 StateDir 根、
xnc-host.log / xnc-shell.log / xnc-core-service.log 在安装目录
(`C:\Program Files\XNC\`),`logs\` 目录无任何代码创建(xnc.iss 无 [Dirs]),
全部日志零轮转(XIAOXIN host 日志 8.9MB)。另:安装目录 xnc.exe.old2 残留、
StateDir 根散落会话临时脚本 `xnc-<uuid>.ps1`。

## 1. 采集启动健壮性(首连接 GDI 兜底 5s)

### 1.1 需求语义

"首次连接 fallback GDI 时间改为 5s" 的精确化:首连接场景下 DXGI 兜底
判定必须远快于现状的 30s(以确定性**首帧探针**实现,亚秒级);"5s"作为
**会话中途产帧需求得不到满足**时的兜底时限;稳态静止(无产帧需求)的
30s 误降级保护语义**保持不变**。

### 1.2 设计(三层判定 + 回探)

1. **首帧探针(主机制,亚秒级)**:DXGI duplication 创建后的**首次**
   AcquireNextFrame 必定返回当前桌面帧(静止桌面亦然——所有 duplication
   采集器依赖此语义取基础帧;实测的死屏机器恰是首帧都拿不到)。
   `ScreenCapturer` 构造时记 `first_frame_deadline: Instant`(now+1.5s);
   `next()` WouldBlock 分支判定 `frames_captured == 0 && deadline 已过
   && !is_gdi()` → 立即 `set_gdi()`,日志 `"DXGI delivered no initial
   frame within 1.5s, falling back to GDI"`。探针与 QUIC 连 relay
  (实测 48ms)、编码器探测(266ms)完全并行,在 viewer 绑定(~2.7s)
   之前完成判定。附带收益:探针成功则 `last_raw` 非空,force 产帧盲区
   (capture.rs:244)自动消除。
2. **5s 产帧需求兜底(用户指定值)**:`force_frame()`(:124-126,调用点:
   viewer 0→N、frameLoss IDR 请求——后者现状缺失,见 6)置
   `frame_demand_since`;成功产出 Frame 清零。WouldBlock 分支判定
   `demand 挂起 && elapsed > 5s && !is_gdi()` → `set_gdi()`,日志
   `"DXGI unresponsive for 5s with pending frame demand, falling back
   to GDI"`。覆盖探针之后的窗口:会话中途显示器入睡 / 纯 IDR 请求在
   静止桌面饿死等场景。
3. **30s 稳态计时器(不变)**:capture.rs:157-171 原语义原值保留
   (健康静止桌面不误降级:GDI 全屏 BitBlt 慢,CAPTUREBLT 光标烤帧)。
4. **GDI 驻留期 DXGI 回探(新增)**:因上述 1/2 降级进入 GDI 时记
   `dxgi_retry_at`;处于 GDI 且每 60s(或收到输入注入事件时)重建
   duplication 试产一帧,成功即回切 DXGI(复用现有 Reinit 路径)——
   把 GDI 的画质/性能劣化限制在"DXGI 真不可用"的时段,显示器苏醒后
   自动恢复。
5. **force 盲区标注**:capture.rs:244 的 force 分支在 `last_raw` 为空时
   不产出——保持 `force_next` 置位语义(供 demand 计时判定),加注释;
   GDI 首个 BitBlt 填充 `last_raw` 后下一 tick 即可产出。
6. **纯 IDR 请求补 force**:frameLoss → `idr_requested` 路径(无 viewer
   计数变化的场景,如中途加入者的解码恢复)在静止桌面下无帧可编码、
   IDR 饿死——main.rs 收 frameLoss 时一并 `force_frame()`(现状仅
   viewer 0→N 触发)。
7. **回切与重建**:不因 30s 计时器主动回切(现状);1/2 降级的回切由
   条 4 承担;capturer 重建(拓扑变化/会话新建)自然回 DXGI 重试。

### 1.3 预期指标(以 §0 基线为准)

| 场景 | 现状 | 目标 | 判定机制 |
|---|---|---|---|
| 冷启动·屏休眠(首连接) | 31.2s | **≤3.5s**(绑定 2.7s + IDR,探针并行完成) | 探针 |
| 会话中途·viewer 加入时屏已睡 | 无限等待(run1 实测 6min) | **≤ join+5.5s** | 5s demand |
| 冷启动·健康屏 | ~3.0s | 不回归(±0.2s;探针成功无扰动) | — |
| warm | 807ms | 不回归 | — |
| 稳态静止(有 viewer,无变化) | 30s 语义不降级 | **行为不变** | 30s |
| GDI 驻留·显示器苏醒 | 永久 GDI | ≤60s 回切 DXGI | 回探 |

风险与防线:首帧探针理论上存在"健康但首帧慢于 1.5s"的误降级(未观测
到;duplication 首帧语义明确)——即使发生,条 4 回探在 60s 内恢复,且
5s/30s 兜底仍在;三条路径全部收敛到 `set_gdi()` 单点,回归面集中。

## 2. 版本单一来源(五进制)

### 2.1 注入矩阵

唯一版本源 = `build.ps1 -Version`(installer.json 的 version 必须等于 agent
自报,硬性契约 1 不变;扩展为五进制一致)。

| 组件 | 现状 | 改法 |
|---|---|---|
| xnc-agent | ✓ ldflags `machineinfo.Version` | 不变 |
| xnc(CLI) | `const cliVersion`(main.go:20) | 改 `var cliVersion = "0.0.0-dev"`;build.ps1 补 `-ldflags "-X main.cliVersion=$Version"`;显示与 UA(client.go:45)自动跟随 |
| xnc-shell(shellhost) | 无版本 | 新增 `var shellVersion = "0.0.0-dev"`(main.go)+ 启动日志行 `shellhost starting version=<v> pid=<p>` + `-X main.shellVersion=$Version` |
| xnc-host(Rust) | `env!("CARGO_PKG_VERSION")`(main.rs:204)= 0.1.0 | 改 `option_env!("XNC_HOST_VERSION").unwrap_or("0.0.0-dev")`;**必须**配套 `src/host/build.rs` 输出 `cargo:rerun-if-env-changed=XNC_HOST_VERSION`(cargo 指纹不含环境变量,否则版本变更时会命中陈旧缓存把旧版本号烤进产物);build.ps1 cargo 步骤前 `$env:XNC_HOST_VERSION = $Version`(作用域内设置,构建后还原);支持 `--version` 参数早退打印 |
| xnc-core(C++) | 无版本 | common 新增 `version.h`:`#ifndef XNC_VERSION #define XNC_VERSION "0.0.0-dev" #endif`;build.bat 编译参数加 `/DXNC_VERSION="..."`(取环境变量 `XNC_VERSION`,build.ps1 设置);service.cpp 启动行与 `--selftest` 输出带 version;支持 `--version` 早退 |

### 2.2 发版校验扩展(build.ps1:127-131)

从"仅 agent --version"扩展为五进制各自执行版本接口并比对(全部输出且仅
输出版本号,`Trim()` 后 == `-Version`):

- `xnc.exe --version` / `xnc-agent.exe --version`(已有)
- `xnc-shell.exe --version`(新增 flag)
- `xnc-host.exe --version`(新增 flag,main.rs 参数早退)
- `xnc-core.exe --version`(新增参数分支,service main 入口早退)

任一不一致 → 构建 fail(与现有 throw 同语义)。CI publish.yml 恢复时沿用
同一组接口。

### 2.3 兼容性

- relay 门禁 `clientSupportsRelayWS`(exec_handlers.go:45-52,门槛 ≥0.3.1):
  修复后新 CLI 报真实版本(≥0.10.x)通过;存量装了 0.10.25 但 UA 冻在
  0.3.1 的 CLI 仍 ≥0.3.1 通过——**无断代**。门禁值本方案不动。
- 服务端节点列表只展示 agent 版本,不受影响;审计日志里 CLI 版本自修复版
  起变为真实值(旧记录 0.3.x 系解释为冻住的常量,不回填)。

## 3. 日志布局与治理

### 3.1 布局裁定(收口 AGENTS.md 与 2026-09-03 spec 的分歧)

```
C:\ProgramData\XNC\
  binding.json  identity.json  core-secret.hex  update-pending.json   (不变)
  installer-cache\   staging\                                    (不变)
  tmp\                        # 新增:会话临时脚本(xnc-<sessionId>.ps1 等)
  logs\                       # 全部运行日志唯一归属
    agent-service.log  [.1 .2 .3]
    xnc-core-service.log[.1 .2 .3]
    xnc-host.log       [.1 .2 .3]
    xnc-shell.log      [.1 .2 .3]
  installer.log               # 豁免:Inno 低频写入,不轮转不迁移
  watchdog.ps1                # 契约文件,不动
```

- **agent-service.log 迁移到 logs\**:agent 启动时若旧路径存在且新路径不
  存在 → rename 迁移(复用 migrateLegacyStateDir 的缓冲回放模式,
  main_windows.go:29-34 已有先例)。AGENTS.md §7 诊断模板路径同步更新。
- **core 侧路径解析**:core(含其为 host/shell 拼的 `--log-file`)新增
  `ResolveLogDir()`:`SHGetKnownFolderPath(FOLDERID_ProgramData)` + `\XNC\logs`,
  启动时 `CreateDirectory`(幂等);创建失败(极端 ACL)回落 exe 目录并
  打 WARN——不因日志路径失败而拒绝服务。替换点:
  - service.cpp:76-86(stderr 重开 `<exe dir>\xnc-core-service.log`)
  - pipe_server.cpp:471(host `--log-file`)
  - pipe_server.cpp:724-725(shell `--log-file`)
- **logs\ 创建双保险**:agent 启动 `MkdirAll(stateDir\logs)` + xnc.iss
  `[Dirs]` 追加(安装器已有 `XNCStateDir()` code 函数可引用)。
- **旧文件处置**:安装目录的旧 host/shell/core 日志**原地留档不迁移**
  (升级不删、卸载 Purge 统一清),避免跨组件迁移耦合;StateDir 根的旧
  agent-service.log 由 agent 迁移(见上)。

### 3.2 轮转规范(零新依赖,四组件统一语义)

- 参数:单文件上限 **8MB**,保留 **3 份**滚动(`.1` 最新 → `.3` 最旧),
  rename 链式(`.2→.3 删,.1→.2,.log→.1`),轮转在写侧进行。
- 实现:
  - **agent**(main_windows.go:38 的文件 logger):slog 的 io.Writer 包
    一层计数字节,每 256KB 检查一次大小超限即轮转(写日志路径低频,
    检查粒度无性能顾虑)。
  - **shellhost**(main.go:213 logFileWriter 懒打开):同上小包装。
  - **core**(service.cpp stderr 重开处):打开时查大小超限轮转 +
    log.h 计数每 1000 行复查(core 日志低频,启动轮转已覆盖绝大多数)。
  - **host**(main.rs:471-498 日志初始化):tracing 文件 writer 外包
    同语义的按大小轮转(host 日志量最大,1s stats 行是主体;轮转时
      tracing 的行缓冲需 flush 后 rename——实现按 tracing MakeWriter
    自定义,实施方案中细化)。
- installer.log、unins 日志:豁免。

### 3.3 卫生清理

- **会话临时脚本**:exec Script 落盘路径(exec.go:285-292)从
  `StateDir\xnc-<id>.ps1` 改为 `StateDir\tmp\xnc-<id>.ps1`(MkdirAll tmp);
  会话结束/超时删除(现有清理路径保留),残留被限制在 tmp\ 内;agent 启动
  时清空 tmp\ 下早于 24h 的 `xnc-*.ps1`(崩溃残留兜底)。
- **xnc.exe.old2**:cleanupOldCLI(cmd_update.go:124-127)扩展清扫
  `.old` 与 `.old2`(现行代码只造/清 `.old`,old2 为历史版本遗物);
  发布说明里提示一次存量机可手工删。

## 4. 明确不做(本方案范围外,另立后续)

- 显示器电源事件订阅(`PowerSettingRegisterNotification` display state)
  作为 GDI 即时切换 HINT:事件驱动最优雅,但多接一套 Windows API;本方案
  的首帧探针 + 回探已把判定压到亚秒级/恢复压到 60s 内,事件订阅留作后续
  增强。
- Live Desktop 启动延迟的结构性优化:relay 回捞等待 viewer / hello 前密
  后疏 / host 空闲保活窗口 / worker 提前创建(实测收益分别 ~1.5s / 3.1s→
  0.8s / 数十 ms)。见实测报告(2026-09-15 会话),独立 spec。
- IDD 虚拟显示器默认启用(屏休眠的根治路径之一,牵涉驱动分发策略)。
- 编码器 force-IDR 重建优化(hwcodec 无强制关键帧 API 的替代方案)。
- Cloudflare /api/* 挑战规则(运维侧,已另行处理)。

## 5. 验收

1. XIAOXIN(盒盖屏休眠)真机冷启动:click→首帧 **≤3.5s**(§1.3,首帧
   探针路径);会话中途 viewer 加入时屏已睡 → join+5.5s 内出帧;warm
   807ms 与健康屏冷启动 ~3s 不回归;稳态静止观看 5 分钟不降级 GDI
  (日志无探针/5s 触发记录,30s 语义行为不变);GDI 驻留下唤醒显示器
   ≤60s 回切 DXGI(日志见 DXGI retry 成功)。
2. `build.ps1 -Version 0.10.26` 构建后:五进制 `--version` 全部输出
   0.10.26;篡改任一注入缺失时构建 fail。
3. 全新安装机:`logs\`、`tmp\` 存在;五份运行日志落 logs\;agent-service.log
   旧位置文件被迁移;卸载 `/PURGEDATA=true` 全清。
4. 轮转:host 日志写满 8MB 后产生 .1,累计 4 份封顶,最老删除。
5. 会话脚本:exec script 会话后 tmp\ 内无残留(正常路径);agent 重启后
   24h 前残留被清。
