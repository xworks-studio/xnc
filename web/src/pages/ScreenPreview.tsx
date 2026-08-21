import { useEffect, useRef, useState } from "react";
import { useParams } from "react-router-dom";
import { api } from "../api";
import type { NodeDTO } from "../types";

/**
 * Live screen preview: canvas + WebCodecs H.264 decode.
 *
 * 1. POST /api/nodes/{id}/screen {} → 202 {websocketUrl}
 * 2. WS to that URL (relative → same-origin ws/wss). binaryType = arraybuffer.
 * 3. Text frames: SCREEN_BEGIN {width,height,state,codec} / SCREEN_STATE
 *    {state}. Binary frames: H.264 NALUs — the decoder is configured on the
 *    first SPS (NALU type 7) and fed key/delta chunks; decoded frames are
 *    drawn to the canvas.
 */
interface ScreenStartResponse {
  sessionId: string;
  websocketUrl: string;
}

interface SessionFrame {
  type: string;
  payload?: { width?: number; height?: number; state?: string; message?: string };
}

/** The server returns a relative websocketUrl; make it absolute against the
 * current origin (ws in dev via the Vite proxy, wss behind https). */
function toWsUrl(url: string): string {
  if (url.startsWith("ws://") || url.startsWith("wss://")) return url;
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${window.location.host}${url}`;
}

type ScreenState = "connecting" | "capturing" | "locked" | "no_session" | "error" | "disconnected";

const STATE_LABELS: Record<ScreenState, string> = {
  connecting: "connecting",
  capturing: "capturing",
  locked: "locked",
  no_session: "no session",
  error: "error",
  disconnected: "disconnected",
};

export default function ScreenPreview() {
  const { id } = useParams<{ id: string }>();
  const canvasRef = useRef<HTMLCanvasElement>(null);
  const [state, setState] = useState<ScreenState>("connecting");
  const [dims, setDims] = useState<string | null>(null);
  const [nodeName, setNodeName] = useState<string | null>(null);
  const [startError, setStartError] = useState<string | null>(null);

  // Node name for the status bar (best-effort; falls back to the node id).
  useEffect(() => {
    if (!id) return;
    let alive = true;
    api<NodeDTO>(`/api/nodes/${id}`)
      .then((n) => alive && setNodeName(n.name))
      .catch(() => {
        /* status bar falls back to the node id */
      });
    return () => {
      alive = false;
    };
  }, [id]);

  // Screen session lifecycle: POST /screen → WS → WebCodecs → canvas.
  useEffect(() => {
    if (!id) return;
    const canvas = canvasRef.current;
    if (!canvas) return;
    const ctx = canvas.getContext("2d");
    if (!ctx) return;

    let disposed = false;
    let ws: WebSocket | null = null;
    let decoder: VideoDecoder | null = null;

    const ensureDecoder = (): VideoDecoder | null => {
      if (decoder) return decoder;
      if (typeof VideoDecoder === "undefined") {
        setState("error");
        setStartError("this browser does not support WebCodecs");
        return null;
      }
      decoder = new VideoDecoder({
        output: (frame) => {
          if (canvas.width === 0 || canvas.height === 0) {
            canvas.width = frame.displayWidth;
            canvas.height = frame.displayHeight;
          }
          ctx.drawImage(frame, 0, 0, canvas.width, canvas.height);
          frame.close();
        },
        error: (e) => {
          setState("error");
          setStartError(`decoder error: ${e.message}`);
        },
      });
      decoder.configure({ codec: "avc1.42E01E", optimizeForLatency: true });
      return decoder;
    };

    api<ScreenStartResponse>(`/api/nodes/${id}/screen`, {
      method: "POST",
      body: JSON.stringify({}),
    })
      .then((res) => {
        if (disposed) return;
        ws = new WebSocket(toWsUrl(res.websocketUrl));
        ws.binaryType = "arraybuffer";
        ws.onmessage = (ev: MessageEvent) => {
          if (disposed) return;
          if (typeof ev.data === "string") {
            let frame: SessionFrame;
            try {
              frame = JSON.parse(ev.data);
            } catch {
              return;
            }
            if (frame.type === "SCREEN_BEGIN") {
              const p = frame.payload ?? {};
              if (p.width && p.height) {
                canvas.width = p.width;
                canvas.height = p.height;
                setDims(`${p.width}x${p.height}`);
              }
              if (p.state) setState(p.state as ScreenState);
            } else if (frame.type === "SCREEN_STATE") {
              const s = frame.payload?.state;
              if (s) setState(s as ScreenState);
            } else if (frame.type === "ERROR") {
              setState("error");
              setStartError(frame.payload?.message ?? "screen session failed");
            }
            return;
          }
          const data = new Uint8Array(ev.data);
          // NALU type: first byte after the 3-byte start code (00 00 01).
          const nalType = data.length > 4 ? data[4] & 0x1f : 0;
          const dec = ensureDecoder();
          if (!dec) return;
          if (!dec && nalType !== 7) return;
          try {
            dec.decode(
              new EncodedVideoChunk({
                type: nalType === 5 || nalType === 7 || nalType === 8 ? "key" : "delta",
                timestamp: performance.now(),
                data,
              }),
            );
          } catch {
            /* frame rejected (e.g. pre-SPS delta) — dropped */
          }
        };
        ws.onclose = () => {
          if (!disposed) setState("disconnected");
        };
        ws.onerror = () => {
          if (!disposed) setState("error");
        };
      })
      .catch((err) => {
        if (disposed) return;
        setState("error");
        setStartError(err instanceof Error ? err.message : "failed to start screen session");
      });

    return () => {
      disposed = true;
      try {
        decoder?.close();
      } catch {
        /* already closed */
      }
      ws?.close();
    };
  }, [id]);

  return (
    <div className="screen-preview">
      <div className="screen-bar">
        <strong>{nodeName ?? id}</strong>
        <span className={`screen-state screen-state-${state}`}>{STATE_LABELS[state]}</span>
        {dims && <span className="dim mono">{dims}</span>}
        <span className="dim mono">h264</span>
        {startError && <span className="form-error" role="alert">{startError}</span>}
      </div>
      <canvas ref={canvasRef} className="screen-canvas" />
    </div>
  );
}
