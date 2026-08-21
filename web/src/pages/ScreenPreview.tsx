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
 *    {state}. Binary frames carry a 1-byte subheader (protocol-owned frame
 *    type — no NALU sniffing): 0x01 H.264 key / 0x02 H.264 delta / 0x03
 *    JPEG single. The decoder is configured from the SPS of the first key
 *    frame (key frames always start with SPS per the helper contract).
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

    // 关键帧首 SPS（含起始码）提取 codec string：avc1.<profile><compat><level>。
    // 关键帧契约：首 NALU 必为 SPS（helper 保证 IDR 前带 SPS/PPS）。
    const codecFromSPS = (data: Uint8Array): string | null => {
      let hdr = -1;
      if (data[0] === 0 && data[1] === 0 && data[2] === 1) hdr = 3;
      else if (data[0] === 0 && data[1] === 0 && data[2] === 0 && data[3] === 1) hdr = 4;
      if (hdr < 0 || data.length < hdr + 4) return null;
      return (
        "avc1." +
        [data[hdr + 1], data[hdr + 2], data[hdr + 3]]
          .map((b) => b.toString(16).padStart(2, "0"))
          .join("")
          .toUpperCase()
      );
    };

    const ensureDecoder = (codec: string): VideoDecoder | null => {
      if (decoder) return decoder;
      if (typeof VideoDecoder === "undefined") {
        setState("error");
        setStartError("this browser does not support WebCodecs");
        return null;
      }
      decoder = new VideoDecoder({
        output: (frame) => {
          // 画布尺寸以解码帧为准（SPS 裁剪后可能与 SCREEN_BEGIN 声明差几像素）。
          if (canvas.width !== frame.displayWidth || canvas.height !== frame.displayHeight) {
            canvas.width = frame.displayWidth;
            canvas.height = frame.displayHeight;
            setDims(`${frame.displayWidth}x${frame.displayHeight}`);
          }
          ctx.drawImage(frame, 0, 0);
          frame.close();
        },
        error: (e) => {
          setState("error");
          setStartError(`decoder error: ${e.message}`);
          // 回传 agent（SCREEN_FEEDBACK）：解码错误此前死在浏览器 console，
          // 现在第一时间 surfaced 到 agent 日志。
          try {
            ws?.send(
              JSON.stringify({
                type: "SCREEN_FEEDBACK",
                payload: { message: e.message },
              }),
            );
          } catch {
            /* session already dead */
          }
        },
      });
      decoder.configure({ codec, optimizeForLatency: true });
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
          if (data.length < 1) return;
          const sub = data[0];
          if (sub === 3) {
            // JPEG 单帧（快照模式经同一页面查看）。
            createImageBitmap(new Blob([data.subarray(1)]))
              .then((bmp) => {
                if (canvas.width !== bmp.width || canvas.height !== bmp.height) {
                  canvas.width = bmp.width;
                  canvas.height = bmp.height;
                  setDims(`${bmp.width}x${bmp.height}`);
                }
                ctx.drawImage(bmp, 0, 0);
                bmp.close();
              })
              .catch(() => {
                /* malformed jpeg — dropped */
              });
            return;
          }
          const isKey = sub === 1;
          const au = data.subarray(1); // Annex-B access unit
          // codec string 取自关键帧首 SPS（仅在关键帧上做，帧类型本身来自子头）。
          const codec = isKey ? codecFromSPS(au) : null;
          const dec = codec ? ensureDecoder(codec) : decoder;
          if (!dec) return;
          try {
            dec.decode(
              new EncodedVideoChunk({
                type: isKey ? "key" : "delta",
                timestamp: performance.now(),
                data: au,
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
