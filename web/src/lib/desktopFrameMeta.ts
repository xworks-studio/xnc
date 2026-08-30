/**
 * FrameMetaV1 telemetry: pure codec + bounded metadata map + rVFC
 * correlation (M3 Task 5). Mirrors agent/desktop/frame_meta.go — pure
 * logic only, no React/DOM imports, so it is testable standalone.
 *
 * == wire contract (identical to the Go side; any change is a breaking
 *    protocol change) ==
 *
 * "frame-meta" DataChannel (unordered, MaxRetransmits=0, agent → viewer):
 * one fixed 56-byte record after the last RTP packet of each frame.
 * Loss is expected — metas are telemetry, never media state.
 *
 * 56-byte layout (all fields little-endian):
 *
 *   off  size  field
 *   ---  ----  -----
 *   0    1     version          always 1 (FrameMetaV1)
 *   1    1     flags            reserved, 0
 *   2    2     reserved         0
 *   4    4     rtpTimestamp     90kHz stamp actually put on the frame's
 *                              RTP packets (per-viewer clock)
 *   8    8     codecEpoch       encoder generation (advances on reset)
 *   16   8     contentId        capture-content stable id
 *   24   8     encodeSeq        encode sequence number
 *   32   8     sourceMonoUs     capture time, host monotonic clock, µs
 *   40   8     hash64           session-keyed FNV-1a64 (NOT a reusable
 *                              content fingerprint — the key never
 *                              leaves the session)
 *   48   8     reserved         0
 *
 * Decode validates version === 1 and total length === 56; everything
 * else is rejected as null. decodeFrameMeta NEVER throws — the
 * DataChannel onmessage handler must not propagate failures.
 */

/** Fixed record size (see layout table). */
export const FRAME_META_V1_SIZE = 56;
/** The only version understood here (FrameMetaV1). */
export const FRAME_META_V1_VERSION = 1;
/** Bounded metadata map capacity: last N metas (~17s at 30fps) —
 * unordered-channel loss must never grow memory. */
export const FRAME_META_MAP_CAPACITY = 512;
/** Presentation gap at/above which the rVFC cadence is counted as a
 * plausible freeze (matches Chrome's freezeCount semantics of a pause
 * of at least ~0.5s). */
export const PLAUSIBLE_FREEZE_MS = 500;

/** One decoded FrameMetaV1 record. rtpTimestamp is u32 → number; every
 * u64 field is bigint (full 64-bit fidelity, no float rounding). */
export interface FrameMetaV1 {
  version: number;
  flags: number;
  rtpTimestamp: number;
  codecEpoch: bigint;
  contentId: bigint;
  encodeSeq: bigint;
  sourceMonoUs: bigint;
  hash64: bigint;
}

/**
 * Decode one 56-byte record. Accepts the ArrayBuffer the DataChannel
 * hands over (binaryType "arraybuffer") or any Uint8Array view.
 * Returns null for wrong length, wrong version, or a malformed view —
 * and never throws, so the handler path can swallow failures whole.
 */
export function decodeFrameMeta(data: ArrayBuffer | Uint8Array): FrameMetaV1 | null {
  try {
    const bytes = data instanceof ArrayBuffer ? new Uint8Array(data) : data;
    if (bytes.byteLength !== FRAME_META_V1_SIZE) return null;
    const v = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    const version = v.getUint8(0);
    if (version !== FRAME_META_V1_VERSION) return null;
    return {
      version,
      flags: v.getUint8(1),
      rtpTimestamp: v.getUint32(4, true),
      codecEpoch: v.getBigUint64(8, true),
      contentId: v.getBigUint64(16, true),
      encodeSeq: v.getBigUint64(24, true),
      sourceMonoUs: v.getBigUint64(32, true),
      hash64: v.getBigUint64(40, true),
    };
  } catch {
    return null; // defensive: handler must never see an exception
  }
}

