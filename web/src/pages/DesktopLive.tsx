import { useEffect, useRef, useState } from "react";
import { useParams } from "react-router-dom";
import type {
  MouseEvent as ReactMouseEvent,
  WheelEvent as ReactWheelEvent,
} from "react";
import { api } from "../api";

/**
 * RTV desktop viewer (2026-09-08 rewrite): WebTransport 主路 + WebSocket
 * 兜底，Worker 内组帧/FEC/WebCodecs 硬解，主线程 rAF Pacer 渲染。
 * Not in the sidebar — reach it directly at /desktop/<nodeId>.
 *
 * 1. POST /api/nodes/{id}/desktop {} → 202 {token, wtUrl, wsUrl, lease}。
 * 2. WT 主路（真实 CA 证书，标准 Web PKI——无 serverCertificateHashes 层）；
 *    失败自动回退 WS 兜底（?transport=ws 强制）。
 * 3. 控制词汇：hello/heartbeat/feedback/frameLoss（viewer→host 方向），
 *    config/frameStats/qosAdjust/hostOffline（host→viewer 方向）。
 * 4. 渲染：Worker 解码出 VideoFrame 转移主线程，队列≤3 迟到丢帧不追、
 *    延迟一帧释放、空转重绘（moonlight-qt Pacer 语义，worker.js 配套）。
 * 5. 输入：仅鼠标（键盘为后续 PATCH）；lease 授予方可开启；绝对坐标
 *    letterbox 映射，move 16ms 合并。
 * 6. 恢复：hostOffline → hello 周期重发（host 回来 config 重下发，无感）；
 *    传输层断开 → 重建会话（epoch 递增 + 退避，最多 8 次）。
 */

interface DesktopOpenResp {
  sessionId: string;
  token: string;
  expiresAt: string;
  wtUrl: string;
  wsUrl: string;
  lease: { granted: boolean; leaseId: string };
}

interface WorkerStats {
  pkts: number;
  bytes: number;
  framesComplete: number;
  fecRecovered: number;
  fecFailed: number;
  lossSent: number;
  decoded: number;
  decodeQueue: number;
  arrivalGapP95Ms: number;
}

interface HostConfig {
  type: "config";
  width: number;
  height: number;
  fps: number;
  encoder: string;
  encoderHw: boolean;
  fecPercentage: number;
}

const MAX_QUEUE = 3;
const MAX_RETRIES = 8;

