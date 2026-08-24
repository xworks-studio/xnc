# 生产演进盘点与架构审查(2026-08-24)

## 一、本阶段改动盘点(按架构层)

### 传输层(server)
| 改动 | 位置 | 状态 |
|---|---|---|
| TURN 池(XNC_TURN_POOL,round-robin 分配 + STUN 健康探测 + 回落) | server/internal/api/turnpool.go | ✅ 生产生效(境内节点已接入) |
| TURN 凭据 env 双写(XNC_TURN_PASSWORD + XNC_TURN_CREDENTIAL) | deploy compose/deploy_srv | ✅ 已统一(server 只读 CREDENTIAL;渲染时 PASSWORD 缺省取 CREDENTIAL 值) |
| 单 URL(XNC_TURN_URLS)与池(XNC_TURN_POOL)双路径 | config/desktop_handlers | ⚠️ 兼容保留,可统一 |

### 境内部署
| 改动 | 位置 | 状态 |
|---|---|---|
| install-turn-chn.sh(coturn + 配置 + 防火墙 + 自检) | scripts/ | ✅ |
| external-ip 双值修复 / 凭据 env 独立 | 脚本内 | ✅ |
| **带宽要求未文档化**(实测 4Mbps 瓶颈) | 脚本/文档 | ⚠️ 待补 |

### 编码/采集(native/desktop)
| 改动 | 位置 | 状态 |
|---|---|---|
| 硬件 MFT 梯子(枚举 QSV → 自检 → 软编回退) | mf_encoder.cpp | ✅ 结构正确 |
| QSV NV12 输入协商(Nv12Variant ladder) | mf_encoder.cpp | ✅ 已清理(c62190a;Part A 归 M4,D3D11 纹理路径) |
| rt 编码缩放(ScaledCapture,--max-w,core spawn 1920) | scaled_capture.* / pipe_server | ✅ 生效(32fps) |
| 固定码率 kDiagBitrateBps=2.3M(不随分辨率) | xnc-desktop.cpp | ✅ 已自适应(diag.h BitrateForDims 查表) |

### agent
| 改动 | 位置 | 状态 |
|---|---|---|
| 凭据解析统一(ShellHostFromStateDir) | shellhost_windows.go | ✅ |
| desktop session.go 581 行(信令/帧泵/输入/光标/intent 混一文件) | agent/desktop/session.go | ✅ 已拆分(abce440,session.go 581→239 行) |

### 其他(复盘 8 项 + web)
版本注入/退役清理/删除端点/日志单一化/coturn 配置化/部署脚本/web 修复(路由/lease 重试/DataChannel 占位/提示)——均 ✅ 合理。

## 二、架构审查结论

**总体:架构方向正确**(信令中心化 + 媒体边缘化 + 权限分离 + 降级梯子),近期改动基本落在合理位置。发现的可改进点:

### 高价值改进(实施状态)
1. **TURN 配置统一** ✅ 已完成:server 只读单一 `XNC_TURN_CREDENTIAL`(config.go);deploy_srv.py 渲染 turnserver.conf 时 `XNC_TURN_PASSWORD` 缺省取 CREDENTIAL 值(compose/turnserver.conf 注释已说明「coturn user 密码须与 CREDENTIAL 同值,经渲染自动同步」);`scripts/install-turn-chn.sh` 补 `TURN_USER_PASSWORD` 与 server CREDENTIAL 同值要求(脚本头部说明)。URLS 与 POOL 合并语义:池为空 = 回落 URLs,保持向后兼容,文档化「池为唯一分配真相」(turn-pool-architecture.md)。
2. **码率按分辨率自适应** ✅ 已完成(diag.h `BitrateForDims`):按编码(缩放后)宽度查表 **≤1280→1.5M / ≤1920→2.3M / ≤2560→3.2M / >2560→4.2M**(selftest 表驱动覆盖),避免高分辨率下码率不足的模糊/丢帧,也避免低分辨率下浪费带宽贴顶。
3. **QSV NV12 协商代码清理** ✅ 已完成(c62190a):Part A 未成功(需 D3D11 纹理,归 M4),保留「硬编枚举+自检→软编」主干,移除未生效的 Nv12Variant ladder(输入类型单一标准尝试)。
4. **agent desktop session.go 拆分** ✅ 已完成(abce440):信令/事件/帧泵/光标/输入拆独立文件,降低单文件耦合(session.go 581 → 239 行)。

### 后续(M3/M4)
- 带宽要求入部署文档(境内机需 ≥10Mbps);媒体码率与带宽联动(QoS 已有雏形)
- D3D11 硬编纹理路径(替代 QSV CPU NV12 尝试)
- 境内/境外 TURN 凭据单一来源管理(REST 时效凭据统一)
