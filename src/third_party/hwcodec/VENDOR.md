# hwcodec vendored copy

## 来源

- 上游：https://github.com/rustdesk-org/hwcodec
- 基线 commit：`778df1f99597722473b29443bac22ae6c23946fe`（v0.7.1，与 vendor 前
  Cargo.lock 固定的 rev 一致）
- 上游**无 LICENSE 文件**（2026-09-17 核对）。xnc 在 RTV 重构（2026-09-08）起即以
  cargo git 依赖编译分发同一份代码，vendor 不改变既有许可姿态；后续如上游补充
  许可证，按其条款复核。

## 为什么 vendor（而非继续 git 依赖）

本地 vcpkg 升至 ffmpeg 9.0.1#1 后暴露两类必须改 hwcodec 源码/构建脚本的问题，
git 依赖无法承载本地补丁（AGENTS.md §8 记载的 CI "ffmpeg 头漂移（FF_PROFILE_*）"
即同类问题，vendor 是其记载的修复方向）：

1. `cpp/common/util.cpp`：FFmpeg 9 移除了公共头中的 `FF_PROFILE_*` 宏 →
   编译失败。补丁：`#ifndef` 数值回退（与 libavcodec/defs.h 历史值一致）。
2. `build.rs`：
   - ffmpeg 启用 `[openh264]`（软编回退，xnc 无 GPU 节点的必备项）与
     `[qsv]→oneVPL` 后，avcodec.lib 引用 openh264/vpl 符号，静态链接需显式
     补链——按 vcpkg "Dependencies" 清单条件链接（存在才链）。
   - Windows 系统导入库补齐（secur32/ncrypt/crypt32/ws2_32/mfuuid/strmiids）。

## 裁剪

- 未携带 `externals/`（AMF/NV Codec SDK/MediaSDK 等，仅上游 `vram` feature 与
  Linux ffnvcodec 头需要；xnc 只用默认 feature = ffmpeg_ram 路径，Windows 不受
  影响）。`build.rs` 中对应的 rerun-if-changed 引用已移除。
- 未携带 `dev/`、`docs/`、`vs/`、`.gitmodules`。
- **vram feature 在本 vendor 副本不可用**（externals 缺失）；如将来需要，从上游
  补回 externals/ 或改回 git 依赖 + fork。

## 本地补丁索引（改动均带 "XNC 补丁"/"XNC vendor" 注释）

| 文件 | 内容 |
| --- | --- |
| `cpp/common/util.cpp` | FF_PROFILE_H264_HIGH / FF_PROFILE_HEVC_MAIN `#ifndef` 回退 |
| `build.rs` main() | 移除 externals rerun-if-changed（目录未 vendor） |
| `build.rs` link_vcpkg() | 条件补链 openh264/vpl 静态库 |
| `build.rs` link_os() | Windows 系统导入库补齐 |

升级上游时：以新 commit 覆盖本目录后重放上表补丁（或逐条移植），并在本文件
更新基线 commit。
