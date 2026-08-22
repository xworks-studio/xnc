import { useEffect, useRef, useState } from "react";
import { useParams } from "react-router-dom";
import { api } from "../api";
import type { NodeDTO } from "../types";

/**
 * Experimental desktop-live page (M1-Slice2): WebRTC H.264 via TURN relay.
 * Not in the sidebar — reach it directly at /desktop/<nodeId>.
 *
 * 1. POST /api/nodes/{id}/desktop {} → 202 {websocketUrl, turn}
 *    (auth = the usual localStorage bearer via api(); login first).
 * 2. Session WS carries the desktop signaling vocabulary (agent/desktop/
 *    session.go): agent → {ready, answer, ice, state, error}, viewer →
 *    {offer, ice}. Text frames only.
 * 3. After "ready": RTCPeerConnection(iceTransportPolicy:"relay",
 *    iceServers:[turn]) + recvonly video transceiver → offer → answer →
 *    trickle both ways. Track lands in <video autoplay muted playsinline>.
 *
 * The page MUST be opened against the LAN server origin (dev:
 * http://192.168.1.12:18080): the server derives the agent-side session
 * dial URL from the request Host header.
 *
 * PLI button: browsers expose no RTCP PLI from JS, so it sends a
 * {"type":"keyframe-req"} signaling frame instead (agent maps it to the
 * same RequestKeyframe path a real PLI takes).
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

/** Module-scoped live-WebSocket handle so the PLI button can reach the
 * effect-owned socket (set right after connect, cleared on teardown). */
const wsHandle: { __xncDesktopWs?: WebSocket } = {};

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
          if (video.videoWidth) setDims(`${video.videoWidth}x${video.videoHeight}`);
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
      // drop the PLI handle only if this effect still owns it
      if (wsHandle.__xncDesktopWs === ws) wsHandle.__xncDesktopWs = undefined;
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
        wsHandle.__xncDesktopWs = ws; // PLI button sends over the live socket
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
          if (!disposed) setState((s) => (s === "error" ? s : "disconnected"));
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
  }, [nodeId]);

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
        {startError && (
          <span className="form-error" role="alert">
            {startError}
          </span>
        )}
      </div>
      <div className="desktop-video-wrap">
        <video ref={videoRef} className="screen-canvas" autoPlay muted playsInline />
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
