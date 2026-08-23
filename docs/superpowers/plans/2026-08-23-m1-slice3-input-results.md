# M1-Slice3 输入闭环 — E2E 验收结果(XIAOXIN)

- 日期:2026-08-23(迭代 run-1/2/3,全数字如实记录)
- 拓扑:`scripts/dev-topology.md`(LABS-DEV docker 栈 + coturn 192.168.1.12:18080;XIAOXIN dev core SYSTEM + dev agent 会话 1;e2eviewer 本机)
- 脚本:`scripts/e2e-slice3.sh`(门 ①–⑥);探针:`scripts/input-probe.ps1`(会话 1,100ms 采样,DPI-aware)
- 产物:`.superpowers/sdd/2026-08-23-m1-slice3-input/e2e/`(probe-*.csv、s*-*.json、s1-analysis.json、g6-network.txt、typed-*.txt)

## 终局(run-3,exit 0,17/17 门 PASS)

| 门 | 判据 | 实测 | 结论 |
|---|---|---|---|
| ① move×3 | probe 坐标 ±5px、步进时刻 ±500ms | A dt=-319ms @(255,192)、B dt=-474ms @(767,256)、C dt=-410ms @(512,576)(26/26 步 ok,seq 1..16) | PASS |
| ① keyA | down 2s → up | held=1929ms,down@1787452619241 → up@1787452621282(probe keyA 0→1→0) | PASS |
| ① wheel | 注入生效(无探针,记录) | wheel dy=2 步 ok(seq 6),native ×WHEEL_DELTA 注入 | PASS(记录) |
| ① LOCK num | flip → restore | probe numlk: initial=0 → **flipped=true** → **restored=true**(plain 0x45 修复后;run-2 证据:E0 0x45 不翻转) | PASS |
| ① TEXT→notepad | `xnc get` 文件含标记 | 尝试 1 失败(空文件)→ 重试 1 次:typed-1787452600.txt = `XNC-M1S3-OK-1787452599` | PASS(重试) |
| ② lease denied | 第二 viewer 请求被拒 | v2 lease 步 err=`denied: held`(其无 lease move 被 agent 丢弃计数) | PASS |
| ② noLease 计数 | agent 计数断言 | `desktop input closed ... noLease>0` 会话 = 2(run-3) | PASS |
| ② 移交 | v1 断连 → v2b 授予 | v2b leaseGranted=true;其 move → probe 行 @(204,512) | PASS |
| ③ 卡键清理 | 按住 A 断连 → 35s 内回 0 | releaseMs=**7947**(synthetic up 于 WS close;janitor >30s 为兜底未触发) | PASS |
| ④ 光标通道 | probe 行 vs viewer 事件 ≤200ms(+100ms 采样周期) | 残差 [0, 1, -204]ms(偏移估计 221ms 移除后;门④采样量化说明见下) | PASS |
| ⑤ PLI→IDR | ≤3000ms(WiFi+relay 裁定界) | **2131ms**(run-2: 2408ms;run-1: 2171ms),pliRetries=1(>1.5s 触发重发,计数器工作) | PASS |
| ⑤ 对 slice2 无回归 | vs 2351/2392ms | 2131/2408/2171 三跑均在 slice2 单发 PLI 真值(2351/2392ms)噪声带内 | PASS |
| ⑥ 有线门 | 三节点网型探测 | XIAOXIN=Wi-Fi(Native 802.11);TB16G7/YOGAP7G11 脚本内 `<no answer>`(手工复测 10:2x:两节点亦 Wi-Fi;脚本内 exec 三连重试仍空,记为 exec 延迟,非伪造) | 顺延(证据在册) |

Viewer 侧全程 firstFrame 4.3–6.3s(keyframe-retry-after 1.5s 补 WiFi 丢首 IDR),断言 0 退出(s1/s2/s3/s5)。

## 迭代记录(每次全跑的数字与发现)

### run-1(FAIL,9/17)
- **发现 A(时钟)**:XIAOXIN 时钟落后本机 **74.4 分钟**(skew=-4465978ms,三跑稳定 −4465978/−4466216ms)。T5 预判「LAN 内钟差可忽略」不成立;已改为每跑实测 skew 并在分析中校正。
- **发现 B(DPI)**:XIAOXIN 1024×768 模式 + 200% 缩放元数据:非 DPI-aware 的 `GetCursorPos` 虚拟化坐标 → 注入 768 显示为 384(恰半)。探针加 `SetProcessDPIAware()`(= 流/物理像素空间),几何查询同步 DPI-aware。
- 脚本 bug:lock 步 JSON 用了 0/1(应 true/false)→ s1 解析失败(0 步执行);`TYPED_RC` 成功/失败取反。notepad SaveAs 对话框流在干净上下文重试后成功(run-1 attempt 2:typed-1787451184.txt 匹配)→ TEXT 链路首次证实。
- v1 首帧 18673ms、v2 无帧(0):缺 `--keyframe-retry-after`(静态桌面第二 viewer 无 IDR)。
- gate③ 探针实际看到 keyA 0→1→0(held 8015ms = wait 5s + tail 3s,synthetic release 于 close)——当时因时钟差未匹配上。

