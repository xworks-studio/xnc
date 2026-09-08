// gen-fixture.js — 生成 RS 黄金向量 fixture（双端交叉验证基准）。
// 用法: node web/scripts/gen-rs-fixture.mjs → 写 proto-fixtures/fixture-rs.json
import { rsEncode } from '../src/lib/rtv/rs.js';
import { writeFileSync } from 'node:fs';

// mulberry32 确定性 PRNG，保证 fixture 可复现
function prng(seed) {
  let a = seed >>> 0;
  return () => {
    a |= 0; a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

const hex = (u8) => Buffer.from(u8).toString('hex');

const cases = [
  { k: 4, r: 2, len: 64, seed: 11 },
  { k: 16, r: 6, len: 512, seed: 22 },
  { k: 100, r: 20, len: 300, seed: 33 },
];

for (const c of cases) {
  const rand = prng(c.seed);
  c.data = Array.from({ length: c.k }, () => {
    const b = new Uint8Array(c.len);
    for (let i = 0; i < c.len; i++) b[i] = Math.floor(rand() * 256);
    return hex(b);
  });
  const shards = c.data.map((h) => new Uint8Array(Buffer.from(h, 'hex')));
  c.parity = rsEncode(shards, c.r).map(hex);
  // 擦除图案：覆盖“仅数据/仅校验/混合”，每个 ≤ r 个（MDS 保证可恢复）
  const total = c.k + c.r;
  const erases = [];
  for (let i = 0; i < c.r; i++) erases.push([i]); // 丢数据 shard
  for (let i = 0; i < c.r; i++) erases.push([c.k + i]); // 丢校验 shard
  erases.push([0, total - 1]); // 混合
  if (c.r >= 2) erases.push([Math.floor(c.k / 2), c.k, c.k + 1].slice(0, c.r));
  c.erases = erases;
}

const fixture = {
  poly: '0x11D',
  generator: 2,
  note: 'Cauchy RS GF(2^8): a[i][j]=inv(i ^ (r+j)); data[k] -> parity[r]; erases patterns recoverable',
  cases,
};
writeFileSync(new URL('../../proto-fixtures/fixture-rs.json', import.meta.url), JSON.stringify(fixture));
console.log(`fixture written: ${cases.map((c) => `k=${c.k},r=${c.r}`).join(' ')}`);
