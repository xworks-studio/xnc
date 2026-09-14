import { useEffect, useRef, useState } from "react";
import { Link, useParams } from "react-router-dom";
import type { MouseEvent as ReactMouseEvent } from "react";
import { api } from "../api";
import type { DesktopCandidate } from "../types";
import {
  classifyKeyDown,
  isPasteCombo,
  isTextPathKey,
  makeSetChunks,
  truncateUtf8,
  ClipAssembler,
} from "../lib/rtvInput";

/**
 * RTV desktop viewer (2026-09-08 rebuild): WebTransport 主路 + WebSocket
 * 兜底，Worker 内组帧/FEC/WebCodecs 硬解，主线程 rAF Pacer 渲染。
 * 沉浸式独立路由（无侧栏，占满视口）——直达 /desktop/<nodeId>。
 *
 * 1. POST /api/nodes/{id}/desktop {} → 202 {token, wtUrl, wsUrl, lease,
 *    candidates?}。带 candidates（relay-plane）时按候选序顺序 fallback
 *    （wt→ws）；certSha256 候选用 serverCertificateHashes 钉扎。无该字段
 *    走 wtUrl/wsUrl 既有逻辑。
 * 2. WT 主路（真实 CA 证书，标准 Web PKI——无 serverCertificateHashes 层）；
 *    失败自动回退 WS 兜底（?transport=ws 强制）。
 * 3. 控制词汇：hello/heartbeat/feedback/frameLoss（viewer→host 方向），
 *    config/frameStats/qosAdjust/hostOffline（host→viewer 方向）。
 * 4. 渲染：Worker 解码出 VideoFrame 转移主线程，队列≤3 迟到丢帧不追、
 *    延迟一帧释放、空转重绘（moonlight-qt Pacer 语义，worker.js 配套）。
 * 5. 输入：鼠标 + 键盘；lease 授予方可开启（inputAllowed = 本地开关 &&
 *    controlSelf）。鼠标：绝对坐标 letterbox 映射，move 16ms 合并；canvas
 *    坐标即编码分辨率空间，host 侧换算回原生桌面像素（降采样场景）。
 *    光标：本地十字准星（浏览器原生渲染，零延迟连续；mvp input.js 同
 *    款），流内不含光标——host 默认不合成（--cursor 调试开关）。
 *    键盘：无修饰键的可打印字符走 text 事件（KEYEVENTF_UNICODE，布局
 *    无关），其余走 code 物理键位（快捷键在远端成立）；输入态全量
 *    preventDefault（F5/Tab/空格不再撞本地；Ctrl+W/T 等浏览器保留键除外）。
 *    剪贴板：Ctrl+V 拦下 → 本地 readText → 分块发 host 写远端剪贴板 →
 *    收 set-ack 才补发合成 Ctrl+V（时序闭环）；远端剪贴板变化推送 →
 *    通知栏 + 点击复制（writeText 需手势）。IME 中文经“粘贴”对话框。
 * 6. 恢复：hostOffline → hello 周期重发（host 回来 config 重下发，无感）；
 *    传输层断开 → 重建会话（epoch 递增 + 退避，最多 8 次）；8 次烧尽后
 *    不直接 fatal——≥10s 节流 re-POST 重建会话再试，最多 3 轮后才维持
 *    失败 UI。
 */

