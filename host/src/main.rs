//! XNC 桌面采集端入口（xnc-host）。
//!
//! 线程模型（对应研究报告中的 Sunshine video.cpp 结构）：
//! - 主线程 = 采集+编码循环（frame pacing，`critical` 越权不设，保持系统友好）
//! - tokio 运行时：QUIC 传输 / 控制分发 / QoS(3s) / 统计日志(1s)
//!
//! 采集→编码→组帧→FEC→pacing 发送的数据流：
//!   ScreenCapturer(BGRA+光标) → VideoEncoder(NV12→H.264) → framing::build_packets
//!   → Shared::media_send → 令牌桶 → QUIC datagram → server 扇出
//!
//! 配置源（XNC 集成缝）：`--stdin-config` 时从 stdin 读 JSON（xnc-core spawn 时
//! 写入，endpoint/token 经管道传递、绝不进 argv）；CLI 参数仅作本地调试与
//! 未提供字段的回退。密钥纪律：token 只出现在 stdin 配置与 TLS/hello 载荷，
//! 不入任何日志与 tracing 字段。

mod capture;
mod encoder;
mod framing;
mod input;
mod qos;
mod rs;
mod shared;
mod stats;
mod transport;

use std::net::{SocketAddr, ToSocketAddrs};
use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::{Duration, Instant};

use anyhow::{Context, Result};
use clap::Parser;

use capture::{CaptureOutcome, ScreenCapturer};
use encoder::VideoEncoder;
use shared::{ControlMsg, EncoderMeta, Shared};

#[derive(Parser, Debug)]
#[command(about = "XNC desktop host: capture + encode + FEC + QUIC")]
struct Args {
    /// 中转服务器 QUIC 地址（host leg，UDP）。本地调试用；生产经 stdin 配置下发
    #[arg(long)]
    server: Option<String>,
    /// TLS SNI/证书校验名；缺省取 endpoint 的 host 部分
    #[arg(long)]
    server_name: Option<String>,
    /// 节点标识（本地调试缺省 debug-node；生产经 stdin 配置下发）
    #[arg(long)]
    node_id: Option<String>,
    /// HostToken（本地调试可省；生产经 stdin 配置下发，绝不入 argv）
    #[arg(long)]
    token: Option<String>,
    #[arg(long, default_value_t = 30)]
    fps: i32,
    #[arg(long, default_value_t = 15000)]
    bitrate_kbps: i32,
    /// FEC 百分比（初始值，QoS 可动态调节）
    #[arg(long, default_value_t = 20)]
    fec: u8,
    /// 显示器序号（0 = 主屏）
    #[arg(long, default_value_t = 0)]
    display: usize,
    /// 发送端整流速率上限（kbps）
    #[arg(long, default_value_t = 50000)]
    send_kbps: u32,
    /// 不叠加光标（调试用）
    #[arg(long, default_value_t = false)]
    no_cursor: bool,
    /// 强制指定编码器名（如 h264_qsv / libx264），跳过自动探测
    #[arg(long)]
    encoder: Option<String>,
    /// 日志文件（追加写；xnc-core 传 ProgramData\XNC\logs\xnc-host.log）
    #[arg(long)]
    log_file: Option<String>,
    /// 从 stdin 读 JSON 配置（xnc-core spawn 契约：JSON + EOF）
    #[arg(long, default_value_t = false)]
    stdin_config: bool,
    /// 跳过服务端证书校验（仅本地调试自签 dev server；生产绝不使用）
    #[arg(long, default_value_t = false)]
    tls_insecure: bool,
}

/// stdin 配置（xnc-core → xnc-host 的 spawn 契约载荷）。
/// 字段与 agent/desktop 会话参数对齐（见 server 侧 DesktopParams）。
#[derive(serde::Deserialize, Default, Debug)]
struct StdinConfig {
    endpoint: Option<String>,
    #[serde(rename = "serverName")]
    server_name: Option<String>,
    #[serde(rename = "nodeId")]
    node_id: Option<String>,
    token: Option<String>,
    fps: Option<i32>,
    #[serde(rename = "bitrateKbps")]
    bitrate_kbps: Option<i32>,
    #[serde(default)]
    fec: Option<u8>,
    display: Option<usize>,
    #[serde(rename = "sendKbps")]
    send_kbps: Option<u32>,
    #[serde(rename = "logFile")]
    log_file: Option<String>,
    #[serde(default, rename = "tlsInsecure")]
    tls_insecure: Option<bool>,
}

