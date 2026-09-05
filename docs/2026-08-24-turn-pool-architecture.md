# TURN 池(境内媒体中转)架构设计

日期:2026-08-24
状态:已确认(owner 需求)

## 背景与目标

- **现状**:信令 + 媒体都经 xnc.app(阿里云国际区)。境内用户访问跨境链路,视频延迟/丢包偏高。
- **需求**:WebUI/信令保持主站(xnc.app)不变;**图像数据(媒体)中转拆分到境内服务器集群**;境内用 **IP 池**寻址(免 ICP 备案,不使用域名);**可扩容**(池增删机器即扩)。
- **效果**:浏览器与节点都连接分配的境内 TURN,媒体走境内快链路;信令仍走境外主站(低频小包,跨境影响小)。

## 架构

```text
Browser ──WSS 信令──▶ xnc.app(境外主站:UI/REST/会话管理)
   │                    │ 签发会话时从 TURN 池分配一台(IP)
   │◀──ICE/TURN──▶ 境内 TURN #1 (1.2.3.4:3478, coturn)
   │              ▲
   └──DTLS/SRTP──┘ │ 媒体(RTP/DataChannel)经境内 TURN 中继
                   │
Node agent ──WSS──▶ xnc.app(信令,不变)
   └──TURN──▶ 境内 TURN #2 (5.6.7.8:3478) ← 分配
```

- 信令面:唯一入口 xnc.app(不变)。
- 媒体面:TURN 池(境内多台),每会话从池中分配一台,浏览器与节点连**同一台**(同一会话的 ICE 双方都拿到该 TURN)。
- 池内所有 coturn **共享同一 realm 与凭据**(统一部署脚本),任一台可服务任意会话。
- **免备案**:TURN 地址为纯 IP(`turn:1.2.3.4:3478?transport=udp`),不下发域名。

## 池管理(server 侧)

- 配置:`XNC_TURN_POOL` = 逗号分隔 `ip[:port]` 列表(默认端口 3478),如 `1.2.3.4:3478,5.6.7.8:3478`。
- 分配:每会话 round-robin 选一台,下发**单个** TURN URL(而非全列表)→ 浏览器/节点固定连分配的机器,池内负载均衡。
- 健康:后台每 30s 对池内每台发 STUN binding(UDP,3s 超时);失败标记 unhealthy,分配时跳过;连续 2 次成功恢复。
- 回落:池为空或全部不健康 → 回落 `XNC_TURN_URLS`(现行为,全列表下发)。
- 凭据:`XNC_TURN_USERNAME`/`XNC_TURN_CREDENTIAL` 沿用(池内统一)。

## 扩容

加一台境内机器 → 运行 `scripts/install-turn-chn.sh`(coturn+配置+防火墙+systemd)→ 把 IP 追加进主站 `.env` 的 `XNC_TURN_POOL` → server 自动纳入分配(健康探测 30s 内生效)。缩容同理(移除 IP)。

## 兼容

- 池为空/未配置:行为与现状完全一致(单 TURN URL 或列表)。
- 客户端(浏览器/agent/e2eviewer):无改动——iceServers 来自会话响应,分配逻辑在 server。
- 境外 coturn 可保留(作为池外 fallback,或并入池)。

## 部署(境内机器,由 owner 提供)

`scripts/install-turn-chn.sh`:
1. apt 装 coturn
2. 写 `/etc/turnserver.conf`:realm=xnc.app、lt-cred-mech、user=<共享凭据>、listening 3478、relay 49160-49200、external-ip=<本机公网 IP>(参数传入)
3. systemd 启用
4. 防火墙(UFW):3478 TCP/UDP + 49160-49200 UDP
5. 自检:本机 STUN 探测 3478 通过

## 验收

- 境内 TURN 部署后,desktop 会话的 iceServers 指向分配的境内 IP,媒体经其中继,延迟显著低于跨境(境内实测)。
- 池多台时轮询分配;停掉一台后 30s 内自动跳过。
- 主站信令/WebUI 行为不变。

## 实现说明(feat/turn-pool,2026-08-24)

- 配置:`XNC_TURN_POOL`(逗号分隔 `ip[:port]`,缺省端口 3478,纯 IP 池免备案;hostname/非法项在 `NewTurnPoolManager` 解析时跳过)。空 = 未配置 → 行为与现状一致。
- 下发形态(本分支确认):池命中时下发**单台 TURN、两个 URL**——`turn:<ip>:<port>?transport=udp` 优先 + `?transport=tcp` 兜底,浏览器/节点取第一个可达的;凭据与 `XNC_TURN_USERNAME`/`XNC_TURN_CREDENTIAL` 共用(池内 coturn 统一 realm/凭据,任一台可服务任意会话)。IPv6 地址 URL 加方括号(`turn:[::1]:5349?...`)。
- 分配:每会话(每次 `turnConfig()` 调用)round-robin 选 healthy 台;探测未开始前全部按 healthy(先试跑,≤30s 内校正)。全池不健康/池空 → 回落 `XNC_TURN_URLS` 全列表(现状)。
- 健康探测:manager 生命周期内后台 goroutine(Start/Stop),启动立即跑一轮、之后每 30s 一轮;对每台发 20B STUN binding request(UDP,3s 超时),应答校验 binding success(0x0101)+ 魔数 + 同 transaction id + ≥28B(coturn 典型应答 = 20B 头 + XOR-MAPPED-ADDRESS 8B = 28B,或附 SOFTWARE 更长)——**不要求固定 40B**,按 RFC 5389 形态校验。连续 2 次失败 → unhealthy;1 次成功恢复。探测在锁外发网络请求,状态更新短持锁,分配只读快照,互不阻塞。
- 并发:mutex 保护池状态与 round-robin 游标;探测 goroutine 随 router 生命周期(生产:main.go 优雅停机经 `NewApp().Close()` 先停探测、再排空 HTTP;测试:manager.Start/Stop 直接治理)。
- 部署:compose xnc-server 与 deploy_srv.py `cmd_env` 均带 `XNC_TURN_POOL`(默认空,兼容既有环境);新增池机器只需在 `.env` 追加 IP。
