# scrap（XNC vendored trim）

源自 [rustdesk](https://github.com/rustdesk/rustdesk) `libs/scrap`，
检出 rev `92d787b88`（2026-09-07，desktop-reserch 工作区）。

## 裁剪范围（相对上游）

**保留**：Windows DXGI/GDI 采集（`src/dxgi/{mod,gdi,mag}.rs`）、
`src/common/{mod,convert,dxgi}.rs`、libyuv 绑定（`yuv_ffi.h` + build.rs 生成）。

**移除**：codec/vpxcodec/vpx/aom/camera/record/vram/mediacodec 模块、
quartz/x11/wayland/android 平台、`hbb_common` 依赖（以 `anyhow` +
`tracing` 垫片替代 `bail`/`log`/`ResultType`）、protobuf 类型转换
（`From<&VideoFrame>`、`GoogleImage`）、webm/nokhwa 依赖、`hwcodec` feature
（宿主 crate 直接依赖 hwcodec，不经 scrap）。

## 构建要求

- `VCPKG_ROOT` 指向含 `installed/x64-windows-static` 的 vcpkg（libyuv）
- `LIBCLANG_PATH`（bindgen 生成 libyuv FFI）

## 上游升级规程

以 rustdesk `libs/scrap` 对应文件 diff 后**手工回移**到本目录，勿整目录覆盖
（会带回 hbb_common 与已删模块）。