/// 解析后的有效配置（stdin 优先，CLI 回退）。
struct RunConfig {
    endpoint: SocketAddr,
    server_name: String,
    node_id: String,
    token: String,
    fps: i32,
    bitrate_kbps: i32,
    fec: u8,
    send_kbps: u32,
    log_file: Option<String>,
    tls_insecure: bool,
    // 仅 CLI 的调试旋钮
    display: usize,
    no_cursor: bool,
    encoder: Option<String>,
}

fn resolve_config(args: &Args) -> Result<RunConfig> {
    let stdin_cfg = if args.stdin_config {
        let mut buf = String::new();
        std::io::Read::read_to_string(&mut std::io::stdin(), &mut buf)
            .context("read stdin config")?;
        serde_json::from_str(&buf).context("parse stdin config json")?
    } else {
        StdinConfig::default()
    };

    let endpoint_str = stdin_cfg
        .endpoint
        .clone()
        .or_else(|| args.server.clone())
        .context("server endpoint required (--server or stdin config)")?;
    // endpoint 允许 host:port 域名形态（生产 = xnc.app:4433）——SocketAddr::
    // parse 只认 IP 字面量，先经 getaddrinfo 解析（IP 字面量同样命中）。
    let endpoint: SocketAddr = endpoint_str
        .to_socket_addrs()
        .context("server addr resolve")?
        .next()
        .context("server addr resolved empty")?;
    let server_name = stdin_cfg
        .server_name
        .clone()
        .or_else(|| args.server_name.clone())
        .unwrap_or_else(|| {
            endpoint_str
                .rsplit_once(':')
                .map(|(h, _)| h.to_string())
                .unwrap_or_else(|| endpoint_str.clone())
        });
    Ok(RunConfig {
        endpoint,
        server_name,
        node_id: stdin_cfg
            .node_id
            .or_else(|| args.node_id.clone())
            .unwrap_or_else(|| "debug-node".into()),
        token: stdin_cfg.token.or_else(|| args.token.clone()).unwrap_or_default(),
        fps: stdin_cfg.fps.unwrap_or(args.fps),
        bitrate_kbps: stdin_cfg.bitrate_kbps.unwrap_or(args.bitrate_kbps),
        fec: stdin_cfg.fec.unwrap_or(args.fec),
        send_kbps: stdin_cfg.send_kbps.unwrap_or(args.send_kbps),
        log_file: stdin_cfg.log_file.or_else(|| args.log_file.clone()),
        tls_insecure: stdin_cfg.tls_insecure.unwrap_or(args.tls_insecure),
        display: args.display,
        no_cursor: args.no_cursor,
        encoder: args.encoder.clone(),
    })
}

