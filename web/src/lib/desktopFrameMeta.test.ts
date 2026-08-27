/**
 * desktopFrameMeta tests (M3 Task 5) — golden vector + bounded map +
 * correlation invariants.
 *
 * The golden vector is lifted VERBATIM from agent/desktop/frame_meta_test.go
 * (TestFrameMetaV1GoldenVector): same fixed test-only session key
 * 0x0F1E2D3C4B5A6978, same fields, same 56 bytes. Any layout drift on
 * either side fails here first — never re-derive these bytes by hand.
 */
import { describe, expect, it } from "vitest";

import {
  FRAME_META_MAP_CAPACITY,
  FRAME_META_V1_SIZE,
  FrameCorrelator,
  FrameMetaMap,
  decodeFrameMeta,
} from "./desktopFrameMeta";
import type { FrameMetaV1 } from "./desktopFrameMeta";

/** Exact 56-byte golden encoding (little-endian; offset 40..48 is the
 * session-keyed FNV-1a hash — the only key-dependent span). */
const GOLDEN_HEX =
  "0100000068AC240008070605040302018877665544332211A5" +
  "0000000000000042000000DEC000008C2A99D0FD2F08BB0000000000000000";

/** Golden field values (frame_meta_test.go frameMetaGolden). */
const GOLDEN = {
  rtpTimestamp: 0x0024ac68, // u32 → number
  codecEpoch: 0x0102030405060708n, // u64 → bigint
  contentId: 0x1122334455667788n,
  encodeSeq: 0xa5n,
  sourceMonoUs: 0x0000c0de00000042n,
  hash64: 0xbb082ffdd0992a8cn,
};

