//! 视频编码：hwcodec（FFmpeg 硬/软编）QSV→NVENC→AMF→libx264 探测回退。
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

pub const ENCODER_PREFERENCE: &[&str] = &["h264_qsv", "h264_nvenc", "h264_amf", "libx264"];

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
    /// 枚举可用编码器（可观测性：启动时打印完整列表）
    pub fn available_names(width: usize, height: usize, fps: i32, bitrate_kbs: i32) -> Vec<String> {
        let ctx = probe_ctx(width, height, fps, bitrate_kbs);
        Encoder::available_encoders(ctx, None)
            .into_iter()
            .filter(|c| c.name.contains("h264"))
            .map(|c| c.name)
            .collect()
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
        let ctx = EncodeContext {
            name: name.to_string(),
            mc_name: None,
            width: width as i32,
            height: height as i32,
            pixfmt: AVPixelFormat::AV_PIX_FMT_NV12,
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

    /// BGRA → NV12（libyuv, scrap 内置）→ 编码。
    pub fn encode_bgra(&mut self, bgra: &[u8], ms: i64) -> Result<Vec<EncodedPacket>> {
        let yuvfmt = self.yuvfmt();
        let w = self.width;
        let h = self.height;
        let pb = scrap::PixelBuffer::new(bgra, Pixfmt::BGRA, w, h);
        let frame = Frame::PixelBuffer(pb);
        let input = frame
            .to(yuvfmt, &mut self.yuv, &mut self.mid)
            .map_err(|e| anyhow!("bgra->nv12 convert: {e}"))?;
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

    /// 与 scrap HwRamEncoder::yuvfmt() 相同的构造（NV12 布局跟随编码器 linesize/offset）。
    pub fn yuvfmt(&self) -> EncodeYuvFormat {
        let stride = self.enc.linesize.clone().drain(..).map(|i| i as usize).collect();
        EncodeYuvFormat {
            pixfmt: Pixfmt::NV12,
            w: self.width,
            h: self.height,
            stride,
            u: self.enc.offset[0] as usize,
            v: 0,
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
            pixfmt: AVPixelFormat::AV_PIX_FMT_NV12,
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
