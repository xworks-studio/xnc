import { useEffect, useRef, useState } from "react";
import { Link, useParams } from "react-router-dom";
import type { MouseEvent as ReactMouseEvent } from "react";
import { api } from "../api";

/**
 * RTV desktop viewer (2026-09-08 rebuild): WebTransport 主路 + WebSocket
 * 兜底，Worker 内组帧/FEC/WebCodecs 硬解，主线程 rAF Pacer 渲染。
 * 沉浸式独立路由（无侧栏，占满视口）——直达 /desktop/<nodeId>。
 *
 * 1. POST /api/nodes/{id}/desktop {} → 202 {token, wtUrl, wsUrl, lease}。
 * 2. WT 主路（真实 CA 证书，标准 Web PKI——无 serverCertificateHashes 层）；
 *    失败自动回退 WS 兜底（?transport=ws 强制）。
 * 3. 控制词汇：hello/heartbeat/feedback/frameLoss（viewer→host 方向），
 *    config/frameStats/qosAdjust/hostOffline（host→viewer 方向）。
 * 4. 渲染：Worker 解码出 VideoFrame 转移主线程，队列≤3 迟到丢帧不追、
 *    延迟一帧释放、空转重绘（moonlight-qt Pacer 语义，worker.js 配套）。
 * 5. 输入：仅鼠标（键盘为后续 PATCH）；lease 授予方可开启；绝对坐标
 *    letterbox 映射，move 16ms 合并。canvas 坐标即编码分辨率空间，host
 *    侧换算回原生桌面像素（降采样场景）。
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

type Phase = "connecting" | "waiting" | "live" | "hostOffline" | "fatal";

const MAX_QUEUE = 3;
const MAX_RETRIES = 8;

export default function DesktopLive() {
  const { nodeId = "" } = useParams();
  const [epoch, setEpoch] = useState(0);
  const [phase, setPhase] = useState<Phase>("connecting");
  const [fatalMsg, setFatalMsg] = useState("");
  const [transport, setTransport] = useState<"wt" | "ws" | "-">("-");
  const [codecInfo, setCodecInfo] = useState("");
  // 控制权归属（服务器 controlState 广播；null = 尚未收到）
  const [control, setControl] = useState<{
    holder: string;
    name: string;
  } | null>(null);
  const selfSessionRef = useRef("");
  const [inputOn, setInputOn] = useState(false);
  const [statsOpen, setStatsOpen] = useState(false);
  const [isFs, setIsFs] = useState(false);
  const [hud, setHud] = useState<WorkerStats | null>(null);
  // 每秒差分速率（worker 计数器为累计值）
  const [rates, setRates] = useState({ mbps: 0, pktRate: 0, fps: 0 });
  const [rttMs, setRttMs] = useState(0);
  const [e2eMs, setE2eMs] = useState<number | null>(null);
  const [logLines, setLogLines] = useState<string[]>([]);

  const rootRef = useRef<HTMLDivElement | null>(null);
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  // effect 内部状态（不经 React 渲染路径的高频对象）
  const sendCtrlRef = useRef<((obj: unknown) => void) | null>(null);
  const inputOnRef = useRef(false);
  const pendingMoveRef = useRef<{ x: number; y: number } | null>(null);
  const retryRef = useRef(0);
  const prevStatsRef = useRef<WorkerStats | null>(null);
  const rttRef = useRef(0);

  useEffect(() => {
    inputOnRef.current = inputOn;
  }, [inputOn]);

  // 全屏切换（F 键 / 按钮）
  const toggleFullscreen = () => {
    const el = rootRef.current;
    if (!el) return;
    if (document.fullscreenElement) void document.exitFullscreen();
    else void el.requestFullscreen().catch(() => {});
  };
  useEffect(() => {
    const onFs = () => setIsFs(Boolean(document.fullscreenElement));
    const onKey = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement | null;
      if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA")) return;
      if (e.key === "f" || e.key === "F") toggleFullscreen();
    };
    document.addEventListener("fullscreenchange", onFs);
    window.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("fullscreenchange", onFs);
      window.removeEventListener("keydown", onKey);
    };
  }, []);

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
    let gotFrame = false;
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
        setFatalMsg("连接重试次数用尽；请检查网络后重试。");
        setPhase("fatal");
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
          setCodecInfo(
            `H.264 ${config.width}×${config.height}@${config.fps} · ${config.encoder}${config.encoderHw ? " 硬编" : ""} · FEC ${config.fecPercentage}%`,
          );
          worker?.postMessage({ type: "config", ...v });
          if (canvasRef.current) {
            canvasRef.current.width = config.width;
            canvasRef.current.height = config.height;
          }
          // hostOffline 恢复：回到等画面（下一帧到达即 live）
          setPhase((p) => (p === "hostOffline" ? "waiting" : p));
          break;
        case "qosAdjust":
          log(
            `QoS: fps=${v.fps} kbps=${v.bitrateKbps} fec=${v.fecPercentage}% (${v.reason})`,
          );
          break;
        case "controlState": {
          // 控制权归属变更（含自己接管/被接管/他人释放）。
          const holder = typeof v.holderSession === "string" ? v.holderSession : "";
          const name = typeof v.holderName === "string" ? v.holderName : "";
          setControl({ holder, name });
          // 被接管：持有者不再是自己且本地仍在注入 → 自动让位。
          if (
            holder &&
            holder !== selfSessionRef.current &&
            inputOnRef.current
          ) {
            setInputOn(false);
            log(`控制权被 ${name || "其他用户"} 接管，输入已停止`);
          }
          break;
        }
        case "controlResult":
          if (v.ok === false) {
            const why =
              v.reason === "cooldown" ? "接管过于频繁，稍候再试" : String(v.reason);
            setInputOn(false);
            log(`接管控制失败：${why}`);
          }
          break;
        case "hostOffline":
          // 置空 config 恢复 hello 周期重发；host 回来后 config 重新下发。
          config = null;
          setPhase("hostOffline");
          break;
        case "heartbeat":
          if (typeof v.tMs === "number") {
            const rtt = Date.now() - v.tMs;
            rttRef.current = rtt;
            setRttMs(rtt);
          }
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
          rttMs: Math.round(rttRef.current),
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
            if (!gotFrame) {
              gotFrame = true;
              setPhase("live");
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
            // 每秒差分（计数器均为累计值）
            {
              const prev = prevStatsRef.current;
              if (prev) {
                setRates({
                  mbps: ((m.bytes - prev.bytes) * 8) / 1e6,
                  pktRate: m.pkts - prev.pkts,
                  fps: m.decoded - prev.decoded,
                });
              }
              prevStatsRef.current = m;
            }
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
      // lease.granted 仅是创建时刻的被动授予提示；实时归属以 controlState 为准
      selfSessionRef.current = resp.sessionId;
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
      setPhase("waiting");
    };

    run().catch((e) => {
      if (cancelled) return;
      const msg = e instanceof Error ? e.message : String(e);
      setFatalMsg(`会话建立失败：${msg}`);
      setPhase("fatal");
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

  // 滚轮要走原生非被动监听：React 合成 wheel 在根节点按 passive 注册，
  // preventDefault 无效（输入态下页面会被滚出去）。
  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas || !inputOn) return;
    const onWheel = (e: WheelEvent) => {
      e.preventDefault();
      sendCtrlRef.current?.({
        type: "input",
        event: "mouse",
        kind: "wheel",
        dy: Math.sign(e.deltaY) * Math.min(3, Math.abs(e.deltaY) / 100),
        dx: Math.sign(e.deltaX),
      });
    };
    canvas.addEventListener("wheel", onWheel, { passive: false });
    return () => canvas.removeEventListener("wheel", onWheel);
  }, [inputOn, phase]);

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

  const controlSelf = control !== null && control.holder === selfSessionRef.current;
  const controlOther =
    control !== null && control.holder !== "" && control.holder !== selfSessionRef.current;
  const inputAllowed = inputOn && controlSelf;
  const sendMouse = (kind: string, extra: Record<string, unknown>) => {
    if (!inputAllowed) return;
    sendCtrlRef.current?.({ type: "input", event: "mouse", kind, ...extra });
  };

  // 控制权按钮：自己持约=开始/停止控制（本地开关）；他人持约或空闲=接管
  // （takeControl——他人活约时为抢占，服务器 3s 防抖）。
  const onControlClick = () => {
    if (controlSelf && inputOn) {
      setInputOn(false);
      sendCtrlRef.current?.({ type: "releaseControl" });
      return;
    }
    setInputOn(true);
    if (!controlSelf) sendCtrlRef.current?.({ type: "takeControl" });
  };
  const controlLabel = controlSelf
    ? inputOn
      ? "停止控制"
      : "开始控制"
    : controlOther
      ? `接管控制（当前：${control?.name || "其他用户"}）`
      : "接管控制";

  const onMouseMove = (e: ReactMouseEvent<HTMLCanvasElement>) => {
    if (!inputAllowed) return;
    const p = mapToVideo(e.clientX, e.clientY);
    if (p) pendingMoveRef.current = p;
  };

  const retryNow = () => {
    retryRef.current = 0;
    setFatalMsg("");
    setPhase("connecting");
    setEpoch((e) => e + 1);
  };

  return (
    <div className="dt-root" ref={rootRef}>
      <header className="dt-topbar">
        <Link className="dt-back" to={`/nodes/${nodeId}`} title="返回节点详情">
          ←
        </Link>
        <span className="dt-title">桌面 · {nodeId.slice(0, 8)}</span>
        <div className="dt-chips">
          <span
            className={`dt-chip ${transport === "wt" ? "ok" : transport === "ws" ? "warn" : ""}`}
          >
            {transport === "-"
              ? "连接中"
              : transport === "wt"
                ? "WebTransport"
                : "WS 兜底"}
          </span>
          {phase === "live" && <span className="dt-chip ok">● 在线</span>}
          {phase === "hostOffline" && (
            <span className="dt-chip bad">○ 主机离线</span>
          )}
          {controlOther && (
            <span className="dt-chip warn" title="其他用户持有控制权">
              控制权：{control?.name || "其他用户"}
            </span>
          )}
          <span className="dt-chip" title="控制环往返（心跳测得）">
            rtt {Math.round(rttMs)}ms
          </span>
          {phase === "live" && (
            <span className="dt-chip" title="解码帧率（每秒差分）">
              {rates.fps} fps
            </span>
          )}
          <span
            className="dt-chip"
            title="采集→渲染端到端（含双端时钟偏差，参考值）"
          >
            e2e≈{e2eMs ?? "–"}ms
          </span>
          {codecInfo && (
            <span className="dt-chip" title={codecInfo}>
              {codecInfo}
            </span>
          )}
        </div>
        <div className="dt-actions">
          <button
            className={`btn${inputAllowed ? " dt-btn-on" : ""}`}
            title={
              controlSelf
                ? inputOn
                  ? "停止注入并释放控制权"
                  : "开始注入鼠标输入（已持有控制权）"
                : "接管控制权（他人持有时为抢占，3 秒防抖）"
            }
            onClick={onControlClick}
          >
            {controlLabel}
          </button>
          <button
            className="btn"
            onClick={() => setStatsOpen((v) => !v)}
            title="统计面板"
          >
            统计
          </button>
          <button className="btn" onClick={toggleFullscreen} title="全屏（F）">
            {isFs ? "退出全屏" : "全屏"}
          </button>
        </div>
      </header>

      <div className="dt-body">
        <div className="dt-stage">
          <canvas
            ref={canvasRef}
            className={inputAllowed ? "dt-canvas dt-capture" : "dt-canvas"}
            onMouseMove={onMouseMove}
            onMouseDown={(e) => {
              if (!inputAllowed) return;
              e.preventDefault();
              sendMouse("down", { button: e.button });
            }}
            onMouseUp={(e) => sendMouse("up", { button: e.button })}
            onContextMenu={(e) => {
              if (inputAllowed) e.preventDefault();
            }}
          />
          {phase === "connecting" && (
            <div className="dt-overlay">
              <div className="dt-spin" />
              <div>正在建立会话…</div>
            </div>
          )}
          {phase === "waiting" && (
            <div className="dt-overlay dim">
              <div className="dt-spin" />
              <div>已连接，等待主机画面…</div>
            </div>
          )}
          {phase === "hostOffline" && (
            <div className="dt-overlay dim">
              <div className="dt-pulse" />
              <div>主机离线，等待重连…</div>
            </div>
          )}
          {phase === "fatal" && (
            <div className="dt-overlay">
              <div className="dt-err">{fatalMsg}</div>
              <button className="btn" onClick={retryNow}>
                重新连接
              </button>
            </div>
          )}
        </div>

        <aside className={`dt-stats${statsOpen ? " open" : ""}`}>
          <div className="dt-stats-inner">
            <div className="dt-stats-title">实时统计</div>
            <div className="dt-mgrid">
              <div className="dt-m">
                <div className="k">码率</div>
                <div className="v">{rates.mbps.toFixed(2)} Mbps</div>
              </div>
              <div className="dt-m">
                <div className="k">收包</div>
                <div className="v">{rates.pktRate} /s</div>
              </div>
              <div className="dt-m">
                <div className="k">解码帧率</div>
                <div className="v">{rates.fps} fps</div>
              </div>
              <div className="dt-m">
                <div className="k">解码队列</div>
                <div className="v">{hud?.decodeQueue ?? 0}</div>
              </div>
              <div className="dt-m">
                <div className="k">FEC 恢复 / 失败</div>
                <div className="v">
                  {hud?.fecRecovered ?? 0} / {hud?.fecFailed ?? 0}
                </div>
              </div>
              <div className="dt-m">
                <div className="k">完整帧</div>
                <div className="v">{hud?.framesComplete ?? 0}</div>
              </div>
              <div className="dt-m">
                <div className="k">丢帧上报</div>
                <div className="v">{hud?.lossSent ?? 0}</div>
              </div>
              <div className="dt-m">
                <div className="k">到帧间隔 p95</div>
                <div className="v">{hud?.arrivalGapP95Ms ?? 0} ms</div>
              </div>
            </div>
            <div className="dt-stats-title">事件</div>
            <div className="dt-log">
              {logLines.length === 0 ? (
                <div style={{ opacity: 0.5 }}>（暂无）</div>
              ) : (
                logLines.map((l, i) => <div key={i}>{l}</div>)
              )}
            </div>
          </div>
        </aside>
      </div>

      {inputOn && (
        <footer className="dt-hint">
          {inputAllowed
            ? "鼠标控制已开启，移动/点击/滚轮将注入远程桌面（键盘输入为后续版本）"
            : "正在接管控制权…（他人持有时为抢占）"}
        </footer>
      )}
    </div>
  );
}
