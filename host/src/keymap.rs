//! 键位映射：浏览器 KeyboardEvent.code（物理键位，W3C UI Events）→
//! Windows 虚拟键码（VK_*）。
//!
//! 注入采用 VK 模式（不带 KEYEVENTF_SCANCODE）：Windows 按远端当前键盘
//! 布局解释 VK，物理键位语义与 e.code 对齐（远端 CN/US 布局的字母数字
//! 区一致）。扩展键（小键盘回车/除号、右 Ctrl/Alt、导航键区等）必须置
//! KEYEVENTF_EXTENDEDKEY，否则 Windows 会注入成左区键（历史坑：右 Alt
//! 与 AltGr）。
//!
//! 浏览器保留键（Ctrl+W/T、Alt+Tab、F12 等）根本到不了页面，本表不含；
//! IME 组合键（isComposing）由 web 侧跳过，中文文本走剪贴板粘贴路径。

use winapi::ctypes::c_int;
use winapi::um::winuser::*;

/// 单条映射：vk 为 0 表示该键不注入（未知/保留键）。
#[derive(Clone, Copy)]
pub struct KeyDef {
    pub vk: u16,
    pub extended: bool,
}

const fn k(vk: c_int) -> KeyDef {
    KeyDef { vk: vk as u16, extended: false }
}

const fn kx(vk: c_int) -> KeyDef {
    KeyDef { vk: vk as u16, extended: true }
}

// Windows 头文件把字母/数字 VK（0x30-0x39、0x41-0x5A）视为保留字面量，
// winapi 0.3 不导出——本地补齐。
const VK_0: c_int = 0x30;
const VK_1: c_int = 0x31;
const VK_2: c_int = 0x32;
const VK_3: c_int = 0x33;
const VK_4: c_int = 0x34;
const VK_5: c_int = 0x35;
const VK_6: c_int = 0x36;
const VK_7: c_int = 0x37;
const VK_8: c_int = 0x38;
const VK_9: c_int = 0x39;
const VK_A: c_int = 0x41;
const VK_B: c_int = 0x42;
const VK_C: c_int = 0x43;
const VK_D: c_int = 0x44;
const VK_E: c_int = 0x45;
const VK_F: c_int = 0x46;
const VK_G: c_int = 0x47;
const VK_H: c_int = 0x48;
const VK_I: c_int = 0x49;
const VK_J: c_int = 0x4A;
const VK_K: c_int = 0x4B;
const VK_L: c_int = 0x4C;
const VK_M: c_int = 0x4D;
const VK_N: c_int = 0x4E;
const VK_O: c_int = 0x4F;
const VK_P: c_int = 0x50;
const VK_Q: c_int = 0x51;
const VK_R: c_int = 0x52;
const VK_S: c_int = 0x53;
const VK_T: c_int = 0x54;
const VK_U: c_int = 0x55;
const VK_V: c_int = 0x56;
const VK_W: c_int = 0x57;
const VK_X: c_int = 0x58;
const VK_Y: c_int = 0x59;
const VK_Z: c_int = 0x5A;

/// code → KeyDef 查表；未命中返回 None（安全忽略）。
pub fn lookup(code: &str) -> Option<KeyDef> {
    KEYMAP
        .iter()
        .find(|(c, _)| *c == code)
        .map(|(_, d)| *d)
}

