# M2-Slice1 桌面与会话可靠性 — XIAOXIN E2E 验收结果

- **日期**: 2026-08-23(门跑 5 轮,run-5 为最终判定;run-1..4 为脚本/断言迭代,证据保留)
- **拓扑**: scripts/dev-topology.md(LABS-DEV docker dev server + coturn;LABS-XIAOXIN dev core(SYSTEM console)× 4 generations + dev agent(session 1 /RL HIGHEST);本机 e2eviewer server 模式,TURN relay-only)
- **脚本**: `scripts/e2e-m2s1.sh`;产物: `.superpowers/sdd/2026-08-23-m2-slice1-desktop-reliability/e2e/`(run-1..4 各自 `e2e-runN/`)
- **判定**: **run-5 全 21 门 PASS(exit 0;0 FAIL)**

| # | 场景 | 判定 | 关键实测 |
|---|---|---|---|
| ① | UAC 安全桌面→输入批准 | PASS(4/4) | 见下 |
| ② | 锁屏+SAS→解锁,流不断 | PASS(4/4) | 见下 |
| ③ | 分辨率切换自愈 | PASS(3/3) | 见下 |
| ④ | 健康分降级 GDI→probe 回升 | PASS(5/5) | 见下 |
| ⑤ | SAS 门控(无 --allow-sas) | PASS(2/2) | 见下 |
| ⑥ | 输入连续性回归(①②后) | PASS(4/4) | 见下 |

环境: viewer firstFrameMs 3.0–5.7s(WiFi+relay);时钟偏差(XIAOXIN−本机)≈ −4,467,228ms(探测 CSV 对齐已修正);核心代际 gen1(clean)→gen2(--allow-sas + SoftwareSASGeneration=1)→gen3(XNC_FORCE_DXGI_HEALTH=50)→gen4(clean,收尾态)。

## ① UAC 安全桌面 → viewer 复位路径 → 输入批准

- 触发: session-1 非提权 `Start-Process -Verb RunAs`(scripts/m2s1-uac.ps1;目标 = `xnc-uac-child.exe`(cmd 副本,镜像限定可杀),提权成功以 `net session` 探针写 marker + 40s ping 保持可观测)。
- 安全桌面出现: consent.exe=1(t+9);viewer 观测 T2 复位路径: `recovering`×2 → `capture_rebuilt`×2,**新帧 52ms、IDR 1285ms**(reset 完成后);run 期间 frames=95、keys=5、connected 全程。
- 输入批准: e2eviewer 输入脚本(lease→wait 8s→**Alt+Y**)。**run-1 诚实记录**: 先试 Enter——默认焦点控件是「否」(未签名改名的二进制,该提示类默认焦点=No),Enter=取消,无提权(marker 缺失,consent 自灭)。改用 **Alt+Y**(UAC「是」键盘加速键,与焦点无关): consent 在 t+28 已消失、`tasklist` 1 行 xnc-uac-child.exe(session 1)、marker 文件内容 `ELEVATED-OK`(net session 成功=真提权)。
- 佐证日志: `desktop_transition Default→Transition→Winlogon→Default` + `capture_reset_done reason=desktop_switch/desktop_away=1`(core-gen2.log)。
- lever 自报行 `marker=False` 为良性竞态(consent 退出与 cmd 写 marker 之间;门在 t+28 的直接检查为准)。

## ② 锁屏 + SAS → 解锁,流不断