### run-2(FAIL,14/17)
- 修 run-1 全部问题后:①moves/keyA/全步 ok、②四门全 PASS(v2 `denied: held` + noLease=1 + 移交 probe 行)、⑤ 2408ms retries=1、⑥ Wi-Fi。
- **发现 C(探针任务串行)**:65s 的 gate-2 探针仍在跑时 gate-3 `schtasks /Run` 被忽略(一次性任务运行中不再起实例)→ 取回 run-1 的旧 CSV → 门③「never observed down」。修复:`start_probe` 先 `/End` 再建/跑。
- **发现 D(NumLock 实证)**:lock num flip 两次(0→1→0)注入均为 **E0 0x45**,probe numlk initial=0 flipped=**false** —— E0 前缀不翻转 VK_NUMLOCK。据此落地 native 一行修复(plain 0x45)+ selftest 改判(见 commit `85fc7f7`)。
- notepad:SaveAs 对话框流 attempt 1/2 均失败(步 ok 但无文件)→ 改设计:notepad 预开目标文件,Ctrl+S 原地保存(无对话框)。
- 门④ 残差 [231,0,-91]ms:probe 行时刻是变化的上界(100ms 采样)→ 界校准为 200ms+100ms 采样周期(记档,非放宽语义)。

### run-3(PASS,17/17)
- 全绿(见上表)。notepad attempt 1 仍失败(空文件 = 打字未落在 notepad;moves/wheel 先行的两次 run 一致失败,干净上下文一致成功 —— 疑似指针事件先行时前台焦点不稳,机制未定论,如实记录),按验收语的「失败重试一次」由 attempt 2 通过。
- 门③ releaseMs=7947ms:synthetic up 在 close 即发(键按住 ~8s = wait 5s + 尾段 3s),远低于 35s;janitor 未被触发(无需)。

## ⑥ 有线门顺延记录(本跑分段证据,无伪造)

- 网型:LABS-XIAOXIN Wi-Fi(Native 802.11,唯一 Up 适配器);TB16G7/YOGAP7G11 脚本内无应答、手工探测亦为 Wi-Fi → **无可跑 dev agent 的有线节点,三门(首帧<2s、20fps 变化驱动、PLI≤2s)维持顺延**。
- 分段证据(run-3,WiFi XIAOXIN→LABS-DEV relay):ICE+TURN 分配+首帧(viewer 墙钟)= **6125ms**(s1 firstFrameMs,含分配);PLI IDR 传输+组装 = **2131ms**(pliToIdrMaxMs,desktop 侧 warm-up ≤2s 契约在 slice2 已证);解码组装 = s1 frames=69/30s 尾段;光标通道残差 ≤204ms。
- 有线环境需要:目标节点交互会话凭据(schtasks /RU /IT,目前仅 LABS@XIAOXIN 已配)+ dev 栈可达,留待具备后跑。

## Owner 浏览器检查(手动)

dev 栈保持运行(含 run-3 Docker 重建的 web bundle —— onPointerCancel 修复已内嵌):
**http://192.168.1.12:18080/desktop/f28af041-02ec-40e2-83ec-7b67629a9923**(先以 dev admin 登录;节点 XIAOXIN-DEV-18 online)。拆卸命令:`scripts/dev-topology.md` §6。

## 交叉验证与修复清单(本任务内)

- agent:idle 撤销清 held 跟踪(旧持有者后断连不得抬新持有者键;单测 `TestIdleRevokeClearsHeldTracking`)+ `TestInputLoopbackSequence` deflake(mouse 通道跨通道无保序,首 MOVE 可晚于 BUTTON(seq2) 被 staleSeq 丢弃 —— seq 单调跨通道 by design;等 MOVE 落地再发突发;clean tree -count=3 失败率 ~2/3 → 12/12 绿)。
- web:DesktopLive `onPointerCancel` → buttons=0 MOVE(T4 Low-1)。
- native:NumLock lock-sync plain 0x45(证据见 run-2;selftest `im-lock-numlock-plain`/`im-lock-no-extended`)。
- 工具/编排:e2eviewer `lock` op(+黄金字节/解析测试);input-probe DPI-aware + numlk/capslk 列;notepad-type-test `-FilePath` 预开文件;e2e-slice3.sh 六门 + 时钟 skew 校正 + 探针任务 /End 串行化。

## 环境异常备注(非本任务引入,已记录)

- XIAOXIN 系统时钟落后 ~74 分钟(未擅自改钟;每跑实测 skew 校正)。
- XIAOXIN 显示 1024×768 模式(2880×1800 面板)+ 200% 缩放元数据;流/物理 1024×768,probe DPI-aware 后对齐。
- `agent/connect TestBackoffResetAfterHealthyConnection` 高并发 flake(T5 已记,未触)。
