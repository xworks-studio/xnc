# 浏览器桌面"10 秒体感延迟"根因与修复（2026-08-25）

## 症状

- 用户在浏览器桌面看到：画面卡顿、抽动、高频"跳显前一帧"、体感延迟可达 10 秒。
- 管线诊断看似健康：capture ~3ms、encode ~17ms、pipe_latency ~15ms、浏览器解码 30fps。

## 证据链（生产实测）

1. **零丢包**：8 分钟会话 RTCP NACK=4、TWCC 正常 → 网络路径无丢包，排除 TURN relay 丢包论。
2. **周期性 PLI**：agent 日志每 5.000s 收到一次浏览器 PLI（`client_reason=pli`），运动时频率升至 ~1/s。
3. **强制 IDR 的编码器代价**：每次 PLI → `ForceNextIdr` → 软件 MFT 重新走 17 帧 lookahead，
   `idr_delivered feeds=16`（~533ms）内**无任何输出**——浏览器"帧丢失"→ PLI → 自激循环。
4. **RTP 时间轴丢失**：agent `frameDuration()` 将 >500ms 的 mono 差钳位回 33ms。
   每次 IDR 的 lookahead 间隙（~533ms）在 RTP 时间轴上只记 33ms → 播放时间轴每循环
   落后真实时间 ~0.5s。8 分钟 32 次 PLI → 累积漂移 ~16s。浏览器播放的始终是
   "过去的内容"——体感延迟随观看时长增长，几分钟后即达 10 秒。
5. **IDR 帧"迟到"**：钳位后 IDR 的时戳比真实到达时刻"早"500ms，Chrome jitter buffer
   （playoutDelayHint=0）将之判为迟到帧丢弃 → 解码器 hold 旧帧（"跳显前一帧"）→ 再 PLI。

## 修复（agent 0.5.2）

`agent/desktop/transport.go` `frameDuration()`：合法 mono 差上限 500ms → 2s。
QPC 单调不回退，>500ms 的间隙只可能是编码器 lookahead 回填（真实时间），必须原样
计入 RTP 时间轴。2s 上限仅防御时钟异常。

## 验证（XIAOXIN 生产）

| 指标 | 修复前 (0.5.1) | 修复后 (0.5.2/0.5.3) |
| --- | --- | --- |
| 浏览器 PLI | 静态每 5s、运动 ~1/s | **0**（仅 attach/sub_join 各一次） |
| 编码器 warmup 黑洞 | 每次 IDR 16 feeds (~533ms) | **0** |
| 静态桌面 | 每 5s 一次 IDR 风暴 | 0fps、零 key、零 PLI |
| 运动（FPS test 动画页） | 卡顿 + 时间轴漂移 | 30fps 稳定、IDR 随场景 ~1/s 无黑洞 |

三节点（XIAOXIN / TB16G7 / YOGAP7G11）经 stable 频道自更新至 0.5.2 → 0.5.3，apply 无回滚。

## 遗留

- 运动时 MFT 场景检测仍 ~1 IDR/s（无害：带内交付、无黑洞，仅码率 ~2x）。
- 大陆 TURN（ecs.e-c1m1.large ~4.9Mbps）建议升 ≥10Mbps：IDR 帧 ~120KB 突发时余量更足。
- 页面 "first frame" 指标未显示（rvfc 未触发），仅 UI 指标问题。

## 附带修复（agent 0.5.3）

`exec --file/stdin`：agent（SYSTEM）把脚本写入 `os.TempDir()`（25H2 为
`C:\Windows\SystemTemp`），用户令牌侧 xnc-shell 无权读取（"is not recognized"
生产回归）。改为脚本落 StateDir（`C:\xnc`，Users 可读），TmpDir 注入仍优先（测试）。
