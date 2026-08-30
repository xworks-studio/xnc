import { useCallback, useEffect, useRef, useState } from "react";
import { useParams } from "react-router-dom";
import type {
  CSSProperties,
  KeyboardEvent as ReactKeyboardEvent,
  MouseEvent as ReactMouseEvent,
  PointerEvent as ReactPointerEvent,
  WheelEvent as ReactWheelEvent,
} from "react";
import { api } from "../api";
import type { NodeDTO } from "../types";
import { FrameCorrelator, decodeFrameMeta } from "../lib/desktopFrameMeta";
import { lookupScan } from "./desktop/keymap";
import { cursorDotStyle, decodeCursor, streamMapping } from "./desktop/cursor";
import {
  LEASE_NOTICES,
  SAS_NOTICES,
  STATE_NOTICES,
} from "./desktop/signaling";
import type {
  DisplayEntry,
  SignalingFrame,
  StreamDims,
} from "./desktop/signaling";

/**
 * Experimental desktop-live page (M1-Slice2): WebRTC H.264 via TURN relay.
 * Not in the sidebar — reach it directly at /desktop/<nodeId>.
 *
 * 1. POST /api/nodes/{id}/desktop {} → 202 {websocketUrl, turn}
 *    (auth = the usual localStorage bearer via api(); login first).
 * 2. Session WS carries the desktop signaling vocabulary (agent/desktop/
 *    session.go): agent → {ready, answer, ice, state, error,
 *    lease_granted/denied/revoked}, viewer → {offer, ice, lease_request,
 *    keyframe-req}. Text frames only.
 * 3. After "ready": RTCPeerConnection(iceTransportPolicy:"relay",
 *    iceServers:[turn]) + recvonly video transceiver → offer → answer →
 *    trickle both ways. Track lands in <video autoplay muted playsinline>.
 *
 * Slice3 additions (input/lease/cursor, agent/desktop/input.go contract):
 * - Agent pre-creates three DataChannels; the viewer gets them via
 *   ondatachannel: "input" (reliable, KEY/TEXT/LOCK/WHEEL), "mouse"
 *   (unreliable, MOVE) and "cursor" (unreliable, dot position).
 * - One monotonic seq counter shared by input+mouse, assigned in send
 *   order (the agent drops non-increasing seq as stale).
 * - Coordinates are stream logical pixels (the "ready" frame width/
 *   height); the video letterbox maps them to/from element pixels.
 * - All input sends are gated on holding the input lease; the PLI button
 *   stays signaling-side (browsers expose no RTCP PLI from JS).
 *
 * The page MUST be opened against the LAN server origin (dev:
 * http://192.168.1.12:18080): the server derives the agent-side session
 * dial URL from the request Host header.
 */
interface DesktopStartResponse {
  sessionId: string;
  websocketUrl: string;
  turn?: { urls: string[]; username: string; credential: string };
  iceTransportPolicy?: string;
}

type LiveState =
  | "opening"
  | "signaling"
  | "streaming"
  | "error"
  | "disconnected";

const STATE_LABELS: Record<LiveState, string> = {
  opening: "opening",
  signaling: "signaling",
  streaming: "streaming",
  error: "error",
  disconnected: "disconnected",
};

type LeaseState = {
  status: "none" | "requested" | "granted" | "denied" | "revoked";
  reason?: string;
  id?: string;
};

/** Relative websocketUrl → absolute against the current origin (as
 * ScreenPreview does; ws in dev via the Vite proxy, wss behind https). */
