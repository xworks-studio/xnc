import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";
import { useAuth } from "../auth";
import { formatRelativeTime } from "../reltime";

/** /installer.json 清单结构（服务端动态生成，见 server setup_handlers.go）。 */
export interface SetupManifest {
  version: string;
  url: string;
  sha256: string;
  size: number;
  releasedAt: string;
}

/** 字节数 → 人类可读 MB（一位小数）。 */
function formatMB(bytes: number): string {
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

type CardState =
  | { kind: "loading" }
  | { kind: "ready"; manifest: SetupManifest }
  | { kind: "missing" }; // 无 release（404）或清单拉取失败

/**
 * 单个频道的下载卡片。/installer.json 是公开端点（无需 JWT），不走 api()
 * 封装；fetch 失败只把本卡片置为"暂无发布"，不影响另一张卡片。
 */
function ChannelCard({ channel, title, note }: { channel: string; title: string; note: string }) {
  const [state, setState] = useState<CardState>({ kind: "loading" });
  // "copied" 复制成功；"select" 剪贴板不可用 → 已选中文本等待手动 Ctrl+C。
  const [copyHint, setCopyHint] = useState<"copied" | "select" | null>(null);
  const shaRef = useRef<HTMLSpanElement>(null);

  useEffect(() => {
    let cancelled = false;
    fetch(`/installer.json?channel=${channel}`)
      .then(async (res) => {
        if (!res.ok) throw new Error(`HTTP ${res.status}`); // 如 dev 频道尚无 release → 404
        return (await res.json()) as SetupManifest;
      })
      .then((m) => {
        if (!cancelled) setState({ kind: "ready", manifest: m });
      })
      .catch(() => {
        if (!cancelled) setState({ kind: "missing" });
      });
    return () => {
      cancelled = true;
    };
  }, [channel]);

  // 复制提示 2 秒后自动消失。
  useEffect(() => {
    if (!copyHint) return;
    const t = setTimeout(() => setCopyHint(null), 2000);
    return () => clearTimeout(t);
  }, [copyHint]);

  async function onCopy() {
    if (state.kind !== "ready") return;
    try {
      await navigator.clipboard.writeText(state.manifest.sha256);
      setCopyHint("copied");
    } catch {
      // 剪贴板不可用（如非安全上下文）：选中完整哈希，提示手动复制。
      const sel = window.getSelection();
      const node = shaRef.current;
      if (sel && node) {
        const range = document.createRange();
        range.selectNodeContents(node);
        sel.removeAllRanges();
        sel.addRange(range);
      }
      setCopyHint("select");
    }
  }

  return (
    <div className="card download-card">
      <div className="download-card-head">
        <h2>{title}</h2>
        <span className="dim">{note}</span>
      </div>
      {state.kind === "loading" && <div className="loading">Loading…</div>}
      {state.kind === "missing" && (
        <div className="empty">No release yet — check back later.</div>
      )}
      {state.kind === "ready" && (
        <>
          <dl className="detail-grid">
            <dt>Version</dt>
            <dd className="mono">{state.manifest.version}</dd>
            <dt>Released</dt>
            <dd>{formatRelativeTime(state.manifest.releasedAt)}</dd>
            <dt>Size</dt>
            <dd>{formatMB(state.manifest.size)}</dd>
          </dl>
          <div className="download-sha-row">
            <span className="dim">sha256</span>
            <button
              type="button"
              className="secondary download-sha"
              onClick={() => void onCopy()}
              aria-label="Copy sha256"
              title={state.manifest.sha256}
            >
              {/* 完整值留在 DOM 与 title 中，视觉截断交给 CSS。 */}
              <span className="mono" ref={shaRef}>
                {state.manifest.sha256}
              </span>
            </button>
            {copyHint === "copied" && <span className="notice">Copied</span>}
            {copyHint === "select" && <span className="dim">Press Ctrl+C to copy</span>}
          </div>
          <a className="download-btn" href={`/installer?channel=${channel}`}>
            Download installer
          </a>
        </>
      )}
    </div>
  );
}

/** 公开下载页：无侧栏的独立布局（与 Login 同级，在 LoginGuard 之外）。 */
export default function Download() {
  const { user } = useAuth();
  return (
    <div className="download-page">
      <header className="download-head">
        <span className="download-brand">XNC</span>
        <Link to={user ? "/nodes" : "/login"}>{user ? "Back to console" : "Sign in to console"}</Link>
      </header>
      <main className="download-main">
        <h1>Download the installer</h1>
        <p className="download-sub dim">
          Windows installer for XNC nodes — the xnc CLI ships inside.
        </p>
        <div className="download-channels">
          <ChannelCard channel="stable" title="Stable" note="Recommended" />
          <ChannelCard channel="dev" title="Dev" note="Latest builds" />
        </div>
        <section className="card download-steps">
          <h2>Install in three steps</h2>
          <ol>
            <li>
              <strong>Run the installer</strong> — download and run it
              (administrator privileges required).
            </li>
            <li>
              <strong>Register the node</strong> — open a terminal and run{" "}
              <code className="mono">xnc register</code>: sign in with your
              account, pick a cluster, and the node comes online.
            </li>
            <li>
              <strong>Manage from the console</strong> — monitor nodes, open
              terminals and manage users after signing in.
            </li>
          </ol>
        </section>
      </main>
    </div>
  );
}
