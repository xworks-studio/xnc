//! 桌面会话期间的电源保活（2026-09-14，Live Desktop 息屏锁屏优化）。
//!
//! SetThreadExecutionState(ES_CONTINUOUS | ES_SYSTEM_REQUIRED |
//! ES_DISPLAY_REQUIRED)：会话期间显示器不熄、系统不睡——观看中不会因空闲
//! 触发"息屏 → 锁定"，DXGI 采集也不会因屏灭掉进全黑帧（AGENTS 已知缺口
//! "显示器休眠 → RTV 采集全黑"的根治）。
//!
//! 作用域天然安全：host 进程 = 一次桌面会话的生命周期（agent 每会话拉起），
//! ES_CONTINUOUS 绑定调用线程（main 线程贯穿全程），进程退出时系统自动清除
//! 保活，无需显式复位。
//!
//! 局限（有意不做）：机器在会话开始前已锁定时，锁屏运行在 Winlogon 安全
//! 桌面，用户身份的 SendInput 无法注入——保活只防患、不解锁。已锁会话的
//! 解锁出路 = Windows RDP（mstsc 经 `xnc rdp` 隧道，RDP 协议栈有 TCB 权限
//! 可操作锁屏）；Live Desktop 直接操作锁屏属后续架构项（SYSTEM 会话内注入
//! 通道，需安全评审）。

/// 持有"显示常亮 + 系统不睡"直到本进程退出。返回 false = API 失败（仅告警，
/// 不阻断会话——大不了回到原有息屏行为）。
#[cfg(windows)]
pub fn keep_display_awake() -> bool {
    use winapi::um::winbase::{
        SetThreadExecutionState, ES_CONTINUOUS, ES_DISPLAY_REQUIRED, ES_SYSTEM_REQUIRED,
    };
    // SAFETY: 纯电源状态声明调用，无句柄/缓冲区参数。
    let r = unsafe {
        SetThreadExecutionState(ES_CONTINUOUS | ES_SYSTEM_REQUIRED | ES_DISPLAY_REQUIRED)
    };
    r != 0
}

/// 非 Windows 平台 no-op（本 crate 跨平台编译通过；输入/采集仅在 Windows 生效）。
#[cfg(not(windows))]
pub fn keep_display_awake() -> bool {
    true
}
