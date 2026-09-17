//! 输入注入：鼠标（移动/按键/滚轮）+ 键盘（键位/Unicode 文本）—— Windows
//! SendInput / SetCursorPos。
//!
//! 对应 rustdesk 的 enigo→SendInput 路径（win_impl.rs）：绝对坐标模式（远程桌面
//! 风格），注入坐标为显示器物理像素（与 DXGI 采集坐标系一致，DPI 缩放无关）。
//! 在交互会话内运行（部署方式保证了这一点，见 deploy-host.ps1）。
//!
//! 安全桌面交互（2026-09-17）：SendInput 按调用线程所属桌面生效——登录/
//! 锁屏/UAC 的 Winlogon 桌面上，只有锚定到该桌面的线程能把输入送进去。
//! 因此注入收敛到**专用 OS 线程**（injector）：tokio 侧只做解析/坐标换算
//! （handle），动作经 channel 串行进入 injector，injector 周期性跟随输入
//! 桌面（desktop.rs，VNC lineage 的专线程纪律——持窗口/钩子的线程不能
//! SetThreadDesktop，本线程无窗口无钩子）。粘键表也随之移入 injector
//! （单线程自有，免锁）。
//!
//! 键盘两类事件（web 端分类，见 DesktopLive 键盘捕获）：
//! - `kind:"down"/"up"` + `code`（浏览器物理键位）→ keymap 查 VK 注入；
//!   修饰键自然成对转发，快捷键（Ctrl+A 等）在远端成立。
//! - `kind:"text"` + `text`（无修饰键的可打印字符）→ KEYEVENTF_UNICODE，
//!   绕开本地/远端键盘布局差异。
//! 剪贴板事件（`event:"clipboard"`）转交 clipboard 模块（粘贴通道）。
//!
//! 粘键防漏：控制者断连/全部 viewer 离开时 release_all_keys() 松开所有
//! 仍按住的键（Shared::disconnect 与 main 循环 viewers→0 调用）。已知
//! 限制：连接不断但租约被抢时 relay 不会通知 host（releaseControl 在
//! relay 终结），旧控制者的修饰键可能残留——按一次该键即恢复。

use serde_json::Value;
use std::collections::HashSet;
use std::sync::atomic::{AtomicI32, AtomicUsize, Ordering};
use std::sync::mpsc::{Receiver, SyncSender, TrySendError};
use std::sync::OnceLock;
use winapi::ctypes::c_int;
use winapi::shared::minwindef::UINT;
use winapi::um::winuser::{
    SendInput, SetCursorPos, INPUT, INPUT_KEYBOARD, INPUT_MOUSE, KEYBDINPUT,
    KEYEVENTF_EXTENDEDKEY, KEYEVENTF_KEYUP, KEYEVENTF_UNICODE, MOUSEEVENTF_LEFTDOWN,
    MOUSEEVENTF_LEFTUP, MOUSEEVENTF_MIDDLEDOWN, MOUSEEVENTF_MIDDLEUP, MOUSEEVENTF_RIGHTDOWN,
    MOUSEEVENTF_RIGHTUP, MOUSEEVENTF_WHEEL, MOUSEEVENTF_XDOWN, MOUSEEVENTF_XUP, MOUSEINPUT,
    WHEEL_DELTA, XBUTTON1, XBUTTON2,
};

use crate::keymap;

// web 端坐标在编码分辨率空间（等比降采样后 ≠ 原生桌面分辨率），注入前
// 换算回原生物理像素。采集分辨率在 host 生命周期内不变（变更即重启），
// 用原子量保存即可。
static NATIVE_W: AtomicUsize = AtomicUsize::new(0);
static NATIVE_H: AtomicUsize = AtomicUsize::new(0);
static ENC_W: AtomicUsize = AtomicUsize::new(0);
static ENC_H: AtomicUsize = AtomicUsize::new(0);
// 被采集显示器在虚拟屏中的原点：SetCursorPos/GetCursorInfo 都是虚拟屏绝对
// 坐标，副屏（origin≠0,0）不加减原点会注入/绘制到错误位置。
static ORIGIN_X: AtomicI32 = AtomicI32::new(0);
static ORIGIN_Y: AtomicI32 = AtomicI32::new(0);

// ---- injector：专用注入线程（安全桌面跟随） ----

