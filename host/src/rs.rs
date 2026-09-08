//! Cauchy Reed-Solomon over GF(2^8)（本原多项式 0x11D，生成元 2）。
//!
//! 与 web 端 `rs.ts`（web/src/lib/rtv/）严格同源；由 `../proto-fixtures/fixture-rs.json`
//! 黄金向量在双端测试中交叉验证（协议规范 §1.3）。矩阵构造：a[i][j] = inv(x_i ^ y_j)，
//! x_i = i（0..r），y_j = r + j（0..k）——两区间不相交保证无零元，k+r ≤ 255。

use std::sync::OnceLock;

// ---------------- GF(2^8) 表 ----------------

static TABLES: OnceLock<([u8; 512], [u8; 256])> = OnceLock::new();

fn tables() -> &'static ([u8; 512], [u8; 256]) {
    TABLES.get_or_init(|| {
        let mut exp = [0u8; 512];
        let mut log = [0u8; 256];
        let mut x: usize = 1;
        for i in 0..255 {
            exp[i] = x as u8;
            log[x] = i as u8;
            x <<= 1;
            if x & 0x100 != 0 {
                x ^= 0x11d;
            }
        }
        for i in 255..512 {
            exp[i] = exp[i - 255];
        }
        (exp, log)
    })
}

#[inline]
fn gf_mul(a: u8, b: u8) -> u8 {
    if a == 0 || b == 0 {
        return 0;
    }
    let (exp, log) = tables();
    exp[log[a as usize] as usize + log[b as usize] as usize]
}

#[inline]
fn gf_inv(a: u8) -> u8 {
    assert_ne!(a, 0, "gf_inv(0)");
    let (exp, log) = tables();
    exp[255 - log[a as usize] as usize]
}

// ---------------- Rs ----------------

#[derive(Clone)]
pub struct Rs {
    k: usize,
    r: usize,
    /// r×k Cauchy 矩阵（行主序）
    matrix: Vec<Vec<u8>>,
}

impl Rs {
    pub fn new(k: usize, r: usize) -> Self {
        assert!(k > 0 && r > 0 && k + r <= 255, "bad k={k} r={r}");
        let matrix = (0..r)
            .map(|i| (0..k).map(|j| gf_inv((i ^ (r + j)) as u8)).collect())
            .collect();
        Rs { k, r, matrix }
    }

    pub fn k(&self) -> usize {
        self.k
    }
    pub fn r(&self) -> usize {
        self.r
    }

    /// 对 k 个定长 shard 生成 r 个 parity shard。
    pub fn encode(&self, data: &[Vec<u8>]) -> Vec<Vec<u8>> {
        assert_eq!(data.len(), self.k);
        let shard_len = data[0].len();
        let mut parity = vec![vec![0u8; shard_len]; self.r];
        for (i, row) in self.matrix.iter().enumerate() {
            let p = &mut parity[i];
            for (j, coef) in row.iter().enumerate() {
                if *coef == 0 {
                    continue;
                }
                let dj = &data[j];
                for t in 0..shard_len {
                    p[t] ^= gf_mul(*coef, dj[t]);
                }
            }
        }
        parity
    }

