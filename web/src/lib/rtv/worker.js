// worker.js — 收包组装 / RS FEC 恢复 / WebCodecs 解码（Worker 内）。
//
// 职责对应 moonlight-common-c 的 RtpVideoQueue.c + VideoDepacketizer.c：
// 按 frameIndex 聚合 shard、块级 FEC 恢复（恢复包过 flags/magic 校验）、
// Annex-B 组帧、丢帧上报（带 IDR 防风暴）、WebCodecs 硬解（optimizeForLatency）。
// 解码出的 VideoFrame 转移给主线程渲染（Pacer 在主线程 rAF）。

import { parseHdr, expectDataFlags, HDR_LEN, FLAG_PARITY, FLAG_SOF, FLAG_EOF } from './proto.js';
import { rsRecover } from './rs.js';

// 帧定稿等待（新帧到达或超时）。WT=不可靠 datagram，超时≈丢包需上报；
// WS=可靠有序流，超时只是排队晚到，放宽且不上报（否则 IDR 风暴）。
// 25ms 是 MVP 的 LAN 参数；公网 Chrome WT 投递存在跨帧抖动（实测 shard
// 迟到数十 ms 常态），超时过紧会把可恢复帧提前判死（FEC 失败暴增）。
// 120ms 为超时兜底上限（确定性丢帧检测=新帧首包到达，不受此影响）。
const FRAME_GC_MS = 120;
const FRAME_GC_MS_RELIABLE = 500;
let transportReliable = false; // WS 兜底路径
const MAX_PENDING = 16;

let decoder = null;
let decoderConfigured = false;
let lastConfiguredCodec = '';
let waitingIdr = true; // 解码从 IDR 开始
let lossSentPending = false; // IDR 防风暴：成功帧前不再发 frameLoss
let consecutiveDrops = 0;

const pending = new Map(); // frameIndex -> frame

class FrameBlk {
  constructor() {
    this.k = 0;
    this.r = 0;
    this.known = false;
    this.shards = new Map(); // shardIndex -> Uint8Array(完整包)
  }
}
class Frame {
  constructor(frameIndex) {
    this.frameIndex = frameIndex;
    this.blocks = new Map();
    this.blockTotal = 0;
    this.keyframe = false;
    this.captureUnixUs = 0;
    this.firstArrivalMs = performance.now();
    this.createdAt = Date.now();
  }
}

const stats = {
  pkts: 0, bytes: 0,
  framesComplete: 0, fecRecovered: 0, fecFailed: 0,
  lossSent: 0, decoded: 0,
  arrivalGaps: [],
  lastFirstArrival: 0,
};

self.onmessage = (ev) => {
  const m = ev.data;
  switch (m.type) {
    case 'config':
      setupDecoder(m);
      break;
    case 'datagram':
      onDatagram(m.buf);
      break;
    case 'transport':
      transportReliable = m.reliable === true;
      break;
    case 'reset':
      pending.clear();
      waitingIdr = true;
      lossSentPending = false;
      break;
  }
};

function onDatagram(buf) {
  const h = parseHdr(buf);
  if (!h) return;
  stats.pkts++;
  stats.bytes += buf.byteLength;

  // 新帧首包：到帧间隔统计 + 定稿更早的帧（确定性丢帧检测）
  if (!pending.has(h.frameIndex)) {
    noteArrivalGap();
    if (pending.size > MAX_PENDING) pending.clear(); // 防积压（异常场景）
    const f = new Frame(h.frameIndex);
    pending.set(h.frameIndex, f);
    for (const [idx, pf] of pending) {
      if (idx < h.frameIndex) finalize(pf);
    }
  }
  const f = pending.get(h.frameIndex);
  f.blockTotal = Math.max(f.blockTotal, h.blockTotal);
  f.keyframe = f.keyframe || h.keyframe;
  if (!f.captureUnixUs) f.captureUnixUs = h.captureUnixUs;
  let blk = f.blocks.get(h.blockIdx);
  if (!blk) {
    blk = new FrameBlk();
    f.blocks.set(h.blockIdx, blk);
  }
  if (!blk.known || h.dataShards > blk.k) {
    blk.k = h.dataShards;
    blk.r = h.parityShards;
    blk.known = true;
  }
  // 去重
  if (!blk.shards.has(h.shardIndex)) {
    blk.shards.set(h.shardIndex, new Uint8Array(buf));
  }
  // 该块到齐即定稿（不等整帧，尽早解耦）
  if (h.shardIndex < h.dataShards && blk.shards.size >= h.dataShards && f.blockTotal > 0) {
    const allDone = [...f.blocks.values()].every((b) => !b.known || b.shards.size >= b.k + b.r);
    if (allDone && f.blocks.size >= f.blockTotal) finalize(f);
  }
}

