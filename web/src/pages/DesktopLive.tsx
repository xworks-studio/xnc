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
import { lookupScan } from "./desktop/keymap";
import { cursorDotStyle, decodeCursor, streamMapping } from "./desktop/cursor";
import type { StreamDims } from "./desktop/cursor";

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
}

interface SignalingFrame {
  type: string;
  sdp?: string;
  candidate?: RTCIceCandidateInit | null;
  code?: string;
  message?: string;
  width?: number;
  height?: number;
  fps?: number;
  /** display_changed generation */
  generation?: number;
  /** display_changed geometry */
  w?: number;
  h?: number;
  /** display_changed reset reason / lease_denied / lease_revoked */
  reason?: string;
  /** lease_granted */
  leaseId?: string;
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

const LEASE_NOTICES: Record<string, string> = {
  held: "input lease held by another viewer — retry later",
  idle: "input lease revoked: 30s without input",
  disconnect: "input lease revoked",
};

/** Relative websocketUrl → absolute against the current origin (as
 * ScreenPreview does; ws in dev via the Vite proxy, wss behind https). */
function toWsUrl(url: string): string {
  if (url.startsWith("ws://") || url.startsWith("wss://")) return url;
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${window.location.host}${url}`;
}

/** requestVideoFrameCallback feature probe (Chrome/Edge have it; the
 * Firefox fallback polls getStats().framesDecoded). */
interface VideoFrameCallbackMetadataLike {
  mediaTime: number;
}
type Rvfc = (
  cb: (now: number, meta: VideoFrameCallbackMetadataLike) => void,
) => number;
type VideoElementWithRvfc = HTMLVideoElement & { requestVideoFrameCallback?: Rvfc };

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

export default function DesktopLive() {
  const { nodeId } = useParams<{ nodeId: string }>();
  const videoRef = useRef<HTMLVideoElement>(null);
  const [state, setState] = useState<LiveState>("opening");
  const [iceState, setIceState] = useState<string>("new");
  const [dims, setDims] = useState<string | null>(null);
  const [nodeName, setNodeName] = useState<string | null>(null);
  const [startError, setStartError] = useState<string | null>(null);
  const [agentState, setAgentState] = useState<string | null>(null);
  const [stats, setStats] = useState({
    fps: 0,
    firstFrameMs: 0,
    framesDecoded: 0,
    keyframesDecoded: 0,
    plis: 0,
  });

  // Slice3: lease / cursor dot / stream dims / text injection.
  const [lease, setLease] = useState<LeaseState>({ status: "none" });
  const [notice, setNotice] = useState<string | null>(null);
  /** Dot style computed in the cursor-channel handler (event context, not
   * render) — refs must not be read during render. */
  const [cursorDot, setCursorDot] = useState<CSSProperties | null>(null);
  const [text, setText] = useState("");
  const ioRef = useRef<DesktopIo>(freshIo());
  const dimsRef = useRef<StreamDims | null>(null);
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
      try {
        const report = await pc.getStats();
        report.forEach((s) => {
          if (s.type === "inbound-rtp" && s.kind === "video") {
            const r = s as RTCInboundRtpStreamStats & {
              framesDecoded?: number;
              keyFramesDecoded?: number;
            };
            decoded = r.framesDecoded ?? 0;
            keys = r.keyFramesDecoded ?? 0;
          }
        });
      } catch {
        /* pc closing */
      }
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
              // relay-only PC per the dev topology (non-TLS coturn = M2)
              pc = new RTCPeerConnection({
                iceServers: [
                  {
                    urls: res.turn!.urls,
                    username: res.turn!.username,
                    credential: res.turn!.credential,
                  },
                ],
                iceTransportPolicy: "relay",
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
                }
              };
              pc.ontrack = (e) => {
                const video = videoRef.current;
                if (!video) return;
                video.srcObject = e.streams[0] ?? new MediaStream([e.track]);
                startFrameLoop(video);
              };
              pc.onicecandidate = (e) => {
                // null = end-of-candidates; the agent ignores it, so skip
                if (e.candidate) send({ type: "ice", candidate: e.candidate.toJSON() });
              };
              pc.addTransceiver("video", { direction: "recvonly" });
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
              break;
            case "lease_revoked":
              ioRef.current.lease = false;
              setLease({ status: "revoked", reason: f.reason });
              setNotice(LEASE_NOTICES[f.reason ?? ""] ?? `input lease revoked: ${f.reason ?? "unknown"}`);
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
      })
      .catch((err) => {
        if (disposed) return;
        setState("error");
        setStartError(err instanceof Error ? err.message : "failed to start desktop session");
      });

    return teardown;
    // sendLock is a stable ref-only callback; node changes re-run the session
  }, [nodeId, sendLock]);

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
        <span className="dim mono">h264/relay</span>
        {agentState && <span className="dim mono">{agentState}</span>}
        <button onClick={sendPli} className="desktop-pli" type="button">
          PLI
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
      </div>
    </div>
  );
}