fn main() -> Result<()> {
    let args = Args::parse();
    let cfg = resolve_config(&args)?;
    init_tracing(cfg.log_file.as_deref())?;

    tracing::info!(
        version = env!("CARGO_PKG_VERSION"),
        endpoint = %cfg.endpoint,
        server_name = %cfg.server_name,
        node_id = %cfg.node_id,
        has_token = !cfg.token.is_empty(),
        fps = cfg.fps,
        bitrate_kbps = cfg.bitrate_kbps,
        fec = cfg.fec,
        "xnc-host starting"
    );
    if cfg.tls_insecure {
        tracing::warn!("TLS certificate verification DISABLED (debug only)");
    }

    let shared = Arc::new(Shared::new(
        cfg.node_id.clone(),
        cfg.token.clone(),
        cfg.fps,
        cfg.bitrate_kbps,
        cfg.fec,
    ));

    let rt = tokio::runtime::Builder::new_multi_thread()
        .enable_all()
        .build()
        .context("tokio runtime")?;

    // 传输任务
    {
        let shared = shared.clone();
        let endpoint = cfg.endpoint;
        let server_name = cfg.server_name.clone();
        let send_kbps = cfg.send_kbps;
        let tls_insecure = cfg.tls_insecure;
        rt.spawn(async move {
            if let Err(e) =
                transport::run(shared, endpoint, &server_name, send_kbps, tls_insecure).await
            {
                tracing::error!(?e, "transport task exited");
                std::process::exit(2);
            }
        });
    }
    // QoS 任务（3s 窗口）
    {
        let shared = shared.clone();
        rt.spawn(async move {
            let mut qos = qos::QosController::new();
            loop {
                tokio::time::sleep(Duration::from_millis(500)).await;
                qos.evaluate(&shared);
            }
        });
    }
    // 统计任务（1s 日志）
    {
        let shared = shared.clone();
        rt.spawn(async move {
            loop {
                tokio::time::sleep(Duration::from_secs(1)).await;
                let enc = shared
                    .encoder_meta()
                    .map(|m| m.name)
                    .unwrap_or_else(|| "pending".into());
                shared.stats.log_periodic(
                    &enc,
                    shared.is_connected(),
                    shared.viewers.load(Ordering::SeqCst),
                    shared.fps.load(Ordering::SeqCst),
                    shared.bitrate_kbs.load(Ordering::SeqCst),
                    shared.fec_percentage.load(Ordering::SeqCst),
                );
            }
        });
    }

    // 采集 + 编码主循环（带重建：显示器/编码器异常恢复）
    let _rt = rt;
    let mut backoff = Duration::from_millis(200);
    loop {
        match run_pipeline(&shared, &cfg) {
            Ok(()) => unreachable!("pipeline only returns on error"),
            Err(e) => {
                tracing::warn!(?e, retry_in_ms = backoff.as_millis() as u64, "pipeline restart");
                std::thread::sleep(backoff);
                backoff = (backoff * 2).min(Duration::from_secs(3));
            }
        }
    }
}

