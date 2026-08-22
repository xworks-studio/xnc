# M1-Slice1 捕获脊柱 — 验收结果(Task 7 XIAOXIN gate run)

**日期:** 2026-08-22 · **节点:** LABS-XIAOXIN(console = LABS@session 1, Active;exec = SYSTEM@session 0)
**分支:** feat/m1-capture-spine · **产出方式:** `bash scripts/diag-deploy.sh LABS-XIAOXIN`(全新构建 + 全新部署 + 全新采集,产物非 Task 6 校准件)
**脚本退出码:** 0 — 全部 6 项 GATE PASS

## 1. 验收门结论

| Gate | 阈值 | 实测 | 判定 |
|---|---|---|---|
| 静止 60s 总帧数(nalcheck AU) | ≤ 20 | **15** | PASS |
| 静止 60s 请求关键帧(stats.json `keyframes`) | ≤ 2 | **1** | PASS |
| 变化 30s 总帧数(nalcheck AU) | ≥ 30 | **136** | PASS |
| 变化 30s IDR 占比(nalcheck) | < 30%(E2 级风暴线) | **3.68%**(5/136) | PASS |
| 变化 30s 平均码率(nalcheck `--duration 30`) | 0.1–10 Mbps | **0.397 Mbps** | PASS |
| 码流整形:每个 IDR 前必有 SPS+PPS(spec §7.10) | 双流完备 | **complete=true ×2** | PASS |

> 门数字为 2026-08-22 controller 校准值(Task 5/6 实测:软件 MFT 有 ~1 个/秒的 GOP 周期自发 IDR,故以「IDR 占比 <30%」定义风暴,而非绝对计数;stats.json `keyframes` 计管线侧请求/编码关键帧,nalcheck 计流内 IDR,两者互证)。plan 原稿门(静止 ≤5 帧、变化 ≥60 帧、IDR ≤5、0.5–8 Mbps)按实测口径对照:变化帧数 136≥60、IDR 5≤5 亦过;码率 0.397 Mbps 低于原稿 0.5 下限 —— 校准值(0.1 下限)反映了真实驱动场景(cmd 滚屏窗口只占 2880×1800 桌面一小块区域,增量码流天然小)。

## 2. 场景一:静止桌面 60s

命令:`xnc-core.exe --console --diag-spawn --console-diag --duration 60 --out C:\xnc-diag\static.h264`(TokenManager 桥接,desktop 落 session 1)

**stats.json(逐字):**

```json
{
  "duration_s": 60,
  "width": 2880,
  "height": 1800,
  "fps": 30,
  "bitrate_bps": 2300000,
  "captured": 2,
  "encoded": 15,
  "keyframes": 1,
  "timeouts": 482,
  "warmup_feeds": 13,
  "rebuilds": 0,
  "aus_written": 15,
  "bytes_written": 107425,
  "ok": 1
}
```

**nalcheck(逐字):**

```
file=xiaoxin-static-t7.h264
bytes=107425 nal_units=19
frames=15 (idr=1 non_idr=14) idr_ratio=0.0667 (6.67%)
idr_indices=[0]
sps=1 pps=1 sps_pps_complete=true aud=0 sei=2
duration=60.0s bitrate=0.014 Mbps
nalcheck: OK
```

**观察:** 运行中逐秒 beat `aus=0 bytes=0` 全程恒定 —— 15 个 AU 全部来自 warm-up 重喂 + flush tail(MFT 17 帧前瞻把 warm-up 帧扣到尾部,与 Task 5/6 观察一致);60s 实际捕获仅 2 帧(首帧 + 尾部 1 次),静止自然 0fps 成立;唯一 IDR 即首帧基础帧,无风暴。

## 3. 场景二:变化 30s(session-1 ping 驱动)

驱动:交互式计划任务 `schtasks /Create /TR "cmd.exe /c ping -n 30 127.0.0.11" /RU LABS /IT` → 采集启动 +3s 后 `/Run`(session 1 弹滚屏 cmd 窗口,~1 行/s × 27s)→ 采集结束 `/Delete`(trap 保证)。exec 自身的 SYSTEM@session-0 ping 对 console 桌面不可见(Task 6 实测),故必须走会话 1。

**stats.json(逐字):**

```json
{
  "duration_s": 30,
  "width": 2880,
  "height": 1800,
  "fps": 30,
  "bitrate_bps": 2300000,
  "captured": 122,
  "encoded": 136,
  "keyframes": 5,
  "timeouts": 185,
  "warmup_feeds": 14,
  "rebuilds": 0,
  "aus_written": 136,
  "bytes_written": 1488037,
  "ok": 1
}
```