// 周期定稿超时帧
setInterval(() => {
  const now = Date.now();
  const gc = transportReliable ? FRAME_GC_MS_RELIABLE : FRAME_GC_MS;
  for (const f of pending.values()) {
    if (now - f.createdAt >= gc) finalize(f);
  }
}, 5);

function noteArrivalGap() {
  const now = performance.now();
  if (stats.lastFirstArrival) {
    stats.arrivalGaps.push(now - stats.lastFirstArrival);
    if (stats.arrivalGaps.length > 2048) stats.arrivalGaps = stats.arrivalGaps.slice(-1024);
  }
  stats.lastFirstArrival = now;
}

// ---------------- 帧定稿：完整性判定 + FEC 恢复 ----------------
function finalize(f) {
  if (!pending.delete(f.frameIndex)) return; // 已处理
  let missing = 0;
  let parityAvail = 0;
  const blocksSorted = [...f.blocks.keys()].sort((a, b) => a - b);
  for (const bi of blocksSorted) {
    const blk = f.blocks.get(bi);
    for (let i = 0; i < blk.k; i++) if (!blk.shards.has(i)) missing++;
    for (let j = blk.k; j < blk.k + blk.r; j++) if (blk.shards.has(j)) parityAvail++;
  }
  if (missing > 0) {
    if (parityAvail >= missing) {
      // 逐块 RS 恢复（moonlight reconstructFrame 对应物）
      let recovered = true;
      for (const bi of blocksSorted) {
        const blk = f.blocks.get(bi);
        const need = [];
        for (let i = 0; i < blk.k; i++) if (!blk.shards.has(i)) need.push(i);
        if (!need.length) continue;
        // FEC 运算域（docs/proto.md §1.2）：数据 shard=完整包（含头，短包补零）；
        // 校验 shard=校验包的内容（剥掉校验包自己的头）。shardLen 取两者最大对齐。
        let shardLen = 0;
        for (const [idx, s] of blk.shards) {
          shardLen = Math.max(shardLen, idx < blk.k ? s.length : s.length - HDR_LEN);
        }
        const shards = new Array(blk.k + blk.r).fill(null);
        for (const [idx, s] of blk.shards) {
          const content = idx < blk.k ? s : s.subarray(HDR_LEN);
          const padded = new Uint8Array(Math.max(shardLen, content.length));
          padded.set(content);
          shards[idx] = padded;
        }
        const rec = rsRecover(shards, blk.k);
        if (!rec) { recovered = false; break; }
        for (const i of need) {
          const pkt = rec[i];
          const h2 = parseHdr(pkt);
          const okFlags = h2 && h2.frameIndex === f.frameIndex && h2.blockIdx === bi &&
            h2.shardIndex === i && h2.flags === expectDataFlags(bi, i, blk.k, bi === blocksSorted[blocksSorted.length - 1]);
          if (!okFlags) { recovered = false; break; }
          // 校验通过：按恢复头里的 payloadLen 截断（尾部零填充去除）
          blk.shards.set(i, pkt.slice(0, HDR_LEN + h2.payloadLen));
        }
        if (!recovered) break;
      }
      if (recovered) {
        stats.fecRecovered++;
        emitFrame(f, blocksSorted);
        return;
      }
    }
    // 不可恢复 → 上报（防风暴）+ 连续丢帧保护
    stats.fecFailed++;
    reportLoss(f, 'unrecoverable');
    return;
  }
  stats.framesComplete++;
  emitFrame(f, blocksSorted);
}

// ---------------- 组帧 → 解码 ----------------
function emitFrame(f, blocksSorted) {
  consecutiveDrops = 0;
  lossSentPending = false;
  if (f.keyframe) waitingIdr = false;
  if (waitingIdr && !f.keyframe) return; // moonlight：首帧必须 IDR

  const parts = [];
  for (const bi of blocksSorted) {
    const blk = f.blocks.get(bi);
    for (let i = 0; i < blk.k; i++) {
      const s = blk.shards.get(i);
      if (s) parts.push(s.subarray(HDR_LEN));
    }
  }
  const total = parts.reduce((n, p) => n + p.length, 0);
  const data = new Uint8Array(total);
  let off = 0;
  for (const p of parts) { data.set(p, off); off += p.length; }
  decodeFrame(f, data);
}