/**
 * Bounded last-N map of metas keyed by rtpTimestamp (number key —
 * exact for u32). Insertion-order eviction keeps the newest N; the
 * channel is unordered so a late duplicate simply refreshes its entry.
 */
export class FrameMetaMap {
  private readonly capacity: number;
  private readonly metas = new Map<number, FrameMetaV1>();

  constructor(capacity: number = FRAME_META_MAP_CAPACITY) {
    this.capacity = Math.max(1, Math.floor(capacity));
  }

  insert(meta: FrameMetaV1): void {
    this.metas.delete(meta.rtpTimestamp); // re-stamp refreshes recency
    this.metas.set(meta.rtpTimestamp, meta);
    while (this.metas.size > this.capacity) {
      const oldest = this.metas.keys().next();
      if (oldest.done) break;
      this.metas.delete(oldest.value);
    }
  }

  lookup(rtpTimestamp: number): FrameMetaV1 | undefined {
    return this.metas.get(rtpTimestamp);
  }

  get size(): number {
    return this.metas.size;
  }

  clear(): void {
    this.metas.clear();
  }
}

/** Correlation counters exposed to the diagnostics panel and the 1s
 * viewer_feedback report. All monotonically increasing except lastHit. */
export interface CorrelationSnapshot {
  /** Valid metas received from the channel. */
  metas: number;
  /** Records that failed decode (wrong version/length/garbage). */
  decodeFailures: number;
  /** Presented frames seen by the correlator (rVFC ticks). */
  presented: number;
  /** Presented frames whose rtpTimestamp matched a stored meta. */
  hits: number;
  /** Presented frames with an rtpTimestamp but no matching meta
   * (unordered drop or pre-correlation frames). */
  misses: number;
  /** Presented frames whose metadata carried no rtpTimestamp. */
  noTimestamp: number;
  /** Hits whose codecEpoch/contentId/encodeSeq regressed vs the last
   * presented hit (presentation-order anomaly — warned, never thrown). */
  regressions: number;
  /** Presentation gaps >= PLAUSIBLE_FREEZE_MS. */
  plausibleFreezes: number;
  /** High-water identity of the last non-regressed hit. */
  lastHit: FrameMetaV1 | null;
  /**
   * [M4 deliverable] End-to-end of the LAST correlated frame (ms), source
   * capture stamp -> presented timestamp — min-offset normalized (see
   * FrameCorrelator.onPresented). null before the first correlated frame.
   */
  e2eLastMs: number | null;
  /**
   * [M4 deliverable] p95 of the end-to-end over the recent correlated
   * frames (ms), null with no samples yet.
   */
  e2eP95Ms: number | null;
}

/** Result of correlating one presented frame. */
export interface PresentedCorrelation {
  meta: FrameMetaV1 | null;
  regressed: boolean;
  /** [M4 deliverable] Min-offset-normalized end-to-end of THIS frame
   * (ms), null when nowMs was not supplied / no meta hit. */
  e2eMs: number | null;
}

/** Bounded ring of recent end-to-end samples (p95 window; ~1s at 30fps
 * of correlated frames). */
const E2E_WINDOW = 32;

/**
 * FrameCorrelator ties the two async streams together:
 *  - onMeta: every decoded "frame-meta" record (null = decode failure,
 *    counted — the channel handler must never throw);
 *  - onPresented: every rVFC-presented frame, looked up by Chrome's
 *    optional metadata.rtpTimestamp. On a hit, codecEpoch/contentId/
 *    encodeSeq must not regress vs the last presented hit — a
 *    regression is counted and flagged; the caller decides whether to
 *    console.warn. The high-water mark survives regressions so a
 *    repeated backtrack counts each time.
 *
 * [M4 deliverable] End-to-end per correlated frame: raw =
 * presentedNowMs − meta.sourceMonoUs/1000 mixes TWO clock domains (the
 * host monotonic clock vs browser performance.now()), so the absolute
 * value is meaningless. The session MINIMUM of raw approximates the
 * clock offset plus the best-case path, and raw − min is the per-frame
 * end-to-end EXCESS over the best observed path — a correct relative
 * latency (trend / p95) without any shared clock. The minimum can only
 * decrease, so the metric never drifts upward from clock skew.
 */