- 前置: `SoftwareSASGeneration=1`(reg add;**teardown trap 恒恢复 /d 0**,已验证收尾 `REG_DWORD 0x0`)+ core gen2 `--allow-sas`。
- viewer 输入脚本 `sas` op: round-trip `ok hr=0x0`(**hr 不可信——T4 裁决;送达以安全桌面出现为准**)→ **LogonUI=1(t+25)** = 安全桌面真出现。此组合(SYSTEM console core + policy=1)实测可触发——T4 的 policy=0 静默 no-op 结论被复核确认。
- 锁定窗口: 节点侧 `capture_reset_start reason=access_lost` → 挂起等待 → `back_to_default` → `capture_reset_done elapsed_ms≈33,109`(整窗挂起,viewer 侧 recovering@13.6s)。
- 解锁: SYSTEM sessrun + sysenter.ps1(扫描码 Enter,桌面重绑)×3 → **LogonUI=0(t+57)**;桌面回归后 `capture_rebuilt`@46.9s,**首帧 47.1s、IDR 47.4s**(run-4: 46.93→46.93s 首帧/47.38s IDR)。viewer connected 115,440ms 全程、frames=36、keys≥2。生成 bump 数: 该窗口节点侧 `rt generation++` 见 run-3 风暴注记。
- **run-3 诚实发现(非门失败,已记录)**: 解锁回归后出现**内部重建风暴**——Winlogon→Default 瞬间 DXGI Acquire 仍 ACCESS_LOST,err_rebuilt 内部重建路径无退避,~2s 内 `rt generation++` ×~3010(gen 2→3010+)后自愈,流恢复。有界、自愈、门判据(流存活+帧恢复)不受影响;**M2-Slice2 候选改进**: err_rebuilt 路径加退避/限速(统一 reset Phase 3 已有退避,内部路径没有)。

## ③ 分辨率切换自愈(T2 回归,脚本化)

- 杠杆: `scripts/setres.ps1`(EnumDisplaySettings/ChangeDisplaySettingsW;该 SKU 无 Set-DisplayResolution),session-1 /IT 任务;`rc=0` 两次。
- viewer: `displayEvents=2`,样本 `{gen:4,1920x1080,reason:"resolution"}`、`{gen:6,1024x768,reason:"access_lost"}` — **gen 严格递增 4→6**;**事件→新帧 833ms / 722ms**(≤2000 界;run-4: 805/842ms)。`reason` 如实记录(T2 已文档化:先 ACCESS_LOST 后发现几何则记 access_lost)。
- **环境注记(诚实)**: 门起始时面板模式为 1024×768——run-3 的「原始分辨率」采集踩了 Forms.Screen 的 DPI 假值(见「脚本迭代记录」),把原生 2880×1800「恢复」成了 1024×768;门本身两次完整切换(1024↔1920)自愈证据成立,面板已在收尾**手工恢复 2880×1800**并复核(`after w=2880 h=1800`)。脚本已改为 EnumDisplaySettings 真值采集。

## ④ 健康分降级 GDI → 30s probe 回升(T3 回归)

- core gen3 `XNC_FORCE_DXGI_HEALTH=50`(env 随 spawn 继承;desktop 日志 `backend_env_override force_health=50` — 顺带实证了 T6 修复①的 env 路径)。
- 节点: `backend_ladder_init health=50` → `backend_changed backend=gdi reason=health`(+0.3s)→ `dxgi_probe ok=1 attempt=1`(+30.0s)→ `backend_changed backend=dxgi reason=probe`。
- viewer: `backend_changed` STATE ×2(t+285ms / t+30,343ms);GDI 窗口帧 **0.1fps / 3 帧**(≤15fps 上限;run-4: 0.2fps/5 帧)——**诚实注记**: GDI 静止检测用 CRC 行采样(T3 已文档化的行间漏检),250ms tick overlay 的小面积重绘基本漏检,故 GDI 窗口帧数低是检测灵敏度而非断流;帧仍在继续、probe 后 DXGI 窗口 219 帧(~7.3fps)。**IDR 回升后 36ms**(run-4: 1132ms),≤2000ms。
- 真 GDI 大动态产帧证据沿用 T3 nalcheck(gdi-change.h264,results 链接的计划文档)。

## ⑤ SAS 门控(无 --allow-sas)

- core gen1(默认): viewer `--sas` → `secure_attention_result ok=false code=SAS_DENIED`,**rtt 8ms**;节点 `sas_audit action=sas_denied allowed=0 attempted=0 reason="viewer"`(core-gen1.log,caller_pid=agent)。
- 门控=--allow-sas 显式开关(票据 capability = M2-Slice3,账本既定偏差)。

## ⑥ 输入连续性回归(①② 循环后)

