//! 三线程共享状态：采集线程 / 传输任务 / QoS 与统计任务之间的低锁耦合点。

use std::sync::atomic::{AtomicBool, AtomicI32, AtomicU8, AtomicU64, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use tokio::sync::mpsc::UnboundedSender;

use crate::stats::Stats;

pub struct ControlMsg(pub serde_json::Value);

#[derive(Clone)]
pub struct EncoderMeta {
    pub name: String,
    pub hw: bool,
    pub width: usize,
    pub height: usize,
}

/// 最近一次 viewer feedback 汇总（QoS 输入）
#[derive(Clone, Copy, Debug, Default)]
pub struct FeedbackSummary {
    pub rtt_ms: Option<f64>,
    pub decode_queue: Option<f64>,
    pub decoded_fps: Option<f64>,
    pub arrival_gap_p95_ms: Option<f64>,
    pub at: Option<Instant>,
}

pub struct Shared {
    pub session: String,
    pub connected: AtomicBool,
    pub idr_requested: AtomicBool,
    /// QoS 调整后的采集帧率（初始为 CLI 值）
    pub fps: AtomicI32,
    pub fps_max: AtomicI32,
    pub bitrate_kbs: AtomicI32,
    pub bitrate_max_kbs: AtomicI32,
    pub fec_percentage: AtomicU8,
    pub viewers: AtomicUsize,
    pub encoder_meta: Mutex<Option<EncoderMeta>>,
    pub latest_feedback: Mutex<FeedbackSummary>,
    pub stats: Stats,
    media_tx: Mutex<Option<UnboundedSender<Vec<Vec<u8>>>>>,
    ctrl_tx: Mutex<Option<UnboundedSender<ControlMsg>>>,
}

impl Shared {
    pub fn new(session: String, fps: i32, bitrate_kbs: i32, fec_percentage: u8) -> Self {
        Self {
            session,
            connected: AtomicBool::new(false),
            idr_requested: AtomicBool::new(false),
            fps: AtomicI32::new(fps),
            fps_max: AtomicI32::new(fps),
            bitrate_kbs: AtomicI32::new(bitrate_kbs),
            bitrate_max_kbs: AtomicI32::new(bitrate_kbs),
            fec_percentage: AtomicU8::new(fec_percentage),
            viewers: AtomicUsize::new(0),
            encoder_meta: Mutex::new(None),
            latest_feedback: Mutex::new(FeedbackSummary::default()),
            stats: Stats::new(),
            media_tx: Mutex::new(None),
            ctrl_tx: Mutex::new(None),
        }
    }

    pub fn publish_senders(
        &self,
        media: UnboundedSender<Vec<Vec<u8>>>,
        ctrl: UnboundedSender<ControlMsg>,
    ) {
        *self.media_tx.lock().unwrap() = Some(media);
        *self.ctrl_tx.lock().unwrap() = Some(ctrl);
    }

    pub fn disconnect(&self) {
        self.connected.store(false, Ordering::SeqCst);
        self.viewers.store(0, Ordering::SeqCst);
        *self.media_tx.lock().unwrap() = None;
        *self.ctrl_tx.lock().unwrap() = None;
        self.stats.session_disconnected();
    }

    pub fn is_connected(&self) -> bool {
        self.connected.load(Ordering::SeqCst)
    }

    /// 媒体包投递（未连接时丢弃并计数）。
    pub fn media_send(&self, packets: Vec<Vec<u8>>) {
        let bytes: u64 = packets.iter().map(|p| p.len() as u64).sum();
        match &*self.media_tx.lock().unwrap() {
            Some(tx) => {
                let _ = tx.send(packets);
            }
            None => self.stats.frames_dropped_disconnected(),
        }
        let _ = bytes;
    }

    /// 控制消息投递。
    pub fn ctrl_send(&self, msg: ControlMsg) {
        if let Some(tx) = &*self.ctrl_tx.lock().unwrap() {
            let _ = tx.send(msg);
        }
    }

    pub fn encoder_meta(&self) -> Option<EncoderMeta> {
        self.encoder_meta.lock().unwrap().clone()
    }

    pub fn set_encoder_meta(&self, meta: EncoderMeta) {
        *self.encoder_meta.lock().unwrap() = Some(meta);
    }

    pub fn record_feedback(&self, rtt_ms: Option<f64>, decode_queue: Option<f64>,
        decoded_fps: Option<f64>, arrival_gap_p95_ms: Option<f64>) {
        *self.latest_feedback.lock().unwrap() = FeedbackSummary {
            rtt_ms,
            decode_queue: decode_queue.map(|v| v.clamp(0.0, 10_000.0)),
            decoded_fps,
            arrival_gap_p95_ms,
            at: Some(Instant::now()),
        };
    }

    pub fn unix_us() -> u64 {
        SystemTime::now().duration_since(UNIX_EPOCH).unwrap_or_default().as_micros() as u64
    }
}

/// 帧统计帧（frameStats 消息内容）
pub struct FrameReport {
    pub frame_index: u32,
    pub capture_ms: f64,
    pub encode_ms: f64,
    pub bytes: u64,
    pub keyframe: bool,
    pub shards: usize,
}

pub fn fmt_duration(d: Duration) -> u64 {
    d.as_millis() as u64
}