    /// 从存活 shard 恢复数据 shard。shards 长度 k+r，缺失为 None。
    /// 返回 None 表示存活数 < k（或矩阵奇异，正确构造下不会发生）。
    pub fn recover(&self, shards: &[Option<Vec<u8>>]) -> Option<Vec<Vec<u8>>> {
        let total = self.k + self.r;
        assert_eq!(shards.len(), total);
        let survivors: Vec<usize> = (0..total)
            .filter(|&i| shards[i].is_some())
            .take(self.k)
            .collect();
        if survivors.len() < self.k {
            return None;
        }
        let shard_len = shards[survivors[0]].as_ref().unwrap().len();

        // 方程组 M·D = V；V 拷贝（消元原地改写，缓冲属于调用方）
        let mut m: Vec<Vec<u8>> = Vec::with_capacity(self.k);
        let mut v: Vec<Vec<u8>> = Vec::with_capacity(self.k);
        for &s in &survivors {
            let mut coef = vec![0u8; self.k];
            if s < self.k {
                coef[s] = 1;
            } else {
                coef.copy_from_slice(&self.matrix[s - self.k]);
            }
            m.push(coef);
            v.push(shards[s].clone().unwrap());
        }

        // GF 高斯消元
        for col in 0..self.k {
            let piv = (col..self.k).find(|&i| m[i][col] != 0)?;
            m.swap(col, piv);
            v.swap(col, piv);

            let inv = gf_inv(m[col][col]);
            if inv != 1 {
                for c in m[col].iter_mut() {
                    *c = gf_mul(*c, inv);
                }
                for b in v[col].iter_mut() {
                    *b = gf_mul(*b, inv);
                }
            }
            let pivot_row = m[col].clone();
            let pivot_val = v[col].clone();
            for i in 0..self.k {
                if i == col {
                    continue;
                }
                let c = m[i][col];
                if c == 0 {
                    continue;
                }
                for (j, pc) in pivot_row.iter().enumerate() {
                    if *pc != 0 {
                        m[i][j] ^= gf_mul(c, *pc);
                    }
                }
                for t in 0..shard_len {
                    let term = gf_mul(c, pivot_val[t]);
                    if term != 0 {
                        v[i][t] ^= term;
                    }
                }
            }
        }
        Some(v)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn hex_decode(s: &str) -> Vec<u8> {
        (0..s.len() / 2)
            .map(|i| u8::from_str_radix(&s[i * 2..i * 2 + 2], 16).unwrap())
            .collect()
    }
    fn hex_encode(v: &[u8]) -> String {
        v.iter().map(|b| format!("{b:02x}")).collect()
    }

    /// 黄金向量交叉验证：编码 parity 一致 + 各擦除图案恢复 == 原始数据。
    /// fixture 由 web 端 rs.js 生成（test/gen-fixture.js），双端对拍。
    #[test]
    fn golden_fixture() {
        let path = concat!(env!("CARGO_MANIFEST_DIR"), "/../proto-fixtures/fixture-rs.json");
        let fixture = std::fs::read_to_string(path)
            .unwrap_or_else(|_| panic!("fixture 缺失：先运行 node test/gen-fixture.js"));
        let v: serde_json::Value = serde_json::from_str(&fixture).unwrap();
        for case in v["cases"].as_array().unwrap() {
            let k = case["k"].as_u64().unwrap() as usize;
            let r = case["r"].as_u64().unwrap() as usize;
            let data: Vec<Vec<u8>> = case["data"]
                .as_array()
                .unwrap()
                .iter()
                .map(|h| hex_decode(h.as_str().unwrap()))
                .collect();
            let parity_expect: Vec<String> = case["parity"]
                .as_array()
                .unwrap()
                .iter()
                .map(|h| h.as_str().unwrap().to_lowercase())
                .collect();

            let rs = Rs::new(k, r);
            let parity = rs.encode(&data);
            for (i, p) in parity.iter().enumerate() {
                assert_eq!(hex_encode(p), parity_expect[i], "parity mismatch k={k} shard={i}");
            }

            for e in case["erases"].as_array().unwrap() {
                let erased: Vec<usize> = e
                    .as_array()
                    .unwrap()
                    .iter()
                    .map(|x| x.as_u64().unwrap() as usize)
                    .collect();
                let mut shards: Vec<Option<Vec<u8>>> =
                    data.iter().chain(parity.iter()).map(|s| Some(s.clone())).collect();
                assert_eq!(shards.len(), k + r);
                for &idx in &erased {
                    shards[idx] = None;
                }
                let rec = rs.recover(&shards).expect("recover failed");
                for (i, rr) in rec.iter().enumerate() {
                    assert_eq!(rr, &data[i], "recovered mismatch k={k} erase={erased:?} shard={i}");
                }
            }

            // 存活 < k 必须失败
            let mut shards: Vec<Option<Vec<u8>>> =
                data.iter().chain(parity.iter()).map(|s| Some(s.clone())).collect();
            for i in 0..=r {
                shards[i] = None;
            }
            assert!(rs.recover(&shards).is_none(), "underflow must fail k={k}");
        }
    }

    #[test]
    fn gf_sanity() {
        for a in [1u8, 2, 3, 5, 17, 128, 255] {
            assert_eq!(gf_mul(a, gf_inv(a)), 1);
        }
    }

    #[test]
    fn mds_exhaustive_small() {
        let (k, r, len) = (4usize, 2usize, 48usize);
        let data: Vec<Vec<u8>> = (0..k).map(|i| vec![i as u8 + 1; len]).collect();
        let rs = Rs::new(k, r);
        let parity = rs.encode(&data);
        // 穷举删任意 r 个
        let total = k + r;
        for a in 0..total {
            for b in (a + 1)..total {
                let mut shards: Vec<Option<Vec<u8>>> =
                    data.iter().chain(parity.iter()).map(|s| Some(s.clone())).collect();
                shards[a] = None;
                shards[b] = None;
                let rec = rs.recover(&shards).unwrap_or_else(|| panic!("a={a} b={b}"));
                assert!(rec.iter().zip(data.iter()).all(|(x, y)| x == y));
            }
        }
    }
}

#[cfg(test)]
mod k1_tests {
    use super::*;
    #[test]
    fn k1_r2_roundtrip() {
        let mut shard = vec![0x4d, 0x56, 0x50, 0x31, 7, 42, 0, 0, 1, 0, 2, 0, 1, 0, 0, 0, 7, 0, 0, 0, 1, 1, 0, 0];
        shard.extend(std::iter::repeat(99u8).take(134 - 24));
        let data = vec![shard];
        let rs = Rs::new(1, 2);
        let parity = rs.encode(&data);
        // 丢唯一数据 shard，用 parity0 恢复
        let shards = vec![None, Some(parity[0].clone()), Some(parity[1].clone())];
        let rec = rs.recover(&shards).expect("recover");
        assert_eq!(rec[0], data[0], "k=1 recovery mismatch");
    }
}