function setupDecoder(cfg) {
  pending.clear();
  waitingIdr = true;
  lastConfiguredCodec = '';
  if (decoder) { try { decoder.close(); } catch {} }
  decoder = null;
  decoderConfigured = !!cfg;
  if (!cfg) return;
  self.postMessage({ type: 'status', msg: `config: ${cfg.width}x${cfg.height}@${cfg.fps} enc=${cfg.encoder}${cfg.encoderHw ? '(hw)' : ''} fec=${cfg.fecPercentage}%` });
}

async function decodeFrame(f, data) {
  if (!decoderConfigured) return;
  // 首个关键帧：从 SPS 推导 codec 字符串后建解码器
  if (!decoder) {
    if (!f.keyframe) return;
    const codec = codecStringFromSps(data);
    if (!codec) return;
    const base = {
      codec,
      optimizeForLatency: true,
      hardwareAcceleration: 'no-preference',
    };
    let support = await VideoDecoder.isConfigSupported(base);
    if (!support.config?.hardwareAcceleration) {
      // 该实现可能拒绝 hardwareAcceleration 字段——去掉重试
      support = await VideoDecoder.isConfigSupported({ codec, optimizeForLatency: true });
    }
    decoder = new VideoDecoder({
      output: (frame) => {
        stats.decoded++;
        self.postMessage({ type: 'decoded', frame, captureUnixUs: f.captureUnixUs, frameIndex: f.frameIndex, keyframe: f.keyframe }, [frame]);
      },
      error: (e) => {
        self.postMessage({ type: 'decoderError', error: String(e) });
        try { decoder?.close(); } catch {}
        decoder = null;
        waitingIdr = true;
        reportLoss(f, 'decoder');
      },
    });
    decoder.configure(support.config ?? base);
    lastConfiguredCodec = codec;
  }
  const chunk = new EncodedVideoChunk({
    type: f.keyframe ? 'key' : 'delta',
    timestamp: f.captureUnixUs, // 单调性由 host 时钟保证
    duration: 0,
    data,
  });
  try {
    decoder.decode(chunk);
  } catch (e) {
    self.postMessage({ type: 'decoderError', error: String(e) });
    try { decoder.close(); } catch {}
    decoder = null;
    waitingIdr = true;
    reportLoss(f, 'decoder');
  }
  if (decoder?.decodeQueueSize > 8) {
    // 解码积压：丢到只剩最近帧由主线程负责；此处仅上报反馈
  }
}

/** 从 Annex-B 里找 SPS，构造 avc1.PPCCLL（profile, constraints, level）。 */
function codecStringFromSps(data) {
  for (let i = 0; i + 4 < data.length && i < 64; i++) {
    if (data[i] === 0 && data[i + 1] === 0 && data[i + 2] === 1 && (data[i + 3] & 0x1f) === 7) {
      const p = data[i + 4], c = data[i + 5], l = data[i + 6];
      const hex = (n) => n.toString(16).padStart(2, '0');
      return `avc1.${hex(p)}${hex(c)}${hex(l)}`;
    }
    // 4 字节起始码
    if (i + 5 < data.length && data[i] === 0 && data[i + 1] === 0 && data[i + 2] === 0 && data[i + 3] === 1 && (data[i + 4] & 0x1f) === 7) {
      const p = data[i + 5], c = data[i + 6], l = data[i + 7];
      const hex = (n) => n.toString(16).padStart(2, '0');
      return `avc1.${hex(p)}${hex(c)}${hex(l)}`;
    }
  }
  return null;
}

function reportLoss(f, reason) {
  // 可靠传输（WS）下帧不会丢，只是晚到——不上报，避免 IDR 风暴
  if (transportReliable) return;
  consecutiveDrops++;
  // moonlight 双保险：防风暴 + 连续 120 帧强制
  if (lossSentPending && consecutiveDrops < 120) return;
  lossSentPending = true;
  stats.lossSent++;
  self.postMessage({ type: 'frameLoss', frameIndex: f.frameIndex, reason });
}

// ---------------- 秒级统计上报 ----------------
setInterval(() => {
  const gaps = stats.arrivalGaps.slice().sort((a, b) => a - b);
  const p95 = gaps.length ? gaps[Math.floor(gaps.length * 0.95)] : 0;
  self.postMessage({
    type: 'stats',
    pkts: stats.pkts, bytes: stats.bytes,
    framesComplete: stats.framesComplete,
    fecRecovered: stats.fecRecovered, fecFailed: stats.fecFailed,
    lossSent: stats.lossSent, decoded: stats.decoded,
    decodeQueue: decoder ? decoder.decodeQueueSize : 0,
    arrivalGapP95Ms: Math.round(p95 * 10) / 10,
  });
}, 1000);