/// 注入动作（tokio 侧解析换算完毕的最终形态；injector 串行执行）。
/// SyncSender/Receiver：有界（256）——注入端过载时丢弃新事件并告警，
/// 绝不反压阻塞 QUIC 控制分发（旧事件的时序价值高于新事件的完整性）。
enum Op {
    Cursor(i32, i32),
    Mouse(u32, i32),
    Key(u16, bool, bool),
    Unicode(u16, bool),
    /// 松开全部按住的键（控制权丢失）
    ReleaseAll,
}

static TX: OnceLock<SyncSender<Op>> = OnceLock::new();

/// injector 通道（懒启动线程，host 生命周期单例）。
fn tx() -> SyncSender<Op> {
    let tx = TX.get_or_init(|| {
        // 256 缓冲：覆盖 wheel/text 突发；满则 try_send 丢弃（见上）。
        let (tx, rx) = std::sync::mpsc::sync_channel(256);
        std::thread::Builder::new()
            .name("xnc-input".into())
            .spawn(move || injector_loop(rx))
            .expect("spawn input injector");
        tx
    });
    tx.clone()
}

fn send_op(op: Op) {
    if let Err(TrySendError::Full(_)) | Err(TrySendError::Disconnected(_)) = tx().try_send(op) {
        tracing::debug!("input injector queue full/disconnected, dropping event");
    }
}

/// injector 主循环：跟随输入桌面（切换即重锚 + 记日志）→ 串行注入。
/// 桌面检查每批事件前做一次 + 空闲时 200ms 轮询（登录界面无事件时也要
/// 跟随到 Winlogon，否则首个按键丢失）。
fn injector_loop(rx: Receiver<Op>) {
    let mut bound = crate::desktop::bind_thread_to_input_desktop()
        .unwrap_or_else(|_| crate::desktop::DEFAULT_DESKTOP.to_string());
    let mut pressed: HashSet<(u16, bool)> = HashSet::new();
    let mut last_check = std::time::Instant::now();
    loop {
        match rx.recv_timeout(std::time::Duration::from_millis(200)) {
            Ok(op) => {
                maybe_follow(&mut bound, &mut last_check);
                inject(op, &mut pressed);
                // 批量排空（同一 tick 的事件共享一次桌面检查）。
                while let Ok(op) = rx.try_recv() {
                    inject(op, &mut pressed);
                }
            }
            Err(std::sync::mpsc::RecvTimeoutError::Timeout) => {
                maybe_follow(&mut bound, &mut last_check);
            }
            Err(std::sync::mpsc::RecvTimeoutError::Disconnected) => {
                return; // 发送端全 drop（进程拆除）
            }
        }
    }
}

/// 输入桌面跟随：名字变化才重锚（重锚含 SetThreadDesktop，保守调用）。
fn maybe_follow(bound: &mut String, last_check: &mut std::time::Instant) {
    if last_check.elapsed() < std::time::Duration::from_millis(100) {
        return; // 限频：高频事件批不重复查询
    }
    *last_check = std::time::Instant::now();
    if let Ok(name) = crate::desktop::input_desktop_name() {
        if !name.eq_ignore_ascii_case(bound) {
            match crate::desktop::bind_thread_to_input_desktop() {
                Ok(new) => {
                    tracing::warn!(from = %bound, to = %new, "injector followed input desktop");
                    *bound = new;
                }
                Err(e) => tracing::warn!(?e, "injector desktop rebind failed"),
            }
        }
    }
}

/// 执行一条注入动作（含粘键表维护）。
fn inject(op: Op, pressed: &mut HashSet<(u16, bool)>) {
    match op {
        Op::Cursor(x, y) => unsafe {
            if SetCursorPos(x, y) == 0 {
                tracing::debug!(x, y, "SetCursorPos failed");
            }
        },
        Op::Mouse(flags, data) => send_mouse(flags, data),
        Op::Key(vk, extended, up) => {
            send_key(vk, extended, up);
            if up {
                pressed.remove(&(vk, extended));
            } else {
                pressed.insert((vk, extended));
            }
        }
        Op::Unicode(unit, up) => send_unicode(unit, up),
        Op::ReleaseAll => {
            if !pressed.is_empty() {
                tracing::info!(n = pressed.len(), "releasing held keys after control loss");
            }
            for &(vk, extended) in pressed.iter() {
                send_key(vk, extended, true);
            }
            pressed.clear();
        }
    }
}

