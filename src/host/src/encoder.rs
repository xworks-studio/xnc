//! 视频编码：hwcodec（FFmpeg）QSV→NVENC→AMF 硬编探测，libopenh264/libx264
//! 软编显式试开回退（无 GPU 机器——如无厂商驱动的 VM——硬编恒空，2026-09-17
//! LABS-RUNNER-26 事故：available_encoders 只枚举硬编，软编不在其中）。
//!
//! 用法镜像 rustdesk `libs/scrap/src/common/hwcodec.rs`（HwRamEncoder），但去掉了
//! rustdesk 的协商/protobuf 耦合，直接面向本协议。
//! 限制：hwcodec 未暴露强制关键帧 API（仅 gop_size），按需 IDR 通过**同参数重建
//! 编码器**实现（新编码器首帧必为 IDR），并做 100ms 限频（IDR 防风暴的 host 侧
//! 对应物）。码率可通过 set_bitrate 热调（QoS 用），无需重建。

use anyhow::{anyhow, Context, Result};
use hwcodec::{
    common::{Quality, RateControl},
    ffmpeg::AVPixelFormat,
    ffmpeg_ram::encode::{EncodeContext, EncodeFrame, Encoder},
};
use scrap::{EncodeYuvFormat, Frame, HW_STRIDE_ALIGN, Pixfmt};

/// libopenh264 = BSD（仓库许可白名单内），默认软编回退；libx264 = GPL，
/// 仅当构建显式带上时可用（当前 build.ps1 的 vcpkg 特性集不含）。
pub const ENCODER_PREFERENCE: &[&str] = &[
    "h264_qsv",
    "h264_nvenc",
    "h264_amf",
    "libopenh264",
    "libx264",
];

/// 软编候选：hwcodec 的 available_encoders 只枚举硬编（qsv/nvenc/amf/vaapi），
/// 软编必须显式 Encoder::new + 试编一帧验证（镜像其内部对硬编的探测语义）。
const SOFT_ENCODER_CANDIDATES: &[&str] = &["libopenh264", "libx264"];

/// 软编像素格式：ffmpeg 的 libopenh264 封装只接受 yuv420p/yuvj420p（硬编走
/// NV12）。libx264 同样按 I420 供给（普遍支持），软编统一 I420。
fn encoder_pixfmt(name: &str) -> AVPixelFormat {
    if SOFT_ENCODER_CANDIDATES.contains(&name) {
        AVPixelFormat::AV_PIX_FMT_YUV420P
    } else {
        AVPixelFormat::AV_PIX_FMT_NV12
    }
}

pub struct EncodedPacket {
    pub data: Vec<u8>,
    pub key: bool,
}

pub struct VideoEncoder {
    enc: Encoder,
    pub name: String,
    pub hw: bool,
    width: usize,
    height: usize,
    fps: i32,
    bitrate_kbs: i32,
    gop: i32,
    yuv: Vec<u8>,
    mid: Vec<u8>,
    last_idr_recreate: std::time::Instant,
}

impl VideoEncoder {
    /// 枚举可用编码器（可观测性：启动时打印完整列表）。硬编走 hwcodec 的
    /// available_encoders（open+试编探测）；软编不在其枚举范围，逐个显式试开。
    pub fn available_names(width: usize, height: usize, fps: i32, bitrate_kbs: i32) -> Vec<String> {
        let ctx = probe_ctx(width, height, fps, bitrate_kbs);
        let mut names: Vec<String> = Encoder::available_encoders(ctx.clone(), None)
            .into_iter()
            .filter(|c| c.name.contains("h264"))
            .map(|c| c.name)
            .collect();
        for soft in SOFT_ENCODER_CANDIDATES {
            if names.iter().any(|n| n == soft) {
                continue;
            }
            if soft_encoder_usable(soft, ctx.clone()) {
                names.push(soft.to_string());
            }
        }
        names
    }

    pub fn new(width: usize, height: usize, fps: i32, bitrate_kbs: i32) -> Result<Self> {
        let names = Self::available_names(width, height, fps, bitrate_kbs);
        tracing::info!(?names, "available h264 encoders");
        let name = ENCODER_PREFERENCE
            .iter()
            .find(|p| names.iter().any(|n| n == *p))
            .map(|p| p.to_string())
            .ok_or_else(|| anyhow!("no usable h264 encoder among {names:?}"))?;
        Self::with_name(&name, width, height, fps, bitrate_kbs)
    }

