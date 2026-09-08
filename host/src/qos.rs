//! host 侧 QoS 控制器 v1（docs/proto.md §2.3）。
//!
//! 分档思想移植自 rustdesk `src/server/video_qos.rs`：按反馈 RTT 分档升降
//! fps/码率；FEC 按不可恢复丢帧上报调节。每次调整产出 qosAdjust 控制消息并
//! 打 log（决策可观测）。所有变更通过 Shared 原子量生效：
//! - fps → 采集线程下一帧起生效
//! - bitrate → VideoEncoder::set_bitrate（无需重建）
//! - fec → 下一帧 build_packets 生效

use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::{Duration, Instant};

use crate::shared::{ControlMsg, Shared};

const WINDOW: Duration = Duration::from_secs(3);

pub struct QosController {
    last_adjust: Instant,
    fec_bad_windows: u32,
    fec_good_windows: u32,
}

impl QosController {
    pub fn new() -> Self {
        Self {
            last_adjust: Instant::now(),
            fec_bad_windows: 0,
            fec_good_windows: 0,
        }
    }

    /// 每 3s 调用一次。返回是否发生了调整。
    pub fn evaluate(&mut self, shared: &Arc<Shared>) -> bool {
        if self.last_adjust.elapsed() < WINDOW {
            return false;
        }
        self.last_adjust = Instant::now();

        let connected = shared.is_connected();
        let viewers = shared.viewers.load(Ordering::SeqCst);
        let fb = shared.latest_feedback.lock().unwrap().clone();
        let fb_fresh = fb.at.map(|t| t.elapsed() < WINDOW * 2).unwrap_or(false);

        // 无 viewer / 未连接：恢复默认，静默
        if !connected || viewers == 0 || !fb_fresh {
            return self.maybe_restore(shared);
        }

        let rtt = fb.rtt_ms.unwrap_or(f64::MAX);
        let mut fps = shared.fps.load(Ordering::SeqCst);
        let mut kbps = shared.bitrate_kbs.load(Ordering::SeqCst);
        let fps_max = shared.fps_max.load(Ordering::SeqCst);
        let kbps_max = shared.bitrate_max_kbs.load(Ordering::SeqCst);
        let mut fec = shared.fec_percentage.load(Ordering::SeqCst);
        let (mut nfps, mut nkbps, mut nfec) = (fps, kbps, fec);
        let mut reason = String::new();

        if rtt > 500.0 || fb.decode_queue.unwrap_or(0.0) > 5.0 {
            nfps = ((fps as f64 * 0.8) as i32).max(5);
            nkbps = ((kbps as f64 * 0.9) as i32).max(2000);
            reason = format!("rtt={rtt:.0}ms/queue={:.1}", fb.decode_queue.unwrap_or(0.0));
        } else if rtt > 150.0 {
            nkbps = ((kbps as f64 * 0.95) as i32).max(2000);
            reason = format!("rtt={rtt:.0}ms");
        } else {
            // 健康：缓慢回升
            if fps < fps_max {
                nfps = (fps + 5).min(fps_max);
                reason.push_str("fps+");
            }
            if kbps < kbps_max {
                nkbps = ((kbps as f64 * 1.1) as i32).min(kbps_max);
                reason.push_str(" kbps+");
            }
            if reason.is_empty() {
                reason = "healthy".into();
            }
        }

        // FEC：按窗口内 viewer 的不可恢复丢帧上报调节（读取后清零计数）
        let loss_reports = shared.stats.loss_reports_rx.swap(0, Ordering::SeqCst);
        if loss_reports > 0 {
            self.fec_bad_windows += 1;
            self.fec_good_windows = 0;
            if self.fec_bad_windows >= 3 {
                nfec = (fec + 5).min(60);
                reason = format!("{reason} fecUp(lossReports={loss_reports})");
                self.fec_bad_windows = 0;
            }
        } else {
            self.fec_bad_windows = 0;
            self.fec_good_windows += 1;
            if self.fec_good_windows >= 6 && fec > 10 {
                nfec = fec - 5;
                reason = format!("{reason} fecDown");
                self.fec_good_windows = 0;
            }
        }

        if nfps == fps && nkbps == kbps && nfec == fec {
            return false;
        }
        shared.fps.store(nfps, Ordering::SeqCst);
        shared.bitrate_kbs.store(nkbps, Ordering::SeqCst);
        shared.fec_percentage.store(nfec, Ordering::SeqCst);
        tracing::info!(
            from = format!("fps={fps},kbps={kbps},fec={fec}"),
            to = format!("fps={nfps},kbps={nkbps},fec={nfec}"),
            reason = %reason,
            "qos adjust"
        );
        shared.ctrl_send(ControlMsg(serde_json::json!({
            "type": "qosAdjust",
            "fps": nfps,
            "bitrateKbps": nkbps,
            "fecPercentage": nfec,
            "reason": reason,
        })));
        true
    }

    /// 无压力时逐步回到初始配置。
    fn maybe_restore(&mut self, shared: &Arc<Shared>) -> bool {
        let fps_max = shared.fps_max.load(Ordering::SeqCst);
        let kbps_max = shared.bitrate_max_kbs.load(Ordering::SeqCst);
        let fps = shared.fps.load(Ordering::SeqCst);
        let kbps = shared.bitrate_kbs.load(Ordering::SeqCst);
        if fps == fps_max && kbps == kbps_max {
            return false;
        }
        shared.fps.store(fps_max, Ordering::SeqCst);
        shared.bitrate_kbs.store(kbps_max, Ordering::SeqCst);
        tracing::info!("qos restore defaults (no viewers/no feedback)");
        shared.ctrl_send(ControlMsg(serde_json::json!({
            "type": "qosAdjust", "fps": fps_max, "bitrateKbps": kbps_max,
            "fecPercentage": shared.fec_percentage.load(Ordering::SeqCst),
            "reason": "restore",
        })));
        true
    }
}
