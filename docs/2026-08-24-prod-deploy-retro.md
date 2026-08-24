# 生产部署复盘:版本更新 / 分发问题 + 结构优化清单

日期:2026-08-24
范围:0.4.0→0.4.5 生产发布全程(server + 三节点 + coturn + web),本次会话踩坑实录。

---

## 一、版本更新 / 分发过程中发现的问题(按严重度)

### P0(已修,曾造成生产事故/死循环)

1. **版本常量未 bump → 更新死循环**
   `agent/machineinfo.Version` 手工维护,与 bundle 版本脱节。0.4.0 的 bundle 里 agent 二进制仍自报 0.3.0 → server 永远 offer 0.4.0 → agent 反复 download→apply→重启→再 offer(文件已换新但版本号没变)。修复:每次 bump 三处一致(常量+bundle 版本+release 版本)。**根因:版本号没有单一来源。**

2. **server 心跳缓存 targetVer**
   `agentws.go` 握手时算一次 `targetVer` 并缓存进所有心跳 ACK;pin 变更对已连接的长连接**永不生效**(新版本永远不下发)。修复:每拍心跳重解析 `targetReleaseFor`。

3. **旧 apply 不搬新文件集、不停 XNCCore**
   0.3.0→0.4.0 过渡期:旧 apply 只知道 2 个文件(agent+helper),不搬 core/desktop/shell;且不停止 XNCCore 服务 → 新文件 rename 被锁失败 → 回滚 → 重启又收到 offer → **死循环**。新 apply 已含(停 XNCCore + 5 文件搬运)。修复过程:手动停 XNCCore 一次让循环收敛。

### P1(已修,功能/可观测性)

4. **put 覆盖运行中 exe 不可靠(「幽灵上传」)**
   现象:put 报 ok=true 但目标文件未更新/消失。两个叠加原因:①file 会话 `os.Rename` 覆盖被锁的 exe 失败(错误被映射为 FILE_NOT_FOUND,且 CLI 端偶发 ok);②`build.bat selftest` 只编 selftest **不重编主 exe**,put 的常常是旧二进制,看起来像「假成功」。
   **结论(语义):exe 更新一律走自更新通道(新 apply 停服务+换文件),put 只用于首装/脚本/数据文件。**
   **修复(2026-08-24 复盘跟进):rename/mkdir/create 失败按底层 errno 映射——权限/占用类(ERROR_ACCESS_DENIED、共享冲突、EACCES/EPERM)→ `ACCESS_DENIED`,其余 → `INTERNAL`;底层错误文本随 FILE_ERROR `message` 透传,CLI 直接展示(exit 250)。不再有「报 FILE_NOT_FOUND 掩盖目标被锁」的误导;「ok=true 但文件没变」已被消除(rename 失败必回错误终态)。**

5. **TURN/coturn 配置链四个坑**(逐个修,STUN 探测是排查主线)
   - server 读 `XNC_TURN_CREDENTIAL`,compose 只给了 `XNC_TURN_PASSWORD` → 503 TURN_UNCONFIGURED
   - coturn 无 `--external-ip` → relay 候选是 docker 内网地址
   - `--relay-ip=0.0.0.0` 无法分配 relay 端口
   - **entrypoint `eval echo $i` 吃掉 `-n`** → `Unknown argument:` 致命错误,UDP 从未监听;日志还误导(STUN 应答来自 docker-proxy)
   **教训:coturn 配置应配 STUN 自检进入部署 verify。**

6. **服务模式子进程日志全盲**
   服务 spawn 无 console 句柄,继承 stdio 重定向失败(产生 0 字节日志文件)→ desktop 的 backend/UAC/健康分诊断全不可见。修复:desktop `--log-file`(子进程自开日志),core spawn 时传入。

