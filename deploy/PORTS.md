# 生产端口与安全组清单（SRV / control.xnc.app）

| 端口 | 协议 | 用途 | 来源 |
|---|---|---|---|
| 443 | TCP | TLS：Web UI / REST / WS 信令 / 安装端点（Caddy → xnc-server） | Caddyfile |
| 3478 | TCP+UDP | coturn STUN/TURN 监听（NAT 打洞 + 中继候选） | turnserver.conf `listening-port` |
| 49160-49200 | UDP | coturn relay 动态段（TURN 中继数据） | turnserver.conf `min-port`/`max-port` |

## 说明

- 443 为唯一必需入站端口；agent 与 CLI 均出站连 `https://control.xnc.app`。
- 3478 必须 TCP+UDP 同时放行（TURN 候选含 TCP 与 UDP transport，见 `docker-compose.yml` 的 `XNC_TURN_URLS`）。
- 49160-49200：单端口转发修复后实际数据不依赖该段，但建议保留（宽松 TURN 场景、`external-ip` 候选回退备用）。安全组漏开不影响 3478 打洞成功路径。

## 排查命令

```bash
# 443 TLS 可达性
curl -sI https://control.xnc.app/api/health | head -1

# 3478 TCP 可达性（Windows）
powershell -NoProfile -Command "Test-NetConnection control.xnc.app -Port 3478 -InformationLevel Quiet"

# 3478 UDP 可达性（nmap，SRV 或本机）
nmap -sU -p 3478 control.xnc.app

# relay 段 UDP 可达性
nmap -sU -p 49160-49200 control.xnc.app

# STUN 探测（binding request 往返，验证 3478 UDP 真实可用）
#   Windows: 从 coturn 源码工具 turnutils_stunclient（或在线 STUN 工具）
#   指向 control.xnc.app:3478，期望收到 MAPPED-ADDRESS
# 端到端: 浏览器/CLI 发起 xnc screen --open 或 xnc rdp，观察 server 日志
#   relay 会话是否建立（journalctl -u docker 或 server 容器日志 grep turn）
```
