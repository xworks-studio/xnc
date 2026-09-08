//! QUIC 传输：不可靠 datagram（媒体，带令牌桶整流）+ 可靠流（控制 JSON）。
//!
//! 对应架构（研究报告/Sunshine）：
//! - 媒体 = 单向不可靠流（Sunshine RTP/UDP 的 QUIC 化），发送端按令牌桶整流
//!   （Sunshine videoBroadcastThread 80% 线速 pacing 的对应物，速率可配）。
//! - 控制 = 可靠有序（Moonlight ENet 控制通道的 QUIC Stream 对应物）。
//! - 断线自动重连（指数退避），重连后重发 config。
//!
//! 通道模型：生产者（采集线程 / QoS / 回显）只往 `Shared` 的发送槽位投递；
//! 传输层每次连接成功后接管槽位消费，断开时清空 —— 重连不丢生产者引用。

use std::net::SocketAddr;
use std::sync::atomic::Ordering;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result};
use bytes::Bytes;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::sync::{mpsc, Mutex};

use crate::shared::{ControlMsg, Shared};

pub const ALPN: &[&str] = &["xnc-host/1"];
const RECONNECT_BACKOFF: Duration = Duration::from_secs(2);
const CONTROL_BUF: usize = 256 * 1024;

/// 连接服务器并运行到进程结束（内部自动重连）。
pub async fn run(
    shared: Arc<Shared>,
    server: SocketAddr,
    server_name: &str,
    send_kbps: u32,
    tls_insecure: bool,
) -> Result<()> {
    let endpoint = quinn::Endpoint::client("0.0.0.0:0".parse().unwrap())
        .context("bind quic client endpoint")?;

    let mut backoff = RECONNECT_BACKOFF;
    loop {
        let t0 = std::time::Instant::now();
        match connect_and_serve(&endpoint, server, server_name, &shared, send_kbps, tls_insecure)
            .await
        {
            Err(e) => {
                shared.disconnect();
                tracing::warn!(
                    ?e,
                    next_retry_ms = backoff.as_millis() as u64,
                    was_connected_for_ms = t0.elapsed().as_millis() as u64,
                    "connection lost/failed, retrying"
                );
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(Duration::from_secs(10));
            }
            Ok(()) => unreachable!("connect_and_serve only returns on error"),
        }
    }
}

async fn connect_and_serve(
    endpoint: &quinn::Endpoint,
    server: SocketAddr,
    server_name: &str,
    shared: &Arc<Shared>,
    send_kbps: u32,
    tls_insecure: bool,
) -> anyhow::Result<()> {
    let t0 = std::time::Instant::now();
    let conn = endpoint
        .connect_with(client_config(tls_insecure)?, server, server_name)?
        .await
        .context("quic connect")?;
    tracing::info!(
        rtt_ms = conn.rtt().as_millis() as u64,
        connect_ms = t0.elapsed().as_millis() as u64,
        "connected to relay"
    );

    // 控制流（host 主动 open_bi）
    let (mut ctrl_tx, ctrl_rx_stream) = conn.open_bi().await.context("open control stream")?;

    // 注册 + 会话参数（token 为 server 经 SESSION_OPEN 链路下发的注册凭据）
    send_control(&mut ctrl_tx, &serde_json::json!({
        "type": "hello", "role": "host",
        "nodeId": shared.node_id, "token": shared.token,
    }))
    .await?;
    if let Some(meta) = shared.encoder_meta() {
        send_control(&mut ctrl_tx, &config_msg(shared, &meta)).await?;
    }

    // 建立本连接的媒体/控制发送通道，并发布到 Shared 槽位
    let (media_tx, media_rx) = mpsc::unbounded_channel::<Vec<Vec<u8>>>();
    let (ctrl_msg_tx, ctrl_msg_rx) = mpsc::unbounded_channel::<ControlMsg>();
    shared.publish_senders(media_tx, ctrl_msg_tx);
    shared.connected.store(true, Ordering::SeqCst);
    shared.stats.session_reconnected();

    let media_task = tokio::spawn(media_sender(conn.clone(), shared.clone(), send_kbps, media_rx));
    let ctrl_send_task = tokio::spawn(control_sender(ctrl_tx, ctrl_msg_rx));
    let ctrl_recv_task = tokio::spawn(control_receiver(ctrl_rx_stream, shared.clone()));

    let res: anyhow::Result<()> = tokio::select! {
        r = media_task => r.context("media sender task").and_then(|inner| inner),
        r = ctrl_send_task => r.context("ctrl sender task").and_then(|inner| inner),
        r = ctrl_recv_task => r.context("ctrl receiver task").and_then(|inner| inner),
    };
    res
}

