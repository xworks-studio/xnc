// @vitest-environment node
import { describe, expect, it } from "vitest";
import {
  CLIP_CHUNK_BYTES,
  ClipAssembler,
  chunkText,
  classifyKeyDown,
  isPasteCombo,
  isTextPathKey,
  makeSetChunks,
  truncateUtf8,
} from "./rtvInput";

function utf8Len(s: string): number {
  return new TextEncoder().encode(s).length;
}

describe("chunkText", () => {
  it("chunks ascii and never exceeds max bytes", () => {
    const s = "a".repeat(CLIP_CHUNK_BYTES * 3 + 10);
    const chunks = chunkText(s);
    expect(chunks.length).toBe(4);
    for (const c of chunks) expect(utf8Len(c)).toBeLessThanOrEqual(CLIP_CHUNK_BYTES);
    expect(chunks.join("")).toBe(s);
  });

  it("never splits CJK code points", () => {
    const s = "中".repeat(CLIP_CHUNK_BYTES); // 3 字节/字
    const chunks = chunkText(s);
    expect(chunks.join("")).toBe(s);
    for (const c of chunks) {
      // 块必须是合法 UTF-8（TextEncoder roundtrip 校验不了；用 decode fatal）
      new TextDecoder("utf-8", { fatal: true }).decode(
        new TextEncoder().encode(c),
      );
      expect(utf8Len(c)).toBeLessThanOrEqual(CLIP_CHUNK_BYTES);
    }
  });

  it("keeps surrogate pairs (emoji) together", () => {
    const s = "🎉".repeat(10_000); // 代理对，4 字节/码点
    const chunks = chunkText(s);
    expect(chunks.join("")).toBe(s);
  });

  it("returns empty array for empty text", () => {
    expect(chunkText("")).toEqual([]);
  });
});

describe("truncateUtf8", () => {
  it("no-op under limit", () => {
    expect(truncateUtf8("hello", 10)).toEqual({ text: "hello", truncated: false });
  });

  it("truncates at code point boundary", () => {
    // 7 字节 = 2 个“中”+1 字节余量
    expect(truncateUtf8("中中中", 7)).toEqual({ text: "中中", truncated: true });
    // 恰好整码点
    expect(truncateUtf8("中中", 6)).toEqual({ text: "中中", truncated: false });
  });
});

describe("makeSetChunks + ClipAssembler roundtrip", () => {
  it("reassembles in order and out of order", () => {
    const s = `line1\n中文内容 🎉 ${"x".repeat(CLIP_CHUNK_BYTES * 2)}`;
    const asm = new ClipAssembler();
    const msgs = makeSetChunks(s, 42);
    expect(msgs.length).toBeGreaterThanOrEqual(3);
    // 乱序压入（最后一块先到）
    const shuffled = [...msgs].reverse();
    let done: { seq: number; text: string } | null = null;
    for (const m of shuffled) done = asm.push(m);
    expect(done).toEqual({ seq: 42, text: s });
  });

  it("duplicate chunks are idempotent", () => {
    const asm = new ClipAssembler();
    const text = "x".repeat(CLIP_CHUNK_BYTES + 5); // 恰好 2 块
    const msgs = makeSetChunks(text, 7);
    expect(msgs.length).toBe(2);
    expect(asm.push(msgs[0])).toBeNull();
    expect(asm.push(msgs[0])).toBeNull(); // 重复
    expect(asm.push(msgs[1])).toEqual({ seq: 7, text });
  });

  it("new seq discards stale partial", () => {
    const asm = new ClipAssembler();
    // 两个 seq 的文本都必须多块，压入首块后仍是半截
    const aText = "a".repeat(CLIP_CHUNK_BYTES * 2);
    const bText = "y".repeat(CLIP_CHUNK_BYTES + 3);
    const a = makeSetChunks(aText, 1);
    const b = makeSetChunks(bText, 2);
    expect(a.length).toBeGreaterThanOrEqual(2);
    expect(b.length).toBeGreaterThanOrEqual(2);
    expect(asm.push(a[0])).toBeNull(); // seq1 半截
    expect(asm.push(b[0])).toBeNull();
    expect(asm.push(b[1])).toEqual({ seq: 2, text: bText });
  });

  it("rejects malformed chunks", () => {
    const asm = new ClipAssembler();
    expect(asm.push({ seq: 1, index: -1, total: 2, text: "x" })).toBeNull();
    expect(asm.push({ seq: 1, index: 5, total: 2, text: "x" })).toBeNull();
    expect(asm.push({ seq: 1, index: 0, total: 0, text: "x" })).toBeNull();
    expect(
      asm.push({ seq: 1, index: 0, total: 10_000, text: "x" }),
    ).toBeNull();
  });
});

describe("classifyKeyDown", () => {
  const base = {
    ctrlKey: false,
    altKey: false,
    metaKey: false,
  };

  it("printable without modifiers → text", () => {
    expect(classifyKeyDown({ ...base, key: "a", code: "KeyA" })).toEqual({
      kind: "text",
      text: "a",
    });
    expect(classifyKeyDown({ ...base, key: "A", code: "KeyA" })).toEqual({
      kind: "text",
      text: "A",
    }); // Shift 组合由 e.key 承载
  });

  it("modifier combos → code", () => {
    expect(
      classifyKeyDown({ ...base, key: "c", code: "KeyC", ctrlKey: true }),
    ).toEqual({ kind: "code", code: "KeyC" });
    expect(
      classifyKeyDown({ ...base, key: "Meta", code: "MetaLeft", metaKey: true }),
    ).toEqual({ kind: "code", code: "MetaLeft" });
  });

  it("non-printable keys → code", () => {
    expect(classifyKeyDown({ ...base, key: "Enter", code: "Enter" })).toEqual({
      kind: "code",
      code: "Enter",
    });
    expect(classifyKeyDown({ ...base, key: "ArrowLeft", code: "ArrowLeft" })).toEqual(
      { kind: "code", code: "ArrowLeft" },
    );
  });

  it("IME composing / Process / Dead → null", () => {
    expect(
      classifyKeyDown({ ...base, key: "a", code: "KeyA", isComposing: true }),
    ).toBeNull();
    expect(classifyKeyDown({ ...base, key: "Process", code: "KeyA" })).toBeNull();
    expect(classifyKeyDown({ ...base, key: "Dead", code: "Quote" })).toBeNull();
  });
});

describe("isPasteCombo / isTextPathKey", () => {
  const base = { ctrlKey: false, altKey: false, metaKey: false };

  it("Ctrl+V and Cmd+V are paste; Alt+V is not", () => {
    expect(isPasteCombo({ ...base, key: "v", code: "KeyV", ctrlKey: true })).toBe(
      true,
    );
    expect(isPasteCombo({ ...base, key: "v", code: "KeyV", metaKey: true })).toBe(
      true,
    );
    expect(isPasteCombo({ ...base, key: "µ", code: "KeyV", ctrlKey: true, altKey: true })).toBe(
      false,
    );
    expect(isPasteCombo({ ...base, key: "c", code: "KeyC", ctrlKey: true })).toBe(
      false,
    );
  });

  it("text path detection matches classify", () => {
    expect(isTextPathKey({ ...base, key: "a", code: "KeyA" })).toBe(true);
    expect(
      isTextPathKey({ ...base, key: "a", code: "KeyA", ctrlKey: true }),
    ).toBe(false);
    expect(isTextPathKey({ ...base, key: "Enter", code: "Enter" })).toBe(false);
  });
});
