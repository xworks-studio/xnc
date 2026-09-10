// proto.js — MVP-RTV 媒体包头解析（与 docs/proto.md §1.1、host/src/framing.rs 对齐）。

export const HDR_LEN = 34;
export const MAGIC = 'MVP1';

export const FLAG_SOF = 0x01;
export const FLAG_EOF = 0x02;
export const FLAG_PIC = 0x04;
export const FLAG_PARITY = 0x08;

const dv = new DataView(new ArrayBuffer(HDR_LEN));

/**
 * 解析包头。返回 null 表示非法（含 magic 校验）。
 * 注意：buf 通常是从网络收到的整个 datagram 副本。
 */
export function parseHdr(buf) {
  if (buf.byteLength < HDR_LEN) return null;
  const b = buf instanceof Uint8Array ? buf : new Uint8Array(buf);
  if (b[0] !== 0x4d || b[1] !== 0x56 || b[2] !== 0x50 || b[3] !== 0x31) return null;
  const v = new DataView(b.buffer, b.byteOffset, b.byteLength);
  return {
    flags: b[4],
    blockIdx: b[5],
    shardIndex: v.getUint16(6, true),
    dataShards: v.getUint16(8, true),
    parityShards: v.getUint16(10, true),
    blockTotal: v.getUint32(12, true),
    frameIndex: v.getUint32(16, true),
    codecId: b[20],
    keyframe: b[21] === 1,
    captureUnixUs: Number(v.getBigUint64(24, true)),
    payloadLen: v.getUint16(32, true),
  };
}

/** 数据 shard 的期望 flags（恢复校验用，与 host data_flags 一致）。 */
export function expectDataFlags(blockIdx, shardIndex, k, isLastBlock) {
  const first = blockIdx === 0 && shardIndex === 0;
  const last = isLastBlock && shardIndex + 1 === k;
  if (first && last) return FLAG_SOF | FLAG_EOF;
  if (first) return FLAG_SOF;
  if (last) return FLAG_EOF;
  return FLAG_PIC;
}
