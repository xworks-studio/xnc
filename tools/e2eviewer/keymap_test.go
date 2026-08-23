package main

import "testing"

// TestKeymapParity — 黄金样本锁 keymap.go 与 web/src/pages/desktop/keymap.ts
// 逐字节一致(样本独立手写自 keymap.ts 源,覆盖每个簇 + extended 变体)。
func TestKeymapParity(t *testing.T) {
	golden := []struct {
		code string
		scan uint16
		ext  bool
	}{
		// 字母簇
		{"KeyA", 0x1e, false}, {"KeyM", 0x32, false}, {"KeyZ", 0x2c, false},
		{"KeyP", 0x19, false}, {"KeyL", 0x26, false},
		// 数字行 + 标点
		{"Digit0", 0x0b, false}, {"Digit9", 0x0a, false},
		{"Minus", 0x0c, false}, {"Equal", 0x0d, false}, {"Backspace", 0x0e, false},
		{"BracketLeft", 0x1a, false}, {"Quote", 0x28, false}, {"Backquote", 0x29, false},
		{"Backslash", 0x2b, false}, {"Comma", 0x33, false}, {"Period", 0x34, false},
		{"Slash", 0x35, false}, {"Semicolon", 0x27, false},
		// 功能键 + 控制键
		{"Escape", 0x01, false}, {"Tab", 0x0f, false}, {"Space", 0x39, false},
		{"Enter", 0x1c, false}, {"NumpadEnter", 0x1c, true},
		{"F1", 0x3b, false}, {"F10", 0x44, false}, {"F11", 0x57, false}, {"F12", 0x58, false},
		{"PrintScreen", 0x37, true},
		// 修饰键(左右簇 extended 语义)
		{"ShiftLeft", 0x2a, false}, {"ShiftRight", 0x36, false},
		{"ControlLeft", 0x1d, false}, {"ControlRight", 0x1d, true},
		{"AltLeft", 0x38, false}, {"AltRight", 0x38, true},
		{"MetaLeft", 0x5b, true}, {"MetaRight", 0x5c, true}, {"ContextMenu", 0x5d, true},
		// 锁定键(native input_manager.h 常量交叉)
		{"CapsLock", 0x3a, false}, {"NumLock", 0x45, false}, {"ScrollLock", 0x46, false},
		// 编辑/导航簇(全 extended)
		{"Home", 0x47, true}, {"ArrowUp", 0x48, true}, {"PageUp", 0x49, true},
		{"ArrowLeft", 0x4b, true}, {"ArrowRight", 0x4d, true},
		{"End", 0x4f, true}, {"ArrowDown", 0x50, true}, {"PageDown", 0x51, true},
		{"Insert", 0x52, true}, {"Delete", 0x53, true},
		// Numpad(除 Enter/Divide 外非 extended;+/* 差一位易错)
		{"Numpad0", 0x52, false}, {"Numpad1", 0x4f, false}, {"Numpad3", 0x51, false},
		{"NumpadDecimal", 0x53, false}, {"NumpadAdd", 0x4e, false},
		{"NumpadSubtract", 0x4a, false}, {"NumpadMultiply", 0x37, false},
		{"NumpadDivide", 0x35, true},
		// Intl(JIS 布局)
		{"IntlBackslash", 0x56, false}, {"IntlRo", 0x73, false}, {"IntlYen", 0x7d, false},
	}
	for _, g := range golden {
		sc, ok := keymapLookup(g.code)
		if !ok {
			t.Errorf("keymapLookup(%q): not found", g.code)
			continue
		}
		if sc.scan != g.scan || sc.ext != g.ext {
			t.Errorf("keymapLookup(%q) = {scan:%#x ext:%v}, want {scan:%#x ext:%v}",
				g.code, sc.scan, sc.ext, g.scan, g.ext)
		}
	}
	// 键数锁:keymap.ts TABLE 恰 106 键(增删键 = 两表漂移,须双向同步)。
	if got := len(keyTable); got != 106 {
		t.Errorf("keyTable size = %d, want 106 (parity with keymap.ts)", got)
	}
	// 未知 code 必须安全失败(Pause = E1 序列,刻意缺席)。
	for _, bad := range []string{"", "Pause", "KeyA ", "keya", "AudioVolumeUp", "Unidentified"} {
		if _, ok := keymapLookup(bad); ok {
			t.Errorf("keymapLookup(%q) should miss", bad)
		}
	}
}