export class FrameCorrelator {
  private readonly map: FrameMetaMap;
  private readonly snap: CorrelationSnapshot;
  private lastPresentedAt: number | null = null;
  private minRawMs: number | null = null;
  private readonly e2e: number[] = [];

  constructor(capacity: number = FRAME_META_MAP_CAPACITY) {
    this.map = new FrameMetaMap(capacity);
    this.snap = {
      metas: 0,
      decodeFailures: 0,
      presented: 0,
      hits: 0,
      misses: 0,
      noTimestamp: 0,
      regressions: 0,
      plausibleFreezes: 0,
      lastHit: null,
      e2eLastMs: null,
      e2eP95Ms: null,
    };
  }

  /** Feed one decoded record (or null when decode rejected it). */
  onMeta(meta: FrameMetaV1 | null): void {
    if (!meta) {
      this.snap.decodeFailures++;
      return;
    }
    this.snap.metas++;
    this.map.insert(meta);
  }

  /** Correlate one presented frame; nowMs (rVFC's high-res timestamp)
   * optionally drives the plausible-freeze cadence accounting. */
  onPresented(
    rtpTimestamp: number | undefined,
    nowMs?: number,
  ): PresentedCorrelation {
    this.snap.presented++;
    if (typeof nowMs === "number") {
      if (
        this.lastPresentedAt !== null &&
        nowMs - this.lastPresentedAt >= PLAUSIBLE_FREEZE_MS
      ) {
        this.snap.plausibleFreezes++;
      }
      this.lastPresentedAt = nowMs;
    }
    if (typeof rtpTimestamp !== "number") {
      this.snap.noTimestamp++;
      return { meta: null, regressed: false, e2eMs: null };
    }
    const meta = this.map.lookup(rtpTimestamp);
    if (!meta) {
      this.snap.misses++;
      return { meta: null, regressed: false, e2eMs: null };
    }
    this.snap.hits++;
    const last = this.snap.lastHit;
    const regressed =
      last !== null &&
      (meta.codecEpoch < last.codecEpoch ||
        meta.contentId < last.contentId ||
        meta.encodeSeq < last.encodeSeq);
    if (regressed) {
      this.snap.regressions++;
    } else {
      this.snap.lastHit = meta; // high-water mark
    }
    let e2eMs: number | null = null;
    if (typeof nowMs === "number") {
      e2eMs = this.noteE2e(meta, nowMs);
    }
    return { meta, regressed, e2eMs };
  }

  /** Min-offset-normalized end-to-end of one correlated frame (see the
   * class comment for why the raw cross-clock difference is normalized). */
  private noteE2e(meta: FrameMetaV1, nowMs: number): number | null {
    const raw = nowMs - Number(meta.sourceMonoUs) / 1000;
    if (!Number.isFinite(raw)) return null;
    if (this.minRawMs === null || raw < this.minRawMs) this.minRawMs = raw;
    const e2e = raw - this.minRawMs;
    this.snap.e2eLastMs = e2e;
    this.e2e.push(e2e);
    if (this.e2e.length > E2E_WINDOW) this.e2e.splice(0, this.e2e.length - E2E_WINDOW);
    const sorted = [...this.e2e].sort((a, b) => a - b);
    this.snap.e2eP95Ms = sorted[Math.min(sorted.length - 1, Math.floor(sorted.length * 0.95))];
    return e2e;
  }

  snapshot(): CorrelationSnapshot {
    return { ...this.snap, lastHit: this.snap.lastHit };
  }
}
