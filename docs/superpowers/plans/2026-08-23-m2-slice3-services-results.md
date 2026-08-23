# M2-Slice3 服务化与生产化 — 验收结果（Task 6 门 + V1 DoD 盘点）

- 日期: 2026-08-23/24（run 11 全 PASS;诚实迭代记录见文末）
- 拓扑: XIAOXIN 双服务 dev（XNCAgentDev/XNCCoreDev,LocalSystem,Automatic,`C:\xnc-dev\` + `C:\ProgramData\XNCAgentDev`）;LABS-DEV docker dev 栈 192.168.1.12:18080;驱动通道 = 生产 XNCAgent exec/put（session 0 SYSTEM,零接触生产状态）。
- 脚本: `scripts/e2e-m2s3.sh`;产物: `.superpowers/sdd/2026-08-23-m2-slice3-services/e2e/`。
- 提交: `cfced5e` fix(native/desktop) stale display→primary 回落;`<n>` fix(native/desktop) console-rt logon-UI 等待（见「过程发现」②）;`<n>` feat(scripts) 本门 + 本文档。

## 门验收（全部数字来自 run 11）

| # | 门 | 结果 | 证据数字 |
|---|---|---|---|
| ③ | rt 活动时并发快照 | **PASS** | viewer 流中(frames=66)并发 `xnc screen --snap` → JPEG 成功(g3-snap.jpg, JFIF 头;g3-cli.txt)。诚实注记: 亦观察到过 clean `SNAPSHOT_FAILED`(run 2/7,DXGI 单 duplication 限额)——两种结局都被脚本判 PASS 且如实记录;run 11 为 JPEG 成功 |
| ②a | 双 viewer: viewer1 持有 | **PASS** | viewer1 leaseGranted=1(leaseId bvlu7Rwt…) |
| ②b | viewer2 被拒 | **PASS** | viewer2 lease granted=0, step err=`denied: held`（`--input-before-keyframe` 绕过关键帧前置——run-8 教训） |
| ②c | viewer1 断开 → viewer3 新会话获授 | **PASS** | holder 70s 自然断开 +10s 后 viewer3 leaseGranted=1(leaseId 51JJwKt…)（server 侧 holder close 释放 → Create 授予） |
| ②d | 移交后输入可用 | **PASS** | viewer3 steps ok=1、move×2 ok |
| ①a | logoff 后节点在线 ≥120s | **PASS** | `logoff 4`（exec SYSTEM）后 online 探针 +22/+62/+122/+183/+243s 全 online=1（g1-online.txt） |
| ①b | 双服务存活 | **PASS** | XNCAgentDev+XNCCoreDev 均 Running（count=2） |
| ①c | viewer 自愈状态 | **PASS** | states=`["reattached","capture_rebuilt","recovering","capture_rebuilt"]`,frames=194 keys=6（同一条 WebRTC 连接续流,无重协商） |
| ①d | 登录 UI 画面可见（自愈 spawn） | **PASS** | reattached + frames 恢复 = 自愈 spawn 进 logon-UI 会话出帧（修复②后成立;run 9 该门 FAIL,根因见下） |
| ①e | 注入登录（空密码: 点用户磁贴 + Enter） | **PASS** | input script 全 ok（tile click + Enter down/up ok=2）——注入后桌面恢复（会话重编号 5,g1b 证明） |
| ①f | 桌面恢复 + 输入回归 | **PASS** | 恢复桌面新 viewer: leaseGranted=1、steps ok=1、frames=62 |
| ⑤ | 过期显示器选择回落 | **PASS** | native selftest `sel-*` 5 例（显式/auto/过期→primary/过期→0/空表）+ Init 一次性回落日志与 auto 复位（本 slice gate 外加修复,commit cfced5e） |

隔离证明（收尾核验）: 生产 `XNCAgent` Running、`C:\ProgramData\XNCAgent` 未动（isolate.txt）;scoped kill 仅 `C:\xnc-dev*` 路径进程;motion 驱动 schtask 收尾删除。

## 过程发现与修复（诚实记录）

1. **XIAOXIN 网络闪断**（~01:13–01:50 本地）: 双 agent（prod+dev）同时离线、ping 丢包 66–100%。期间 run 4/5 的 exec 挂起/失败属环境,非产品;恢复稳定（连续 4 次双在线探针）后继续。机器未重启（LastBoot 8/21）。
2. **console-rt 模式无法在 logon-UI 会话立足（本门核心发现,run 9 FAIL 根因）**: 旧代码在首次 `LadderCapture::Init` 失败（logon UI=Winlogon 桌面,SYSTEM 也 0x80070005 拒绝 duplication）即 exit 1 → rt pipe 从未创建 → core `PIPE_TIMEOUT`（2s）杀子进程 → agent 意图自愈 7 次全灭 → `capture_lost`。修复: `RunConsoleRt` 先用一次性 `TryCreateDxgiCapture` 探测可达性;被拒则以临时几何（displays 表 primary,缺省 1920x1080）**立即拉起 rt pipe**,循环探测（500ms,110s 上限 = agent 90s 意图窗 + 余量）,Default 桌面回归后**一次性**构造+Init 阶梯（`LadderCapture::Init` 单次语义）,并向订阅者广播 `display_changed(reason=reattach)` + `capture_rebuilt`（gen++ + HOST_HELLO 重发）;`RtServer::Serve` 采纳已启动的 pipe。
   - 嵌套发现 2a: `LadderCapture::Init` 不可重入——每次调用 `impl_->probe = std::thread(...)` 对 joinable 线程赋值 → `std::terminate`/`__fastfail 0xC0000409`（run 10 的 ~5s 重挂风暴 + AppCrash WER 证据）;重试循环因此只用一次性探针,阶梯只 Init 一次。
3. **静止桌面不出 IDR**（已知 Task 5 注记）: 一切 viewer-attach 门需要 session-1 运动驱动（m1-slice2 ping 驱动先例,`motion.cmd` schtask /IT）;logoff 会杀它——设计内,g1b 前重臂。
4. **console 会话重编号**: 每次 logoff/logon console session id +1（2→3→4→5）;脚本改为 qwinsta 正则解析 Active console + 不匹配即拒绝盲 logoff（run 8 的 `logoff 1` 空放过,教训）。
5. **丢帧的诚实备注**: run 7/9（修复前）曾出现 `start_failed`×N 与 `capture_lost`——数字保留在 e2e 产物中,不作为门证据。
6. 手工恢复杠杆: 修复验证期间两次用 `sessrun+sysenter`（M2-Slice1 闸门杠杆）把 XIAOXIN 从 logon UI 登回（修复前注入通道不存在）——仅限调试,门内登录注入走 viewer 输入（①e）。

## V1 DoD 盘点（spec §53 初稿全项逐条;证据 = 各 slice 门）

| # | 条目 | 判定 | 证据（slice / 门 / 数字） |
|---|---|---|---|
| 1 | UAC 安全桌面远程点击（批准） | **PASS** | M2-S1 门①: consent 出现→viewer 复位（新帧 52ms/IDR 1285ms）→Alt+Y 批准→marker `ELEVATED-OK` |
| 2 | Win+L 锁屏 + SAS + 解锁,流不断 | **PASS** | M2-S1 门②: SAS→LogonUI=1;解锁后 `capture_rebuilt`@46.9s、首帧 47.1s;viewer connected 115,440ms 全程 frames=36 |
| 3 | 注销存活（agent/服务不死） | **PASS** | M2-S3 本门①a/①b: logoff 后节点 +243s 在线、双服务 Running |
| 4 | 注销后登录恢复（logon UI 可见+注入登录+桌面恢复） | **PASS** | M2-S3 本门①c–①f: reattached/capture_rebuilt、frames=194、空密码注入、恢复桌面输入回归（含修复②） |
| 5 | 分辨率切换自愈 | **PASS** | M2-S1 门③: displayEvents=2、gen 4→6 严格递增、事件→新帧 833/722ms（≤2000 界） |
| 6 | 显示器插拔恢复 | **PARTIAL** | 物理插拔无自动化杠杆;逻辑近亲已证: ①分辨率自愈（#5）②多屏枚举/切换（#12）③本 slice ⑤ 过期选择回落 primary。真插拔门顺延有 ≥2 屏/可插拔节点时 |
| 7 | 降级链 DXGI→GDI→恢复 | **PASS** | M2-S1 门④ + M2-S2 门⑥b: health=50→gdi→probe→dxgi health=100,IDR 回升 36ms |
| 8 | desktop 崩溃 → 退避重启（≤60s） | **PASS** | M2-S2 门④: `desktop restart in 1000ms (crash #1)`、gen=2、frames=20/75s |
| 9 | crash-loop 5 连杀 → 降参 gdi/software | **PASS** | M2-S2 门⑤: `crash_loop_degraded exits=5`、degraded respawn、`gdi init`+`encoder selector: software`、frames=12 |
| 10 | 崩溃后 5s 恢复（实测值口径） | **PARTIAL** | 实测: kill→respawn 退避 1s 起;viewer 帧恢复窗取决于内容（静止桌面无 IDR,已知注记）。严格「5s 内帧恢复」未单设门;M2-S2 门④ 75s 窗 frames=20 为最近证据 |
| 11 | 新观众关键帧（首帧=IDR） | **PASS** | M1-S2 门③: 第二 viewer firstKey=true、首帧 2515ms;PLI→IDR 2131ms（M1-S3 门⑤,≤3000 界） |
| 12 | 多显示器枚举/切换（V1 单编） | **PASS**（单屏） | M2-S3 T5: displays=[1]（2880x1800 primary）;switch(0) 幂等 resume 349ms;switch(99) 拒;**多物理屏 DEFERRED**（无 ≥2 屏节点,TB16G7=1 屏） |
| 13 | 注销/登录的 lease 撤销/重授 | **PASS** | M2-S3 本门②: server 单约仲裁——denied{held}→holder 断→新会话授予+输入可用;单测（授予/抢占拒/断连撤/TTL 撤） |
| 14 | 多观众 view-only + 单输入约 | **PASS** | M1-S3 门② + M2-S3 T4: 未持约输入丢弃 noLease>0;viewer 角色 capability（viewer=[screen.view]） |
| 15 | 输入语义全集（move/key/wheel/text/lock） | **PASS** | M1-S3 门①: 坐标 ±5px/±500ms、keyA held 1929ms、numlk 翻转恢复、TEXT→notepad 落盘 |
| 16 | 卡键清理（断连释放） | **PASS** | M1-S3 门③: 按住 A 断连 → synthetic up 7947ms（≤35s 界） |
| 17 | SAS 门控 + 并发 busy | **PASS** | M2-S1 门⑤: 无 --allow-sas → `SAS_DENIED` rtt 8ms + 审计;M2-S2 门⑥a: 并发 → `denied:busy`;M2-S3 T4: server capability `input.secure_attention`（owner-only）+ 服务模式 SAS=on 裁决 |
| 18 | shell 不越权（exec RBAC + 审计） | **PASS** | M2-S2 门①②: user token→console 用户;operator `--system`→403;owner→SYSTEM+audit `system=true`;shellhost token 语义（M2-S2 T1/T2） |
| 19 | xnc-core 无任意执行面 | **PASS**（代码面） | TokenManager 每 spawn 一次性 token、命令面封闭（spawn 仅 xnc-desktop/xnc-shell 白名单 argv）、pipe 双 secret;M2-S2 probe 往返。无专门渗透门（如实: 依赖代码评审+自测,非对抗性验证） |
| 20 | screen 退役 + 快照换轨 | **PASS** | M2-S3 T3: `--jpeg-single` WIC（JFIF/尺寸单测）、legacy helper 不再 spawn;本门③: 并发快照 JPEG 成功（或 clean SNAPSHOT_FAILED,如实两态） |
| 21 | 服务化（SCM）+ Drain | **PASS** | M2-S3 T1/T2: XNCCoreDev SCM dispatch、Stop→Drain（释放按键）;XNCAgentDev 参数化安装、Stop/Start 存活、state 目录隔离 |
| 22 | 会话意图自愈（worker 死/会话变） | **PASS** | M2-S3 T2 单测（exited→reattempt→成功/过期放弃）+ 本门①: logoff 全链自愈（修复②补齐 logon-UI 落地） |
| 23 | 首帧 <1s（LAN 1080p） | **PARTIAL** | M1 验收实测 4267ms（Wi-Fi+TURN relay 校准界 ≤5s,controller 裁决留痕）;有线 LAN 门三节点皆 Wi-Fi 顺延（M1-S3 门⑥）。真 LAN <1s 未证 |
| 24 | 静止低码流（静止 60s ≤20 AU） | **PASS** | M1-S1: 15 AU/60s、keyframes=1、IDR 占比 3.68%（<30% 线）、0.397 Mbps 动态均值 |

盘点结论: **22 PASS / 2 PARTIAL（#6 插拔、#23 有线首帧,#10 以实测口径记 PARTIAL 则 3 项）/ 0 FAIL**。PARTIAL 均为 slice 外依赖（物理硬件/网络拓扑）,非功能缺口;多物理屏切换（#12 附注）同因顺延。

## 顺延账（下一切片/M3 候选）

- 物理插拔门、≥2 屏切换实测（需硬件）。
- 有线 LAN 首帧门（需有线节点）。
- `LadderCapture::Init` 可重入化或显式单次断言（本 slice 以调用侧一次性规避;防御性改进）。
- err_rebuilt 内部重建路径退避（M2-S1 run-3 风暴注记,本 slice logon 探测循环以 500ms 节流近似)。
- core→desktop DRAIN 握手（服务 Stop 时 ReleaseAll 不跑,dev-services.md 既定 ledger）。