7. **web 与后端退役/能力不同步(三个独立缺陷)**
   - screen 流退役后 ScreenPreview 仍指向旧流 → 黑屏(改路由到 DesktopLive)
   - 浏览器 offer 无 m=application → agent 的 input/mouse/cursor DataChannel 永不协商 → **无输入无光标**(e2eviewer 因占位 DC 通过,web 漏了)
   - lease denied 无自动重试 → 用户刷新反复撞 60s 窗口「一直被占用」

### P2(记录在案,未修)

8. release 无删除端点(坏 0.4.0/0.4.2 留库,latest 已绕过)
9. CLI token 2h 过期,部署中途多次失效(脚本应自动 re-login)
10. SSH banner 超时 + 孤儿容器名冲突(d00097cd9d8c_*)导致 rebuild 失败(deploy_srv.py 应清孤儿)
11. 安全组端口清单(3478 TCP/UDP)无文档化;relay 段 UDP 实际未开也不影响(修复后单端口转发)
12. 新 apply 的 XNCCore 停机窗口最长 ~3 分钟(文档化)

---

## 二、结构上奇怪 / 冗余,建议优化

### 版本与发布

1. **版本号三处手工同步**(agent 常量 / bundle 版本 / server release 版本)——应单一来源:从 git describe 或构建时生成注入;server 的 `/api/health` 恒报 0.1.0(server 版本从未 bump,观测失真)。
2. **bundle 历史版本无限堆积** server DB(无删除/GC 端点);本机 bin/ 曾留 9 个历史 bundle。

### 分发双通道职责不清

3. **put(file 会话)与自更新通道职责重叠且 put 语义不完整**:覆盖已有文件/被锁文件的行为不可靠且错误码误导(FILE_NOT_FOUND 掩盖 ACCESS_DENIED)。**已修(2026-08-24 复盘跟进):语义定为「exe 更新走自更新通道,put 仅首装/数据/脚本」;rename 失败错误码透传(ACCESS_DENIED/INTERNAL + message),不再吞错。**

### 退役不彻底

4. **screen-helper 分发面残留**:代码已退役,但 bundle 仍打包、`update_handlers` 仍强制要求该文件、install_scripts 仍复制安装——死代码+攻击面(M3 移除)。
5. **web ScreenPreview.tsx 死代码**(不再被引用,保留)。
6. **spawn 的继承 stdio 重定向分支**已被 `--log-file` 取代但仍保留(产生 0 字节文件);应删或简化,日志通道单一化。

### 工具与模块

7. **诊断工具重叠/过期**:e2eviewer / nalcheck / screendiag(含 6 个子工具)/ shellprobe / mockagent / shellsmoke / sessrun——职责交叉,部分被 e2eviewer 取代(sessrun/shellsmoke)。建议盘点保留最小集。
8. **DesktopLive.tsx 单文件 ~1000 行**(信令/输入/UI/租赁混合)——可拆(信令层/输入层/渲染层)。
9. **core 凭据解析双份**:desktop 的 `resolveCoreEndpoint` 与 session 的 `DefaultShellHost` env 链逻辑重复(已导出复用但仍有两处入口)。
10. **coturn 配置内联在 compose command**(十余个 flag)——建议配置文件化 + 部署 verify 加 STUN/TURN 分配自检。

---

## 三、可操作的跟进项(按优先级)

| # | 项 | 归属 |
|---|---|---|
| 1 | 版本单一来源(git describe 注入 + server 版本 bump) | 基建 |
| 2 | screen-helper 分发面移除(bundle/update_handlers/install) | M3 |
| 3 | file 会话覆盖语义修复(失败透传/先删后写)或文档化禁用 | 基建 |
| 4 | release 删除端点 + 历史清理 | 基建 |
| 5 | 工具盘点收敛(sessrun/shellsmoke 等) | 清理 |
| 6 | spawn 继承 stdio 分支删除(日志单一化) | M3 |
| 7 | coturn 配置化 + STUN 自检入 verify | 基建 |
| 8 | 部署脚本:自动 re-login / 清孤儿容器 / 端口清单文档 | 基建 |
