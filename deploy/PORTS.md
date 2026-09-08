# 生产端口与安全组清单（SRV / xnc.app）

| 端口 | 协议 | 用途 | 来源 |
|---|---|---|---|
| 443 | TCP | TLS：Web UI / REST / RTV WS 兜底腿 / 安装端点（Caddy → xnc-server） | Caddyfile |
| 443 | UDP | RTV WebTransport 主路（浏览器 H3 → xnc-server WT 腿） | compose `XNC_RTV_WT_ADDR` |
| 4433 | UDP | RTV host 腿 raw QUIC（xnc-host → xnc-server，ALPN `xnc-host/1`） | compose `XNC_RTV_HOST_ADDR` |

## 说明（RTV 重构，2026-09-08）

- TCP443 仍为 Caddy（H3 已显式关闭让出 UDP443）；WS 兜底腿走
  `wss://xnc.app/ws` 经 Caddy 反代到主 mux，UDP 全封的网络里浏览器仍可看。
- UDP443/4433 为 RTV 媒体面：**纯客户端→服务端 QUIC，无连接迁移**，容器
  NAT 端口映射足够（coturn 式 host 网络不必要）。
- **UDP4433 被严格网络封锁时 host 腿无兜底**（QUIC 必须 UDP）——已知约束，
  记入重构 spec §5；浏览器侧有 WS/TCP443 兜底，host 侧没有。
- coturn（3478/49160-49200）已退役：desktop 是其唯一用户，媒体面改经
  xnc-server 三腿中继。旧端口可从安全组收回；回滚旧版 = git checkout
  上一版 compose（turnserver.conf 模板仍保留在仓库供回滚窗口使用）。
- ACME DNS-01（`XNC_ACME_DOMAIN`，默认 xnc.app）签发 RTV 腿证书——
  Aliyun DNS 凭据（`ALIDNS_ACCESS_KEY/SECRET_KEY`）须在 deploy/.env。

## 排查命令

```bash
# TCP443 TLS 可达性
curl -sI https://xnc.app/api/health | head -1

# UDP 443/4433 可达性（nmap，SRV 或本机）
nmap -sU -p 443,4433 xnc.app

# RTV 观测面（管理端 JWT）
curl -s -H "Authorization: Bearer <admin-jwt>" https://xnc.app/api/rtv/stats

# 端到端: 浏览器开 /desktop/:id（HUD 看 transport=WT），或
#   rtvload -url "https://xnc.app/wt?token=<会话token>" 观察帧统计
```