/// 采集管线启动时登记两套分辨率与显示器原点（run_pipeline 调用）。
pub fn set_viewport(native: (usize, usize), encoded: (usize, usize), origin: (i32, i32)) {
    NATIVE_W.store(native.0, Ordering::Relaxed);
    NATIVE_H.store(native.1, Ordering::Relaxed);
    ENC_W.store(encoded.0, Ordering::Relaxed);
    ENC_H.store(encoded.1, Ordering::Relaxed);
    ORIGIN_X.store(origin.0, Ordering::Relaxed);
    ORIGIN_Y.store(origin.1, Ordering::Relaxed);
}

/// 编码空间坐标 → 被采集显示器的原生物理像素（像素中心对齐 + 边界钳制
/// + 虚拟屏原点平移）。
fn to_native(x: f64, y: f64) -> (i32, i32) {
    let (nw, nh, ew, eh) = (
        NATIVE_W.load(Ordering::Relaxed) as f64,
        NATIVE_H.load(Ordering::Relaxed) as f64,
        ENC_W.load(Ordering::Relaxed) as f64,
        ENC_H.load(Ordering::Relaxed) as f64,
    );
    let (ox, oy) = (
        ORIGIN_X.load(Ordering::Relaxed),
        ORIGIN_Y.load(Ordering::Relaxed),
    );
    if ew <= 0.0 || eh <= 0.0 {
        return (x as i32 + ox, y as i32 + oy); // 未登记（直连调试形态）——按原样注入
    }
    let sx = nw / ew;
    let sy = nh / eh;
    let nx = ((x + 0.5) * sx).floor().clamp(0.0, (nw - 1.0).max(0.0));
    let ny = ((y + 0.5) * sy).floor().clamp(0.0, (nh - 1.0).max(0.0));
    (nx as i32 + ox, ny as i32 + oy)
}

/// 处理 web 端发来的 input 控制消息（docs/proto.md §2 控制面扩展）。
/// 出错只告警不中断——注入失败不应影响媒体流。
pub fn handle(v: &Value) {
    match v["event"].as_str().unwrap_or("") {
        "mouse" => handle_mouse(v),
        "keyboard" => handle_keyboard(v),
        "clipboard" => crate::clipboard::on_viewer_chunk(v),
        _ => {}
    }
}

fn handle_mouse(v: &Value) {
    let kind = v["kind"].as_str().unwrap_or("");
    match kind {
        "move" => {
            let (Some(x), Some(y)) = (v["x"].as_f64(), v["y"].as_f64()) else { return };
            let (nx, ny) = to_native(x, y);
            send_op(Op::Cursor(nx, ny));
        }
        "down" | "up" => {
            let down = kind == "down";
            let button = v["button"].as_i64().unwrap_or(0); // 0=左 1=中 2=右 3/4=X1/X2
            let flags = match (button, down) {
                (0, true) => MOUSEEVENTF_LEFTDOWN,
                (0, false) => MOUSEEVENTF_LEFTUP,
                (1, true) => MOUSEEVENTF_MIDDLEDOWN,
                (1, false) => MOUSEEVENTF_MIDDLEUP,
                (2, true) => MOUSEEVENTF_RIGHTDOWN,
                (2, false) => MOUSEEVENTF_RIGHTUP,
                (3, true) => MOUSEEVENTF_XDOWN,
                (3, false) => MOUSEEVENTF_XUP,
                _ => return,
            };
            let mouse_data: UINT = if button == 3 { XBUTTON1 as UINT } else if button == 4 { XBUTTON2 as UINT } else { 0 };
            send_op(Op::Mouse(flags, mouse_data as i32));
        }
        "wheel" => {
            // dy/dx 为“格”数（浏览器 deltaY/deltaX / 100）；换算 WHEEL_DELTA。
            // Windows：WHEEL 正值=向上滚，HWHEEL 正值=向右滚；浏览器正值分别为向下/向右。
            let dy = v["dy"].as_f64().unwrap_or(0.0);
            if dy != 0.0 {
                send_op(Op::Mouse(MOUSEEVENTF_WHEEL, (-dy * WHEEL_DELTA as f64) as i32));
            }
            let dx = v["dx"].as_f64().unwrap_or(0.0);
            if dx != 0.0 {
                const MOUSEEVENTF_HWHEEL: u32 = 0x0800; // winapi 0.3 未导出
                send_op(Op::Mouse(MOUSEEVENTF_HWHEEL, (dx * WHEEL_DELTA as f64) as i32));
            }
        }
        _ => {}
    }
}