    pub fn with_name(name: &str, width: usize, height: usize, fps: i32, bitrate_kbs: i32) -> Result<Self> {
        if width % 2 != 0 || height % 2 != 0 {
            return Err(anyhow!("encoder requires even dimensions, got {width}x{height}"));
        }
        let gop = (fps * 2).max(1); // 基线 GOP 2s；按需 IDR 走重建
        let pixfmt = encoder_pixfmt(name);
        let ctx = EncodeContext {
            name: name.to_string(),
            mc_name: None,
            width: width as i32,
            height: height as i32,
            pixfmt,
            align: HW_STRIDE_ALIGN as i32,
            fps,
            gop,
            rc: RateControl::RC_CBR,
            quality: Quality::Quality_Default,
            kbs: bitrate_kbs,
            q: -1,
            thread_count: 2,
        };
        let enc = Encoder::new(ctx.clone()).map_err(|_| anyhow!("Encoder::new failed for {name}"))?;
        let is_hw = name.contains("qsv") || name.contains("nvenc") || name.contains("amf");
        tracing::info!(
            encoder = name, width, height, fps, bitrate_kbs, gop, hw = is_hw,
            pixfmt = ?pixfmt,
            "video encoder created"
        );
        Ok(Self {
            enc,
            name: name.to_string(),
            hw: name.contains("qsv") || name.contains("nvenc") || name.contains("amf"),
            width,
            height,
            fps,
            bitrate_kbs,
            gop,
            yuv: Vec::new(),
            mid: Vec::new(),
            last_idr_recreate: std::time::Instant::now(),
        })
    }

    /// BGRA → YUV（libyuv, scrap 内置）→ 编码。软编 I420，硬编 NV12。
    pub fn encode_bgra(&mut self, bgra: &[u8], ms: i64) -> Result<Vec<EncodedPacket>> {
        let yuvfmt = self.yuvfmt();
        let w = self.width;
        let h = self.height;
        let pb = scrap::PixelBuffer::new(bgra, Pixfmt::BGRA, w, h);
        let frame = Frame::PixelBuffer(pb);
        let input = frame
            .to(yuvfmt, &mut self.yuv, &mut self.mid)
            .map_err(|e| anyhow!("bgra->yuv convert: {e}"))?;
        let yuv = input.yuv().map_err(|e| anyhow!("yuv input: {e}"))?;
        let frames = self
            .enc
            .encode(yuv, ms)
            .map_err(|errno| anyhow!("encode errno={errno}"))?;
        Ok(frames
            .iter()
            .map(|f| EncodedPacket { data: f.data.clone(), key: f.key == 1 })
            .collect())
    }

    /// 与 scrap HwRamEncoder::yuvfmt() 相同的构造（布局跟随编码器
    /// linesize/offset）：NV12 单 UV 平面（v=0），I420 双平面（u/v 各自 offset）。
    pub fn yuvfmt(&self) -> EncodeYuvFormat {
        let stride = self.enc.linesize.clone().drain(..).map(|i| i as usize).collect();
        let pixfmt = if SOFT_ENCODER_CANDIDATES.contains(&self.name.as_str()) {
            Pixfmt::I420
        } else {
            Pixfmt::NV12
        };
        let v = if pixfmt == Pixfmt::I420 {
            self.enc.offset.get(1).copied().unwrap_or(0) as usize
        } else {
            0
        };
        EncodeYuvFormat {
            pixfmt,
            w: self.width,
            h: self.height,
            stride,
            u: self.enc.offset[0] as usize,
            v,
        }
    }

    /// 热调码率（QoS），失败仅告警。
    pub fn set_bitrate(&mut self, kbs: i32) {
        if kbs == self.bitrate_kbs {
            return;
        }
        match self.enc.set_bitrate(kbs) {
            Ok(()) => {
                tracing::info!(from = self.bitrate_kbs, to = kbs, "bitrate adjusted");
                self.bitrate_kbs = kbs;
            }
            Err(()) => tracing::warn!(kbs, "set_bitrate failed"),
        }
    }

    pub fn bitrate_kbs(&self) -> i32 {
        self.bitrate_kbs
    }

