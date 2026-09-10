// XNC vendored trim：仅 Windows（dxgi）。quartz/x11/wayland/android 与
// hbb_common 的 libc 再导出已随裁剪移除。
#[cfg(dxgi)]
extern crate winapi;

pub use common::*;

#[cfg(dxgi)]
pub mod dxgi;

mod common;