/// 媒体 datagram 发送：令牌桶按 send_kbps 整流，允许 64KB 突发
/// （Sunshine 批量+pacing 思路；QUIC 自带拥塞控制，此处 pacing 用于平滑突发，
///   避免 keyframe 瞬时风暴触发路径队列丢弃）。
async fn media_sender(
    conn: quinn::Connection,
    shared: Arc<Shared>,
    send_kbps: u32,
    mut rx: mpsc::UnboundedReceiver<Vec<Vec<u8>>>,
) -> anyhow::Result<()> {
    let rate = send_kbps as f64 * 1000.0 / 8.0; // bytes/s
    let burst = 64.0 * 1024.0;
    let mut tokens = burst;
    let mut last = std::time::Instant::now();
    while let Some(packets) = rx.recv().await {
        let now = std::time::Instant::now();
        tokens = (tokens + now.duration_since(last).as_secs_f64() * rate).min(burst);
        last = now;
        for pkt in packets {
            while tokens < pkt.len() as f64 {
                let need = (pkt.len() as f64 - tokens) / rate;
                let sleep = need.clamp(0.0002, 0.05);
                tokio::time::sleep(Duration::from_secs_f64(sleep)).await;
                let now2 = std::time::Instant::now();
                tokens = (tokens + now2.duration_since(last).as_secs_f64() * rate).min(burst);
                last = now2;
            }
            tokens -= pkt.len() as f64;
            conn.send_datagram(Bytes::from(pkt.clone()))
                .map_err(|e| anyhow::anyhow!("send_datagram: {e}"))?;
            shared.stats.media_pkt_sent(pkt.len());
        }
    }
    Err(anyhow::anyhow!("media channel closed"))
}

async fn control_sender(
    mut tx: quinn::SendStream,
    mut rx: mpsc::UnboundedReceiver<ControlMsg>,
) -> anyhow::Result<()> {
    while let Some(msg) = rx.recv().await {
        send_control(&mut tx, &msg.0).await?;
    }
    Err(anyhow::anyhow!("control channel closed"))
}

async fn send_control(tx: &mut quinn::SendStream, v: &serde_json::Value) -> Result<()> {
    let body = serde_json::to_vec(v)?;
    tx.write_all(&(body.len() as u32).to_le_bytes()).await?;
    tx.write_all(&body).await?;
    Ok(())
}

/// 控制接收：JSON 分发（frameLoss→IDR / feedback→QoS / heartbeat 回显 / viewers）。
async fn control_receiver(mut rx: quinn::RecvStream, shared: Arc<Shared>) -> anyhow::Result<()> {
    let mut len_buf = [0u8; 4];
    let mut body = Vec::with_capacity(CONTROL_BUF);
    loop {
        rx.read_exact(&mut len_buf).await?;
        let len = u32::from_le_bytes(len_buf) as usize;
        if len == 0 || len > CONTROL_BUF {
            anyhow::bail!("bad control frame len {len}");
        }
        body.resize(len, 0);
        rx.read_exact(&mut body).await?;
        shared.stats.ctrl_msg_rx(len as u64);
        let v: serde_json::Value = match serde_json::from_slice(&body) {
            Ok(v) => v,
            Err(e) => {
                tracing::warn!(?e, "bad control json");
                continue;
            }
        };
        match v["type"].as_str().unwrap_or("") {
            "frameLoss" => {
                let frame = v["frameIndex"].as_u64().unwrap_or(0);
                let reason = v["reason"].as_str().unwrap_or("?");
                tracing::warn!(frame, reason, "viewer reported frame loss -> IDR");
                shared.stats.loss_report_rx();
                shared.idr_requested.store(true, Ordering::SeqCst);
            }
            "feedback" => {
                shared.record_feedback(
                    v["rttMs"].as_f64(),
                    v["decodeQueueDepth"].as_f64(),
                    v["decodedFps"].as_f64(),
                    v["arrivalGapP95Ms"].as_f64(),
                );
            }
            "heartbeat" => {
                // 原样回显（含 tMs），web 侧测控制环 RTT
                let mut echo = v.clone();
                echo["type"] = "heartbeat".into();
                echo["hostNowMs"] = now_ms().into();
                shared.ctrl_send(ControlMsg(echo));
            }
            "viewers" => {
                let n = v["count"].as_u64().unwrap_or(0) as usize;
                shared.viewers.store(n, Ordering::SeqCst);
                tracing::info!(n, "viewer count changed");
            }
            "setParams" => {
                if let Some(fps) = v["fps"].as_i64() {
                    shared.fps.store(fps.clamp(1, 120) as i32, Ordering::SeqCst);
                    tracing::info!(fps, "fps overridden by control");
                }
            }
            "input" => crate::input::handle(&v),
            other => tracing::debug!(r#type = other, "unhandled control msg"),
        }
    }
}

fn config_msg(shared: &Shared, meta: &crate::shared::EncoderMeta) -> serde_json::Value {
    serde_json::json!({
        "type": "config",
        "codec": "h264",
        "width": meta.width,
        "height": meta.height,
        "fps": shared.fps.load(Ordering::SeqCst),
        "fecPercentage": shared.fec_percentage.load(Ordering::SeqCst),
        "shardPayload": crate::framing::SHARD_PAYLOAD_TARGET,
        "encoder": meta.name,
        "encoderHw": meta.hw,
        "startedUnixMs": now_ms(),
    })
}

fn now_ms() -> u64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_millis() as u64
}