    /// 按需 IDR：重建同参数编码器（新编码器首帧为 IDR）。100ms 限频。
    pub fn force_idr(&mut self) -> Result<()> {
        if self.last_idr_recreate.elapsed() < std::time::Duration::from_millis(100) {
            return Ok(());
        }
        self.last_idr_recreate = std::time::Instant::now();
        let enc = Encoder::new(EncodeContext {
            name: self.name.clone(),
            mc_name: None,
            width: self.width as i32,
            height: self.height as i32,
            pixfmt: encoder_pixfmt(&self.name),
            align: HW_STRIDE_ALIGN as i32,
            fps: self.fps,
            gop: self.gop,
            rc: RateControl::RC_CBR,
            quality: Quality::Quality_Default,
            kbs: self.bitrate_kbs,
            q: -1,
            thread_count: 2,
        })
        .map_err(|_| anyhow!("idr recreate failed"))
        .context("recreate encoder for IDR")?;
        self.enc = enc;
        tracing::info!(frame_rate_hint = self.fps, "encoder recreated for IDR");
        Ok(())
    }
}

fn probe_ctx(width: usize, height: usize, fps: i32, bitrate_kbs: i32) -> EncodeContext {
    EncodeContext {
        name: String::new(),
        mc_name: None,
        width: width as i32,
        height: height as i32,
        pixfmt: AVPixelFormat::AV_PIX_FMT_NV12,
        align: HW_STRIDE_ALIGN as i32,
        fps,
        gop: fps * 2,
        rc: RateControl::RC_CBR,
        quality: Quality::Quality_Default,
        kbs: bitrate_kbs,
        q: -1,
        thread_count: 2,
    }
}

/// 软编可用性探测：open + 试编一帧全灰。缓冲按 linesize/offset 布局
/// 足量分配（多余尾部无害——C 侧按自身布局读取，不校验总长）。
/// 像素格式随编码器（软编 I420，见 encoder_pixfmt）。
fn soft_encoder_usable(name: &str, ctx: EncodeContext) -> bool {
    let probe = EncodeContext {
        name: name.to_string(),
        pixfmt: encoder_pixfmt(name),
        ..ctx
    };
    let Ok(mut enc) = Encoder::new(probe) else {
        return false;
    };
    // NV12：Y 平面 linesize[0]*h + UV 平面 linesize[1]*h/2，外加 offset 余量
    let h = ctx.height as usize;
    let mut len = 0usize;
    for (i, ls) in enc.linesize.iter().enumerate() {
        let plane_h = if i == 0 { h } else { h / 2 };
        len += (*ls as usize) * plane_h;
    }
    len += enc.offset.iter().map(|o| *o as usize).max().unwrap_or(0);
    let dummy = vec![0x80u8; len];
    match enc.encode(&dummy, 0) {
        Ok(frames) => !frames.is_empty(),
        Err(_) => false,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// 软编回退回归（2026-09-17 LABS-RUNNER-26 事故）：无 GPU 机器上硬编枚举
    /// 恒空，available_names 必须仍能报出软编（libopenh264/libx264 至少其一），
    /// VideoEncoder::new 不得再落入 "no usable h264 encoder" 重启死循环。
    #[test]
    fn available_names_never_empty_via_soft_fallback() {
        let names = VideoEncoder::available_names(640, 480, 30, 4000);
        assert!(
            names.iter().any(|n| SOFT_ENCODER_CANDIDATES.contains(&n.as_str())),
            "no soft encoder available (ffmpeg built without openh264/x264?): {names:?}"
        );
        // 生产构造路径必须成功（选型含软编）
        let enc = VideoEncoder::new(640, 480, 30, 4000)
            .expect("VideoEncoder::new must succeed with soft fallback");
        assert!(ENCODER_PREFERENCE.contains(&enc.name.as_str()));
    }

    /// 软编端到端：BGRA→I420→libopenh264 编码出首帧（关键帧）。强制走
    /// libopenh264（不与硬编选型耦合），软编不可用（ffmpeg 未带）时跳过。
    #[test]
    fn soft_encoder_end_to_end_bgra() {
        let names = VideoEncoder::available_names(640, 480, 30, 4000);
        if !names.iter().any(|n| n == "libopenh264") {
            // 硬编机器 + 未带 openh264 的构建：软编路径由构建矩阵另一端覆盖
            return;
        }
        let mut enc = VideoEncoder::with_name("libopenh264", 640, 480, 30, 4000)
            .expect("libopenh264 open (probed available)");
        assert!(!enc.hw);
        let bgra = vec![0x80u8; 640 * 480 * 4];
        let pkts = enc.encode_bgra(&bgra, 0).expect("encode first frame");
        assert!(!pkts.is_empty() && pkts[0].key, "first packet must be a keyframe");
        // 第二帧（非关键帧路径）：同编码器续编
        let pkts2 = enc.encode_bgra(&bgra, 33).expect("encode second frame");
        assert!(!pkts2.is_empty());
    }
}
