//! 输入注入：鼠标（移动/按键/滚轮）+ 键盘（键位/Unicode 文本）—— Windows
//! SendInput / SetCursorPos。
//!
//! 对应 rustdesk 的 enigo→SendInput 路径（win_impl.rs）：绝对坐标模式（远程桌面
//! 风格），注入坐标为显示器物理像素（与 DXGI 采集坐标系一致，DPI 缩放无关）。
//! 在交互会话内运行（部署方式保证了这一点，见 deploy-host.ps1）。
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
use std::sync::atomic::{AtomicBool, AtomicI32, AtomicUsize, Ordering};
use std::sync::Mutex;
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
// GDI→DXGI 回探提示：输入注入（SendInput/SetCursorPos 是真实输入事件）
// 会重置电源空闲计时、大概率唤醒显示器，DXGI 通常随之恢复——采集循环
// 每 tick 检查此提示，GDI 驻留时立即试切，免去最长 60s 的 GDI 驻留
//（capture.rs DXGI_RETRY_INTERVAL）。
static DXGI_RETRY_HINT: AtomicBool = AtomicBool::new(false);

/// 采集循环每 tick 取走输入提示（30fps tick 粒度足够）。
pub fn take_dxgi_retry_hint() -> bool {
    DXGI_RETRY_HINT.swap(false, Ordering::SeqCst)
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
    DXGI_RETRY_HINT.store(true, Ordering::SeqCst);
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
            unsafe {
                if SetCursorPos(nx, ny) == 0 {
                    tracing::debug!(nx, ny, "SetCursorPos failed");
                }
            }
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
            send_mouse(flags, mouse_data as i32);
        }
        "wheel" => {
            // dy/dx 为“格”数（浏览器 deltaY/deltaX / 100）；换算 WHEEL_DELTA。
            // Windows：WHEEL 正值=向上滚，HWHEEL 正值=向右滚；浏览器正值分别为向下/向右。
            let dy = v["dy"].as_f64().unwrap_or(0.0);
            if dy != 0.0 {
                send_mouse(MOUSEEVENTF_WHEEL, (-dy * WHEEL_DELTA as f64) as i32);
            }
            let dx = v["dx"].as_f64().unwrap_or(0.0);
            if dx != 0.0 {
                const MOUSEEVENTF_HWHEEL: u32 = 0x0800; // winapi 0.3 未导出
                send_mouse(MOUSEEVENTF_HWHEEL, (dx * WHEEL_DELTA as f64) as i32);
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
            send_key(def.vk, def.extended, up);
            track_pressed(def.vk, def.extended, !up);
        }
        "text" => {
            // Unicode 文本：按 UTF-16 码元逐个 down+up（代理对拆成两个
            // UNICODE 事件——enigo key_sequence 同款；chars().as u16 会把
            // 增补平面字符截断）。KEYEVENTF_UNICODE 不受布局影响，也不携
            // 带修饰键状态（web 端仅在无修饰键时走此路径）。
            let Some(text) = v["text"].as_str() else { return };
            for unit in text.encode_utf16() {
                send_unicode(unit, false);
                send_unicode(unit, true);
            }
        }
        _ => {}
    }
}

// ---- 粘键防漏：记录当前按下的键，断连/无 viewer 时统一松开 ----

static PRESSED: Mutex<Option<HashSet<(u16, bool)>>> = Mutex::new(None);

fn track_pressed(vk: u16, extended: bool, down: bool) {
    let mut guard = PRESSED.lock().unwrap();
    let set = guard.get_or_insert_with(HashSet::new);
    if down {
        set.insert((vk, extended));
    } else {
        set.remove(&(vk, extended));
    }
}

/// 松开全部仍按住的键（Shared::disconnect 与 viewers→0 时调用）。
/// 键盘注入通道已断，直接 SendInput 抬键即可。
pub fn release_all_keys() {
    let mut guard = PRESSED.lock().unwrap();
    if let Some(set) = guard.as_mut() {
        if !set.is_empty() {
            tracing::info!(n = set.len(), "releasing held keys after control loss");
        }
        for &(vk, extended) in set.iter() {
            send_key(vk, extended, true);
        }
        set.clear();
    }
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
