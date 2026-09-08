//! 输入注入：鼠标（移动/按键/滚轮）—— Windows SendInput / SetCursorPos。
//!
//! 对应 rustdesk 的 enigo→SendInput 路径（win_impl.rs）：绝对坐标模式（远程桌面
//! 风格），注入坐标为显示器物理像素（与 DXGI 采集坐标系一致，DPI 缩放无关）。
//! 在交互会话内运行（部署方式保证了这一点，见 deploy-host.ps1）。

use serde_json::Value;
use std::sync::atomic::{AtomicUsize, Ordering};
use winapi::ctypes::c_int;
use winapi::shared::minwindef::UINT;
use winapi::um::winuser::{
    SendInput, SetCursorPos, INPUT, INPUT_MOUSE, MOUSEEVENTF_LEFTDOWN,
    MOUSEEVENTF_LEFTUP, MOUSEEVENTF_MIDDLEDOWN, MOUSEEVENTF_MIDDLEUP, MOUSEEVENTF_RIGHTDOWN,
    MOUSEEVENTF_RIGHTUP, MOUSEEVENTF_WHEEL, MOUSEEVENTF_XDOWN, MOUSEEVENTF_XUP, MOUSEINPUT,
    WHEEL_DELTA, XBUTTON1, XBUTTON2,
};

// web 端坐标在编码分辨率空间（等比降采样后 ≠ 原生桌面分辨率），注入前
// 换算回原生物理像素。采集分辨率在 host 生命周期内不变（变更即重启），
// 用原子量保存即可。
static NATIVE_W: AtomicUsize = AtomicUsize::new(0);
static NATIVE_H: AtomicUsize = AtomicUsize::new(0);
static ENC_W: AtomicUsize = AtomicUsize::new(0);
static ENC_H: AtomicUsize = AtomicUsize::new(0);

/// 采集管线启动时登记两套分辨率（run_pipeline 调用）。
pub fn set_viewport(native: (usize, usize), encoded: (usize, usize)) {
    NATIVE_W.store(native.0, Ordering::Relaxed);
    NATIVE_H.store(native.1, Ordering::Relaxed);
    ENC_W.store(encoded.0, Ordering::Relaxed);
    ENC_H.store(encoded.1, Ordering::Relaxed);
}

/// 编码空间坐标 → 原生物理像素（像素中心对齐 + 边界钳制）。
fn to_native(x: f64, y: f64) -> (i32, i32) {
    let (nw, nh, ew, eh) = (
        NATIVE_W.load(Ordering::Relaxed) as f64,
        NATIVE_H.load(Ordering::Relaxed) as f64,
        ENC_W.load(Ordering::Relaxed) as f64,
        ENC_H.load(Ordering::Relaxed) as f64,
    );
    if ew <= 0.0 || eh <= 0.0 {
        return (x as i32, y as i32); // 未登记（直连调试形态）——按原样注入
    }
    let sx = nw / ew;
    let sy = nh / eh;
    let nx = ((x + 0.5) * sx).floor().clamp(0.0, (nw - 1.0).max(0.0));
    let ny = ((y + 0.5) * sy).floor().clamp(0.0, (nh - 1.0).max(0.0));
    (nx as i32, ny as i32)
}

/// 处理 web 端发来的 input 控制消息（docs/proto.md §2 控制面扩展）。
/// 出错只告警不中断——注入失败不应影响媒体流。
pub fn handle(v: &Value) {
    if v["event"].as_str() != Some("mouse") {
        return;
    }
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