fn handle_keyboard(v: &Value) {
    match v["kind"].as_str().unwrap_or("") {
        "down" | "up" => {
            let Some(code) = v["code"].as_str() else { return };
            let Some(def) = keymap::lookup(code) else {
                tracing::debug!(code, "unmapped key code ignored");
                return;
            };
            let up = v["kind"].as_str() == Some("up");
            send_op(Op::Key(def.vk, def.extended, up));
        }
        "text" => {
            // Unicode 文本：按 UTF-16 码元逐个 down+up（代理对拆成两个
            // UNICODE 事件——enigo key_sequence 同款；chars().as u16 会把
            // 增补平面字符截断）。KEYEVENTF_UNICODE 不受布局影响，也不携
            // 带修饰键状态（web 端仅在无修饰键时走此路径）。
            let Some(text) = v["text"].as_str() else { return };
            for unit in text.encode_utf16() {
                send_op(Op::Unicode(unit, false));
                send_op(Op::Unicode(unit, true));
            }
        }
        _ => {}
    }
}

/// 松开全部仍按住的键（Shared::disconnect 与 viewers→0 时调用）。
/// 经 injector 执行（粘键表在其线程内）。
pub fn release_all_keys() {
    send_op(Op::ReleaseAll);
}

fn send_mouse(flags: u32, mouse_data: i32) {
    unsafe {
        let mut input: INPUT = std::mem::zeroed();
        input.type_ = INPUT_MOUSE;
        // dwFlags 用 MOUSEEVENTF_ABSOLUTE 无需（相对事件）；坐标字段忽略
        *input.u.mi_mut() = MOUSEINPUT {
            dx: 0,
            dy: 0,
            mouseData: mouse_data as u32,
            dwFlags: flags,
            time: 0,
            dwExtraInfo: 0,
        };
        if SendInput(1, &mut input, std::mem::size_of::<INPUT>() as c_int) != 1 {
            tracing::debug!("SendInput failed");
        }
    }
}

/// VK 模式键注入（enigo win_impl 同款）：wVk 与 wScan 同时填充——扫描码
/// 取自前台窗口线程的键盘布局（MAPVK_VK_TO_VSC_EX，带扩展前缀），扫描码
/// 高字节 0xE0/0xE1 置 KEYEVENTF_EXTENDEDKEY；与 keymap 表的静态扩展标记
/// 取并集（e.code 已区分左右修饰键，表标记覆盖 _EX 缺失的形态）。
fn send_key(vk: u16, extended: bool, up: bool) {
    let (scan, scan_ext) = unsafe {
        let fg = winapi::um::winuser::GetForegroundWindow();
        let tid = winapi::um::winuser::GetWindowThreadProcessId(fg, std::ptr::null_mut());
        let layout = winapi::um::winuser::GetKeyboardLayout(tid);
        let scan = winapi::um::winuser::MapVirtualKeyExW(
            vk as UINT,
            winapi::um::winuser::MAPVK_VK_TO_VSC_EX,
            layout,
        ) as u32;
        (scan, scan >> 8 == 0xE0 || scan >> 8 == 0xE1)
    };
    let mut flags = 0u32;
    if extended || scan_ext {
        flags |= KEYEVENTF_EXTENDEDKEY;
    }
    if up {
        flags |= KEYEVENTF_KEYUP;
    }
    unsafe {
        let mut input: INPUT = std::mem::zeroed();
        input.type_ = INPUT_KEYBOARD;
        *input.u.ki_mut() = KEYBDINPUT {
            wVk: vk,
            wScan: scan as u16,
            dwFlags: flags,
            time: 0,
            dwExtraInfo: 0,
        };
        if SendInput(1, &mut input, std::mem::size_of::<INPUT>() as c_int) != 1 {
            tracing::debug!(vk, "SendInput key failed");
        }
    }
}

/// Unicode 码元注入（KEYEVENTF_UNICODE；UTF-16 码元放 wScan）。
fn send_unicode(unit: u16, up: bool) {
    let mut flags = KEYEVENTF_UNICODE;
    if up {
        flags |= KEYEVENTF_KEYUP;
    }
    unsafe {
        let mut input: INPUT = std::mem::zeroed();
        input.type_ = INPUT_KEYBOARD;
        *input.u.ki_mut() = KEYBDINPUT {
            wVk: 0,
            wScan: unit,
            dwFlags: flags,
            time: 0,
            dwExtraInfo: 0,
        };
        if SendInput(1, &mut input, std::mem::size_of::<INPUT>() as c_int) != 1 {
            tracing::debug!("SendInput unicode failed");
        }
    }
}
