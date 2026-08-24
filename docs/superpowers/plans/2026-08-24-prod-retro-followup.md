# 生产复盘跟进:基建清理与结构优化 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 把 docs/2026-08-24-prod-deploy-retro.md 的 8 项跟进 + 结构清单全部落地:版本单一来源、screen-helper 分发面移除、file 会话错误语义、release 删除端点、工具收敛、spawn 日志单一化、coturn 配置化+自检、部署脚本改进,最后发布 0.4.6 全量更新并验证。

**Architecture:** 版本注入(go build -ldflags);退役面清理(构建/分发/服务端三处);server admin API 扩展;native 日志通道简化;deploy 自检;最后统一发布。

**Spec:** docs/2026-08-24-prod-deploy-retro.md(本文档是唯一需求来源);spec.md §45 自更新 / §57 CLI。

## Global Constraints

- 生产路径行为不变(除明确修复);所有 Go 测试绿;native selftest 绿;web tsc/build 绿
- 版本注入缺省 fallback "0.0.0-dev";server health 版本与发布对齐
- screen-helper:分发面全移除(bundle/updater required/update_handlers/install_scripts/go.work/目录);节点旧文件不主动删(无害),bundle 不再携带
- file 会话:rename 失败透传具体码(ACCESS_DENIED 等),不吞;put 语义文档化(exe 走自更新)
- release 删除端点:admin-only;删除后 latest 自动回落;DB 历史清理(0.4.0/0.4.2)
- 工具收敛:仅删被取代的(sessrun/shellsmoke),保留 mockagent/e2eviewer/nalcheck/shellprobe/screendiag
- spawn stdio:保留 console 继承(dev 便利),删文件回退分支(服务模式由 --log-file 覆盖),不再产生 0 字节文件
- coturn:配置从 compose flag 移到 turnserver.conf(挂载),verify 加 STUN 探测
- 部署脚本:up 前清孤儿容器;端口清单入 docs

---

### Task 1: 版本单一来源 + server 版本 bump

**Files:** agent/machineinfo/info.go(Version → 注入变量+fallback)、server 版本常量(查 server/internal 的 version,同样注入)、scripts/build-bundle.go(打包时校验/记录版本)、Makefile/构建文档(统一 -ldflags)、deploy 相关(server Dockerfile 构建时注入)。

**验收:** `go build -ldflags "-X xnc/agent/machineinfo.Version=9.9.9"` 后二进制自报 9.9.9;无注入时 0.0.0-dev;bundle 脚本产出的 bundle 版本与 agent 自报一致;server /api/health version 随发布 bump。全测试绿。

### Task 2: screen-helper 分发面彻底移除 + 工具收敛

**Files:** scripts/build-bundle.go(names 去 helper)、agent/updater/apply_windows.go(requiredFiles 去 helper)、server/internal/api/update_handlers.go(强制含 helper 检查)、install_scripts.go(复制 helper 步骤)、go.work/构建脚本引用、删除 agent/screen-helper/ 与 tools 下 sessrun/shellsmoke(先 grep 全仓引用确认无依赖)。

**验收:** bundle 5 exe(无 helper);updater 对旧 bundle(含 helper)兼容(多余文件容忍);server 不再要求 helper;仓库无 screen-helper/sessrun/shellsmoke 引用残留;go.work 干净;全测试绿。

### Task 3: file 会话错误语义 + put 文档化

**Files:** agent/session/file.go(rename 失败错误码映射:ACCESS_DENIED→CodeAccessDenied 或透传具体;查 proto 错误码表)、CLI put 错误展示、docs 部署说明(put 仅首装/数据,exe 走自更新)。

**验收:** 覆盖运行中 exe 的 put 返回明确错误(非 FILE_NOT_FOUND);单测覆盖;文档更新。

### Task 4: server release 删除端点 + 历史清理

**Files:** server/internal/api/update_handlers.go(+DELETE /api/admin/releases/{id}:删 artifact+release;校验无节点 pin 指向?或允许删并自动回落)、router.go、测试;上线后清理 DB 0.4.0/0.4.2。

**验收:** 单测(删除/404/权限);curl 删除坏版本后 latest 回落 0.4.5(生产验证)。

### Task 5: spawn stdio 简化 + 日志单一化

**Files:** native/core/spawn.cpp(删文件回退分支,保留 console 继承;stdin secret 通道不动)、pipe_server.cpp(spawn 参数 --log-file 保留)。

**验收:** selftest 绿;console 模式 desktop 日志仍可见;服务模式 0 字节文件不再产生。

### Task 6: coturn 配置化 + STUN 自检

**Files:** deploy/docker-compose.yml(coturn command → `-c /etc/coturn/turnserver.conf` + 挂载配置;配置含 realm/user/listening/external-ip/min-max-port,env 插值)、deploy/turnserver.conf 模板、deploy_srv.py verify(+STUN UDP/TCP 探测 coturn)。

**验收:** 重建后 coturn 正常(TURN Allocate 本机探测过);verify 输出 STUN ok。

### Task 7: 部署脚本改进 + 凭据解析合并 + DesktopLive 保守拆分

**Files:** deploy_srv.py(up 前清孤儿容器名冲突;verify 端口自检)、scripts/dev-topology.md(端口清单)、agent/session/shellhost_windows.go(DefaultShellHost 合并到 desktop.ResolveCoreEndpoint,删 env 双份)、agent/desktop/core_windows.go(导出复用)、web DesktopLive.tsx(抽 signaling 常量/类型/词汇到 desktop/signaling.ts,组件主体不动)。

**验收:** 脚本幂等;凭据解析单测绿(env 优先级不变);web tsc/build 绿。

### Task 8: 发布 0.4.6 全量 + 生产验证

**Files:** bump 0.4.6(注入式),重建全部,上传 release,pin 三节点。

**验收:** 三节点 0.4.6 + core/desktop 新版本;exec 双令牌/snap 回归;server 重建(新 web/删除端点/版本 bump);release 历史清理(0.4.0/0.4.2 删除,latest=0.4.6);全绿。

## Self-Review
- 依赖:1→8(build 链);2/3/4/5/6/7 相对独立;8 收尾
- 风险:T2 删目录前必须 grep 全仓引用;T4 删除端点上线后 immediate 验证
- 每任务 TDD/验证要求明确;全部经 review 后合入
