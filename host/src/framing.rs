//! 媒体包组帧/FEC（docs/proto.md §1）。
//!
//! 一帧 H.264 Annex-B → ≤4 个 FEC block → 每 block k 数据 shard + r 校验 shard。
//! shard = 完整包字节（34B 头 + payload）；发送端不填充短包（解码端补零运算）。
//! 模型移植自 moonlight-common-c 的 NV_VIDEO_PACKET + RtpVideoQueue FEC 编码侧。

use crate::rs::Rs;

pub const HEADER_LEN: usize = 34;
pub const MAGIC: [u8; 4] = *b"MVP1";
/// 单 shard payload 目标字节数（QUIC datagram 安全上限内）
pub const SHARD_PAYLOAD_TARGET: usize = 1100;
pub const MAX_BLOCKS: usize = 4;
pub const MAX_SHARDS_PER_BLOCK: usize = 255;

pub const FLAG_SOF: u8 = 0x01;
pub const FLAG_EOF: u8 = 0x02;
pub const FLAG_PIC_DATA: u8 = 0x04;
pub const FLAG_PARITY: u8 = 0x08;

pub const CODEC_H264: u8 = 1;

/// FEC 计划（对一帧）
#[derive(Debug, Clone)]
pub struct FramePlan {
    pub blocks: usize,
    pub k: Vec<usize>,
    pub r: Vec<usize>,
    /// 每 block 的 shard 定长（= 34 + 该 block 最大 payload）
    pub shard_len: Vec<usize>,
}

/// 计算一帧的切分与 FEC 参数（docs/proto.md §1.2 规则）。
/// 返回 (plan, 各 block 的数据 payload 切分)。
pub fn plan_frame(frame_len: usize, fec_percentage: u8) -> FramePlan {
    // 先按单 block 计算
    let k1 = frame_len.div_ceil(SHARD_PAYLOAD_TARGET).max(1);
    if k1 <= MAX_SHARDS_PER_BLOCK {
        let blocks = 1;
        let k = k1;
        let r = parity_for(k, fec_percentage).min(MAX_SHARDS_PER_BLOCK - k);
        return FramePlan {
            blocks,
            k: vec![k],
            r: vec![r],
            shard_len: vec![HEADER_LEN + actual_payload_len(frame_len, k)],
        };
    }
    // 超单 block 容量 → 均分 ≤4 block
    let blocks = k1.div_ceil(MAX_SHARDS_PER_BLOCK);
    if blocks > MAX_BLOCKS {
        // 超大帧：跳过 FEC（Sunshine 超大帧禁 FEC 同款行为）
        return FramePlan {
            blocks: 1,
            k: vec![k1],
            r: vec![0],
            shard_len: vec![HEADER_LEN + SHARD_PAYLOAD_TARGET],
        };
    }
    let per = frame_len.div_ceil(blocks);
    let mut plan = FramePlan { blocks, k: vec![], r: vec![], shard_len: vec![] };
    for b in 0..blocks {
        let start = b * per;
        let blen = frame_len.saturating_sub(start).min(per);
        let k = blen.div_ceil(SHARD_PAYLOAD_TARGET).max(1);
        let r = parity_for(k, fec_percentage).min(MAX_SHARDS_PER_BLOCK.saturating_sub(k));
        plan.k.push(k);
        plan.r.push(r);
        plan.shard_len.push(HEADER_LEN + actual_payload_len(blen, k));
    }
    plan
}

/// 末 shard 不足时其他 shard 的实际 payload（= 每块均分后的目标长度，≥1）
fn actual_payload_len(block_len: usize, k: usize) -> usize {
    (block_len.div_ceil(k)).max(1).min(SHARD_PAYLOAD_TARGET)
}

/// r = max(2, ceil(k × fec% / 100))（moonlight minRequiredFecPackets=2 同款下限）
fn parity_for(k: usize, fec_percentage: u8) -> usize {
    let r = k.saturating_mul(fec_percentage as usize).div_ceil(100);
    r.max(2)
}