interface DesktopOpenResp {
  sessionId: string;
  token: string;
  expiresAt: string;
  wtUrl: string;
  wsUrl: string;
  lease: { granted: boolean; leaseId: string };
  // relay 候选（relay-plane）：同一 relay 的传输变体，wt 在前。旧形态
  // 服务器不带此字段 → 走 wtUrl/wsUrl 既有逻辑（零行为变化）。
  candidates?: DesktopCandidate[];
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
// 重试烧尽后的会话重建（re-POST /desktop 拿新 token/端点）：最多轮数与
// 轮间节流（≥10s，避免高频打服务器）。
const REPOST_ROUNDS = 3;
const REPOST_THROTTLE_MS = 10_000;
// RTT/时钟偏差平滑窗口（中值）：媒体突发会短暂阻塞控制流（真实排队），
// 逐样本显示会双峰抖动 + 顶栏回流；中值滤波兼顾展示与 QoS 反馈。
const SMOOTH_WIN = 9;

// 窗口最低基线 + 最优样本：控制期媒体突发会让心跳回显在 host→viewer
// 发送队列里排队（与媒体同拥塞域），RTT 采样呈"真值 / 真值+排队延迟"
// 双峰——中值随多数派翻转来回跳（真机踩坑）。下包络才是网络往返的
// 诚实度量；拥塞感知由 arrivalGapP95 / 解码队列承担，不靠 RTT。
function minOf(nums: number[]): number {
  return nums.reduce((m, v) => Math.min(m, v), Infinity);
}

/** 64 位 hex → 32 字节（certSha256 → WebTransport 钉扎指纹）；非法输入
 * 返回 null（调用方跳过该候选，绝不静默降级为无钉扎连接）。
 * 注：显式 ArrayBuffer 参数化满足 WebTransportHash 的 BufferSource 约束。 */
function hexTo32Bytes(hex: string): Uint8Array<ArrayBuffer> | null {
  if (!/^[0-9a-fA-F]{64}$/.test(hex)) return null;
  const out = new Uint8Array(32);
  for (let i = 0; i < 32; i++) {
    out[i] = parseInt(hex.slice(i * 2, i * 2 + 2), 16);
  }
  return out;
}

export default function DesktopLive() {
  const { nodeId = "" } = useParams();
  const [epoch, setEpoch] = useState(0);
  const [phase, setPhase] = useState<Phase>("connecting");
  const [fatalMsg, setFatalMsg] = useState("");
  const [transport, setTransport] = useState<"wt" | "ws" | "-">("-");
  // 当前使用的中继（连接成功时按候选/URL 记录；统计面板展示）
  const [relayInfo, setRelayInfo] = useState<string | null>(null);
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
  const [rttMs, setRttMs] = useState<number | null>(null);
  const [e2eMs, setE2eMs] = useState<number | null>(null);
  const [logLines, setLogLines] = useState<string[]>([]);
  // 剪贴板：远端推送的最新文本 + 通知条 + 手动粘贴对话框
  const [remoteClipLen, setRemoteClipLen] = useState(0);
  const [clipNotice, setClipNotice] = useState<string | null>(null);
  const [pasteOpen, setPasteOpen] = useState(false);
  const [pasteText, setPasteText] = useState("");

  const rootRef = useRef<HTMLDivElement | null>(null);
  const canvasRef = useRef<HTMLCanvasElement | null>(null);
  // effect 内部状态（不经 React 渲染路径的高频对象）
  const sendCtrlRef = useRef<((obj: unknown) => void) | null>(null);
  const inputOnRef = useRef(false);
  const pendingMoveRef = useRef<{ x: number; y: number } | null>(null);
  const retryRef = useRef(0);
  // 烧尽 re-POST 已用轮数（跨 epoch 存活；手动“重新连接”清零）
  const repostRef = useRef(0);
  const prevStatsRef = useRef<WorkerStats | null>(null);
  const rttRef = useRef(0);
  // RTT/时钟偏差平滑（含 QoS 反馈与 e2e 校正，见 SMOOTH_WIN 注释）。
  // 窗口存 {rtt, off} 成对样本：偏差取最低延迟样本（NTP 惯例——最优
  // 样本的偏差估计最准，见 minOf 注释）。
  const rttWinRef = useRef<{ rtt: number; off: number | null }[]>([]);
  const clockOffsetRef = useRef<number | null>(null);
  // e2e 高频原始值（renderLoop 每帧算，1s 才同步到 state 免整页重渲染）
  const e2eRawRef = useRef<number | null>(null);
  // 剪贴板高频态（不经渲染路径）
  const remoteClipRef = useRef("");
  const clipAssemblerRef = useRef(new ClipAssembler());
  const pasteSeqRef = useRef(0);
  // 等待 set-ack 的粘贴序号（null = 无进行中的粘贴）
  const pendingPasteRef = useRef<number | null>(null);
  // 被 Ctrl+V 拦截的键（其 keyup 不再转发）
  const suppressKeyUpRef = useRef<Set<string>>(new Set());
  // inputAllowed 的 ref 镜像（F 全屏等 window 级常驻监听里读取）
  const inputAllowedRef = useRef(false);

  useEffect(() => {
    inputOnRef.current = inputOn;
  }, [inputOn]);
  // inputAllowed 的镜像（与下方 render 段同式）：F 全屏等 window 级常驻
  // 监听里读取，避免远程打字触发本地全屏切换
  useEffect(() => {
    inputAllowedRef.current =
      inputOn && control !== null && control.holder === selfSessionRef.current;
  });

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
      // 输入态下按键发往远端，F 不再切本地全屏（注入监听会 preventDefault
      // 但两个 window 监听都会执行，这里必须显式让路）
      if (inputAllowedRef.current) return;
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
        // 烧尽 re-POST：不直接 fatal——节流重建会话（re-POST /desktop 拿
        // 新 token/端点）再跑一轮完整重试循环；REPOST_ROUNDS 轮烧尽后
        // 才维持现有失败 UI。epoch 递增本身即触发 effect 重跑（re-POST）。
        if (repostRef.current >= REPOST_ROUNDS) {
          setFatalMsg("连接重试次数用尽；请检查网络后重试。");
          setPhase("fatal");
          return;
        }
        repostRef.current += 1;
        retryRef.current = 0;
        log(
          `重试烧尽，${REPOST_THROTTLE_MS / 1000}s 后重建会话（第 ${repostRef.current}/${REPOST_ROUNDS} 轮）`,
        );
        window.setTimeout(() => {
          if (!cancelled) setEpoch((e) => e + 1);
        }, REPOST_THROTTLE_MS);
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

    // 剪贴板消息（host→viewer 推送 / set-ack 回执）
    function handleClipboardMsg(v: Record<string, unknown>) {
      const ev = v.event;
      if (ev === "text-chunk") {
        const done = clipAssemblerRef.current.push({
          seq: Number(v.seq),
          index: Number(v.index),
          total: Number(v.total),
          text: typeof v.text === "string" ? v.text : "",
        });
        if (done) {
          remoteClipRef.current = done.text;
          setRemoteClipLen(done.text.length);
          setClipNotice(`远程剪贴板已更新（${done.text.length} 字符）`);
          // 机会性写本地剪贴板：无活跃手势时浏览器会拒（静默失败），通知
          // 条上的“复制到本地”按钮（点击=手势）是稳定路径
          navigator.clipboard?.writeText(done.text).catch(() => {});
          log(`远程剪贴板更新：${done.text.length} 字符`);
        }
      } else if (ev === "text-end") {
        if (v.truncated === true) {
          setClipNotice("远程剪贴板已更新（超 256KiB 已截断）");
        }
      } else if (ev === "set-ack") {
        const seq = Number(v.seq);
        if (pendingPasteRef.current === seq) {
          pendingPasteRef.current = null;
          if (v.ok === true) {
            // host 已写入远端剪贴板——补发完整 Ctrl+V 序列（物理 Ctrl down
            // 可能已随松键转发，成对补发幂等）
            const seq2 = [
              ["ControlLeft", "down"],
              ["KeyV", "down"],
              ["KeyV", "up"],
              ["ControlLeft", "up"],
            ] as const;
            for (const [code, kind] of seq2) {
              sendCtrl({ type: "input", event: "keyboard", kind, code });
            }
            log("粘贴完成（远端剪贴板已写入并注入 Ctrl+V）");
          } else {
            setClipNotice("远端剪贴板写入失败，粘贴未执行");
            log("clipboard set-ack: host write failed");
          }
        }
      }
    }

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
        case "clipboard":
          handleClipboardMsg(v);
          break;
        case "heartbeat": {
          if (typeof v.tMs !== "number") break;
          const rtt = Date.now() - v.tMs;
          // 负值/离谱大值 = 本机时钟被 NTP 回拨或样本损坏，丢弃
          if (rtt < 0 || rtt > 10_000) break;
          // 时钟偏差（NTP 中点法）：hostNow - (send+recv)/2，e2e 校正用
          const off =
            typeof v.hostNowMs === "number"
              ? v.hostNowMs - (v.tMs + rtt / 2)
              : null;
          const win = rttWinRef.current;
          win.push({ rtt, off });
          if (win.length > SMOOTH_WIN) win.shift();
          // 显示/QoS 反馈 = 窗口最低基线（双峰时中值会来回跳，见 minOf
          // 注释；基线也防尖峰误伤码率/FEC 调整）。
          rttRef.current = minOf(win.map((s) => s.rtt));
          setRttMs(rttRef.current);
          // 偏差取最低延迟样本（排队污染的样本其中点假设不成立）。
          let best: { rtt: number; off: number | null } | null = null;
          for (const s of win) if (!best || s.rtt < best.rtt) best = s;
          if (best && best.off !== null) clockOffsetRef.current = best.off;
          break;
        }
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
        setE2eMs(e2eRawRef.current); // 1s 同步，避免逐帧 setState 重渲染整页
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
        // e2e = 本地时刻 + 时钟偏差 - 采集时刻；偏差未收敛前不显示
        const off = clockOffsetRef.current;
        e2eRawRef.current =
          off === null
            ? null
            : Math.max(0, Math.round(Date.now() + off - m.captureUnixUs / 1000));
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

    const connectWT = async (
      url: string,
      certHash?: Uint8Array<ArrayBuffer>,
    ) => {
      // certHash 仅纯 IP 自签 relay 候选携带（serverCertificateHashes 钉
      // 扎）；缺省不带该选项 = 标准 Web PKI，与既有行为一致。url 的
      // https 前提由调用方（服务器 URL 形态）保证。
      const t = new WebTransport(
        url,
        certHash
          ? {
              serverCertificateHashes: [
                { algorithm: "sha-256", value: certHash },
              ],
            }
          : undefined,
      );
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
      const withToken = (u: string) =>
        `${u}${sep(u)}token=${encodeURIComponent(resp.token)}`;
      const finish = () => {
        setTransport(curTransport);
        startLoops();
        setPhase("waiting");
      };

      // relay 候选（relay-plane）：按候选序顺序 fallback（服务器保证 wt
      // 在前、ws 兜底在后），单候选失败即试下一个。?transport=ws 仍强制
      // 只取 ws 候选。字段缺失/无可用候选 → 回退下方 wtUrl/wsUrl 既有
      // 逻辑（零行为变化）。
      const cands = Array.isArray(resp.candidates) ? resp.candidates : [];
      const usable =
        wanted === "ws" ? cands.filter((c) => c.transport === "ws") : cands;
      if (usable.length > 0) {
        let lastErr: unknown = new Error("无可用候选");
        for (const c of usable) {
          // 候选为 host/port/path 形态（无 scheme）：wt→https / ws→wss
          const base = `${c.transport === "wt" ? "https" : "wss"}://${c.host}:${c.port}${c.path.startsWith("/") ? c.path : `/${c.path}`}`;
          // certSha256 存在但非法 → 候选不可信，跳过（不静默降级为无钉扎）
          const pin = c.certSha256 ? hexTo32Bytes(c.certSha256) : null;
          if (c.certSha256 && !pin) {
            log(`候选 ${c.host}:${c.port} 的 certSha256 非法，跳过`);
            lastErr = new Error("bad certSha256");
            continue;
          }
          try {
            if (c.transport === "wt") {
              curTransport = "wt";
              await connectWT(withToken(base), pin ?? undefined);
            } else {
              curTransport = "ws";
              await connectWS(withToken(base));
              startWorker(true);
            }
            setRelayInfo(
              c.relayId === "rl-0"
                ? `主站内嵌（${curTransport.toUpperCase()}）`
                : `${base}${c.region ? ` · ${c.region}` : ""}（${curTransport.toUpperCase()}）`,
            );
            finish();
            return;
          } catch (e) {
            lastErr = e;
            log(
              `${c.transport.toUpperCase()} 候选 ${c.host}:${c.port} 失败（${e}），尝试下一候选`,
            );
          }
        }
        throw lastErr;
      }

      const wtUrl = withToken(resp.wtUrl);
      const wsUrl = withToken(resp.wsUrl);
      // 旧响应形态（无 candidates）：从 URL 提取 origin+path 展示（剥
      // query——票据不得进统计面板）
      const legacyUrl = (u: string) => {
        try {
          const p = new URL(u);
          return p.origin + p.pathname;
        } catch {
          return "";
        }
      };
      if (wanted === "ws") {
        curTransport = "ws";
        await connectWS(wsUrl);
        startWorker(true);
        setRelayInfo(`${legacyUrl(wsUrl) || "主站内嵌"}（WS）`);
      } else {
        curTransport = "wt";
        try {
          await connectWT(wtUrl);
          setRelayInfo(`${legacyUrl(wtUrl) || "主站内嵌"}（WT）`);
        } catch (e) {
          log(`WebTransport 失败（${e}），回退 WebSocket`);
          curTransport = "ws";
          await connectWS(wsUrl);
          startWorker(true);
          setRelayInfo(`${legacyUrl(wsUrl) || "主站内嵌"}（WS）`);
        }
      }
      finish();
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

  // 键盘注入：window 级原生监听（wheel 同款模式）。输入态全量
  // preventDefault（F5/Tab/空格不再撞本地；Ctrl+W/T、Alt+Tab 等浏览器/
  // 系统保留键拦不住——平台固有限制）。IME 组合中的键跳过，中文文本走
  // “粘贴到远程”对话框（剪贴板通道）。
  useEffect(() => {
    if (!inputAllowed) return;
    // ref 绑定局部（cleanup 执行时 ref 可能已被替换——lint 要求）
    const suppress = suppressKeyUpRef.current;
    const editable = (e: KeyboardEvent) => {
      const t = e.target as HTMLElement | null;
      return t !== null && (t.tagName === "INPUT" || t.tagName === "TEXTAREA");
    };
    const onDown = (e: KeyboardEvent) => {
      if (editable(e)) return; // 手动粘贴对话框内的输入不注入
      e.preventDefault();
      if (e.isComposing || e.key === "Process") return;
      // Ctrl/Cmd+V：拦下物理键——读本地剪贴板 → 分块写远端 → set-ack 后
      // 补发合成序列（见 handleClipboardMsg）
      if (isPasteCombo(e)) {
        suppress.add(e.code);
        startPaste();
        return;
      }
      const c = classifyKeyDown(e);
      if (!c) return;
      if (c.kind === "text") {
        sendCtrlRef.current?.({
          type: "input",
          event: "keyboard",
          kind: "text",
          text: c.text,
        });
      } else {
        sendCtrlRef.current?.({
          type: "input",
          event: "keyboard",
          kind: "down",
          code: c.code,
        });
      }
    };
    const onUp = (e: KeyboardEvent) => {
      if (editable(e)) return;
      e.preventDefault();
      if (e.isComposing) return;
      if (suppress.delete(e.code)) return; // 被拦截的 Ctrl+V
      if (isTextPathKey(e)) return; // 文本路径不发 down/up
      sendCtrlRef.current?.({
        type: "input",
        event: "keyboard",
        kind: "up",
        code: e.code,
      });
    };
    window.addEventListener("keydown", onDown);
    window.addEventListener("keyup", onUp);
    return () => {
      window.removeEventListener("keydown", onDown);
      window.removeEventListener("keyup", onUp);
      suppress.clear();
    };
  }, [inputAllowed, phase]);

  // Ctrl+V 粘贴：读本地剪贴板 → 分块发 host（relay 租约门控内）→ 等
  // set-ack。读剪贴板需用户激活（keydown 即手势）；被拒（Firefox 逐次
  // 授权等）时引导手动对话框。
  const startPaste = () => {
    const clip = navigator.clipboard;
    if (!clip?.readText) {
      setClipNotice("浏览器不支持剪贴板读取——用工具栏“粘贴”手动输入");
      setPasteOpen(true);
      return;
    }
    clip
      .readText()
      .then((text) => {
        sendPasteText(text);
      })
      .catch(() => {
        setClipNotice("读取本地剪贴板被拒绝——用工具栏“粘贴”手动输入");
        setPasteOpen(true);
      });
  };

  const sendPasteText = (raw: string) => {
    const { text } = truncateUtf8(raw);
    if (!text) return;
    const seq = ++pasteSeqRef.current;
    pendingPasteRef.current = seq;
    for (const c of makeSetChunks(text, seq)) {
      sendCtrlRef.current?.({
        type: "input",
        event: "clipboard",
        kind: "set-text",
        ...c,
      });
    }
    window.setTimeout(() => {
      if (pendingPasteRef.current === seq) {
        pendingPasteRef.current = null;
        setClipNotice("粘贴未获远端确认（3 秒超时）");
      }
    }, 3000);
  };

  // 远端剪贴板 → 本地（点击=手势，writeText 稳定授权）
  const copyRemote = () => {
    navigator.clipboard
      ?.writeText(remoteClipRef.current)
      .then(() => setClipNotice("已复制到本地剪贴板"))
      .catch(() => setClipNotice("复制被浏览器拒绝"));
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
    repostRef.current = 0; // 手动重试从零开始完整重试预算
    setFatalMsg("");
    setPhase("connecting");
    setRelayInfo(null);
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
          <span
            className="dt-chip dt-chip-num"
            title="控制环往返（心跳测得，窗口最低基线——突发排队不抬高显示）"
          >
            rtt {rttMs === null ? "–" : `${Math.round(rttMs)}ms`}
          </span>
          {phase === "live" && (
            <span className="dt-chip dt-chip-num" title="解码帧率（每秒差分）">
              {rates.fps} fps
            </span>
          )}
          <span
            className="dt-chip dt-chip-num"
            title="采集→渲染端到端（已按心跳估出的双端时钟偏差校正）"
          >
            e2e≈{e2eMs === null ? "–" : `${e2eMs}ms`}
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
            disabled={!inputAllowed}
            onClick={() => setPasteOpen(true)}
            title={
              inputAllowed
                ? "粘贴文本到远程剪贴板（中文/IME 输入的正规路径）"
                : "需先接管控制"
            }
          >
            粘贴
          </button>
          <button
            className="btn"
            disabled={remoteClipLen === 0}
            onClick={copyRemote}
            title="把远端剪贴板文本复制到本地"
          >
            复制
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
            className={phase === "live" ? "dt-canvas dt-live" : "dt-canvas"}
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
          {pasteOpen && (
            <div className="dt-overlay">
              <div className="dt-pastebox">
                <div>粘贴/输入文本发送到远端剪贴板（中文请在此输入）</div>
                <textarea
                  value={pasteText}
                  onChange={(e) => setPasteText(e.target.value)}
                  rows={6}
                  autoFocus
                  placeholder="在此粘贴或输入…"
                />
                <div className="dt-pastebox-actions">
                  <button className="btn" onClick={() => setPasteOpen(false)}>
                    取消
                  </button>
                  <button
                    className="btn"
                    disabled={!inputAllowed || !pasteText}
                    title={inputAllowed ? undefined : "需先接管控制"}
                    onClick={() => {
                      sendPasteText(pasteText);
                      setPasteOpen(false);
                      setPasteText("");
                    }}
                  >
                    发送并粘贴
                  </button>
                </div>
              </div>
            </div>
          )}
        </div>

        <aside className={`dt-stats${statsOpen ? " open" : ""}`}>
          <div className="dt-stats-inner">
            <div className="dt-stats-title">实时统计</div>
            <div className="dt-m">
              <div className="k">中继</div>
              <div className="v" title={relayInfo ?? undefined}>
                {relayInfo ?? "–"}
              </div>
            </div>
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

      {clipNotice && (
        <div className="dt-clipnote">
          <span>{clipNotice}</span>
          {remoteClipLen > 0 && (
            <button className="btn" onClick={copyRemote}>
              复制到本地
            </button>
          )}
          <button className="btn" onClick={() => setClipNotice(null)}>
            ×
          </button>
        </div>
      )}
      {inputOn && (
        <footer className="dt-hint">
          {inputAllowed
            ? "键鼠控制已开启：十字准星即指针位置（本地渲染）；Ctrl+V 粘贴本地剪贴板"
            : "正在接管控制权…（他人持有时为抢占）"}
        </footer>
      )}
    </div>
  );
}
