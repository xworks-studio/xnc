# M2-Slice2 XIAOXIN E2E 验收结果(stability + shell-host)

日期:2026-08-23 · 脚本:`scripts/e2e-m2s2.sh` · 节点:LABS-XIAOXIN(dev core SYSTEM console pipe + dev agent `XIAOXIN-DEV`,session 1)· dev server:docker(LAN :18080)
最终轮:**e2e-run11(RESULT: PASS,exit 0)**;此前 run1-10 迭代证据在 `.superpowers/sdd/2026-08-23-m2-slice2-stability-shellhost/e2e-run*.log` + `e2e/` 工件。

## 验收门(全部数字来自 run11,除注明外)

| # | 门 | 结果 | 证据数字 |
|---|---|---|---|
| ① | exec user 令牌 → whoami=console 用户 | **PASS** | `whoami='labs-xiaoxin\labs'`(g1-whoami.json) |
| ② | exec --system:operator 403 / owner 202+SYSTEM / audit | **PASS** | operator `--system` → HTTP 403;owner → 202,`whoami='nt authority\system'`(g2-system-whoami.json);audit `exec.start` metadata `"system":"true"`(g2-audit.json,id=84 系列) |
| ③ | 交互 shell ConPTY 全双工 + resize(pipeprobe 冒烟) | **PASS** | xnc-shell-probe `--interactive --resize 132x43 --expect m2s2-interactive-ok`:BEGIN+echo 回环+`resize 132x43 accepted`(g3-probe.log)。CLI TTY 腿:无控制台会话跑 `xnc shell` 属设计拒绝,plan 认可 probe 承门(g3-cli-note.txt) |
| ④ | desktop 崩溃 → ≤60s 退避重启 → viewer 恢复 | **PASS** | scoped taskkill(session 1)→ core `desktop restart in 1000ms (crash #1)`、respawned=1、gen=2(≥2);75s 窗口 viewer frames=20(g4-core.log / g4-viewer.json) |
| ⑤ | crash-loop 连杀 5× → 降参 gdi/software 重启 | **PASS** | 5× kill 各次 respawn(alive=1);`crash_loop_degraded kind=desktop exits=5 window_ms=60000 (desktop locked to --backend gdi --encoder software)` + degraded respawn(gen=8)+ 子进程日志 `gdi init w=2880 h=1800`、`encoder selector: software`、frames=12(g5-core.log / g5-viewer.json)。WMI cmdline 经 exec 令牌读 SYSTEM 子进程为空 → 以日志证据承门(cmdline=0, log-evidence=1) |
| ⑥a | SAS busy(并发 secure_attention) | **PASS** | sas_async 紧跟 sas:attempt 1 即 `denied:busy`(step detail)+ viewer 日志 `code=busy`(g6a-viewer-1.json/.log) |
| ⑥b | 健康分:强制 50 → GDI → probe 恢复 → 100 | **PASS** | core GEN2 `XNC_FORCE_DXGI_HEALTH=50` → `backend_changed backend=gdi` x1;恢复 → `backend_changed backend=dxgi ... health=100` x1,frames=51(g6b-core.log / g6b-viewer.json) |
| ⑦ | >4MB exec 输出 → truncated=true + marker | **PASS** | 700000×50B echo(≈35MB)→ `EXEC_RESULT.truncated=true`,stdout 封顶 ~4MB,stderr `[xnc] output truncated: 32205732 bytes dropped`(g7-truncation.json,捕获 4355805B) |

## 打磨项说明(门⑦ Slice2 打磨回归余项)
- **storm-backoff / storm 日志**:live 节点无 env 注入钩子,以 native/desktop selftest + pipeline 单测断言覆盖(run11 脚本 NOTE 行;与 run3 同)。
- **GDI away 提示**:viewer 侧 UI 提示属 Slice3 打磨(计划外),未在本门范围。
- **无会话路径(logoff)**:单测覆盖+代码走查记录(Task 5 brief);logoff 实机门=Slice3。

## 过程发现与修复(run10/11 迭代)
1. **exec 截断预算只封积压不封总量**(run10 暴露):快消费方全程透传 37.8MB、truncated=0。修复:`agent/shellpipe/client.go` 预算改为每流累计输出(直发+pend)封顶 4MB,超预算整块丢弃+marker;新增快消费回归测试 `TestExecBudgetTotalCapFastConsumer`。
2. 脚本修正:g2-audit 行提取撞 `Array.prototype.entries`(恒 0);g7 缺 `$NODE_ID`(USAGE);g6a 前需重启 fresh core(gen1b)清 crash-loop 降参锁,否则 viewer 25s 无关键帧中止输入脚本;g5 cmdline 探测为空 → 日志证据承门。

## 结论
M2-Slice2 全部验收门 PASS;XIAOXIN 生产环境未触碰(全部 kill 按 image+session/path scoped;registry 未动;`--allow-sas` 仅 dev core GEN1/1b/2)。
