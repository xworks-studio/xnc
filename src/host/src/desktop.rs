//! 安全桌面跟随（Winlogon secure desktop，2026-09-17 安全桌面交互）。
//!
//! 背景：登录界面/锁屏/UAC 同意框渲染在 Winlogon 安全桌面——独立 desktop
//! 对象，DACL 只授 SYSTEM。xnc-host 以 SYSTEM 运行（core 的
//! SessionSystemToken spawn），因此可以 OpenInputDesktop + SetThreadDesktop
//! 把采集/输入线程锚到当前输入桌面：GDI BitBlt 与 SendInput 都按调用线程
//! 所属 desktop 生效，锚定即"安全桌面可见可控"。
//!
//! 线程纪律（VNC lineage 教训，winvnc vncService.cpp 注释原话：持有钩子与
//! 窗口的线程不能 SetThreadDesktop）：
//! - 采集主线程：无窗口无钩子，管线重启时重锚（capture.rs）；
//! - 输入注入：独立 OS 线程（input.rs 的 injector），tokio 工作线程只做
//!   解析/换算，注入动作经 channel 串行进入专线程。
//!
//! 桌面切换的判定靠名字：输入桌面名从 "Default" 变为其他（典型
//! "Winlogon"）即安全桌面激活；变回 "Default" 即用户桌面恢复。

use std::io::{Error as IoError, ErrorKind, Result as IoResult};

use winapi::shared::minwindef::DWORD;
use winapi::um::winuser::{
    CloseDesktop, GetUserObjectInformationW, OpenInputDesktop, SetThreadDesktop,
    UOI_NAME, DESKTOP_READOBJECTS,
};

/// 会话默认桌面名（winuser 内部保留名）：不是它 = 安全桌面激活中。
pub const DEFAULT_DESKTOP: &str = "Default";

/// 当前输入桌面名（调用线程任意）。查询失败（罕见：winsta 权限）返回 Err。
pub fn input_desktop_name() -> IoResult<String> {
    unsafe {
        // 打开即读：DESKTOP_READOBJECTS 足以取 UOI_NAME；安全桌面（Winlogon）
        // 的 DACL 对 SYSTEM 开放。句柄即开即关——不保留跨调用状态。
        let h = OpenInputDesktop(0, 0, DESKTOP_READOBJECTS);
        if h.is_null() {
            return Err(IoError::last_os_error());
        }
        let name = desktop_name_of(h);
        CloseDesktop(h);
        name
    }
}

/// 把调用线程锚定到当前输入桌面（SetThreadDesktop）。返回锚定后的桌面名。
/// 调用线程必须无窗口/钩子/GDI 对象绑定（见模块注释的线程纪律）。
/// SetThreadDesktop 需要对桌面的 GENERIC 写侧权限：以 SYSTEM 请求
/// DESKTOP_READOBJECTS | 写组合不如直接 MAXIMUM_ALLOWED 稳妥（安全桌面
/// DACL 对 SYSTEM 全开）。
pub fn bind_thread_to_input_desktop() -> IoResult<String> {
    unsafe {
        let h = OpenInputDesktop(0, 0, winapi::um::winnt::MAXIMUM_ALLOWED);
        if h.is_null() {
            return Err(IoError::last_os_error());
        }
        let name = desktop_name_of(h).unwrap_or_else(|_| DEFAULT_DESKTOP.to_string());
        if SetThreadDesktop(h) == 0 {
            let err = IoError::last_os_error();
            CloseDesktop(h);
            return Err(err);
        }
        // SetThreadDesktop 成功后线程自身持有桌面引用，本句柄可关；但跨
        // 多次切换时每轮泄漏一个句柄也无害（切换频度 = 登录/锁屏/UAC 级），
        // 保守起见显式关闭。
        CloseDesktop(h);
        Ok(name)
    }
}

/// 句柄 → 桌面名（UOI_NAME）。
unsafe fn desktop_name_of(h: winapi::shared::windef::HDESK) -> IoResult<String> {
    let mut buf = [0u16; 64];
    let mut needed: DWORD = 0;
    if GetUserObjectInformationW(
        h as *mut _,
        UOI_NAME as i32,
        buf.as_mut_ptr() as *mut _,
        (buf.len() * 2) as DWORD,
        &mut needed,
    ) == 0
    {
        return Err(IoError::last_os_error());
    }
    let len = buf.iter().position(|&c| c == 0).unwrap_or(buf.len());
    String::from_utf16(&buf[..len])
        .map_err(|_| IoError::new(ErrorKind::InvalidData, "desktop name not utf-16"))
}

/// 桌面名 → 是否处于安全桌面（非 Default 即是：Winlogon/屏保等）。
/// 独立成纯函数供巡检判定与单测。
pub fn is_secure(name: &str) -> bool {
    !name.eq_ignore_ascii_case(DEFAULT_DESKTOP)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn secure_by_name() {
        assert!(!is_secure("Default"));
        assert!(!is_secure("default")); // 大小写不敏感
        assert!(is_secure("Winlogon"));
        assert!(is_secure("Screen-saver"));
        assert!(is_secure(""));
    }

    /// 交互会话探针：确认本线程能查询输入桌面名（CI/服务态可能无 winsta
    /// 访问——标 ignore，真机手动跑）：
    /// `cargo test -- --ignored --nocapture probe_input_desktop`
    #[test]
    #[ignore]
    fn probe_input_desktop() {
        match input_desktop_name() {
            Ok(name) => println!("input desktop: {name:?} secure={}", is_secure(&name)),
            Err(e) => println!("query failed: {e}"),
        }
    }
}