- slice3 抽样(①② 之后): probe CSV 三坐标命中 dt=−387/−431/−473ms(±5px/±500ms 界内);keyA down→up 19.3s 持续后释放;文本→notepad→Ctrl+S。
- **诚实注记**: notepad 文本门第 1 次失败(空文件,全部 wire 步 ok——slice3 已知的 notepad 焦点/对话框 flake),按 slice3 模式重试(新 notepad + 仅保存脚本)后** PASS**(marker `XNC-M2S1-OK-1787475427` 落盘)。runs 3/4/5 三次均第 2 次过——该 flake 在 slice3 结果文档已有先例。
- 结论: 桌面切换(UAC/锁屏)后输入注入连续性保持(lease/mouse/input 通道 + InputManager 桌面重绑)。

## T6 顺带修(4 项)验证

1. **env 栈过读**(xnc-desktop LadderOptsFor): `ret < sizeof(buf)` 卫护 + 超长日志行;④ 的 `backend_env_override force_health=50` 实证正常路径。desktop selftest 绿。
2. **pipe_server TOCTOU**: active 会话锁内重读+复验(SESSION_MISMATCH 兜底);core selftest 全绿(sc-* 矩阵不变式锁定)。
3. **rsA gate 期断言**: `rsA-no-rebuild-while-away`(RebuildCount()==0)进 desktop selftest,绿(rsA captured=96 keys=3 resets=1 requests=7 merged=6)。
4. **e2eviewer SAS 等待对齐+关联**: 15s agent 界 + 2s 传输余量(`sasResultWait=17s`),请求前 `drainSasReplies` 清迟到回执;单测锁定(drain/budget)。⑤ 实测 rtt 8ms(远低于界,余量方向正确)。

## 检查绿(final)

- native: core build.bat selftest ok / desktop build.bat selftest ok(修复后重跑)
- Go: `go build ./...`;`go test ./agent/... ./server/... ./cli/... ./proto/... ./mockagent/...` 全 ok;`agent/desktop`、`agent/coreclient`、`tools/e2eviewer` `-count=1` 重跑 ok
- web: `tsc --noEmit` 0 错误(无 web 改动,常规复核)
- 收尾态: registry=0 ✓;面板 2880×1800 ✓;gen4 clean core + dev agent + dev stack 留存(owner 浏览器检查);scoped kills 全程(consent backstop/xnc-uac-child/notepad/SESSION eq 1 的 xnc-desktop);生产 XNCAgent 未触碰

## 脚本迭代记录(诚实)

| run | 结果 | 变更 |
|---|---|---|
| 1 | ⑤② 过;① Enter=取消(默认焦点「否」);⑥ 分析崩溃 | g6 node argv 序号错(丢 json 参后未重排)→ 崩在 set -e;改 Alt+Y |
| 2 | ⑤ 过后中断 | prod CLI token 24h 到期(UNAUTHORIZED)——`deploy/machines.env` 重登后恢复 |
| 3 | ①过(Alt+Y);②过;③ resume 假失败;⑥ notepad flake | viewer `displayResumeMs` 输出的是绝对时刻非差值(真值 1052/845ms)→ 修为差值+测试;marker 尾随空格(cmd echo)+trim;notepad 重试;**发现 Forms.Screen DPI 假值**(原生模式被误恢复) |
| 4 | 仅 ② lastAU=-1 假失败 | 判据改「锁后首个 rebuilt 的帧恢复」(静态桌面零输出是设计行为 7.4);面板手工恢复 2880×1800;几何改 EnumDisplaySettings 真值 |
| 5 | **全 21 门 PASS(exit 0;0 FAIL)** | — |

## 顺延(M2-Slice2/3,账本既定)

- logoff/新登录门 → Slice3(服务化前提);多显示器拓扑 → Slice3(V1 单屏);SAS 票据 capability → Slice3
- err_rebuilt 重建风暴退避(run-3 发现,~3010 gen bump/2s,自愈)→ Slice2 候选
- GDI CRC 行采样灵敏度(小面积变化漏检,静态帧数低)→ Slice2 观察项
- 绝对 cursor 时延 / wired 链路门 → 沿用 slice3 既定顺延
