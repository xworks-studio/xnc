//! 性能计数与周期性日志（可观测性核心）：每秒一行结构化摘要。

use std::sync::atomic::{AtomicU64, Ordering};

#[derive(Default)]
pub struct Stats {
    // 媒体面
    pub frames_sent: AtomicU64,
    pub media_pkts: AtomicU64,
    pub media_bytes: AtomicU64,
    pub bytes_keyframes: AtomicU64,
    // 采集面
    pub frames_captured: AtomicU64,
    pub frames_skipped_equal: AtomicU64,
    pub cursor_only_frames: AtomicU64,
    pub frames_dropped_disconnected: AtomicU64,
    // 编码面（毫秒累计，用于均值）
    pub cap_ms_total: AtomicU64,
    pub enc_ms_total: AtomicU64,
    pub enc_ms_max: AtomicU64,
    // 控制面
    pub ctrl_msgs_rx: AtomicU64,
    pub ctrl_bytes_rx: AtomicU64,
    pub loss_reports_rx: AtomicU64,
    pub idr_forced: AtomicU64,
    // 会话
    pub reconnects: AtomicU64,
    session_started: AtomicU64, // Instant 不适用原子，记 epoch ms
}

impl Stats {
    pub fn new() -> Self {
        let s = Self::default();
        s.session_started.store(now_ms(), Ordering::Relaxed);
        s
    }

    pub fn media_pkt_sent(&self, len: usize) {
        self.media_pkts.fetch_add(1, Ordering::Relaxed);
        self.media_bytes.fetch_add(len as u64, Ordering::Relaxed);
    }
    pub fn frame_sent(&self, keyframe: bool, bytes: u64) {
        self.frames_sent.fetch_add(1, Ordering::Relaxed);
        if keyframe {
            self.bytes_keyframes.fetch_add(bytes, Ordering::Relaxed);
        }
    }
    pub fn capture_time(&self, ms: f64) {
        self.cap_ms_total.fetch_add((ms * 1000.0) as u64, Ordering::Relaxed);
    }
    pub fn encode_time(&self, ms: f64) {
        self.enc_ms_total.fetch_add((ms * 1000.0) as u64, Ordering::Relaxed);
        let us = (ms * 1000.0) as u64;
        self.enc_ms_max.fetch_max(us, Ordering::Relaxed);
    }
    pub fn ctrl_msg_rx(&self, bytes: u64) {
        self.ctrl_msgs_rx.fetch_add(1, Ordering::Relaxed);
        self.ctrl_bytes_rx.fetch_add(bytes, Ordering::Relaxed);
    }
    pub fn loss_report_rx(&self) {
        self.loss_reports_rx.fetch_add(1, Ordering::Relaxed);
    }
    pub fn idr_forced(&self) {
        self.idr_forced.fetch_add(1, Ordering::Relaxed);
    }
    pub fn frames_dropped_disconnected(&self) {
        self.frames_dropped_disconnected.fetch_add(1, Ordering::Relaxed);
    }
    pub fn session_reconnected(&self) {
        self.reconnects.fetch_add(1, Ordering::Relaxed);
        self.session_started.store(now_ms(), Ordering::Relaxed);
    }
    pub fn session_disconnected(&self) {}

    pub fn reset_session(&self) {
        self.session_started.store(now_ms(), Ordering::Relaxed);
    }

    /// 生成并 log 一条秒级摘要。返回 JSON 值便于测试断言。
    pub fn log_periodic(&self, enc_name: &str, connected: bool, viewers: usize,
        fps_setting: i32, bitrate_kbs: i32, fec_pct: u8) -> serde_json::Value {
        let f = |a: &AtomicU64| a.swap(0, Ordering::Relaxed);
        let frames = f(&self.frames_sent);
        let pkts = f(&self.media_pkts);
        let bytes = f(&self.media_bytes);
        let kf_bytes = f(&self.bytes_keyframes);
        let cap_us = f(&self.cap_ms_total);
        let enc_us = f(&self.enc_ms_total);
        let enc_max_us = self.enc_ms_max.swap(0, Ordering::Relaxed);
        let skipped = f(&self.frames_skipped_equal);
        let cursor_only = f(&self.cursor_only_frames);
        let dropped = f(&self.frames_dropped_disconnected);
        let loss_rx = f(&self.loss_reports_rx);
        let idr = f(&self.idr_forced);
        let mbps = bytes as f64 * 8.0 / 1_000_000.0;
        let v = serde_json::json!({
            "enc": enc_name,
            "conn": connected,
            "viewers": viewers,
            "fps": fps_setting,
            "kbps": bitrate_kbs,
            "fec": fec_pct,
            "sent": frames,
            "pkt": pkts,
            "mbps": (mbps * 100.0).round() / 100.0,
            "kfMB": (kf_bytes as f64 / 1e6 * 100.0).round() / 100.0,
            "capMs": if frames > 0 { (cap_us as f64 / 1000.0 / frames as f64 * 100.0).round() / 100.0 } else { 0.0 },
            "encMs": if frames > 0 { (enc_us as f64 / 1000.0 / frames as f64 * 100.0).round() / 100.0 } else { 0.0 },
            "encMaxMs": enc_max_us as f64 / 1000.0,
            "skipEq": skipped,
            "cursorOnly": cursor_only,
            "dropDisc": dropped,
            "lossRx": loss_rx,
            "idr": idr,
        });
        tracing::info!(target: "mvp::stats", "{v}");
        v
    }
}

fn now_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_millis() as u64
}
