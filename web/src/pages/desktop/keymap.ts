/**
 * KeyboardEvent.code → Windows Set 1 scancode (+ extended/E0 flag).
 *
 * Public-domain scancode data (standard IBM PC AT Set 1 make codes, the
 * set KEYEVENTF_SCANCODE + KEYEVENTF_EXTENDEDKEY consumes via SendInput).
 * Cross-checked against native/desktop/input_manager.h constants
 * (CapsLock=0x3A, NumLock=0x45, extended bit rides separately).
 *
 * ~110 keys: KeyA–Z, Digit0–9, F1–F12, arrows, L/R modifiers, editing
 * cluster, punctuation and Numpad (extended where Set 1 says E0).
 *
 * Pause is intentionally absent: its make sequence is E1-prefixed and has
 * no Set 1 scan representation SendInput accepts — it falls through to
 * the unknown-code path (console.debug + ignore).
 */

export interface ScanCode {
  /** Set 1 make code without the E0 prefix. */
  scan: number;
  /** true when the key transmits as E0 <scan> (right cluster, etc.). */
  ext: boolean;
}

const E = (scan: number): ScanCode => ({ scan, ext: false });
const X = (scan: number): ScanCode => ({ scan, ext: true });

const TABLE: Record<string, ScanCode> = {
  Escape: E(0x01),
  Digit1: E(0x02), Digit2: E(0x03), Digit3: E(0x04), Digit4: E(0x05),
  Digit5: E(0x06), Digit6: E(0x07), Digit7: E(0x08), Digit8: E(0x09),
  Digit9: E(0x0a), Digit0: E(0x0b),
  Minus: E(0x0c), Equal: E(0x0d), Backspace: E(0x0e),
  Tab: E(0x0f),
  KeyQ: E(0x10), KeyW: E(0x11), KeyE: E(0x12), KeyR: E(0x13), KeyT: E(0x14),
  KeyY: E(0x15), KeyU: E(0x16), KeyI: E(0x17), KeyO: E(0x18), KeyP: E(0x19),
  BracketLeft: E(0x1a), BracketRight: E(0x1b),
  Enter: E(0x1c), NumpadEnter: X(0x1c),
  ControlLeft: E(0x1d), ControlRight: X(0x1d),
  KeyA: E(0x1e), KeyS: E(0x1f), KeyD: E(0x20), KeyF: E(0x21), KeyG: E(0x22),
  KeyH: E(0x23), KeyJ: E(0x24), KeyK: E(0x25), KeyL: E(0x26),
  Semicolon: E(0x27), Quote: E(0x28), Backquote: E(0x29),
  ShiftLeft: E(0x2a), ShiftRight: E(0x36),
  Backslash: E(0x2b), IntlBackslash: E(0x56),
  KeyZ: E(0x2c), KeyX: E(0x2d), KeyC: E(0x2e), KeyV: E(0x2f), KeyB: E(0x30),
  KeyN: E(0x31), KeyM: E(0x32),
  Comma: E(0x33), Period: E(0x34), Slash: E(0x35),
  NumpadDivide: X(0x35),
  PrintScreen: X(0x37), NumpadMultiply: E(0x37),
  AltLeft: E(0x38), AltRight: X(0x38),
  Space: E(0x39), CapsLock: E(0x3a),
  F1: E(0x3b), F2: E(0x3c), F3: E(0x3d), F4: E(0x3e), F5: E(0x3f),
  F6: E(0x40), F7: E(0x41), F8: E(0x42), F9: E(0x43), F10: E(0x44),
  F11: E(0x57), F12: E(0x58),
  NumLock: E(0x45), ScrollLock: E(0x46),
  Numpad7: E(0x47), Numpad8: E(0x48), Numpad9: E(0x49), NumpadSubtract: E(0x4a),
  Numpad4: E(0x4b), Numpad5: E(0x4c), Numpad6: E(0x4d), NumpadAdd: E(0x4e),
  Numpad1: E(0x4f), Numpad2: E(0x50), Numpad3: E(0x51),
  Numpad0: E(0x52), NumpadDecimal: E(0x53),
  IntlRo: E(0x73), IntlYen: E(0x7d),
  Home: X(0x47), ArrowUp: X(0x48), PageUp: X(0x49),
  ArrowLeft: X(0x4b), ArrowRight: X(0x4d),
  End: X(0x4f), ArrowDown: X(0x50), PageDown: X(0x51),
  Insert: X(0x52), Delete: X(0x53),
  MetaLeft: X(0x5b), MetaRight: X(0x5c), ContextMenu: X(0x5d),
};

/** Lookup with unknown-code fallback: console.debug + undefined (caller
 * ignores the event — never send scan=0, the agent counts it invalid). */
export function lookupScan(code: string): ScanCode | undefined {
  const hit = TABLE[code];
  if (!hit) console.debug("[desktop-live] unmapped KeyboardEvent.code:", code);
  return hit;
}
