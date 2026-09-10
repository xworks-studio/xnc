import { useEffect, useRef, useState } from "react";
import { useParams } from "react-router-dom";
import { Terminal as XTerm } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import "@xterm/xterm/css/xterm.css";
import { api, APIError } from "../api";
import type { NodeDTO } from "../types";

/**
 * Web terminal: xterm.js attached to a shell session WebSocket.
 *
 * Flow (spec §52, unified session design §3.2 — zero protocol changes):
 * 1. POST /api/nodes/{id}/shell {cols, rows} → 202 {websocketUrl, ...}
 * 2. WS to that URL (relative → same-origin ws/wss). binaryType = arraybuffer.
 * 3. Binary frames = raw VT bytes both ways: pty output → term.write(),
 *    keystrokes → TextEncoder → ws.send() (binary).
 *    Text frames = JSON {type, payload}: agent sends SHELL_BEGIN {shell}
 *    before any output; client sends SHELL_RESIZE {cols, rows} on resize.
 * 4. Agent closes the WS on exit → "[xnc] disconnected" notice.
 *
 * RBAC: viewer gets 403 FORBIDDEN on POST /shell → "permission denied";
 * node without a control connection → 409 NODE_OFFLINE → "node offline".
 */

/** POST /api/nodes/{id}/shell 202 response. */
interface ShellStartResponse {
  sessionId: string;
  token: string;
  expiresAt: string;
  /** Relative: /api/session/{id}?token=... */
  websocketUrl: string;
}

/** Shell session WS text-frame envelope ({type, payload}). */
interface SessionFrame {
  type: string;
  payload?: { shell?: string; code?: string; message?: string };
}

type ConnState = "connecting" | "connected" | "disconnected";

/** Initial dims sent with POST /shell (server defaults); the first fit() may
 * correct them via SHELL_RESIZE once the WS is open. */
const INITIAL_COLS = 120;
const INITIAL_ROWS = 30;

const DISCONNECTED = "\r\n\x1b[90m[xnc] disconnected\x1b[0m\r\n";

/** The server returns a relative websocketUrl; make it absolute against the
 * current origin (ws in dev via the Vite proxy, wss behind https). */
function toWsUrl(url: string): string {
  if (url.startsWith("ws://") || url.startsWith("wss://")) return url;
  const proto = window.location.protocol === "https:" ? "wss:" : "ws:";
  return `${proto}//${window.location.host}${url}`;
}