/// 一轮完整采集→编码管线；返回 Err 时上层带退避重启（rustdesk SWITCH 模式）。
fn run_pipeline(shared: &Arc<Shared>, cfg: &RunConfig) -> Result<()> {
    let mut capturer =
        ScreenCapturer::new(cfg.display, !cfg.no_cursor).context("create capturer")?;
    let (w, h) = (capturer.width, capturer.height);
    if capturer.is_gdi() {
        tracing::warn!("running on GDI capture (DXGI unavailable)");
    }

    let fps0 = shared.fps.load(Ordering::SeqCst);
    let bitrate = shared.bitrate_kbs.load(Ordering::SeqCst);
    let mut enc = match &cfg.encoder {
        Some(name) => VideoEncoder::with_name(name, w, h, fps0, bitrate)?,
        None => VideoEncoder::new(w, h, fps0, bitrate)?,
    };
    shared.set_encoder_meta(EncoderMeta {
        name: enc.name.clone(),
        hw: enc.hw,
        width: w,
        height: h,
    });
    // config 在 transport 连接时/已连接时都会发送；已连接则补发一次
    shared.ctrl_send(ControlMsg(serde_json::json!({
        "type": "config", "codec": "h264", "width": w, "height": h,
        "fps": fps0, "fecPercentage": shared.fec_percentage.load(Ordering::SeqCst),
        "shardPayload": framing::SHARD_PAYLOAD_TARGET,
        "encoder": enc.name, "encoderHw": enc.hw,
        "startedUnixMs": now_ms(),
    })));

    let mut frame_index: u32 = 0; // 帧号从 0 单调递增（起始值取 MAX 会在 wrap 后令按序定稿的接收端永久失效）
    let mut next_tick = Instant::now();
    let mut prev_viewers: usize = 0;
    loop {
        // ---- frame pacing（fps 可被 QoS/setParams 动态调整）----
        let fps = shared.fps.load(Ordering::SeqCst).max(1);
        let interval = Duration::from_secs_f64(1.0 / fps as f64);
        let now = Instant::now();
        if next_tick > now {
            std::thread::sleep(next_tick - now);
        }
        next_tick += interval;
        if next_tick <= now {
            next_tick = now + interval; // 掉队重锚定（Sunshine/Linux handle_pacing 同款）
        }

        // ---- viewer 增加时强制产帧（静止桌面下新 viewer 否则等不到 IDR）----
        let viewers = shared.viewers.load(Ordering::SeqCst);
        if viewers > prev_viewers && viewers > 0 {
            tracing::info!(prev = prev_viewers, now = viewers, "viewer joined, force frame");
            capturer.force_frame();
        }
        prev_viewers = viewers;

        // ---- 采集 ----
        let t_cap = Instant::now();
        let outcome = capturer.next(Duration::from_millis(1))?;
        match outcome {
            CaptureOutcome::Reinit => {
                anyhow::bail!("display changed -> pipeline restart")
            }
            CaptureOutcome::NoChange => continue,
            CaptureOutcome::Frame(bgra) => {
                // 未连接不编码（省 CPU；rustdesk 无订阅者时不采）
                if !shared.is_connected() || shared.viewers.load(Ordering::SeqCst) == 0 {
                    continue;
                }
                let cap_ms = t_cap.elapsed().as_secs_f64() * 1000.0;
                shared.stats.capture_time(cap_ms);

                // ---- 按需 IDR（viewer frameLoss / 显式请求）----
                if shared.idr_requested.swap(false, Ordering::SeqCst) {
                    if let Err(e) = enc.force_idr() {
                        tracing::warn!(?e, "force_idr failed");
                    } else {
                        shared.stats.idr_forced();
                    }
                }
                // 码率热调
                enc.set_bitrate(shared.bitrate_kbs.load(Ordering::SeqCst));

                // ---- 编码 ----
                let t_enc = Instant::now();
                let packets = enc
                    .encode_bgra(bgra, frame_index as i64)
                    .with_context(|| format!("encode frame {frame_index}"))?;
                let enc_ms = t_enc.elapsed().as_secs_f64() * 1000.0;
                shared.stats.encode_time(enc_ms);
                if packets.iter().any(|p| p.key) {
                    tracing::debug!(frame = frame_index, "keyframe emitted");
                }
                // 单帧可能产出多个包（编码器内部缓存/重排，理论上 CBR 无 B 帧为 1 个）
                for (i, p) in packets.iter().enumerate() {
                    let pkt_frame = frame_index.wrapping_add(i as u32);
                    let fec = shared.fec_percentage.load(Ordering::SeqCst);
                    let capture_us = Shared::unix_us();
                    let media = framing::build_packets(
                        &p.data, pkt_frame, framing::CODEC_H264, p.key, capture_us, fec,
                    );
                    let bytes: u64 = media.iter().map(|x| x.len() as u64).sum();
                    let shard_count = media.len();
                    shared.stats.frame_sent(p.key, bytes);
                    shared.media_send(media);
                    shared.ctrl_send(ControlMsg(serde_json::json!({
                        "type": "frameStats",
                        "frameIndex": pkt_frame,
                        "captureMs": (cap_ms * 100.0).round() / 100.0,
                        "encodeMs": (enc_ms * 100.0).round() / 100.0,
                        "bytes": bytes,
                        "keyframe": p.key,
                        "shards": shard_count,
                        "sentUnixUs": Shared::unix_us(),
                    })));
                }
                frame_index = frame_index.wrapping_add(packets.len() as u32);
            }
        }
    }
}

fn now_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_millis() as u64
}

fn init_tracing(log_file: Option<&str>) -> Result<()> {
    use tracing_subscriber::EnvFilter;
    let filter =
        EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new("info,mvp=debug"));
    let builder = tracing_subscriber::fmt()
        .with_env_filter(filter)
        .with_target(true)
        .with_thread_names(false)
        .with_ansi(false);
    match log_file {
        Some(path) => {
            // xnc-core 形态：日志落 ProgramData\XNC\logs（追加写，父目录由创建方保证）
            if let Some(parent) = std::path::Path::new(path).parent() {
                std::fs::create_dir_all(parent).ok();
            }
            let file = std::fs::OpenOptions::new()
                .create(true)
                .append(true)
                .open(path)
                .with_context(|| format!("open log file {path}"))?;
            builder
                .with_writer(move || file.try_clone().expect("log file clone"))
                .init();
        }
        None => builder.init(),
    }
    Ok(())
}