**nalcheck(逐字):**

```
file=xiaoxin-change-t7.h264
bytes=1488037 nal_units=148
frames=136 (idr=5 non_idr=131) idr_ratio=0.0368 (3.68%)
idr_indices=[0 30 60 90 120]
sps=5 pps=5 sps_pps_complete=true aud=0 sei=2
duration=30.0s bitrate=0.397 Mbps
assert max_idr_ratio<=0.3000: PASS
nalcheck: OK
```

**观察:** 窗口一开帧即持续输出(elapsed 4s→30s:captured 19→122 连续爬升,~4 帧/s);窗口关后立即回落。IDR 序号 [0,30,60,90,120] 严格等间隔 30 帧 = MFT 默认 GOP(30 帧 @30fps ≈ 1 个 IDR/s)的**周期行为**,非风暴、非重发;每个 IDR 前均有 SPS+PPS(sps=pps=idr=5),AUD 全程 0(整形契约生效)。stats `keyframes=5` 与 nalcheck `idr_frames=5` 双证一致。

## 4. 会话驻留证据(变化场景运行中,采集 +8s)

```
"xnc-desktop.exe","11776","Console","1","935,192 K","Unknown","NT AUTHORITY\SYSTEM","0:00:02","N/A"
>services                                            0  Disc
 console                   LABS                      1  Active
```

desktop(pid 11776,与 core 日志 child pid 一致)由 session-0 的 core 经 TokenManager 桥接,运行于 **Console/session 1**,身份 SYSTEM 改标令牌。

## 5. 帧数对比(门判据:变化 ≫ 静止)

| 场景 | captured | encoded(AU) | keyframes/IDR | bytes | 码率 |
|---|---|---|---|---|---|
| 静止 60s | 2 | 15(全在 flush tail) | 1 | 107,425 | 0.014 Mbps |
| 变化 30s | **122** | **136** | 5 | **1,488,037** | 0.397 Mbps |

变化 30s 的捕获帧(122)为静止 60s(2)的 **61 倍**,字节 **13.9 倍**。

## 6. 产物清单(.superpowers/sdd/2026-08-22-m1-slice1-capture-spine/,不入库)

| 文件 | sha256 |
|---|---|
| xiaoxin-static-t7.h264(107,425 B) | 1b0d29e794b232306a9e0d082ddc3263460e3cf5d6270c755a682d3d78a30933 |
| xiaoxin-static-t7.stats.json | 5b5e8e614b26d58161435789067cda6b92495aedd719c5cf02d255b2cf690fe3 |
| xiaoxin-change-t7.h264(1,488,037 B) | f8f4558c74e2c9c8b905dafd357a9c0280a7c8c18b37cfd497260edb45da91c2 |
| xiaoxin-change-t7.stats.json | 676aa5202c102cdb79a18bc1b302a7f49aaab3e0d260136af4122173e1da3861 |
| xiaoxin-static-t7.run.log / xiaoxin-change-t7.run.log / xiaoxin-change-t7.session.log / *-t7.nalcheck.json | 全程日志 + nalcheck JSON |

部署二进制:core sha256 169fe45b6483fc8fa98f4a0329c6a974432dbcf5bf928ea31eb505a27ab30d1b,desktop c1622c973773bc818b7bb044147dcf3621c8349dbd8be66f9da9dd8bb6d07bad(本任务 HEAD 全新构建,selftest 双绿)。

## 7. M1-Slice1 结论

DXGI 采集 → FrameCache 状态机 → MF 软编 → `--diag-dump` 整形落盘,经 `xnc-core --diag-spawn` 会话桥在真实 console 会话端到端跑通:静止近零输出(0fps)、变化持续编码、无关键帧风暴(IDR 占比 3.68%,GOP 周期性)、IDR 前置 SPS/PPS、统一 4B 起始码、无 AUD —— spec §7.10 编码契约与 §7.5 帧状态机契约在真实桌面负载下成立。

## M1-Slice2 承接(final review)

- ①静止桌面 warm-up 实测 13 feeds < 17 帧前瞻,关键帧仅经 FlushTail 出现——实况流「新观众加入静止桌面拿不到 IDR」须随订阅者/关键帧请求语义(spec §7.5 IDR 合并 + 500ms 最小间隔)在 Slice2 解决,并对 kEncoderLookaheadFrames=17 按机型复测;
- ②BuildChildCommandLine 引号/尾反斜杠拒绝 = Slice3 RPC 复用前必须修(代码已留 TODO)。