export default function TerminalPage() {
  const { id } = useParams<{ id: string }>();
  const containerRef = useRef<HTMLDivElement>(null);
  const [status, setStatus] = useState<ConnState>("connecting");
  const [shell, setShell] = useState<string | null>(null);
  const [nodeName, setNodeName] = useState<string | null>(null);
  const [startError, setStartError] = useState<string | null>(null);
  const [reconnectKey, setReconnectKey] = useState(0);

  // Node name for the status bar (best-effort — viewers can read nodes too;
  // on failure the bar falls back to the node id).
  useEffect(() => {
    let alive = true;
    api<NodeDTO>(`/api/nodes/${id}`)
      .then((n) => {
        if (alive) setNodeName(n.name);
      })
      .catch(() => {
        /* status bar falls back to the node id */
      });
    return () => {
      alive = false;
    };
  }, [id]);

  // Terminal + shell session lifecycle. Creates nothing until POST /shell
  // succeeds, so error paths (403/409) never leave an orphaned terminal.
  useEffect(() => {
    if (!id) return;
    const container = containerRef.current;
    if (!container) return;

    const encoder = new TextEncoder();
    let term: XTerm | null = null;
    let fitAddon: FitAddon | null = null;
    let ws: WebSocket | null = null;
    let disposed = false;
    // Assigned once the terminal exists (inside the POST .then); the cleanup
    // path runs it if the session ever got that far.
    let cleanupExtra: (() => void) | null = null;
    // Dims the agent already knows about (initial POST value, then every
    // SHELL_RESIZE) — dedupes resize frames.
    let sentCols = INITIAL_COLS;
    let sentRows = INITIAL_ROWS;
    const sendResize = (cols: number, rows: number) => {
      if (!ws || ws.readyState !== WebSocket.OPEN) return;
      if (cols === sentCols && rows === sentRows) return;
      sentCols = cols;
      sentRows = rows;
      ws.send(JSON.stringify({ type: "SHELL_RESIZE", payload: { cols, rows } }));
    };

    api<ShellStartResponse>(`/api/nodes/${id}/shell`, {
      method: "POST",
      body: JSON.stringify({ cols: INITIAL_COLS, rows: INITIAL_ROWS }),
    })
      .then((res) => {
        if (disposed) return;

        term = new XTerm({
          cursorBlink: true,
          scrollback: 2000,
          fontFamily: 'ui-monospace, "Cascadia Code", Consolas, monospace',
          fontSize: 13,
          theme: { background: "#1a1a2e", foreground: "#e0e0e0" },
        });
        fitAddon = new FitAddon();
        term.loadAddon(fitAddon);
        term.open(container);
        fitAddon.fit();

        // Keystrokes (UTF-8 string from xterm) → binary frame → pty input.
        term.onData((data) => {
          if (ws && ws.readyState === WebSocket.OPEN) {
            ws.send(encoder.encode(data));
          }
        });
        // fit() resizes the terminal → sync the pty (skipped until the WS
        // is open; onopen sends the correction for the initial fit).
        term.onResize(({ cols, rows }) => sendResize(cols, rows));

        ws = new WebSocket(toWsUrl(res.websocketUrl));
        ws.binaryType = "arraybuffer";
        ws.onopen = () => {
          if (disposed) return;
          // Correct 120x30 → actually fitted dims if they differ. Frames sent
          // before the agent attaches queue in the socket and flow once the
          // server pump starts (both sides attached).
          sendResize(term!.cols, term!.rows);
        };
        ws.onmessage = (ev: MessageEvent) => {
          if (disposed || !term) return;
          if (typeof ev.data === "string") {
            let frame: SessionFrame;
            try {
              frame = JSON.parse(ev.data) as SessionFrame;
            } catch {
              return; // malformed control frame — ignore
            }
            if (frame.type === "SHELL_BEGIN") {
              setShell(frame.payload?.shell ?? "shell");
              setStatus("connected");
            } else if (frame.type === "ERROR") {
              // e.g. SHELL_START_FAILED; the agent closes right after.
              term.write(`\r\n\x1b[31m[xnc] ${frame.payload?.message ?? "shell failed"}\x1b[0m\r\n`);
            }
          } else {
            // Binary frame: raw VT/ANSI bytes from the pty.
            term.write(new Uint8Array(ev.data as ArrayBuffer));
          }
        };
        ws.onclose = () => {
          if (disposed || !term) return;
          setStatus("disconnected");
          term.write(DISCONNECTED);
        };
        // ws.onerror is always followed by onclose — the close handler
        // writes the disconnected notice.

        // Refit on layout changes (window resize + container size changes).
        const refit = () => {
          if (disposed || !fitAddon) return;
          fitAddon.fit();
        };
        window.addEventListener("resize", refit);
        const observer = new ResizeObserver(refit);
        observer.observe(container);
        cleanupExtra = () => {
          window.removeEventListener("resize", refit);
          observer.disconnect();
        };
      })
      .catch((err) => {
        if (disposed) return;
        setStatus("disconnected");
        if (err instanceof APIError && err.code === "FORBIDDEN") {
          setStartError("permission denied — viewers cannot open a terminal");
        } else if (err instanceof APIError && err.code === "NODE_OFFLINE") {
          setStartError("node offline");
        } else {
          setStartError(err instanceof Error ? err.message : "failed to start shell session");
        }
      });

    return () => {
      disposed = true;
      cleanupExtra?.();
      ws?.close();
      term?.dispose();
    };
  }, [id, reconnectKey]);

  return (
    <div className="term-page">
      <div className="term-statusbar">
        <span className="term-node">{nodeName ?? id}</span>
        <span className={`term-conn term-conn-${status}`}>
          {status === "connected" && shell ? `${status} · ${shell}` : status}
        </span>
        {status === "disconnected" && !startError && (
          <button className="btn btn-sm" onClick={() => setReconnectKey(k => k + 1)}>
            Reconnect
          </button>
        )}
      </div>
      {startError ? (
        <div className="term-error form-error" role="alert">
          {startError}
          <button className="btn btn-sm" style={{ marginLeft: 12 }} onClick={() => setReconnectKey(k => k + 1)}>
            Retry
          </button>
        </div>
      ) : (
        <div className="term-container" ref={containerRef} />
      )}
    </div>
  );
}
