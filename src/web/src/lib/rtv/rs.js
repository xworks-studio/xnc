// rs.js — Cauchy Reed-Solomon over GF(2^8), 生成多项式 0x11D。
//
// 与 host/src/rs.rs 严格同源（同一算法、同一矩阵构造），由 proto/fixture-rs.json
// 黄金向量在双端测试中交叉验证（见 docs/proto.md §1.3）。
//
// 码型: 系统码 [I_k ; A]，A 为 r×k Cauchy 矩阵 a[i][j] = 1/(x_i ^ y_j)，
//       x_i = i, y_j = r + j。任意 k 个存活 shard 可恢复全部数据（MDS）。
// 字节序: 无（纯字节运算）。shard 定长，由调用方保证。

'use strict';

// ---------- GF(2^8) 基础表（生成元 2，本原多项式 0x11D） ----------

const GF_EXP = new Uint8Array(512);
const GF_LOG = new Uint8Array(256);
(() => {
  let x = 1;
  for (let i = 0; i < 255; i++) {
    GF_EXP[i] = x;
    GF_LOG[x] = i;
    x <<= 1;
    if (x & 0x100) x ^= 0x11d; // 模本原多项式
  }
  for (let i = 255; i < 512; i++) GF_EXP[i] = GF_EXP[i - 255]; // 免取模
})();

function gfMul(a, b) {
  if (a === 0 || b === 0) return 0;
  return GF_EXP[GF_LOG[a] + GF_LOG[b]];
}

function gfInv(a) {
  if (a === 0) throw new Error('gfInv(0)');
  return GF_EXP[255 - GF_LOG[a]];
}

// ---------- Cauchy 矩阵 ----------

/** 返回 r×k 的 Cauchy 矩阵（行主序嵌套数组）。 */
function cauchyMatrix(r, k) {
  const m = new Array(r);
  for (let i = 0; i < r; i++) {
    const row = new Uint8Array(k);
    for (let j = 0; j < k; j++) row[j] = gfInv(i ^ (r + j)); // x_i=i, y_j=r+j
    m[i] = row;
  }
  return m;
}

// ---------- 编码 ----------

/**
 * 对 k 个定长 shard（各 shardLen 字节）生成 r 个 parity shard。
 * @param {Uint8Array[]} data k 个数据 shard
 * @param {number} r 校验 shard 数
 * @returns {Uint8Array[]} r 个 parity shard
 */
function rsEncode(data, r) {
  const k = data.length;
  if (k === 0 || k + r > 255) throw new Error(`bad k=${k} r=${r}`);
  const shardLen = data[0].length;
  const A = cauchyMatrix(r, k);
  const parity = [];
  for (let i = 0; i < r; i++) {
    const p = new Uint8Array(shardLen);
    const aRow = A[i];
    for (let j = 0; j < k; j++) {
      const c = aRow[j];
      if (c === 0) continue;
      const dj = data[j];
      for (let t = 0; t < shardLen; t++) p[t] ^= gfMul(c, dj[t]);
    }
    parity.push(p);
  }
  return parity;
}

// ---------- 解码（GF 高斯消元） ----------

/**
 * 从存活 shard 恢复全部数据 shard。
 * @param {(Uint8Array|null)[]} shards 长度 k+r，缺失位置为 null（其余定长 shardLen）
 * @param {number} k 数据 shard 数
 * @returns {Uint8Array[]|null} k 个数据 shard；存活数 < k 时返回 null
 */
function rsRecover(shards, k) {
  const total = shards.length;
  const r = total - k;
  const survivors = [];
  for (let i = 0; i < total && survivors.length < k; i++) if (shards[i]) survivors.push(i);
  if (survivors.length < k) return null;
  const shardLen = shards[survivors[0]].length;
  const A = cauchyMatrix(r, k);

  // 构建 k×k 线性方程组 M·D = V：
  //   数据 survivor i  → 行 = 单位向量 e_i，值 = shard_i
  //   校验 survivor j  → 行 = A[j]，值 = parity_j
  // V 必须拷贝：消元会原地改写，而这些缓冲属于调用方（收到的媒体包字节）。
  const M = new Array(k);
  const V = new Array(k);
  for (let row = 0; row < k; row++) {
    const s = survivors[row];
    const coef = new Uint8Array(k);
    if (s < k) coef[s] = 1;
    else coef.set(A[s - k]);
    M[row] = coef;
    V[row] = new Uint8Array(shards[s]);
  }

  // 高斯消元解 D（增广列为每个字节位置的向量，批量处理）
  const rowBytes = new Array(k); // 解出的 D 行（对应 pivot 维度）
  for (let col = 0; col < k; col++) {
    // 选主元
    let piv = -1;
    for (let i = col; i < k; i++) if (M[i][col] !== 0) { piv = i; break; }
    if (piv < 0) return null; // 奇异（正确构造下不应发生，防御性返回）
    if (piv !== col) { [M[col], M[piv]] = [M[piv], M[col]]; [V[col], V[piv]] = [V[piv], V[col]]; }
    // 归一化主元行
    const inv = gfInv(M[col][col]);
    if (inv !== 1) {
      const mr = M[col];
      for (let j = 0; j < k; j++) mr[j] = gfMul(mr[j], inv);
      const v = V[col], nv = new Uint8Array(shardLen);
      for (let t = 0; t < shardLen; t++) nv[t] = gfMul(inv, v[t]);
      V[col] = nv;
    }
    // 消去其他行
    for (let i = 0; i < k; i++) {
      if (i === col) continue;
      const c = M[i][col];
      if (c === 0) continue;
      const mr = M[col], mi = M[i];
      for (let j = 0; j < k; j++) if (mr[j]) mi[j] ^= gfMul(c, mr[j]);
      const vSrc = V[col], vDst = V[i];
      for (let t = 0; t < shardLen; t++) {
        const term = gfMul(c, vSrc[t]);
        if (term) vDst[t] ^= term;
      }
    }
    rowBytes[col] = V[col];
  }
  // 消元后 M = I，V 即 D
  return rowBytes;
}

export { gfMul, gfInv, cauchyMatrix, rsEncode, rsRecover };
