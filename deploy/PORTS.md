# 生产端口与安全组清单（2026-09-11 relay-only 基线）

## 主站 SRV / xnc.app（web + 认证 + 控制信令，零数据流）

| 端口 | 协议 | 用途 | 备注 |
|---|---|---|---|
| 443 | TCP | TLS：Web UI / REST / 会话编排信令 / 安装端点（Caddy → xnc-server） | 唯一生产入站端口 |

- `XNC_RTV_EMBEDDED=false`（默认，2026-09-11 主站缩减）：主站不创建
  媒体面、不挂 `/ws`、不启 ACME。UDP443/4433 **仅在显式开回内嵌形态**
  （过渡/dev）时使用（compose `XNC_RTV_WT_ADDR`/`XNC_RTV_HOST_ADDR`，
  过渡端口 `XNC_RTV_WT_PORT`=14433/udp）。
- relay 池无可用中继时 desktop 会话 503 `RTV_NO_RELAY`（不做内嵌兜底）。

## 中继机 r1/r2.xnc.app（媒体 + 会话数据面，systemd xnc-relay + caddy）

| 端口 | 协议 | 用途 | 备注 |
|---|---|---|---|
| 443 | TCP | caddy（CA 证书）：`/ws` 浏览器 WS 兜底 + 会话数据腿 `/api/session/{sid}`、`/api/agent/session` + `/healthz`，反代 → 127.0.0.1:8080 | caddy 禁 H3（UDP443 归 WT 腿）；域名是数据腿硬前置（浏览器 WS 无法钉扎自签） |
| 443 | UDP | RTV WebTransport 主路（浏览器 H3 → relay WT 腿，自签证书 + serverCertificateHashes 钉扎） | |
| 4433 | UDP | RTV host 腿 raw QUIC（xnc-host → relay，ALPN `xnc-host/1`，certSha256 钉扎） | |
| 8080 | TCP | relay 本地 HTTP 腿（caddy 上游；仅 loopback） | |

## 说明

- 媒体 QUIC 为**纯客户端→服务端**（无连接迁移），容器 NAT / 主机直跑均够。
- **host 腿无 TCP 兜底**（QUIC 必须 UDP）——UDP4433 被严格封锁的节点
  无法出桌面流（已知约束）；浏览器侧有 WS/TCP443 兜底。
- relay 主机 caddy 装配：`deploy/setup_relay_caddy.py <域名>
  [--env-prefix RELAY1_|RELAY2_]`；relay 二进制：
  `deploy/build-relay.ps1 -PublicHost <ip> [--session-host <域名>]`。
- ACME：主站内嵌形态经 `XNC_ACME_DOMAIN` + Aliyun DNS（deploy/.env 的
  `ALIDNS_*`）；relay 主机由各自 caddy 自管（HTTP-01）。
- coturn（3478/49160-49200）已退役（2026-09-08），端口可从安全组收回。

## 排查命令

```bash
# 主站
curl -sI https://xnc.app/api/health | head -1

# relay 域名（caddy TLS + 反代）
curl -s https://r1.xnc.app/healthz

# relay UDP 443/4433 可达性（nmap，从节点或本机）
nmap -sU -p 443,4433 r1.xnc.app

# RTV/relay 观测面（管理端 JWT）
curl -s -H "Authorization: Bearer <admin-jwt>" https://xnc.app/api/rtv/stats

# 端到端: 浏览器开 /desktop/:id（HUD 看 transport=WT + 当前中继），或
#   rtvload -url "https://<relay-ip>:443/wt?token=<会话token>" 观察帧统计
# 会话数据面: xnc exec <node> "echo ok" 后查 relay journal 的
#   "session data legs glued" 事件
```
