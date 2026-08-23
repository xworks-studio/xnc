// keymap.go — KeyboardEvent.code → Windows Set 1 扫描码(+E0 extended 位)。
//
// 这是 web/src/pages/desktop/keymap.ts 的嵌入式 Go 移植,值逐字节一致
// (两表由 keymap_test.go 黄金样本 + 键数交叉锁定;漂移会在测试红)。
// 数据来源同 keymap.ts:公有域 IBM PC AT Set 1 make codes —— 即
// KEYEVENTF_SCANCODE + KEYEVENTF_EXTENDEDKEY 经 SendInput 消费的那个集合;
// CapsLock=0x3A / NumLock=0x45 与 native/desktop/input_manager.h 常量交叉。
//
// Pause 刻意缺席(E1 前缀序列,SendInput 扫描码空间无法表示)——
// lookup 返回 ok=false,调用方必须拒绝该步(绝不能发 scan=0,agent 会计
// invalid;见 agent/desktop/input.go handleInput)。
package main

// scanCode 是一个键的 Set 1 make code(不含 E0 前缀)+ extended 标记
// (右簇等以 E0 <scan> 传输)。
type scanCode struct {
	scan uint16
	ext  bool
}

func keyE(scan uint16) scanCode { return scanCode{scan: scan, ext: false} }
func keyX(scan uint16) scanCode { return scanCode{scan: scan, ext: true} }

// keyTable 与 keymap.ts 的 TABLE 条目一一对应(106 键)。
var keyTable = map[string]scanCode{
	"Escape": keyE(0x01),
	"Digit1": keyE(0x02), "Digit2": keyE(0x03), "Digit3": keyE(0x04), "Digit4": keyE(0x05),
	"Digit5": keyE(0x06), "Digit6": keyE(0x07), "Digit7": keyE(0x08), "Digit8": keyE(0x09),
	"Digit9": keyE(0x0a), "Digit0": keyE(0x0b),
	"Minus": keyE(0x0c), "Equal": keyE(0x0d), "Backspace": keyE(0x0e),
	"Tab": keyE(0x0f),
	"KeyQ": keyE(0x10), "KeyW": keyE(0x11), "KeyE": keyE(0x12), "KeyR": keyE(0x13), "KeyT": keyE(0x14),
	"KeyY": keyE(0x15), "KeyU": keyE(0x16), "KeyI": keyE(0x17), "KeyO": keyE(0x18), "KeyP": keyE(0x19),
	"BracketLeft": keyE(0x1a), "BracketRight": keyE(0x1b),
	"Enter": keyE(0x1c), "NumpadEnter": keyX(0x1c),
	"ControlLeft": keyE(0x1d), "ControlRight": keyX(0x1d),
	"KeyA": keyE(0x1e), "KeyS": keyE(0x1f), "KeyD": keyE(0x20), "KeyF": keyE(0x21), "KeyG": keyE(0x22),
	"KeyH": keyE(0x23), "KeyJ": keyE(0x24), "KeyK": keyE(0x25), "KeyL": keyE(0x26),
	"Semicolon": keyE(0x27), "Quote": keyE(0x28), "Backquote": keyE(0x29),
	"ShiftLeft": keyE(0x2a), "ShiftRight": keyE(0x36),
	"Backslash": keyE(0x2b), "IntlBackslash": keyE(0x56),
	"KeyZ": keyE(0x2c), "KeyX": keyE(0x2d), "KeyC": keyE(0x2e), "KeyV": keyE(0x2f), "KeyB": keyE(0x30),
	"KeyN": keyE(0x31), "KeyM": keyE(0x32),
	"Comma": keyE(0x33), "Period": keyE(0x34), "Slash": keyE(0x35),
	"NumpadDivide": keyX(0x35),
	"PrintScreen": keyX(0x37), "NumpadMultiply": keyE(0x37),
	"AltLeft": keyE(0x38), "AltRight": keyX(0x38),
	"Space": keyE(0x39), "CapsLock": keyE(0x3a),
	"F1": keyE(0x3b), "F2": keyE(0x3c), "F3": keyE(0x3d), "F4": keyE(0x3e), "F5": keyE(0x3f),
	"F6": keyE(0x40), "F7": keyE(0x41), "F8": keyE(0x42), "F9": keyE(0x43), "F10": keyE(0x44),
	"F11": keyE(0x57), "F12": keyE(0x58),
	"NumLock": keyE(0x45), "ScrollLock": keyE(0x46),
	"Numpad7": keyE(0x47), "Numpad8": keyE(0x48), "Numpad9": keyE(0x49), "NumpadSubtract": keyE(0x4a),
	"Numpad4": keyE(0x4b), "Numpad5": keyE(0x4c), "Numpad6": keyE(0x4d), "NumpadAdd": keyE(0x4e),
	"Numpad1": keyE(0x4f), "Numpad2": keyE(0x50), "Numpad3": keyE(0x51),
	"Numpad0": keyE(0x52), "NumpadDecimal": keyE(0x53),
	"IntlRo": keyE(0x73), "IntlYen": keyE(0x7d),
	"Home": keyX(0x47), "ArrowUp": keyX(0x48), "PageUp": keyX(0x49),
	"ArrowLeft": keyX(0x4b), "ArrowRight": keyX(0x4d),
	"End": keyX(0x4f), "ArrowDown": keyX(0x50), "PageDown": keyX(0x51),
	"Insert": keyX(0x52), "Delete": keyX(0x53),
	"MetaLeft": keyX(0x5b), "MetaRight": keyX(0x5c), "ContextMenu": keyX(0x5d),
}

// keymapLookup 查表;未知 code 返回 ok=false(调用方拒绝该步——绝不发
// scan=0)。
func keymapLookup(code string) (scanCode, bool) {
	sc, ok := keyTable[code]
	return sc, ok
}
