# 实施方案:采集兜底加速 + 版本单一来源 + 日志布局治理

> 配套规范:docs/superpowers/specs/2026-09-15-capture-fallback-version-logging-design.md
> 状态:**T1~T6 已实施**(2026-09-15,各任务独立分支待 PR):
> T1 `feature/capture-gdi-fallback-5s`(daece54)· T2
> `feature/version-injection`(71e75fe)· T3 `feature/log-layout`(5480a9f)
> · T4 `feature/log-rotation`(5b8b2d1)· T5 `feature/state-hygiene`
> (4c1ee4d)· T6 `feature/docs-2026-09-15-spec-sync`(本分支)。
> 单测:cargo test 24 ok(含 fallback 分层/轮转)、shellhost/agent/cli
> go test ok、core selftest ok(log dir + 版本行可见)。真机验收
> (XIAOXIN 冷启动 ≤3.5s 等 §5 指标)待合入后发版执行。
> 分支策略:T1~T5 各一 feature 分支独立 PR(逻辑单元独立
> 提交),按 T1→T2→T3→T4→T5→T6 顺序合入(build.ps1 被 T2/T3/T4 共同
> 触碰,串行避免冲突)。T1 最小可先行(用户点名的 5s)。
> 注意:T3/T4 在 spawn.cpp/service.cpp/host main.rs 有同区改动,
> 顺序合并时需手工解几处小冲突(语义互补,无对冲)。

## T1 采集兜底:首帧探针 + 5s demand + GDI 回探

分支 `feature/capture-gdi-fallback-5s` · 模块 src/host(Rust)· 规范 §1.2

1. `src/host/src/capture.rs`
   - 新增字段:`first_frame_deadline: Instant`(构造时 now+1.5s)、
     `frame_demand_since: Option<Instant>`(force_frame 置位,产出 Frame
     的 :238/:270 两处清零)、`dxgi_retry_at: Option<Instant>`(因探针/
     demand 降级时置 now+60s)。
   - `next()` WouldBlock 分支(:157-171)按序判定(全部收敛到单点
     `set_gdi()`,各自带区分性 WARN 日志):
     a. `frames_captured == 0 && now > first_frame_deadline && !is_gdi()`
        → 探针失败降级("DXGI delivered no initial frame within 1.5s");
     b. `frame_demand_since 挂起 && elapsed > 5s && !is_gdi()` → demand
        兜底降级("DXGI unresponsive for 5s with pending frame demand");
     c. 原 30s 稳态判定(:164-170)原样保留。
   - GDI 回探:处于 GDI 且 `dxgi_retry_at` 到期(或被输入事件提前触发,
     见下条)→ 重建 duplication 试产一帧,成功回切 DXGI 并清
     `dxgi_retry_at`(复用 Reinit 语义);失败则顺延 60s。
   - :244 force 分支加注释:`last_raw` 为空时 force 标志保持置位是有意
     的(供 b 判定;GDI 降级后首帧填充 last_raw)。
2. `src/host/src/main.rs`
   - frameLoss → `idr_requested` 路径(transport.rs:205-211)一并调用
     `capturer.force_frame()`(现状仅 viewer 0→N 触发,纯 IDR 请求在
     静止桌面会饿死);收到输入注入时若处于 GDI 且有 `dxgi_retry_at`,
     提前触发回探(显示器大概率被输入唤醒)。
   - 采集循环的 `next()` 调用已在 viewers==0 门控之前(main.rs:390 vs
     :398),探针无需等 viewer——确认该顺序在改动中保持。
3. 验证
   - `cargo build --release` + `cargo test`。
   - 真机 XIAOXIN(盒盖)冷启动:基线 31.2s → **≤3.5s**(探针路径,日志
     应见 "no initial frame within 1.5s" 后 GDI enabled,时间戳在 host
     starting 后 ~1.5s);warm 不回归;健康屏(YOGA9/DEV-N1X)冷启动
     ~3s 不回归且无探针降级日志;静止观看 5 分钟无 5s/30s 触发。
   - GDI 回探:盒盖会话中远程唤醒显示器(注入鼠标移动),≤60s 日志见
     DXGI 回切(输入触发路径应秒级)。
   - 回归面:30s 稳态语义路径未动;force 盲区行为变化仅发生在"DXGI
     死"场景(原本就无帧可出)。

## T2 版本单一来源:五进制注入 + --version + 发版校验

分支 `feature/version-injection` · 模块 cli/shellhost/host(Rust)/native/core/build.ps1

1. `src/cli/main.go:20` `const cliVersion` → `var cliVersion = "0.0.0-dev"`。
2. `src/shellhost/main.go` 增 `var shellVersion = "0.0.0-dev"` + 启动
   `--version` flag 早退 + 首条日志 `shellhost starting version=...`。
3. `src/host/src/main.rs:204` 改 `option_env!("XNC_HOST_VERSION")
   .unwrap_or("0.0.0-dev")`;**新建 `src/host/build.rs`** 输出
   `println!("cargo:rerun-if-env-changed=XNC_HOST_VERSION")`(cargo 指纹
   不含环境变量,无此文件则版本变更命中陈旧缓存);参数解析加 `--version`
   早退。
4. `src/native/common/version.h` 新建(`XNC_VERSION` 缺省 "0.0.0-dev");
   `service.cpp` 启动行(:100)与 selftest 输出带 version;core main
   入口加 `--version` 早退;`build.bat` 编译参数读环境变量
   `XNC_VERSION` 注入 `/D`。