/// 写包头（小端）
pub fn write_header(buf: &mut [u8], flags: u8, block_idx: u8, shard_index: u16,
    data_shards: u16, parity_shards: u16, block_total: u32, frame_index: u32,
    codec_id: u8, keyframe: u8, capture_unix_us: u64, payload_len: u16) {
    buf[0..4].copy_from_slice(&MAGIC);
    buf[4] = flags;
    buf[5] = block_idx;
    buf[6..8].copy_from_slice(&shard_index.to_le_bytes());
    buf[8..10].copy_from_slice(&data_shards.to_le_bytes());
    buf[10..12].copy_from_slice(&parity_shards.to_le_bytes());
    buf[12..16].copy_from_slice(&block_total.to_le_bytes());
    buf[16..20].copy_from_slice(&frame_index.to_le_bytes());
    buf[20] = codec_id;
    buf[21] = keyframe;
    buf[22..24].copy_from_slice(&0u16.to_le_bytes());
    buf[24..32].copy_from_slice(&capture_unix_us.to_le_bytes());
    buf[32..34].copy_from_slice(&payload_len.to_le_bytes());
}

pub struct PacketMeta {
    pub flags: u8,
    pub block_idx: u8,
    pub shard_index: u16,
    pub data_shards: u16,
    pub parity_shards: u16,
    pub block_total: u32,
    pub frame_index: u32,
    pub codec_id: u8,
    pub keyframe: u8,
    pub capture_unix_us: u64,
    pub payload_len: u16,
}

/// 解析包头（含 magic 校验）。
pub fn parse_header(buf: &[u8]) -> Option<PacketMeta> {
    if buf.len() < HEADER_LEN || buf[0..4] != MAGIC {
        return None;
    }
    Some(PacketMeta {
        flags: buf[4],
        block_idx: buf[5],
        shard_index: u16::from_le_bytes([buf[6], buf[7]]),
        data_shards: u16::from_le_bytes([buf[8], buf[9]]),
        parity_shards: u16::from_le_bytes([buf[10], buf[11]]),
        block_total: u32::from_le_bytes([buf[12], buf[13], buf[14], buf[15]]),
        frame_index: u32::from_le_bytes([buf[16], buf[17], buf[18], buf[19]]),
        codec_id: buf[20],
        keyframe: buf[21],
        capture_unix_us: u64::from_le_bytes(buf[24..32].try_into().unwrap()),
        payload_len: u16::from_le_bytes([buf[32], buf[33]]),
    })
}

/// 数据 shard 的 flags（按位置推导；校验/恢复共用此规则）
pub fn data_flags(block_idx: usize, shard_index: usize, k: usize, is_last_block: bool) -> u8 {
    let first = block_idx == 0 && shard_index == 0;
    let last = is_last_block && shard_index + 1 == k;
    match (first, last) {
        (true, true) => FLAG_SOF | FLAG_EOF,
        (true, false) => FLAG_SOF,
        (false, true) => FLAG_EOF,
        (false, false) => FLAG_PIC_DATA,
    }
}