// ---------------- rustls 配置 ----------------
// 生产：系统根校验（server 证书为 CA 签发，SNI=server_name）。
// 调试：--tls-insecure 接受任意证书（仅本地自签 dev server，日志高声告警）。

fn client_config(insecure: bool) -> anyhow::Result<quinn::ClientConfig> {
    let mut tls = if insecure {
        rustls::ClientConfig::builder()
            .dangerous()
            .with_custom_certificate_verifier(Arc::new(AcceptAnyServer))
            .with_no_client_auth()
    } else {
        let mut roots = rustls::RootCertStore::empty();
        let native = rustls_native_certs::load_native_certs();
        if !native.errors.is_empty() {
            anyhow::bail!("load native certs: {:?}", native.errors);
        }
        for cert in native.certs {
            roots
                .add(cert)
                .map_err(|e| anyhow::anyhow!("add native cert: {e}"))?;
        }
        rustls::ClientConfig::builder()
            .with_root_certificates(roots)
            .with_no_client_auth()
    };
    tls.alpn_protocols = ALPN.iter().map(|p| p.as_bytes().to_vec()).collect();
    let mut transport = quinn::TransportConfig::default();
    transport.datagram_receive_buffer_size(Some(64 * 1024));
    transport.keep_alive_interval(Some(std::time::Duration::from_secs(5)));
    let quic_tls = quinn::crypto::rustls::QuicClientConfig::try_from(tls)
        .map_err(|e| anyhow::anyhow!("tls->quic config: {e:?}"))?;
    let mut cfg = quinn::ClientConfig::new(Arc::new(quic_tls));
    cfg.transport_config(Arc::new(transport));
    Ok(cfg)
}

/// 仅 --tls-insecure 调试路径使用的跳过校验器。
#[derive(Debug)]
struct AcceptAnyServer;

impl rustls::client::danger::ServerCertVerifier for AcceptAnyServer {
    fn verify_server_cert(
        &self,
        _end_entity: &rustls::pki_types::CertificateDer<'_>,
        _intermediates: &[rustls::pki_types::CertificateDer<'_>],
        _server_name: &rustls::pki_types::ServerName<'_>,
        _ocsp_response: &[u8],
        _now: rustls::pki_types::UnixTime,
    ) -> Result<rustls::client::danger::ServerCertVerified, rustls::Error> {
        Ok(rustls::client::danger::ServerCertVerified::assertion())
    }
    fn verify_tls12_signature(
        &self,
        _message: &[u8],
        _cert: &rustls::pki_types::CertificateDer<'_>,
        _dss: &rustls::DigitallySignedStruct,
    ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
        Ok(rustls::client::danger::HandshakeSignatureValid::assertion())
    }
    fn verify_tls13_signature(
        &self,
        _message: &[u8],
        _cert: &rustls::pki_types::CertificateDer<'_>,
        _dss: &rustls::DigitallySignedStruct,
    ) -> Result<rustls::client::danger::HandshakeSignatureValid, rustls::Error> {
        Ok(rustls::client::danger::HandshakeSignatureValid::assertion())
    }
    fn supported_verify_schemes(&self) -> Vec<rustls::SignatureScheme> {
        use rustls::SignatureScheme::*;
        vec![
            RSA_PKCS1_SHA256,
            ECDSA_NISTP256_SHA256,
            ED25519,
            RSA_PSS_SHA256,
            RSA_PKCS1_SHA384,
            ECDSA_NISTP384_SHA384,
        ]
    }
}

/// 防未使用告警（Mutex 在重构后仅用于类型命名空间）
#[allow(dead_code)]
type _Unused = Mutex<()>;