5. `src/installer/build.ps1`
   - cli/shellhost 构建步(:46-53)补 `-ldflags "-X main.cliVersion=$Version"`
     / `-X main.shellVersion=$Version`。
   - cargo 步(:74-77)前 `$env:XNC_HOST_VERSION=$Version`、core 的
     cmd 步前 `$env:XNC_VERSION=$Version`(构建后 Remove-Item Env 还原)。
   - 版本校验(:127-131)扩为五进制 `--version` 循环比对,任一不一致
     throw。
6. 验证
   - `build.ps1 -Version 0.0.0-test`:五进制 --version 均 0.0.0-test;
     删掉任一注入 → 构建 fail。
   - **cargo 缓存陷阱回归**:同一工作区连续 `build.ps1 -Version A` 与
     `-Version B`(host 源码零改动),产物 --version 必须分别为 A/B
     (验证 rerun-if-env-changed 生效;不生效则 B 产物仍报 A → fail)。
   - `go test ./...`(cli、shellhost);core selftest;cargo test。
   - 兼容:旧 CLI(UA 0.3.1)对 relay 门禁通过性不变(判定逻辑不动)。

## T3 日志布局:三日志迁 logs\ + agent 迁移 + 目录创建

分支 `feature/log-layout` · 模块 native/core、agent、installer

1. `src/native/core/` 新增 `ResolveLogDir()`(common 或 service.cpp 内):
   ProgramData\XNC\logs,CreateDirectory 幂等,失败回落 exe 目录 + WARN。
   替换:service.cpp:76-86(stderr 重开)、pipe_server.cpp:471(host
   --log-file)、pipe_server.cpp:724-725(shell --log-file)。
2. `src/agent/cmd/xnc-agent/main_windows.go`
   - 启动 `os.MkdirAll(filepath.Join(stateDir, "logs"), 0o700)`。
   - agent-service.log 路径改 `stateDir\logs\agent-service.log`;旧位置
     存在且新位置不存在 → os.Rename 迁移(参照 migrateLegacyStateDir
     缓冲回放模式)。
3. `src/installer/xnc.iss`:`[Dirs]` 增 `{code:XNCStateDir}\logs` 与
   `{code:XNCStateDir}\tmp`(安装器侧双保险;确认 XNCStateDir() 在
   [Dirs] code 段可用,不可用则改用常量默认路径 + iss 自定义函数)。
4. 验证
   - 本机(未注册机)覆盖安装后:新会话日志落 logs\;旧 agent-service.log
     被迁移;core/host/shell 日志不再出现在 Program Files\XNC。
   - core 单测/selftest 不受路径影响;DACL:logs\ 继承 StateDir ACL
     (SYSTEM/Admins)——core(SYSTEM)与 host(用户会话 SYSTEM)均可写,
     实测确认。
   - 卸载 /PURGEDATA=true 清 logs\(沿用 StateDir Purge 逻辑,无需改)。

## T4 日志轮转:四组件 8MB×3 份

分支 `feature/log-rotation` · 依赖 T3(路径先就位)

1. agent:main_windows.go 文件 logger 包 rotating writer(计数每 256KB
   查大小;超限 flush→rename 链→重开)。
2. shellhost:main.go:213 logFileWriter 同包装(shellhost 有 logfile_test.go
   可挂单测:写超限内容断言 .1 产生)。
3. core:service.cpp 打开时轮转 + log.h 行计数 1000 复查(C 侧简单实现)。
4. host:main.rs:471-498 tracing writer 自定义 MakeWriter 包轮转
   (tracing 行级写入,轮转点 flush;实现细节:非原子 rename 与在写
   并发窗口可容忍丢失数行,注释说明)。
5. 验证:单测(shellhost/core 可测)+ 真机把阈值临时调小压测产生 .1/.2/.3
   与最老删除;正常路径日志内容无 interleaving 损坏(抽查)。

## T5 卫生:tmp\ 会话脚本 + .old2 清扫

分支 `feature/state-hygiene` · 模块 agent/session、cli

1. `src/agent/session/exec.go:285-292`:脚本目录改
   `filepath.Join(ex.StateDir, "tmp")`(StateDir 为空时维持 os.TempDir 兜底),
   MkdirAll;释放路径删除不变。
2. agent 启动:清扫 `tmp\xnc-*.ps1` 中 mtime>24h 者。
3. `src/cli/cmd_update.go:124-127`:cleanupOldCLI 增清 `.old2`。
4. 验证:exec_test.go 路径测试更新;真机 exec script 会话后 tmp\ 无残留;
   崩溃模拟(杀进程)后重启清 24h 残留。

## T6 文档同步(随各 PR 分散提交,最后一个 PR 收尾)

- AGENTS.md:契约 3 布局补 logs\ 文件清单与 tmp\;§7 诊断模板路径改
  `C:\ProgramData\XNC\logs\agent-service.log`;§8 host 日志路径表述修正
  (现为 Program Files\XNC\xnc-host.log 的旧描述统一改 logs\)。
- `spec.md` 若引用日志/版本路径,同步(实施时 grep 确认)。
- `scripts/dev-services.md` 若引用路径同步。
- 本方案文档状态更新(附真机验收数据,照 plans 惯例补 Results 节)。

## 里程碑与回滚

- 顺序:T1(独立,先行)→ T2 → T3 → T4 → T5 → T6。T2 与 T3 无代码耦合
  (均改 build.ps1 不同段),严格串行合并避免冲突。
- 每任务独立可回滚(安装器修复机制天然支持逐版本回退);T3 迁移逻辑
  幂等(旧位置不存在则跳过)。
- 版本锚点:修复从下一个 PATCH 版本起(0.10.26 或 0.11.x,发布时定),
  同版本禁止重发(契约不变)。