export default function DesktopLive() {
  const { nodeId = "" } = useParams();
  const [epoch, setEpoch] = useState(0);
  const [status, setStatus] = useState("连接中…");
  const [fatal, setFatal] = useState("");
  const [transport, setTransport] = useState<"wt" | "ws" | "-">("-");
  const [codecInfo, setCodecInfo] = useState("");
  const [hostOnline, setHostOnline] = useState(false);
  const [leaseGranted, setLeaseGranted] = useState(false);
  const [inputOn, setInputOn] = useState(false);
  const [hud, setHud] = useState<WorkerStats | null>(null);
  const [rttMs, setRttMs] = useState(0);
  const [e2eMs, setE2eMs] = useState<number | null>(null);
  const [logLines, setLogLines] = useState<string[]>([]);

  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  // effect 内部状态（不经 React 渲染路径的高频对象）
  const sendCtrlRef = useRef<((obj: unknown) => void) | null>(null);
  const inputOnRef = useRef(false);
  const pendingMoveRef = useRef<{ x: number; y: number } | null>(null);
  const retryRef = useRef(0);

  useEffect(() => {
    inputOnRef.current = inputOn;
  }, [inputOn]);

  useEffect(() => {
    const log = (s: string) =>
      setLogLines((ls) => [
        `[${new Date().toTimeString().slice(0, 8)}] ${s}`,
        ...ls.slice(0, 11),
      ]);

    let cancelled = false;
    let worker: Worker | null = null;
    let wt: WebTransport | null = null;
    let ws: WebSocket | null = null;
    let ctrlWriter: WritableStreamDefaultWriter<Uint8Array> | null = null;
    let helloTimer: number | undefined;
    let hbTimer: number | undefined;
    let fbTimer: number | undefined;
    let raf = 0;
    let moveTimer: number | undefined;
    // Pacer 状态
    const queue: { frame: VideoFrame; captureUnixUs: number }[] = [];
    let lastDrawn: VideoFrame | null = null;
    // 反馈输入
    let workerStats: WorkerStats | null = null;
    let config: HostConfig | null = null;
    let curTransport: "wt" | "ws" = "wt";

    const cleanup = () => {
      cancelled = true;
      window.clearInterval(helloTimer);
      window.clearInterval(hbTimer);
      window.clearInterval(fbTimer);
      window.clearInterval(moveTimer);
      cancelAnimationFrame(raf);
      worker?.terminate();
      for (const m of queue) m.frame.close();
      queue.length = 0;
      lastDrawn?.close();
      lastDrawn = null;
      try {
        wt?.close({});
      } catch {
        /* already closed */
      }
      try {
        ws?.close();
      } catch {
        /* already closed */
      }
      sendCtrlRef.current = null;
    };

    const bumpEpoch = () => {
      if (cancelled) return;
      retryRef.current += 1;
      if (retryRef.current > MAX_RETRIES) {
        setFatal("连接重试次数用尽；请检查网络后刷新。");
        return;
      }
      window.setTimeout(() => {
        if (!cancelled) setEpoch((e) => e + 1);
      }, 2000 * Math.min(retryRef.current, 6));
    };

    const sendCtrl = (obj: unknown) => {
      const body = new TextEncoder().encode(JSON.stringify(obj));
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify(obj));
        return;
      }
      if (ctrlWriter) {
        const frame = new Uint8Array(4 + body.length);
        new DataView(frame.buffer).setUint32(0, body.length, true);
        frame.set(body, 4);
        void ctrlWriter.write(frame).catch(() => {
          /* 传输层故障由 closed 回调兜底 */
        });
      }
    };
    sendCtrlRef.current = sendCtrl;

    const handleCtrl = (v: Record<string, unknown>) => {
      switch (v.type) {
        case "config":
          config = v as unknown as HostConfig;
          setHostOnline(true);
          setCodecInfo(
            `H.264 ${config.width}×${config.height}@${config.fps} · ${config.encoder}${config.encoderHw ? " (硬编)" : ""} · FEC ${config.fecPercentage}%`,
          );
          worker?.postMessage({ type: "config", ...v });
          if (canvasRef.current) {
            canvasRef.current.width = config.width;
            canvasRef.current.height = config.height;
          }
          break;
        case "qosAdjust":
          log(
            `QoS: fps=${v.fps} kbps=${v.bitrateKbps} fec=${v.fecPercentage}% (${v.reason})`,
          );
          break;
        case "hostOffline":
          // 置空 config 恢复 hello 周期重发；host 回来后 config 重新下发。
          config = null;
          setHostOnline(false);
          setStatus("主机离线，等待重连…");
          break;
        case "heartbeat":
          if (typeof v.tMs === "number") setRttMs(Date.now() - v.tMs);
          break;
        default:
          break;
      }
    };

    const startLoops = () => {
      const hello = {
        type: "hello",
        role: "viewer",
        transport: curTransport,
        codecs: ["h264"],
        maxBitrateKbps: 30000,
        fps: 60,
      };
      sendCtrl(hello);
      // 周期 hello：无 config（host 未上线/已下线）时每 2s 重发。不随
      // config 到达而停——hostOffline 恢复靠它（MVP 踩坑结晶）。
      helloTimer = window.setInterval(() => {
        if (!config) sendCtrl(hello);
      }, 2000);
      hbTimer = window.setInterval(
        () => sendCtrl({ type: "heartbeat", tMs: Date.now() }),
        1000,
      );
      fbTimer = window.setInterval(() => {
        const s = workerStats;
        sendCtrl({
          type: "feedback",
          rttMs: Math.round(rttMs),
          arrivalGapP95Ms: s?.arrivalGapP95Ms || 0,
          decodeQueueDepth: s?.decodeQueue || 0,
          decodedFps: s?.decoded || 0,
        });
      }, 1000);
    };

    const startWorker = (reliable: boolean) => {
      worker = new Worker(new URL("../lib/rtv/worker.js", import.meta.url), {
        type: "module",
      });
      worker.postMessage({ type: "transport", reliable });
      worker.onmessage = (ev: MessageEvent) => {
        const m = ev.data;
        switch (m.type) {
          case "decoded":
            queue.push(m);
            while (queue.length > MAX_QUEUE) {
              queue.shift()?.frame.close();
            }
            break;
          case "frameLoss":
            sendCtrl({
              type: "frameLoss",
              frameIndex: m.frameIndex,
              reason: m.reason,
            });
            break;
          case "stats":
            workerStats = m;
            setHud(m);
            break;
          case "status":
            setStatus(String(m.msg));
            break;
          case "decoderError":
            log(`解码器错误: ${m.error}（已请求 IDR）`);
            break;
          default:
            break;
        }
      };
    };

    const renderLoop = () => {
      raf = requestAnimationFrame(renderLoop);
      const canvas = canvasRef.current;
      const ctx = canvas?.getContext("2d", {
        desynchronized: true,
        alpha: false,
      });
      if (!canvas || !ctx) return;
      if (!queue.length) {
        if (lastDrawn) ctx.drawImage(lastDrawn, 0, 0); // 空转重绘保持画面
        return;
      }
      const m = queue.shift()!;
      ctx.drawImage(m.frame, 0, 0, canvas.width, canvas.height);
      if (m.captureUnixUs) {
        setE2eMs(Math.round((Date.now() * 1000 - m.captureUnixUs) / 1000));
      }
      if (lastDrawn) lastDrawn.close(); // 延迟一帧释放（防 GPU 竞争）
      lastDrawn = m.frame;
    };
    raf = requestAnimationFrame(renderLoop);

    const connectWS = async (url: string) => {
      const sock = new WebSocket(url);
      sock.binaryType = "arraybuffer";
      await new Promise<void>((res, rej) => {
        sock.onopen = () => res();
        sock.onerror = () => rej(new Error("ws error"));
      });
      ws = sock;
      sock.onmessage = (ev) => {
        if (typeof ev.data === "string") {
          try {
            handleCtrl(JSON.parse(ev.data));
          } catch {
            /* bad json */
          }
        } else {
          worker?.postMessage({ type: "datagram", buf: ev.data }, [ev.data]);
        }
      };
      sock.onclose = () => {
        if (ws !== sock) return; // 已被新一轮会话替换
        log("WS 断开");
        bumpEpoch();
      };
    };

    const connectWT = async (url: string) => {
      const t = new WebTransport(url);
      // closed 只在 WT 仍是当前活跃传输时才触发重建：握手失败回退 WS 后，
      // 迟到的 closed 回调不得把已建立的 WS 会话杀掉（曾致无限重连循环）。
      void t.closed.then(
        () => {
          if (wt === t) {
            log("WT 关闭");
            bumpEpoch();
          }
        },
        () => {
          if (wt === t) {
            log("WT 异常关闭");
            bumpEpoch();
          }
        },
      );
      await t.ready;
      wt = t;
      const stream = await t.createBidirectionalStream();
      ctrlWriter = stream.writable.getWriter();
      startWorker(false);
      // 控制读取（4B LE 长度前缀 JSON）
      void (async () => {
        const reader = stream.readable.getReader();
        const dec = new TextDecoder();
        let carry = new Uint8Array(0);
        for (;;) {
          const { done, value } = await reader.read();
          if (done || !value) return;
          const buf = new Uint8Array(carry.length + value.length);
          buf.set(carry);
          buf.set(value, carry.length);
          let off = 0;
          for (;;) {
            if (off + 4 > buf.length) break;
            const len = new DataView(buf.buffer, off, 4).getUint32(0, true);
            if (off + 4 + len > buf.length) break;
            try {
              handleCtrl(
                JSON.parse(dec.decode(buf.subarray(off + 4, off + 4 + len))),
              );
            } catch {
              /* bad json */
            }
            off += 4 + len;
          }
          carry = buf.slice(off);
        }
      })();
      // 媒体 datagram → worker
      void (async () => {
        const reader = t.datagrams.readable.getReader();
        for (;;) {
          const { done, value } = await reader.read();
          if (done || !value) return;
          worker?.postMessage({ type: "datagram", buf: value.buffer }, [
            value.buffer,
          ]);
        }
      })();
    };

    const run = async () => {
      const resp = await api<DesktopOpenResp>(
        `/api/nodes/${encodeURIComponent(nodeId)}/desktop`,
        { method: "POST", body: "{}" },
      );
      setLeaseGranted(resp.lease?.granted ?? false);
      const wanted = new URLSearchParams(location.search).get("transport");
      const sep = (u: string) => (u.includes("?") ? "&" : "?");
      const wtUrl = `${resp.wtUrl}${sep(resp.wtUrl)}token=${encodeURIComponent(resp.token)}`;
      const wsUrl = `${resp.wsUrl}${sep(resp.wsUrl)}token=${encodeURIComponent(resp.token)}`;
      if (wanted === "ws") {
        curTransport = "ws";
        await connectWS(wsUrl);
        startWorker(true);
      } else {
        curTransport = "wt";
        try {
          await connectWT(wtUrl);
        } catch (e) {
          log(`WebTransport 失败（${e}），回退 WebSocket`);
          curTransport = "ws";
          await connectWS(wsUrl);
          startWorker(true);
        }
      }
      setTransport(curTransport);
      startLoops();
      setStatus("已连接，等待主机画面…");
    };

    run().catch((e) => {
      if (cancelled) return;
      const msg = e instanceof Error ? e.message : String(e);
      setFatal(`会话建立失败：${msg}`);
    });

    // ---- 输入（鼠标；lease 授予 + 用户开启才注入）----
    moveTimer = window.setInterval(() => {
      if (inputOnRef.current && pendingMoveRef.current) {
        sendCtrl({
          type: "input",
          event: "mouse",
          kind: "move",
          ...pendingMoveRef.current,
        });
        pendingMoveRef.current = null;
      }
    }, 16);

    return cleanup;
  }, [nodeId, epoch]);

  const mapToVideo = (clientX: number, clientY: number) => {
    const canvas = canvasRef.current;
    if (!canvas || canvas.width === 0) return null;
    const r = canvas.getBoundingClientRect();
    const scale = Math.min(r.width / canvas.width, r.height / canvas.height);
    const vw = canvas.width * scale;
    const vh = canvas.height * scale;
    const ox = r.left + (r.width - vw) / 2;
    const oy = r.top + (r.height - vh) / 2;
    const x = Math.round((clientX - ox) / scale);
    const y = Math.round((clientY - oy) / scale);
    return {
      x: Math.max(0, Math.min(canvas.width - 1, x)),
      y: Math.max(0, Math.min(canvas.height - 1, y)),
    };
  };

  const inputAllowed = leaseGranted && inputOn;
  const sendMouse = (kind: string, extra: Record<string, unknown>) => {
    if (!inputAllowed) return;
    sendCtrlRef.current?.({ type: "input", event: "mouse", kind, ...extra });
  };

  const onMouseMove = (e: ReactMouseEvent<HTMLCanvasElement>) => {
    if (!inputAllowed) return;
    const p = mapToVideo(e.clientX, e.clientY);
    if (p) pendingMoveRef.current = p;
  };

  return (
    <div className="page">
      <h1>
        桌面 — {nodeId.slice(0, 8)}
        <span style={{ fontSize: "0.6em", opacity: 0.6 }}>
          {" "}
          RTV/{transport.toUpperCase()}
          {hostOnline ? "" : " · 主机离线"}
        </span>
      </h1>
      {fatal ? (
        <p className="error">{fatal}</p>
      ) : (
        <>
          <div className="desktop-toolbar">
            <span className="status-badge">{status}</span>
            <span style={{ opacity: 0.7 }}>{codecInfo}</span>
            <button disabled={!leaseGranted} onClick={() => setInputOn((v) => !v)}>
              鼠标控制：{inputOn ? "开" : "关"}
              {!leaseGranted && "（无输入权）"}
            </button>
            <span style={{ opacity: 0.7 }}>
              e2e≈{e2eMs ?? "–"}ms rtt={Math.round(rttMs)}ms
            </span>
          </div>
          <div className="desktop-video-wrap">
            <canvas
              ref={canvasRef}
              style={{ maxWidth: "100%", maxHeight: "72vh" }}
              onMouseMove={onMouseMove}
              onMouseDown={(e) => {
                if (!inputAllowed) return;
                e.preventDefault();
                sendMouse("down", { button: e.button });
              }}
              onMouseUp={(e) => sendMouse("up", { button: e.button })}
              onWheel={(e: ReactWheelEvent<HTMLCanvasElement>) => {
                if (!inputAllowed) return;
                e.preventDefault();
                sendMouse("wheel", {
                  dy: Math.sign(e.deltaY) * Math.min(3, Math.abs(e.deltaY) / 100),
                  dx: Math.sign(e.deltaX),
                });
              }}
              onContextMenu={(e) => {
                if (inputAllowed) e.preventDefault();
              }}
            />
          </div>
          {hud && (
            <div className="desktop-hud">
              码率≈{(hud.bytes ? (hud.bytes * 8) / 1e6 : 0).toFixed(2)}Mbps ·
              收包{hud.pkts}/s · 完整帧{hud.framesComplete} · 解码{hud.decoded}
              · FEC 恢复{hud.fecRecovered}/失败{hud.fecFailed} · 丢帧上报
              {hud.lossSent} · 解码队列{hud.decodeQueue} · 到帧间隔p95
              {hud.arrivalGapP95Ms}ms
            </div>
          )}
          <div className="desktop-log">
            {logLines.map((l, i) => (
              <div key={i}>{l}</div>
            ))}
          </div>
        </>
      )}
    </div>
  );
}