/// 把一帧编码数据切成带 FEC 的完整包列表。
pub fn build_packets(frame: &[u8], frame_index: u32, codec_id: u8, keyframe: bool,
    capture_unix_us: u64, fec_percentage: u8) -> Vec<Vec<u8>> {
    let plan = plan_frame(frame.len(), fec_percentage);
    let mut out = Vec::with_capacity(plan.k.iter().sum::<usize>() + plan.r.iter().sum::<usize>());
    let mut offset = 0usize;
    for b in 0..plan.blocks {
        let k = plan.k[b];
        let r = plan.r[b];
        let is_last_block = b + 1 == plan.blocks;
        // 数据 shard：整包（头+payload）
        let mut data_shards: Vec<Vec<u8>> = Vec::with_capacity(k);
        for i in 0..k {
            let shard_payload = plan.shard_len[b] - HEADER_LEN;
            let end = (offset + shard_payload).min(frame.len());
            let payload = &frame[offset..end];
            offset = end;
            let mut pkt = vec![0u8; HEADER_LEN + payload.len()];
            write_header(
                &mut pkt, data_flags(b, i, k, is_last_block), b as u8, i as u16,
                k as u16, r as u16, plan.blocks as u32, frame_index, codec_id,
                keyframe as u8, capture_unix_us, payload.len() as u16,
            );
            pkt[HEADER_LEN..].copy_from_slice(payload);
            data_shards.push(pkt);
        }
        out.extend(data_shards.iter().cloned());
        // 校验 shard：对定长化(补零)的数据整包做 RS
        if r > 0 {
            let shard_len = plan.shard_len[b];
            let padded: Vec<Vec<u8>> = data_shards
                .iter()
                .map(|p| {
                    let mut v = p.clone();
                    v.resize(shard_len, 0);
                    v
                })
                .collect();
            let rs = Rs::new(k, r);
            for (j, parity) in rs.encode(&padded).into_iter().enumerate() {
                let mut pkt = vec![0u8; HEADER_LEN + parity.len()];
                write_header(
                    &mut pkt, FLAG_PARITY, b as u8, (k + j) as u16, k as u16, r as u16,
                    plan.blocks as u32, frame_index, codec_id, keyframe as u8,
                    capture_unix_us, parity.len() as u16,
                );
                pkt[HEADER_LEN..].copy_from_slice(&parity);
                out.push(pkt);
            }
        }
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn unix_us() -> u64 {
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_micros() as u64
    }

    /// 端到端：组包 → 模拟丢包（每 block 丢 ≤r 个数据/校验 shard）
    /// → RS 恢复 → 拼回原帧。移植自 moonlight RtpVideoQueue 的恢复路径。
    #[test]
    fn fec_roundtrip_with_loss() {
        for (frame_len, fec_pct, drops) in [
            (100usize, 20u8, vec![0usize]),                 // 单 shard 帧
            (3_500, 20, vec![0, 3]),                        // 多 shard
            (80_000, 20, vec![0, 5, 9]),                    // 大帧
            (600_000, 20, vec![1, 2, 3, 4]),                // 多 block 大帧
        ] {
            let frame: Vec<u8> = (0..frame_len).map(|i| (i * 31 % 251) as u8).collect();
            let pkts = build_packets(&frame, 7, CODEC_H264, true, unix_us(), fec_pct);
            // 按 (block, shard) 建索引
            use std::collections::HashMap;
            let mut by_block: HashMap<u8, Vec<Option<Vec<u8>>>> = HashMap::new();
            for p in &pkts {
                let m = parse_header(p).unwrap();
                let e = by_block.entry(m.block_idx).or_insert_with(|| {
                    let total = m.data_shards as usize + m.parity_shards as usize;
                    vec![None; total]
                });
                e[m.shard_index as usize] = Some(p.clone());
            }
            // 丢指定 shard（只在数据区且 ≤r 的数量）
            let plan = plan_frame(frame_len, fec_pct);
            let mut blocks_sorted: Vec<_> = by_block.keys().copied().collect();
            blocks_sorted.sort_unstable();
            let mut dropped_total = 0;
            for (bi, &b) in blocks_sorted.iter().enumerate() {
                let slots = by_block.get_mut(&b).unwrap();
                let r = plan.r[bi];
                let take = drops.iter().take(r).copied().collect::<Vec<_>>();
                for &d in &take {
                    if slots[d].is_some() {
                        slots[d] = None;
                        dropped_total += 1;
                    }
                }
            }
            assert!(dropped_total > 0, "frame_len={frame_len}: nothing dropped");

            // 恢复并重组帧
            let mut recovered = Vec::new();
            for (bi, &b) in blocks_sorted.iter().enumerate() {
                let slots = &by_block[&b];
                let k = plan.k[bi];
                let r = plan.r[bi];
                let missing: Vec<usize> = slots.iter().enumerate().filter(|(_, s)| s.is_none()).map(|(i, _)| i).collect();
                if missing.is_empty() {
                    for s in slots.iter().take(k) {
                        let m = parse_header(s.as_ref().unwrap()).unwrap();
                        recovered.extend_from_slice(&s.as_ref().unwrap()[HEADER_LEN..HEADER_LEN + m.payload_len as usize]);
                    }
                    continue;
                }
                // FEC 运算域（proto.md §1）：数据 shard = 完整包（含头），
                // 校验 shard = 校验包的内容部分（剥掉校验包自己的 34B 头）
                let shard_len = plan.shard_len[bi];
                let mut shards: Vec<Option<Vec<u8>>> = slots
                    .iter()
                    .enumerate()
                    .map(|(idx, s)| {
                        s.as_ref().map(|v| {
                            let src = if idx >= k { &v[HEADER_LEN..] } else { &v[..] };
                            let mut nv = src.to_vec();
                            nv.resize(shard_len, 0);
                            nv
                        })
                    })
                    .collect();
                for &mi in &missing {
                    shards[mi] = None;
                }
                let rs = Rs::new(k, r);
                let rec = rs.recover(&shards).expect("rs recover");
                for (i, s) in rec.iter().enumerate() {
                    // 恢复包必须过合法性校验（magic + flags + payloadLen 位置一致）
                    let m = parse_header(s).unwrap_or_else(|| {
                        panic!("bad recovered header b={b} i={i} k={k} r={r} shard_len={shard_len} frame_len={frame_len} missing={missing:?} first8={:02x?}", &s[..8.min(s.len())]);
                    });
                    assert_eq!(m.frame_index, 7);
                    assert_eq!(m.block_idx, b);
                    let expect_flags = data_flags(b as usize, i, k, bi + 1 == blocks_sorted.len());
                    assert_eq!(m.flags, expect_flags, "recovered flags b={b} i={i}");
                    recovered.extend_from_slice(&s[HEADER_LEN..HEADER_LEN + m.payload_len as usize]);
                }
            }
            assert_eq!(recovered, frame, "frame_len={frame_len} recovered mismatch");
        }
    }

    #[test]
    fn plan_rules() {
        let p = plan_frame(50, 20);
        assert_eq!((p.blocks, p.k[0], p.r[0]), (1, 1, 2)); // 最小 r=2
        let p = plan_frame(SHARD_PAYLOAD_TARGET * 10, 20);
        assert_eq!(p.k[0], 10);
        assert_eq!(p.r[0], 2); // ceil(10*20/100)=2
        let p = plan_frame(SHARD_PAYLOAD_TARGET * 250, 20);
        assert_eq!(p.blocks, 1); // 250 ≤ 255
        let p = plan_frame(SHARD_PAYLOAD_TARGET * 300, 20);
        assert_eq!(p.blocks, 2);
        assert!(p.k.iter().all(|&k| k <= 255));
    }

    #[test]
    fn header_roundtrip() {
        let mut buf = [0u8; HEADER_LEN];
        write_header(&mut buf, FLAG_SOF | FLAG_PIC_DATA, 2, 33, 44, 55, 4, 123456, CODEC_H264, 1, 1725700000_123_456, 777);
        let m = parse_header(&buf).unwrap();
        assert_eq!(m.flags, FLAG_SOF | FLAG_PIC_DATA);
        assert_eq!(m.block_idx, 2);
        assert_eq!(m.shard_index, 33);
        assert_eq!(m.data_shards, 44);
        assert_eq!(m.parity_shards, 55);
        assert_eq!(m.block_total, 4);
        assert_eq!(m.frame_index, 123456);
        assert_eq!(m.capture_unix_us, 1725700000_123_456);
        assert_eq!(m.payload_len, 777);
    }
}