function toWsUrl(url: string): string {
  if (url.startsWith("ws://") || url.startsWith("wss://")) return url;
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${window.location.host}${url}`;
}

/** requestVideoFrameCallback feature probe (Chrome/Edge have it; the
 * Firefox fallback polls getStats().framesDecoded).
 * M3 Task 5: Chrome's rVFC metadata also carries the RTP correlation
 * fields — rtpTimestamp links a presented frame to its frame-meta
 * record; capture/receive/processing expose the pipeline latency. All
 * optional (Firefox/older Chrome omit them). */
interface VideoFrameCallbackMetadataLike {
  mediaTime: number;
  rtpTimestamp?: number;
  captureTime?: number;
  receiveTime?: number;
  processingDuration?: number;
}
type Rvfc = (
  cb: (now: number, meta: VideoFrameCallbackMetadataLike) => void,
) => number;
type VideoElementWithRvfc = HTMLVideoElement & { requestVideoFrameCallback?: Rvfc };

/** M3 Task 5: viewer_feedback uplink frame (agent events.go
 * onViewerFeedback parses exactly type/visible/estimatedBps/queueMs/
 * decodeQueue/rttMs — TestViewerFeedbackJSONShape pins the shape; the
 * extra diagnostic fields below are ignored by the agent but ride along
 * for the e2e tooling). Session routing needs NO sessionID field here:
 * the agent fills ViewerFeedback.SessionID from the per-session WS
 * connection itself (the dial URL carries the session id). */
interface ViewerFeedbackFrame {
  type: string;
  visible: boolean;
  /** Downlink available-bandwidth estimate (bps): transport-cc
   * availableIncomingBitrate preferred; goodput fallback (C1). */
  estimatedBps: number;
  /** Avg jitter-buffer delay per emitted frame this window (ms). */
  queueMs: number;
  /** Decoded-but-not-presented surplus this window (frames). */
  decodeQueue: number;
  /** Candidate-pair RTT (ms). */
  rttMs: number;
  framesDecoded?: number;
  framesDropped?: number;
  freezeCount?: number;
  freezes?: number;
  presentedFps?: number;
}

/** Module-scoped live-WebSocket handle so the PLI/lease buttons can reach
 * the effect-owned socket (set right after connect, cleared on teardown). */
const wsHandle: { __xncDesktopWs?: WebSocket } = {};

/** Input-channel message type values (0x0108 mirror; MOVE=1 is
 * mouse-channel-only and never sent here; BUTTON=2 is redundant for the
 * viewer — the MOVE buttons mask drives down/up transitions natively). */
const MSG_WHEEL = 3;
const MSG_KEY = 4;
const MSG_TEXT = 5;
const MSG_LOCK = 6;

/** Agent caps TEXT at 512 UTF-16 units (input.go maxInputTextUnits). */
const MAX_TEXT_UNITS = 512;
/** Pixel-mode wheel deltas accumulate into notches at ~100px/notch
 * (Chrome's per-notch deltaY granularity; LINE mode is 1 notch/line). */
const WHEEL_PX_PER_NOTCH = 100;

/** Mutable input/lease state shared by the stable event handlers and the
 * session effect (never recreated mid-session; reset on teardown). */
interface DesktopIo {
  /** Single monotonic seq across input+mouse, in send order. */
  seq: number;
  lease: boolean;
  /** Last locally observed lock toggles (synced on grant + on change). */
  capsLock?: boolean;
  numLock?: boolean;
  input?: RTCDataChannel;
  mouse?: RTCDataChannel;
  /** Fractional wheel notch accumulators (send whole notches only). */
  wheelX: number;
  wheelY: number;
}
const freshIo = (): DesktopIo => ({ seq: 0, lease: false, wheelX: 0, wheelY: 0 });

/** Secure-attention (Ctrl+Alt+Del) button state (M2-Slice1 Task 5). */
type SasState = { pending: boolean; denied: boolean };

export default function DesktopLive() {
  const { nodeId } = useParams<{ nodeId: string }>();
  const videoRef = useRef<HTMLVideoElement>(null);
  const [state, setState] = useState<LiveState>("opening");
  const [iceState, setIceState] = useState<string>("new");
  const [dims, setDims] = useState<string | null>(null);
  const [nodeName, setNodeName] = useState<string | null>(null);
  const [startError, setStartError] = useState<string | null>(null);
  const [agentState, setAgentState] = useState<string | null>(null);
  /**
   * 控制权被占(lease denied:held)时的自动重试:旧会话的 lease 在 server 侧
   * 60s TTL 内不会释放,用户手动刷新只会再撞窗口。denied 后按 8s 退避
   * 重建会话(重新 POST /desktop → 新 session 有机会拿到 lease),最多 8 次。
   * 2026-08-24 生产事故:用户反复刷新被"一直被占用"卡死。
   */
  const [sessionEpoch, setSessionEpoch] = useState(0);
  const leaseRetry = useRef({ n: 0, timer: undefined as number | undefined });
  /** M2-S3 Task 5: displays from the ready frame + selected index. */
  const [displays, setDisplays] = useState<DisplayEntry[]>([]);
  const [displaySel, setDisplaySel] = useState(0);
  const [stats, setStats] = useState({
    fps: 0,
    firstFrameMs: 0,
    framesDecoded: 0,
    keyframesDecoded: 0,
    plis: 0,
  });

  // 逐帧诊断模式(?framediag=1):rVFC 每呈现一帧记录 {帧号, mediaTime,
  // 呈现间隔},getStats 全量指标每 2s 快照——验证播放顺序单调(无回退帧)、
  // 间隔稳定、无解码丢弃。默认关闭,诊断时开启,数据渲染到 DOM 供抓取。
  const framediag = new URLSearchParams(window.location.search).has("framediag");
  const [diag, setDiag] = useState<{ frames: string[]; stats: string[] } | null>(null);
  const diagRef = useRef<{ frames: string[]; stats: string[]; lastPresent: number }>({
    frames: [],
    stats: [],
    lastPresent: 0,
  });

  // Slice3: lease / cursor dot / stream dims / text injection.
  const [lease, setLease] = useState<LeaseState>({ status: "none" });
  const [notice, setNotice] = useState<string | null>(null);
  /** Ctrl+Alt+Del button state: pending while a round-trip is in flight,
   * denied (permanently disabled for this session) after SAS_DENIED. */
  const [sas, setSas] = useState<SasState>({ pending: false, denied: false });
  /** 20s self-clear timer for the SAS pending latch (one live at a time). */
  const sasTimer = useRef<number | undefined>(undefined);
  /** Dot style computed in the cursor-channel handler (event context, not
   * render) — refs must not be read during render. */
  const [cursorDot, setCursorDot] = useState<CSSProperties | null>(null);
  const [text, setText] = useState("");
  const ioRef = useRef<DesktopIo>(freshIo());
  const dimsRef = useRef<StreamDims | null>(null);
  const iceModeRef = useRef<string>("relay");
  const composingRef = useRef(false);

  // State mirrors ioRef.lease (set alongside it in every transition), so
  // renders stay ref-free while handlers gate sends on the ref.
  const leaseHeld = lease.status === "granted";

  // Transient lease notices (denied/revoked) clear themselves.
  useEffect(() => {
    if (!notice) return;
    const t = window.setTimeout(() => setNotice(null), 6000);
    return () => window.clearTimeout(t);
  }, [notice]);

  // Status bar node name (best-effort; falls back to the node id).
  useEffect(() => {
    if (!nodeId) return;
    let alive = true;
    api<NodeDTO>(`/api/nodes/${nodeId}`)
      .then((n) => alive && setNodeName(n.name))
      .catch(() => {
        /* status bar falls back to the node id */
      });
    return () => {
      alive = false;
    };
  }, [nodeId]);

  // ---- input senders (stable: refs only; seq assigned in send order) ----

  const mouseSend = useCallback((buf: ArrayBuffer) => {
    const dc = ioRef.current.mouse;
    if (dc && dc.readyState === "open") {
      try {
        dc.send(buf);
      } catch {
        /* channel died mid-send */
      }
    }
  }, []);

  const inputSend = useCallback((buf: ArrayBuffer) => {
    const dc = ioRef.current.input;
    if (dc && dc.readyState === "open") {
      try {
        dc.send(buf);
      } catch {
        /* channel died mid-send */
      }
    }
  }, []);

  /** MOVE on the mouse channel: [u64 seq][s32 x][s32 y][u16 buttons]. */
  const sendMove = useCallback(
    (x: number, y: number, buttons: number) => {
      const io = ioRef.current;
      if (!io.lease) return;
      const b = new DataView(new ArrayBuffer(18));
      b.setBigUint64(0, BigInt(++io.seq), true);
      b.setInt32(8, x, true);
      b.setInt32(12, y, true);
      b.setUint16(16, buttons & 0x1f, true);
      mouseSend(b.buffer);
    },
    [mouseSend],
  );

  /** KEY: [u64 seq][u8 4][u16 scan][u8 down][u8 extended]. */
  const sendKey = useCallback(
    (scan: number, ext: boolean, down: boolean) => {
      const io = ioRef.current;
      if (!io.lease) return;
      const b = new DataView(new ArrayBuffer(13));
      b.setBigUint64(0, BigInt(++io.seq), true);
      b.setUint8(8, MSG_KEY);
      b.setUint16(9, scan, true);
      b.setUint8(11, down ? 1 : 0);
      b.setUint8(12, ext ? 1 : 0);
      inputSend(b.buffer);
    },
    [inputSend],
  );

  /** WHEEL: [u64 seq][u8 3][s32 dx][s32 dy][u8 trackpad=0] (notch units). */
  const sendWheel = useCallback(
    (dx: number, dy: number) => {
      const io = ioRef.current;
      if (!io.lease) return;
      const b = new DataView(new ArrayBuffer(18));
      b.setBigUint64(0, BigInt(++io.seq), true);
      b.setUint8(8, MSG_WHEEL);
      b.setInt32(9, dx, true);
      b.setInt32(13, dy, true);
      b.setUint8(17, 0); // notch mode: native scales by WHEEL_DELTA
      inputSend(b.buffer);
    },
    [inputSend],
  );

  /** TEXT: [u64 seq][u8 5][u16 len][utf16le units] (whole string, ≤512
   * units; surrogate pairs ride as units). Returns false when unsent. */
  const sendText = useCallback(
    (s: string): boolean => {
      const io = ioRef.current;
      if (!io.lease || !s) return false;
      const units = s.length > MAX_TEXT_UNITS ? s.slice(0, MAX_TEXT_UNITS) : s;
      const b = new DataView(new ArrayBuffer(11 + 2 * units.length));
      b.setBigUint64(0, BigInt(++io.seq), true);
      b.setUint8(8, MSG_TEXT);
      b.setUint16(9, units.length, true);
      for (let i = 0; i < units.length; i++) {
        b.setUint16(11 + 2 * i, units.charCodeAt(i), true);
      }
      inputSend(b.buffer);
      return true;
    },
    [inputSend],
  );

  /** LOCK: [u64 seq][u8 6][u8 caps][u8 num] — native injects the toggle
   * key when the remote GetKeyState disagrees. */
  const sendLock = useCallback(
    (caps: boolean, num: boolean) => {
      const io = ioRef.current;
      if (!io.lease) return;
      const b = new DataView(new ArrayBuffer(11));
      b.setBigUint64(0, BigInt(++io.seq), true);
      b.setUint8(8, MSG_LOCK);
      b.setUint8(9, caps ? 1 : 0);
      b.setUint8(10, num ? 1 : 0);
      inputSend(b.buffer);
    },
    [inputSend],
  );

  // ---- local-event capture ----

  /** Video rect (object-fit:contain) → stream logical px, clamped. */
  const pointerToStream = (e: ReactPointerEvent): { x: number; y: number } | null => {
    const video = videoRef.current;
    if (!video) return null;
    const w = video.videoWidth || dimsRef.current?.w || 0;
    const h = video.videoHeight || dimsRef.current?.h || 0;
    if (!w || !h) return null;
    const rect = video.getBoundingClientRect();
    if (rect.width <= 0 || rect.height <= 0) return null;
    const scale = Math.min(rect.width / w, rect.height / h);
    const px = e.clientX - rect.left - (rect.width - w * scale) / 2;
    const py = e.clientY - rect.top - (rect.height - h * scale) / 2;
    const clamp = (v: number, max: number) =>
      Math.max(0, Math.min(max, Math.round(v / scale)));
    return { x: clamp(px, w), y: clamp(py, h) };
  };

  const emitMove = (e: ReactPointerEvent) => {
    const p = pointerToStream(e);
    if (p) sendMove(p.x, p.y, e.buttons & 0x1f);
  };

  const onPointerDown = (e: ReactPointerEvent<HTMLDivElement>) => {
    if (!ioRef.current.lease) return;
    e.preventDefault();
    // Capture so up/leave events outside the video still release buttons.
    try {
      e.currentTarget.setPointerCapture(e.pointerId);
    } catch {
      /* capture is best-effort */
    }
    emitMove(e);
  };

  const onPointerMove = (e: ReactPointerEvent<HTMLDivElement>) => {
    if (!ioRef.current.lease) return;
    emitMove(e);
  };

  const onPointerUp = (e: ReactPointerEvent<HTMLDivElement>) => {
    if (!ioRef.current.lease) return;
    emitMove(e); // buttons mask no longer has the released bit
  };

  const onPointerCancel = (e: ReactPointerEvent<HTMLDivElement>) => {
    // Browser ate the interaction (touch palm, pen, alt-tab…): event.buttons
    // is 0 here and no up will ever come — send an explicit all-buttons-up
    // MOVE so the remote side does not keep the press held down.
    if (!ioRef.current.lease) return;
    const p = pointerToStream(e);
    if (p) sendMove(p.x, p.y, 0);
  };

  const onContextMenu = (e: ReactMouseEvent) => {
    // Right-click belongs to the remote desktop while the lease is held.
    if (ioRef.current.lease) e.preventDefault();
  };

  /** Notch accumulation: LINE deltas are whole notches, PAGE ≈ 3 notches,
   * PIXEL deltas accumulate at ~100px/notch; only integer notches go out. */
  const onWheel = (e: ReactWheelEvent) => {
    const io = ioRef.current;
    if (!io.lease) return;
    const unit =
      e.deltaMode === WheelEvent.DOM_DELTA_LINE
        ? 1
        : e.deltaMode === WheelEvent.DOM_DELTA_PAGE
          ? 3
          : 1 / WHEEL_PX_PER_NOTCH;
    io.wheelX += e.deltaX * unit;
    io.wheelY += e.deltaY * unit;
    const dx = Math.trunc(io.wheelX);
    const dy = Math.trunc(io.wheelY);
    if (dx !== 0 || dy !== 0) {
      io.wheelX -= dx;
      io.wheelY -= dy;
      sendWheel(dx, dy);
    }
  };

  /** Window key capture: repeat suppression, editable-target guard, and
   * CapsLock/NumLock sync (LOCK on toggle while holding the lease). */
  useEffect(() => {
    const isEditable = (t: EventTarget | null) =>
      t instanceof HTMLElement &&
      (t.tagName === "INPUT" ||
        t.tagName === "TEXTAREA" ||
        t.tagName === "SELECT" ||
        t.isContentEditable);

    const lockCheck = (e: KeyboardEvent) => {
      const io = ioRef.current;
      const caps = e.getModifierState("CapsLock");
      const num = e.getModifierState("NumLock");
      if (io.capsLock === caps && io.numLock === num) return;
      io.capsLock = caps;
      io.numLock = num;
      if (io.lease) sendLock(caps, num);
    };

    const onKey = (e: KeyboardEvent, down: boolean) => {
      lockCheck(e); // keep learning lock state even without the lease
      if (!ioRef.current.lease || isEditable(e.target)) return;
      const hit = lookupScan(e.code);
      if (!hit) return; // unknown code: ignored (+ console.debug)
      e.preventDefault(); // page must not react while injecting remotely
      if (down && e.repeat) return; // OS auto-repeat suppressed
      sendKey(hit.scan, hit.ext, down);
    };

    const kd = (e: KeyboardEvent) => onKey(e, true);
    const ku = (e: KeyboardEvent) => onKey(e, false);
    window.addEventListener("keydown", kd);
    window.addEventListener("keyup", ku);
    return () => {
      window.removeEventListener("keydown", kd);
      window.removeEventListener("keyup", ku);
    };
  }, [sendKey, sendLock]);

  // ---- text injection (IME-safe: composition Enter never sends) ----

  const sendTextNow = () => {
    if (sendText(text)) setText("");
  };

  const onTextKeyDown = (e: ReactKeyboardEvent<HTMLInputElement>) => {
    if (e.key !== "Enter") return;
    if (composingRef.current || e.nativeEvent.isComposing) return;
    e.preventDefault();
    sendTextNow();
  };

  const requestLease = () => {
    try {
      wsHandle.__xncDesktopWs?.send(JSON.stringify({ type: "lease_request" }));
      setLease({ status: "requested" });
    } catch {
      /* session dead */
    }
  };

  useEffect(() => {
    if (!nodeId) return;
    let disposed = false;
    let ws: WebSocket | null = null;
    let pc: RTCPeerConnection | null = null;
    let statsTimer = 0;
    let feedbackTimer = 0;
    const onVisibilityChange = () => {
      // One visible:false report on the hide transition (cheap: the
      // periodic loop below skips hidden tabs, so without this the agent
      // would only learn via stale-viewer pruning). The agent hides the
      // viewer from QoS decisions but never pauses on it.
      if (!disposed && document.visibilityState === "hidden") void sendFeedback(false);
    };

    // M3 Task 5: frame-meta correlation — the "frame-meta" DataChannel
    // feeds onMeta, each rVFC-presented frame calls onPresented, and the
    // 1s feedback report reads the snapshot. Effect-owned (dies with the
    // session; bounded internals, no teardown state of its own).
    const correlator = new FrameCorrelator();

    // Frame accounting shared by the rvfc loop and the stats poller.
    let frameCount = 0;
    let windowStart = performance.now();
    let framesAtWindowStart = 0;
    const startedAt = performance.now();
    const send = (frame: SignalingFrame) => {
      try {
        ws?.send(JSON.stringify(frame));
      } catch {
        /* session already dead */
      }
    };
    const applyDims = (w: number, h: number) => {
      dimsRef.current = { w, h };
    };

    /** Poll getStats: fps fallback (Firefox has no rvfc) + decoder
     * counters (keyframesDecoded is Chrome/Edge). Returns the decoded
     * frame count (0 while no media) so the caller can flip "streaming". */
    const pollStats = async (): Promise<number> => {
      if (!pc) return 0;
      let decoded = 0;
      let keys = 0;
      const di = diagRef.current;
      try {
        const report = await pc.getStats();
        report.forEach((s) => {
          if (s.type === "inbound-rtp" && s.kind === "video") {
            const r = s as RTCInboundRtpStreamStats & {
              framesDecoded?: number;
              keyFramesDecoded?: number;
              framesReceived?: number;
              framesDropped?: number;
              framesPerSecond?: number;
              jitter?: number;
              packetsLost?: number;
              nackCount?: number;
              pliCount?: number;
            };
            decoded = r.framesDecoded ?? 0;
            keys = r.keyFramesDecoded ?? 0;
            if (framediag && di) {
              di.stats = [
                `rx=${r.framesReceived ?? "?"}`,
                `dec=${r.framesDecoded ?? "?"}`,
                `drop=${r.framesDropped ?? "?"}`,
                `fps=${r.framesPerSecond ?? "?"}`,
                `jit=${((r.jitter ?? 0) * 1000).toFixed(1)}ms`,
                `lost=${r.packetsLost ?? "?"}`,
                `nack=${r.nackCount ?? "?"}`,
                `pli=${r.pliCount ?? "?"}`,
                `key=${r.keyFramesDecoded ?? "?"}`,
              ];
            }
          }
        });
      } catch {
        /* pc closing */
      }
      if (framediag && di && di.stats.length > 0 && !diag) setDiag({ frames: di.frames, stats: di.stats });
      if (decoded > 0) {
        const now = performance.now();
        const elapsed = now - windowStart;
        if (elapsed >= 900) {
          const fps = Math.round(((decoded - framesAtWindowStart) / elapsed) * 1000);
          framesAtWindowStart = decoded;
          windowStart = now;
          setStats((s) => ({ ...s, fps, framesDecoded: decoded, keyframesDecoded: keys }));
        } else {
          setStats((s) => ({ ...s, framesDecoded: decoded, keyframesDecoded: keys }));
        }
      }
      return decoded;
    };

    // ---- M3 Task 5: 1s viewer_feedback uplink ----
    // Metric derivations (all downlink-side; documented per plan ruling 4):
    //  - estimatedBps: candidate-pair availableIncomingBitrate (the
    //    browser's transport-cc estimate of the path's available downlink
    //    bandwidth) when present and > 0 — the agent's target is 85% of
    //    AVAILABLE bandwidth (spec §14.2), and only this stat measures the
    //    path rather than our own sending. Goodput (inbound-rtp
    //    bytesReceived delta × 8 / elapsed) is a FALLBACK for when no
    //    estimate exists: goodput tracks the send rate (≤ encoder bitrate,
    //    further capped ~15% by pacing), so feeding it as "available
    //    bandwidth" made the controller's downshift test an identity and
    //    ratcheted the bitrate to the floor under sustained motion
    //    (final-review C1). With no estimate and no goodput the report is
    //    skipped — a 0 would read as deep congestion to the agent's QoS
    //    controller.
    //  - queueMs: (jitterBufferDelay delta / jitterBufferEmittedCount
    //    delta) × 1000 — average jitter-buffer sojourn per emitted
    //    frame over the window (both stats are cumulative seconds /
    //    frames, hence the deltas).
    //  - decodeQueue: max(0, framesDecoded delta − rVFC presented
    //    delta) — decoded-but-not-yet-presented surplus; Chrome exposes
    //    no direct decode-queue stat, so this is the proxy.
    //  - rttMs: selected candidate-pair currentRoundTripTime × 1000.
    type FeedbackSample = {
      at: number;
      bytes: number;
      framesDecoded: number;
      framesDropped: number;
      freezeCount: number;
      jitterDelay: number;
      jitterEmitted: number;
      presented: number;
      freezes: number;
    };
    let fbBase: FeedbackSample | null = null;
    const readFeedbackStats = async (): Promise<{
      sample: FeedbackSample | null;
      rttMs: number;
      availBps: number;
    }> => {
      if (!pc) return { sample: null, rttMs: 0, availBps: 0 };
      let sample: FeedbackSample | null = null;
      let rttMs = 0;
      let availBps = 0;
      try {
        const report = await pc.getStats();
        const corr = correlator.snapshot();
        report.forEach((st) => {
          if (st.type === "inbound-rtp" && st.kind === "video") {
            const r = st as RTCInboundRtpStreamStats & {
              bytesReceived?: number;
              framesDecoded?: number;
              framesDropped?: number;
              freezeCount?: number;
              jitterBufferDelay?: number;
              jitterBufferEmittedCount?: number;
            };
            sample = {
              at: performance.now(),
              bytes: r.bytesReceived ?? 0,
              framesDecoded: r.framesDecoded ?? 0,
              framesDropped: r.framesDropped ?? 0,
              freezeCount: r.freezeCount ?? 0,
              jitterDelay: r.jitterBufferDelay ?? 0,
              jitterEmitted: r.jitterBufferEmittedCount ?? 0,
              presented: corr.presented,
              freezes: corr.plausibleFreezes,
            };
          } else if (st.type === "candidate-pair") {
            const p = st as RTCIceCandidatePairStats;
            if (typeof p.currentRoundTripTime === "number") {
              // Prefer the nominated (in-use) pair; Chrome reports
              // several pairs, only the selected one is meaningful.
              if (p.nominated || rttMs === 0) rttMs = p.currentRoundTripTime * 1000;
            }
            if (typeof p.availableIncomingBitrate === "number" && p.availableIncomingBitrate > 0) {
              availBps = p.availableIncomingBitrate;
            }
          }
        });
      } catch {
        /* pc closing */
      }
      return { sample, rttMs, availBps };
    };
    const sendFeedback = async (visible: boolean) => {
      if (!pc || disposed) return;
      const { sample, rttMs, availBps } = await readFeedbackStats();
      if (!sample || disposed) return;
      if (sample.framesDecoded <= 0 || !fbBase) {
        fbBase = sample; // no media yet / first sample only primes deltas
        return;
      }
      const elapsedS = Math.max((sample.at - fbBase.at) / 1000, 0.001);
      const goodput = Math.round(((sample.bytes - fbBase.bytes) * 8) / elapsedS);
      // C1 (final review): the transport-cc available-bandwidth estimate is
      // primary; goodput (a shadow of our own send rate) only when absent.
      const estimatedBps = availBps > 0 ? Math.round(availBps) : goodput;
      const base = fbBase;
      fbBase = sample; // advance the window even when we skip below
      if (estimatedBps <= 0) return; // nothing measurable — 0 would look like congestion
      const emitted = sample.jitterEmitted - base.jitterEmitted;
      const queueMs = emitted > 0 ? ((sample.jitterDelay - base.jitterDelay) / emitted) * 1000 : 0;
      const decodedDelta = sample.framesDecoded - base.framesDecoded;
      const presentedDelta = sample.presented - base.presented;
      const fb: ViewerFeedbackFrame = {
        type: "viewer_feedback",
        visible,
        estimatedBps,
        queueMs: Math.round(queueMs * 10) / 10,
        decodeQueue: Math.max(0, decodedDelta - presentedDelta),
        rttMs: Math.round(rttMs * 10) / 10,
        framesDecoded: decodedDelta,
        framesDropped: sample.framesDropped - base.framesDropped,
        freezeCount: sample.freezeCount - base.freezeCount,
        freezes: sample.freezes - base.freezes,
        presentedFps: Math.round((presentedDelta / elapsedS) * 10) / 10,
      };
      send(fb);
    };

    /** rvfc loop: per-second fps + first-frame latency (preferred
     * source of truth in Chrome/Edge — counts presented frames). */
    const startFrameLoop = (video: HTMLVideoElement) => {
      const rvfc = (video as VideoElementWithRvfc).requestVideoFrameCallback;
      if (!rvfc) return; // Firefox: getStats fallback covers fps
      const tick = (_now: number, meta: VideoFrameCallbackMetadataLike) => {
        if (disposed) return;
        frameCount++;
        const first = frameCount === 1;
        const now = performance.now();
        // M3 Task 5: correlate this presented frame against the bounded
        // frame-meta map via Chrome's metadata.rtpTimestamp (when the
        // browser exposes it). A hit whose codecEpoch/contentId/
        // encodeSeq regressed vs the last presented hit is a
        // presentation-order anomaly: warn + count, never throw.
        const corr = correlator.onPresented(meta.rtpTimestamp, _now);
        if (corr.regressed) {
          console.warn(
            "[frame-meta] presented frame identity regressed",
            corr.meta && {
              rtpTimestamp: corr.meta.rtpTimestamp,
              codecEpoch: corr.meta.codecEpoch.toString(),
              contentId: corr.meta.contentId.toString(),
              encodeSeq: corr.meta.encodeSeq.toString(),
            },
          );
        }
        // 逐帧诊断:每呈现一帧记录 帧号:mediaTime(秒,3位):呈现间隔(ms)。
        // mediaTime 倒退 = 回退帧;间隔 0/巨大 = 重复/卡顿。M3 Task 5:
        // 命中 frame-meta 时追加身份列 :E<codecEpoch>:S<encodeSeq>。
        const di = diagRef.current;
        if (framediag && di) {
          const mt = typeof meta.mediaTime === "number" ? meta.mediaTime : NaN;
          const dt = di.lastPresent ? Math.round((_now - di.lastPresent) * 1000) / 1000 : 0;
          di.lastPresent = _now;
          const id = corr.meta ? `:E${corr.meta.codecEpoch}:S${corr.meta.encodeSeq}` : "";
          di.frames.push(`${frameCount}:${mt.toFixed(3)}:${dt.toFixed(1)}${id}`);
          if (di.frames.length > 240) di.frames.splice(0, di.frames.length - 240);
          // 每 ~1s 快照一次 DOM(避免每帧 setState 拖累渲染)。
          if (frameCount % 30 === 0) setDiag({ frames: di.frames.slice(-120), stats: di.stats });
        }
        if (first || now - windowStart >= 900) {
          const elapsed = now - windowStart;
          const fps =
            first || elapsed < 200 ? 0 : Math.round(((frameCount - framesAtWindowStart) / elapsed) * 1000);
          framesAtWindowStart = frameCount;
          windowStart = now;
          setStats((s) => ({
            ...s,
            fps,
            firstFrameMs: first ? Math.round(now - startedAt) : s.firstFrameMs,
          }));
        }
        if (meta.mediaTime !== undefined && first) {
          // dimensions become available with the first presented frame
          if (video.videoWidth) {
            setDims(`${video.videoWidth}x${video.videoHeight}`);
            applyDims(video.videoWidth, video.videoHeight);
          }
        }
        (video as VideoElementWithRvfc).requestVideoFrameCallback?.(tick);
      };
      rvfc(tick);
    };

    const teardown = () => {
      disposed = true;
      window.clearInterval(statsTimer);
      window.clearInterval(feedbackTimer);
      document.removeEventListener("visibilitychange", onVisibilityChange);
      try {
        pc?.close();
      } catch {
        /* already closed */
      }
      try {
        ws?.close();
      } catch {
        /* already closed */
      }
      // drop the WS handle only if this effect still owns it
      if (wsHandle.__xncDesktopWs === ws) wsHandle.__xncDesktopWs = undefined;
      // input state: session over → lease gone, seq counter resets, dot hidden
      ioRef.current = freshIo();
      setLease({ status: "none" });
      setCursorDot(null);
      setSas({ pending: false, denied: false });
      // teardown: cancel any pending lease-retry timer (the effect re-run
      // owns a fresh one); epoch bump is what re-arms it.
      if (leaseRetry.current.timer !== undefined) {
        window.clearTimeout(leaseRetry.current.timer);
        leaseRetry.current.timer = undefined;
      }
    };

    api<DesktopStartResponse>(`/api/nodes/${nodeId}/desktop`, {
      method: "POST",
      body: JSON.stringify({}),
    })
      .then((res) => {
        if (disposed) return;
        if (!res.turn || res.turn.urls.length === 0) {
          setState("error");
          setStartError("server returned no TURN config");
          return;
        }
        ws = new WebSocket(toWsUrl(res.websocketUrl));
        wsHandle.__xncDesktopWs = ws; // PLI/lease buttons send over the live socket
        ws.onmessage = (ev: MessageEvent) => {
          if (disposed || typeof ev.data !== "string") return;
          let f: SignalingFrame;
          try {
            f = JSON.parse(ev.data);
          } catch {
            return;
          }
          switch (f.type) {
            case "ready":
              setState("signaling");
              if (f.width && f.height) applyDims(f.width, f.height);
              if (f.displays?.length) {
                setDisplays(f.displays);
                const active =
                  f.displays.find(
                    (d) => d.primary || (d.w === f.width && d.h === f.height),
                  ) ?? f.displays[0];
                setDisplaySel(active.index);
              }
              // ICE policy: server-controlled (缺省 relay = 安全约束; server
              // config "all" 时下发 all,LAN 直连避免公网 TURN 丢包)。
              const icePolicy =
                res.iceTransportPolicy === "all" ? "all" : "relay";
              iceModeRef.current = icePolicy;
              pc = new RTCPeerConnection({
                iceServers: [
                  {
                    urls: res.turn!.urls,
                    username: res.turn!.username,
                    credential: res.turn!.credential,
                  },
                ],
                iceTransportPolicy: icePolicy as RTCIceTransportPolicy,
              });
              pc.onconnectionstatechange = () => setIceState(pc?.connectionState ?? "new");
              // Agent pre-creates input/mouse/cursor; in-band negotiation
              // delivers them here once the answer lands.
              pc.ondatachannel = (e) => {
                const ch = e.channel;
                if (ch.label === "input") ioRef.current.input = ch;
                else if (ch.label === "mouse") ioRef.current.mouse = ch;
                else if (ch.label === "cursor") {
                  ch.binaryType = "arraybuffer";
                  ch.onmessage = (m: MessageEvent) => {
                    if (disposed || !(m.data instanceof ArrayBuffer)) return;
                    const upd = decodeCursor(m.data);
                    if (!upd) return;
                    const map = streamMapping(videoRef.current, dimsRef.current);
                    setCursorDot(map && upd.visible ? cursorDotStyle(map, upd.x, upd.y) : null);
                  };
                } else if (ch.label === "frame-meta") {
                  // M3 Task 5: per-frame telemetry (agent M3 Task 4 —
                  // unordered, no retransmit, so records may drop).
                  // 56-byte FrameMetaV1 records keyed by rtpTimestamp
                  // for the rVFC correlation; decode failures (wrong
                  // version/length/garbage) are counted inside the
                  // correlator and NEVER thrown from this handler.
                  ch.binaryType = "arraybuffer";
                  ch.onmessage = (m: MessageEvent) => {
                    if (disposed || !(m.data instanceof ArrayBuffer)) return;
                    correlator.onMeta(decodeFrameMeta(m.data));
                  };
                }
              };
              pc.ontrack = (e) => {
                const video = videoRef.current;
                if (!video) return;
                video.srcObject = e.streams[0] ?? new MediaStream([e.track]);
                // 远控延迟/平滑权衡:playoutDelayHint 是 Chrome 的缓冲目标。
                // 中继路径上场景切换 IDR(~1/s,~120KB@1080p)在 4.9Mbps TURN
                // 上要 ~200ms 才传完;hint=0 时迟到的帧被 Chrome 直接丢弃
                // (不发 PLI)→ 每秒一次画面冻结跳变(跳帧)。0.2s 完全吸收
                // IDR 突发,延迟代价 ~200ms(相对旧 10s 体感可忽略)。
                try {
                  (e.receiver as unknown as { playoutDelayHint?: number }).playoutDelayHint = 0.2;
                } catch {
                  /* 旧浏览器不支持,忽略 */
                }
                startFrameLoop(video);
              };
              pc.onicecandidate = (e) => {
                // null = end-of-candidates; the agent ignores it, so skip
                if (e.candidate) send({ type: "ice", candidate: e.candidate.toJSON() });
              };
              pc.addTransceiver("video", { direction: "recvonly" });
              // 占位 DataChannel:WebRTC 的 answer 只能回显 offer 的 m-line——
              // 若 offer 不含 m=application,agent 预建的 input/mouse/cursor
              // 通道永远不会被协商(无输入、无光标;e2eviewer 用同法)。
              // 2026-08-24 生产事故根因。
              pc.createDataChannel("viewer");
              pc.createOffer()
                .then((offer) => pc!.setLocalDescription(offer))
                .then(() => send({ type: "offer", sdp: pc!.localDescription!.sdp }))
                .catch((err) => {
                  setState("error");
                  setStartError(`offer failed: ${err.message}`);
                });
              break;
            case "answer":
              pc
                ?.setRemoteDescription({ type: "answer", sdp: f.sdp ?? "" })
                .catch((err) => {
                  setState("error");
                  setStartError(`answer failed: ${err.message}`);
                });
              break;
            case "ice":
              if (f.candidate?.candidate) {
                pc?.addIceCandidate(f.candidate).catch(() => {
                  /* stale candidate after close — dropped */
                });
              }
              break;
            case "state":
              setAgentState(f.code ?? null);
              // M2-Slice1 Task 2/3 vocabulary: toast the recovery ladder
              // transitions (uniform with the display_changed notice).
              if (f.code && STATE_NOTICES[f.code]) setNotice(STATE_NOTICES[f.code]);
              break;
            case "display_changed":
              // Unified CaptureReset changed the stream geometry
              // (M2-Slice1 Task 2): remap input coords to the new space
              // and surface a toast.
              if (f.w && f.h) applyDims(f.w, f.h);
              setNotice(
                `display changed: ${f.w}x${f.h}` +
                  (f.reason ? ` (${f.reason})` : "") +
                  (f.generation ? ` gen ${f.generation}` : ""),
              );
              break;
            case "lease_granted":
              ioRef.current.lease = true;
              leaseRetry.current.n = 0;
              if (leaseRetry.current.timer !== undefined) window.clearTimeout(leaseRetry.current.timer);
              setLease({ status: "granted", id: f.leaseId });
              // Sync remote lock state to the local toggles now (native
              // injects the toggle key only on mismatch).
              {
                const io = ioRef.current;
                if (io.capsLock !== undefined && io.numLock !== undefined) {
                  sendLock(io.capsLock, io.numLock);
                }
              }
              break;
            case "lease_denied":
              ioRef.current.lease = false;
              setLease({ status: "denied", reason: f.reason });
              setNotice(LEASE_NOTICES[f.reason ?? ""] ?? `input lease denied: ${f.reason ?? "unknown"}`);
              // held = 控制权被其他会话持有(旧页面/刚关闭的会话,server 60s TTL
              // 内不释放)。自动重建会话重试,别让用户手动刷新撞同一个窗口。
              if (f.reason === "held" && leaseRetry.current.n < 8 && leaseRetry.current.timer === undefined) {
                leaseRetry.current.n++;
                const delay = 8000 * leaseRetry.current.n; // 8s/16s/24s…上限 64s
                setNotice(`控制权被其他窗口占用,${Math.round(delay / 1000)} 秒后自动重试 (${leaseRetry.current.n}/8)…`);
                leaseRetry.current.timer = window.setTimeout(() => {
                  leaseRetry.current.timer = undefined;
                  setSessionEpoch((e) => e + 1);
                }, delay);
              }
              break;
            case "lease_revoked":
              ioRef.current.lease = false;
              setLease({ status: "revoked", reason: f.reason });
              setNotice(LEASE_NOTICES[f.reason ?? ""] ?? `input lease revoked: ${f.reason ?? "unknown"}`);
              break;
            case "secure_attention_result":
              // M2-Slice1 Task 5: ok means the core accepted and invoked
              // SendSAS (hr is a synthesized HRESULT; 0 = the call returned
              // without raising, not proof a SAS was delivered). SAS_DENIED
              // latches the button off for this session.
              setSas((s) => ({ ...s, pending: false, denied: f.code === "SAS_DENIED" }));
              if (f.ok) {
                setNotice(`secure attention sent${f.hr ? ` (hr 0x${f.hr.toString(16)})` : ""}`);
              } else {
                setNotice(SAS_NOTICES[f.code ?? ""] ?? `secure attention failed: ${f.code ?? "unknown"}`);
              }
              break;
            case "error":
              setState("error");
              setStartError(f.message ?? f.code ?? "desktop session failed");
              break;
          }
        };
        ws.onopen = () => {
          if (!disposed) setState((s) => (s === "opening" ? "signaling" : s));
        };
        ws.onclose = () => {
          if (disposed) return;
          setState((s) => (s === "error" ? s : "disconnected"));
          ioRef.current.lease = false; // agent released it on our disconnect
          setLease((l) => (l.status === "granted" || l.status === "requested" ? { status: "none" } : l));
        };
        ws.onerror = () => {
          if (!disposed) setState("error");
        };
        // "streaming" flips on the first decoded/presented frame.
        statsTimer = window.setInterval(async () => {
          if (disposed) return;
          const decoded = await pollStats();
          if (decoded > 0 && !disposed) setState((s) => (s === "streaming" ? s : "streaming"));
        }, 1000);
        // M3 Task 5: 1s viewer_feedback uplink over this session WS (the
        // agent's QoS loop consumes it; the frame shape is the flat JSON
        // vocabulary — routing is the connection itself). Hidden tabs
        // skip the periodic report; the hide transition sends exactly
        // one visible:false report via onVisibilityChange above.
        document.addEventListener("visibilitychange", onVisibilityChange);
        feedbackTimer = window.setInterval(() => {
          if (disposed || document.hidden) return;
          void sendFeedback(document.visibilityState === "visible");
        }, 1000);
      })
      .catch((err) => {
        if (disposed) return;
        setState("error");
        setStartError(err instanceof Error ? err.message : "failed to start desktop session");
      });

    return teardown;
    // sendLock is a stable ref-only callback; node changes re-run the session
  }, [nodeId, sendLock, sessionEpoch]);

  const sendPli = () => {
    // Browsers cannot emit RTCP PLI from JS — this signaling frame asks
    // the agent for a keyframe over the same RequestKeyframe path a PLI
    // would take (vocabulary addition; old agents ignore unknown types).
    try {
      wsHandle.__xncDesktopWs?.send(JSON.stringify({ type: "keyframe-req" }));
      setStats((s) => ({ ...s, plis: s.plis + 1 }));
    } catch {
      /* session dead */
    }
  };

  const sendSas = () => {
    // Ctrl+Alt+Del via the core-side SendSAS path (M2-Slice1 Task 5) —
    // never a synthesized keyboard sequence. The result arrives as
    // secure_attention_result (handled in the session effect). The pending
    // latch also self-clears after 20s: an older agent that ignores the
    // unknown frame type would never answer. A new click cancels the
    // previous timer first (M2-Slice2 Task 1): a stale 20s timer must not
    // clear the pending state of a newer request.
    try {
      wsHandle.__xncDesktopWs?.send(JSON.stringify({ type: "secure_attention" }));
      setSas((s) => ({ ...s, pending: true }));
      if (sasTimer.current !== undefined) window.clearTimeout(sasTimer.current);
      sasTimer.current = window.setTimeout(
        () => setSas((s) => (s.pending ? { ...s, pending: false } : s)),
        20000,
      );
    } catch {
      /* session dead */
    }
  };

  // M2-S3 Task 5: display dropdown → {switch_display} signaling frame (old
  // agents ignore the unknown type; results arrive as display_changed
  // reason="switch" / state invalid_display).
  const sendSwitchDisplay = (index: number) => {
    try {
      wsHandle.__xncDesktopWs?.send(
        JSON.stringify({ type: "switch_display", index }),
      );
      setDisplaySel(index);
    } catch {
      /* session dead */
    }
  };

  const leaseLabel =
    lease.status === "granted"
      ? "input held"
      : lease.status === "requested"
        ? "requesting…"
        : "request input";

  return (
    <div className="screen-preview">
      <div className="screen-bar">
        <strong>{nodeName ?? nodeId}</strong>
        <span className={`screen-state screen-state-${state}`}>{STATE_LABELS[state]}</span>
        <span className="dim mono">ice:{iceState}</span>
        {dims && <span className="dim mono">{dims}</span>}
        {displays.length > 1 && (
          <select
            className="desktop-display-select mono"
            value={displaySel}
            onChange={(e) => sendSwitchDisplay(Number(e.target.value))}
            title="capture display (switch_display)"
          >
            {displays.map((d) => (
              <option key={d.index} value={d.index}>
                {`#${d.index} ${d.w}x${d.h}${d.primary ? " *" : ""}`}
              </option>
            ))}
          </select>
        )}
        <span className="dim mono">{`h264/${iceModeRef.current ?? "relay"}`}</span>
        {agentState && <span className="dim mono">{agentState}</span>}
        <button onClick={sendPli} className="desktop-pli" type="button">
          PLI
        </button>
        <button
          onClick={sendSas}
          className="desktop-pli"
          type="button"
          disabled={sas.pending || sas.denied}
          title={
            sas.denied
              ? "secure attention denied by the host core (--allow-sas gate)"
              : "send Ctrl+Alt+Del (secure attention) to the remote desktop via xnc-core SendSAS"
          }
        >
          {sas.pending ? "SAS…" : "Ctrl+Alt+Del"}
        </button>
        <button
          onClick={requestLease}
          className="desktop-pli"
          type="button"
          disabled={lease.status === "granted" || lease.status === "requested"}
        >
          {leaseLabel}
        </button>
        {lease.status === "granted" && lease.id && (
          <span className="desktop-lease-chip ok mono">lease {lease.id.slice(0, 8)}</span>
        )}
        {lease.status === "denied" && (
          <span className="desktop-lease-chip bad mono">denied:{lease.reason ?? "?"}</span>
        )}
        {lease.status === "revoked" && (
          <span className="desktop-lease-chip bad mono">revoked:{lease.reason ?? "?"}</span>
        )}
        {notice && (
          <span className="desktop-notice" role="status">
            {notice}
          </span>
        )}
        {startError && (
          <span className="form-error" role="alert">
            {startError}
          </span>
        )}
      </div>
      <div className="desktop-textbar">
        <span className="dim">inject text</span>
        <input
          value={text}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={onTextKeyDown}
          onCompositionStart={() => (composingRef.current = true)}
          onCompositionEnd={() => (composingRef.current = false)}
          placeholder={
            leaseHeld
              ? "text to type on the remote desktop (Enter or Send)"
              : "request the input lease first"
          }
          maxLength={MAX_TEXT_UNITS}
          disabled={!leaseHeld}
        />
        <button onClick={sendTextNow} className="desktop-pli" type="button" disabled={!leaseHeld || !text}>
          Send
        </button>
      </div>
      <div
        className={`desktop-video-wrap${leaseHeld ? " lease-held" : ""}`}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onPointerCancel={onPointerCancel}
        onWheel={onWheel}
        onContextMenu={onContextMenu}
      >
        <video ref={videoRef} className="screen-canvas" autoPlay muted playsInline />
        {cursorDot && <div className="desktop-cursor" style={cursorDot} />}
        <div className="desktop-stats mono">
          <div>fps {stats.fps}</div>
          <div>first frame {stats.firstFrameMs ? `${stats.firstFrameMs}ms` : "—"}</div>
          <div>decoded {stats.framesDecoded}</div>
          <div>key {stats.keyframesDecoded}</div>
          <div>pli sent {stats.plis}</div>
        </div>
        {framediag && diag && (
          <div className="desktop-framediag mono">
            <div>stats {diag.stats.join(" ")}</div>
            {diag.frames.map((l, i) => (
              <div key={i}>{l}</div>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}
