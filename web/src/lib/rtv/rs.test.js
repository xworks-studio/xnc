// rs.test.js — Web 端 RS 实现验证（vitest）：编码与黄金向量一致 + 各擦除
// 图案恢复 == 原始数据。与 Rust 侧 host/src/rs.rs::golden_fixture 双端对拍
// （fixture：proto-fixtures/fixture-rs.json，由 web/scripts/gen-rs-fixture.mjs
// 生成；断言集忠实保留自 mvp test/rs.test.js）。
import { readFileSync } from 'node:fs';
import { test } from 'vitest';
import { rsEncode, rsRecover, cauchyMatrix, gfMul, gfInv } from './rs.js';

const fixture = JSON.parse(
  readFileSync(new URL('../../../../proto-fixtures/fixture-rs.json', import.meta.url), 'utf8'),
);
const bytesEq = (a, b) => a.length === b.length && a.every((v, i) => v === b[i]);
// fail-fast 断言：失败抛错（case 命名进错误消息）。
const bad = (name, why) => { throw new Error(`FAIL ${name}: ${why}`); };
const ok = (_name) => { /* pass */ };

test('rs golden vectors + erasure recovery + gf invariants', () => {
  // 0) GF 基本性质抽检
  {
    let good = true;
    for (const a of [1, 2, 3, 5, 17, 128, 255]) if (gfMul(a, gfInv(a)) !== 1) good = false;
    if (good) ok('gf identity a*inv(a)==1');
    else bad('gf identity', 'inverse check failed');
  }

  for (const c of fixture.cases) {
    const data = c.data.map((h) => new Uint8Array(Buffer.from(h, 'hex')));
    const parity = rsEncode(data, c.r);
    // 1) 编码与 fixture parity 一致
    let same = parity.length === c.parity.length;
    parity.forEach((p, i) => { if (Buffer.from(p).toString('hex') !== c.parity[i]) same = false; });
    if (same) ok(`encode k=${c.k} r=${c.r}`);
    else bad(`encode k=${c.k} r=${c.r}`, 'parity mismatch');

    // 2) 各擦除图案恢复 == 原始数据
    for (const e of c.erases) {
      const shards = [...data, ...parity].map((s) => new Uint8Array(s));
      for (const idx of e) shards[idx] = null;
      const rec = rsRecover(shards, c.k);
      let eq = rec !== null;
      if (eq) rec.forEach((rr, i) => { if (!bytesEq(rr, data[i])) eq = false; });
      if (eq) ok(`recover k=${c.k} erase=[${e}]`);
      else bad(`recover k=${c.k} erase=[${e}]`, rec === null ? 'returned null' : 'data mismatch');
    }

    // 3) MDS 穷举（仅小 case）：删任意 r 个都可恢复
    if (c.k <= 6 && c.r <= 3) {
      const total = c.k + c.r;
      const combos = [];
      const rec = (start, acc) => {
        if (acc.length === c.r) { combos.push([...acc]); return; }
        for (let i = start; i < total; i++) { acc.push(i); rec(i + 1, acc); acc.pop(); }
      };
      rec(0, []);
      let all = true;
      for (const e of combos) {
        const shards = [...data, ...parity].map((s) => new Uint8Array(s));
        for (const idx of e) shards[idx] = null;
        const r2 = rsRecover(shards, c.k);
        if (!r2 || r2.some((rr, i) => !bytesEq(rr, data[i]))) { all = false; bad(`mds k=${c.k} erase=[${e}]`, 'not recovered'); }
      }
      if (all) ok(`mds exhaustive k=${c.k} r=${c.r} (${combos.length} combos)`);
    }

    // 4) 存活 < k 时必须返回 null
    {
      const shards = [...data, ...parity].map((s) => new Uint8Array(s));
      for (let i = 0; i <= c.r; i++) shards[i] = null; // 删 r+1 个
      if (rsRecover(shards, c.k) === null) ok(`underflow null k=${c.k}`);
      else bad(`underflow k=${c.k}`, 'should be null');
    }
  }

  // 5) Cauchy 矩阵基本约束：元素非零
  {
    const m = cauchyMatrix(6, 16);
    let nz = true;
    for (const row of m) for (const v of row) if (v === 0) nz = false;
    if (nz) ok('cauchy nonzero');
    else bad('cauchy nonzero', 'zero entry');
  }
});
