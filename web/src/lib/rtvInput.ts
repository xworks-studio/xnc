// RTV 输入辅助（纯函数）：键盘事件分类 + 剪贴板 UTF-8 安全分块。
// 与 host 侧对齐：键盘注入见 host/src/keymap.rs（code→VK）与 input.rs
// （text→KEYEVENTF_UNICODE）；分块协议见 host/src/clipboard.rs（块 ≤16KiB
// UTF-8——WS 兜底腿无 SetReadLimit，coder/websocket 默认 32KiB 读上限）。

/** 单块上限（UTF-8 字节）。 */
export const CLIP_CHUNK_BYTES = 16 * 1024;
/** 同步文本总量上限：超限截断（码点边界）。 */
export const CLIP_MAX_BYTES = 256 * 1024;
/** 分块数上限（与 host 侧 MAX_PARTS 一致，异常 total 直接丢弃）。 */
const MAX_PARTS = 64;

/** 码点 UTF-8 编码长度。 */
function cpUtf8Len(cp: number): number {
  if (cp <= 0x7f) return 1;
  if (cp <= 0x7ff) return 2;
  if (cp <= 0xffff) return 3;
  return 4;
}

/** 码点边界截断（永不切断多字节字符/代理对）。 */
export function truncateUtf8(
  text: string,
  maxBytes = CLIP_MAX_BYTES,
): { text: string; truncated: boolean } {
  let n = 0;
  let out = "";
  for (const ch of text) {
    const l = cpUtf8Len(ch.codePointAt(0)!);
    if (n + l > maxBytes) return { text: out, truncated: true };
    out += ch;
    n += l;
  }
  return { text: out, truncated: false };
}

/** UTF-8 安全切块：每块编码后 ≤ maxBytes；空文本返回 []。 */
export function chunkText(text: string, maxBytes = CLIP_CHUNK_BYTES): string[] {
  const out: string[] = [];
  let cur = "";
  let n = 0;
  for (const ch of text) {
    const l = cpUtf8Len(ch.codePointAt(0)!);
    if (n + l > maxBytes) {
      out.push(cur);
      cur = ch;
      n = l;
    } else {
      cur += ch;
      n += l;
    }
  }
  if (cur) out.push(cur);
  return out;
}

export interface ClipChunkMsg {
  seq: number;
  index: number;
  total: number;
  text: string;
}

/** 组装粘贴分块消息（kind:"set-text" 的载荷字段）。 */
export function makeSetChunks(text: string, seq: number): ClipChunkMsg[] {
  const parts = chunkText(text);
  return parts.map((t, i) => ({ seq, index: i, total: parts.length, text: t }));
}

/** 远端推送分块组装（乱序到达可用；新 seq 丢弃旧半截）。 */
export class ClipAssembler {
  private cur: {
    seq: number;
    total: number;
    parts: (string | null)[];
    received: number;
  } | null = null;

  push(m: ClipChunkMsg): { seq: number; text: string } | null {
    if (
      !Number.isInteger(m.seq) ||
      !Number.isInteger(m.index) ||
      m.total <= 0 ||
      m.total > MAX_PARTS ||
      m.index < 0 ||
      m.index >= m.total
    ) {
      return null;
    }
    if (!this.cur || this.cur.seq !== m.seq || this.cur.total !== m.total) {
      this.cur = {
        seq: m.seq,
        total: m.total,
        parts: new Array<string | null>(m.total).fill(null),
        received: 0,
      };
    }
    const a = this.cur;
    if (a.parts[m.index] === null) {
      a.parts[m.index] = m.text;
      a.received += 1;
    }
    if (a.received === a.total) {
      const text = a.parts.join("");
      this.cur = null;
      return { seq: a.seq, text };
    }
    return null;
  }
}

/** KeyboardEvent 的最小面（测试可注入）。 */
export interface KeyLike {
  key: string;
  code: string;
  ctrlKey: boolean;
  altKey: boolean;
  metaKey: boolean;
  isComposing?: boolean;
}

/**
 * keydown 分类：
 * - null：跳过（IME 组合中 / Process / 死键——文本交给剪贴板粘贴路径）；
 * - text：无修饰键的可打印字符 → host KEYEVENTF_UNICODE（布局无关）；
 * - code：其余（修饰键/功能键/快捷键组合）→ host keymap 查 VK 注入，
 *   修饰键自然成对转发，远端快捷键成立。
 */
export function classifyKeyDown(e: KeyLike): {
  kind: "text" | "code";
  text?: string;
  code?: string;
} | null {
  if (e.isComposing || e.key === "Process" || e.key === "Dead") return null;
  if (e.key.length === 1 && !e.ctrlKey && !e.altKey && !e.metaKey) {
    return { kind: "text", text: e.key };
  }
  return { kind: "code", code: e.code };
}

/** 粘贴组合键（Ctrl/Cmd+V，无 Alt——Alt+V 留给远端）。 */
export function isPasteCombo(e: KeyLike): boolean {
  return (e.ctrlKey || e.metaKey) && !e.altKey && e.code === "KeyV";
}

/**
 * 该键是否走文本路径（keyup 时跳过——文本路径不发 down/up）。
 * 与 classifyKeyDown 的判定保持一致。
 */
export function isTextPathKey(e: KeyLike): boolean {
  return e.key.length === 1 && !e.ctrlKey && !e.altKey && !e.metaKey;
}