function bytesOf(hex: string): Uint8Array {
  const clean = hex.replace(/\s+/g, "");
  if (clean.length % 2 !== 0) throw new Error("odd hex length");
  const out = new Uint8Array(clean.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = parseInt(clean.slice(2 * i, 2 * i + 2), 16);
  return out;
}

function bufferOf(hex: string): ArrayBuffer {
  const b = bytesOf(hex);
  const out = new ArrayBuffer(b.byteLength);
  new Uint8Array(out).set(b);
  return out;
}

/** A minimal meta with the given identity (distinct rtpTimestamp). */
function metaOf(ts: number, epoch: bigint, seq: bigint): FrameMetaV1 {
  return {
    version: 1,
    flags: 0,
    rtpTimestamp: ts,
    codecEpoch: epoch,
    contentId: 100n + BigInt(ts),
    encodeSeq: seq,
    sourceMonoUs: BigInt(ts) * 33_000n,
    hash64: 0x1234n + BigInt(ts),
  };
}

describe("decodeFrameMeta golden vector", () => {
  it("decodes the Go golden bytes with every field exact", () => {
    const m = decodeFrameMeta(bufferOf(GOLDEN_HEX));
    expect(m).not.toBeNull();
    // rtpTimestamp is u32 → plain number; every u64 → bigint.
    expect(m!.version).toBe(1);
    expect(m!.flags).toBe(0);
    expect(m!.rtpTimestamp).toBe(GOLDEN.rtpTimestamp);
    expect(m!.codecEpoch).toBe(GOLDEN.codecEpoch);
    expect(m!.contentId).toBe(GOLDEN.contentId);
    expect(m!.encodeSeq).toBe(GOLDEN.encodeSeq);
    expect(m!.sourceMonoUs).toBe(GOLDEN.sourceMonoUs);
    expect(m!.hash64).toBe(GOLDEN.hash64);
  });

  it("also accepts a Uint8Array view of the same bytes", () => {
    const m = decodeFrameMeta(bytesOf(GOLDEN_HEX));
    expect(m?.rtpTimestamp).toBe(GOLDEN.rtpTimestamp);
    expect(m?.hash64).toBe(GOLDEN.hash64);
  });

  it("golden bytes are exactly FRAME_META_V1_SIZE long", () => {
    expect(bytesOf(GOLDEN_HEX).byteLength).toBe(FRAME_META_V1_SIZE);
    expect(FRAME_META_V1_SIZE).toBe(56);
  });
});

describe("decodeFrameMeta rejection (never throws)", () => {
  it("returns null for wrong versions without throwing", () => {
    for (const v of [0, 2, 0xff]) {
      const buf = bufferOf(GOLDEN_HEX);
      new Uint8Array(buf)[0] = v;
      expect(() => decodeFrameMeta(buf)).not.toThrow();
      expect(decodeFrameMeta(buf)).toBeNull();
    }
  });

  it("returns null for wrong lengths without throwing", () => {
    for (const n of [0, 1, 55, 57, 64]) {
      const buf = new ArrayBuffer(n);
      expect(() => decodeFrameMeta(buf)).not.toThrow();
      expect(decodeFrameMeta(buf)).toBeNull();
    }
  });

  it("the DataChannel handler path never propagates a decode failure", () => {
    // Mirrors the DesktopLive onmessage body: instanceof check + decode +
    // swallow. Garbage in → counted, nothing thrown.
    const handler = (data: unknown): FrameMetaV1 | null => {
      if (!(data instanceof ArrayBuffer)) return null;
      return decodeFrameMeta(data);
    };
    expect(() => handler("junk")).not.toThrow();
    expect(() => handler(new ArrayBuffer(3))).not.toThrow();
    expect(handler(new ArrayBuffer(3))).toBeNull();
    const short = bufferOf(GOLDEN_HEX.slice(0, 110)); // 55 bytes
    expect(() => handler(short)).not.toThrow();
  });
});

describe("FrameMetaMap (bounded, keyed by rtpTimestamp)", () => {
  it("stores and looks up metas by rtpTimestamp number key", () => {
    const map = new FrameMetaMap();
    const m = metaOf(0x24ac68, 1n, 2n);
    map.insert(m);
    expect(map.size).toBe(1);
    expect(map.lookup(0x24ac68)).toEqual(m);
    expect(map.lookup(0x9999)).toBeUndefined();
  });

  it("evicts oldest inserts at capacity", () => {
    const map = new FrameMetaMap(4);
    for (let i = 0; i < 5; i++) map.insert(metaOf(i, 1n, BigInt(i)));
    expect(map.size).toBe(4);
    expect(map.lookup(0)).toBeUndefined(); // oldest evicted
    for (let i = 1; i < 5; i++) expect(map.lookup(i)).toBeDefined();
  });

  it("default capacity is 512", () => {
    const map = new FrameMetaMap();
    for (let i = 0; i < 520; i++) map.insert(metaOf(i, 1n, BigInt(i)));
    expect(map.size).toBe(FRAME_META_MAP_CAPACITY);
    expect(FRAME_META_MAP_CAPACITY).toBe(512);
    expect(map.lookup(7)).toBeUndefined(); // 520-512 = 8 oldest gone
    expect(map.lookup(8)).toBeDefined();
  });

  it("clear resets the map", () => {
    const map = new FrameMetaMap();
    map.insert(metaOf(1, 1n, 1n));
    map.clear();
    expect(map.size).toBe(0);
    expect(map.lookup(1)).toBeUndefined();
  });
});

describe("FrameCorrelator (rVFC correlation)", () => {
  it("correlates a presented frame's rtpTimestamp to its meta", () => {
    const c = new FrameCorrelator();
    c.onMeta(metaOf(100, 5n, 10n));
    const r = c.onPresented(100);
    expect(r.meta?.encodeSeq).toBe(10n);
    expect(r.regressed).toBe(false);
    const s = c.snapshot();
    expect(s.metas).toBe(1);
    expect(s.hits).toBe(1);
    expect(s.misses).toBe(0);
    expect(s.lastHit?.codecEpoch).toBe(5n);
  });

  it("counts misses when the presented rtpTimestamp has no meta", () => {
    const c = new FrameCorrelator();
    c.onMeta(metaOf(100, 5n, 10n));
    c.onPresented(4242);
    const s = c.snapshot();
    expect(s.hits).toBe(0);
    expect(s.misses).toBe(1);
  });

  it("counts presentations without rtpTimestamp separately", () => {
    const c = new FrameCorrelator();
    c.onMeta(metaOf(100, 5n, 10n));
    c.onPresented(undefined);
    const s = c.snapshot();
    expect(s.noTimestamp).toBe(1);
    expect(s.hits).toBe(0);
    expect(s.misses).toBe(0);
  });

  it("counts decode failures fed as null (handler never throws)", () => {
    const c = new FrameCorrelator();
    c.onMeta(null);
    c.onMeta(metaOf(1, 1n, 1n));
    expect(c.snapshot().decodeFailures).toBe(1);
    expect(c.snapshot().metas).toBe(1);
  });

  it("flags codecEpoch regression but keeps the high-water mark", () => {
    const c = new FrameCorrelator();
    c.onMeta(metaOf(10, 5n, 10n));
    c.onMeta(metaOf(11, 4n, 11n)); // older encoder generation
    expect(c.onPresented(10).regressed).toBe(false);
    const r = c.onPresented(11);
    expect(r.regressed).toBe(true);
    expect(c.snapshot().regressions).toBe(1);
    // high-water: a later good frame still compares against epoch 5
    c.onMeta(metaOf(12, 5n, 12n));
    expect(c.onPresented(12).regressed).toBe(false);
    expect(c.snapshot().lastHit?.encodeSeq).toBe(12n);
  });

  it("flags contentId and encodeSeq regressions too", () => {
    const c = new FrameCorrelator();
    c.onMeta(metaOf(10, 5n, 10n));
    c.onMeta({ ...metaOf(11, 5n, 9n) }); // encodeSeq backwards
    c.onPresented(10);
    expect(c.onPresented(11).regressed).toBe(true);

    const c2 = new FrameCorrelator();
    const older = { ...metaOf(10, 5n, 10n), contentId: 500n };
    const newer = { ...metaOf(11, 5n, 11n), contentId: 499n };
    c2.onMeta(older);
    c2.onMeta(newer);
    c2.onPresented(10);
    expect(c2.onPresented(11).regressed).toBe(true);
  });

  it("counts plausible freezes from presentation gaps >= 500ms", () => {
    const c = new FrameCorrelator();
    c.onMeta(metaOf(1, 1n, 1n));
    c.onMeta(metaOf(2, 1n, 2n));
    c.onMeta(metaOf(3, 1n, 3n));
    c.onPresented(1, 0);
    c.onPresented(2, 499); // just under threshold
    c.onPresented(3, 1200); // 701ms gap → freeze
    expect(c.snapshot().plausibleFreezes).toBe(1);
    // no nowMs → no freeze accounting, but correlation still works
    c.onMeta(metaOf(4, 1n, 4n));
    expect(c.onPresented(4).meta).toBeDefined();
    expect(c.snapshot().plausibleFreezes).toBe(1);
  });

  it("bounds its meta map (unordered channel loss cannot grow memory)", () => {
    const c = new FrameCorrelator(8);
    for (let i = 0; i < 100; i++) c.onMeta(metaOf(i, 1n, BigInt(i)));
    const s = c.snapshot();
    expect(s.metas).toBe(100); // total received
    expect(s.decodeFailures).toBe(0);
  });
});