/// 全表（code 唯一性由单测保证）。
pub static KEYMAP: &[(&str, KeyDef)] = &[
    // ---- 字母（物理键位，VK 与字母同名）----
    ("KeyA", k(VK_A)), ("KeyB", k(VK_B)), ("KeyC", k(VK_C)), ("KeyD", k(VK_D)),
    ("KeyE", k(VK_E)), ("KeyF", k(VK_F)), ("KeyG", k(VK_G)), ("KeyH", k(VK_H)),
    ("KeyI", k(VK_I)), ("KeyJ", k(VK_J)), ("KeyK", k(VK_K)), ("KeyL", k(VK_L)),
    ("KeyM", k(VK_M)), ("KeyN", k(VK_N)), ("KeyO", k(VK_O)), ("KeyP", k(VK_P)),
    ("KeyQ", k(VK_Q)), ("KeyR", k(VK_R)), ("KeyS", k(VK_S)), ("KeyT", k(VK_T)),
    ("KeyU", k(VK_U)), ("KeyV", k(VK_V)), ("KeyW", k(VK_W)), ("KeyX", k(VK_X)),
    ("KeyY", k(VK_Y)), ("KeyZ", k(VK_Z)),
    // ---- 数字行 ----
    ("Digit0", k(VK_0)), ("Digit1", k(VK_1)), ("Digit2", k(VK_2)), ("Digit3", k(VK_3)),
    ("Digit4", k(VK_4)), ("Digit5", k(VK_5)), ("Digit6", k(VK_6)), ("Digit7", k(VK_7)),
    ("Digit8", k(VK_8)), ("Digit9", k(VK_9)),
    // ---- OEM 标点（US 物理位 → OEM VK）----
    ("Backquote", k(VK_OEM_3)),      // ` ~
    ("Minus", k(VK_OEM_MINUS)),      // - _
    ("Equal", k(VK_OEM_PLUS)),       // = +
    ("BracketLeft", k(VK_OEM_4)),    // [ {
    ("BracketRight", k(VK_OEM_6)),   // ] }
    ("Backslash", k(VK_OEM_5)),      // \ |
    ("Semicolon", k(VK_OEM_1)),      // ; :
    ("Quote", k(VK_OEM_7)),          // ' "
    ("Comma", k(VK_OEM_COMMA)),      // , <
    ("Period", k(VK_OEM_PERIOD)),    // . >
    ("Slash", k(VK_OEM_2)),          // / ?
    ("IntlBackslash", k(VK_OEM_102)), // 102nd key（欧式 \<>）
    // ---- 编辑/导航（插入删除六键区 + 方向键为扩展键）----
    ("Enter", k(VK_RETURN)),
    ("Tab", k(VK_TAB)),
    ("Space", k(VK_SPACE)),
    ("Backspace", k(VK_BACK)),
    ("Escape", k(VK_ESCAPE)),
    ("Delete", kx(VK_DELETE)),
    ("Insert", kx(VK_INSERT)),
    ("Home", kx(VK_HOME)),
    ("End", kx(VK_END)),
    ("PageUp", kx(VK_PRIOR)),
    ("PageDown", kx(VK_NEXT)),
    ("ArrowUp", kx(VK_UP)),
    ("ArrowDown", kx(VK_DOWN)),
    ("ArrowLeft", kx(VK_LEFT)),
    ("ArrowRight", kx(VK_RIGHT)),
    ("PrintScreen", kx(VK_SNAPSHOT)),
    ("ScrollLock", k(VK_SCROLL)),
    ("Pause", k(VK_PAUSE)),
    ("ContextMenu", kx(VK_APPS)),
    ("CapsLock", k(VK_CAPITAL)),
    ("NumLock", kx(VK_NUMLOCK)),
    ("Clear", k(VK_CLEAR)),
    // ---- 修饰键（左右区分；右 Ctrl/Alt 为扩展键）----
    ("ShiftLeft", k(VK_LSHIFT)),
    ("ShiftRight", k(VK_RSHIFT)),
    ("ControlLeft", k(VK_LCONTROL)),
    ("ControlRight", kx(VK_RCONTROL)),
    ("AltLeft", k(VK_LMENU)),
    ("AltRight", kx(VK_RMENU)),
    ("MetaLeft", k(VK_LWIN)),
    ("MetaRight", kx(VK_RWIN)),
    // ---- 小键盘（Enter/除号为扩展键）----
    ("Numpad0", k(VK_NUMPAD0)), ("Numpad1", k(VK_NUMPAD1)), ("Numpad2", k(VK_NUMPAD2)),
    ("Numpad3", k(VK_NUMPAD3)), ("Numpad4", k(VK_NUMPAD4)), ("Numpad5", k(VK_NUMPAD5)),
    ("Numpad6", k(VK_NUMPAD6)), ("Numpad7", k(VK_NUMPAD7)), ("Numpad8", k(VK_NUMPAD8)),
    ("Numpad9", k(VK_NUMPAD9)),
    ("NumpadMultiply", k(VK_MULTIPLY)),
    ("NumpadAdd", k(VK_ADD)),
    ("NumpadSubtract", k(VK_SUBTRACT)),
    ("NumpadDecimal", k(VK_DECIMAL)),
    ("NumpadDivide", kx(VK_DIVIDE)),
    ("NumpadEnter", kx(VK_RETURN)),
    // ---- 功能键 ----
    ("F1", k(VK_F1)), ("F2", k(VK_F2)), ("F3", k(VK_F3)), ("F4", k(VK_F4)),
    ("F5", k(VK_F5)), ("F6", k(VK_F6)), ("F7", k(VK_F7)), ("F8", k(VK_F8)),
    ("F9", k(VK_F9)), ("F10", k(VK_F10)), ("F11", k(VK_F11)), ("F12", k(VK_F12)),
    ("F13", k(VK_F13)), ("F14", k(VK_F14)), ("F15", k(VK_F15)), ("F16", k(VK_F16)),
    ("F17", k(VK_F17)), ("F18", k(VK_F18)), ("F19", k(VK_F19)), ("F20", k(VK_F20)),
    ("F21", k(VK_F21)), ("F22", k(VK_F22)), ("F23", k(VK_F23)), ("F24", k(VK_F24)),
    // ---- 媒体/浏览器键（常见子集）----
    ("AudioVolumeMute", k(VK_VOLUME_MUTE)),
    ("AudioVolumeUp", k(VK_VOLUME_UP)),
    ("AudioVolumeDown", k(VK_VOLUME_DOWN)),
    ("MediaTrackNext", k(VK_MEDIA_NEXT_TRACK)),
    ("MediaTrackPrevious", k(VK_MEDIA_PREV_TRACK)),
    ("MediaPlayPause", k(VK_MEDIA_PLAY_PAUSE)),
    ("BrowserBack", k(VK_BROWSER_BACK)),
    ("BrowserForward", k(VK_BROWSER_FORWARD)),
];

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn all_vk_nonzero() {
        assert!(!KEYMAP.is_empty());
        for (code, def) in KEYMAP {
            assert!(def.vk != 0, "zero vk for {code}");
        }
    }

    #[test]
    fn codes_unique() {
        let mut seen = std::collections::HashSet::new();
        for (code, _) in KEYMAP {
            assert!(seen.insert(*code), "duplicate code {code}");
        }
    }

    #[test]
    fn extended_set_matches_windows_semantics() {
        // 右修饰键与导航/编辑键区必须带扩展位（漏掉会注入成左区等价键）
        for code in [
            "ArrowUp", "ArrowDown", "ArrowLeft", "ArrowRight", "Insert", "Delete",
            "Home", "End", "PageUp", "PageDown", "ControlRight", "AltRight",
            "MetaRight", "NumpadDivide", "NumpadEnter", "NumLock",
            "ContextMenu", "PrintScreen",
        ] {
            let def = lookup(code).unwrap_or_else(|| panic!("{code} expected in map"));
            assert!(def.extended, "{code} should be extended");
        }
        // 左修饰键不得带扩展位
        for code in ["ShiftLeft", "ControlLeft", "AltLeft", "MetaLeft"] {
            let def = lookup(code).unwrap();
            assert!(!def.extended, "{code} should NOT be extended");
        }
    }

    #[test]
    fn unknown_code_is_none() {
        assert!(lookup("IntlRo").is_none());
        assert!(lookup("").is_none());
        assert!(lookup("not-a-key").is_none());
    }
}
