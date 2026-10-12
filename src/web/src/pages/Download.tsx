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

type Platform = "windows" | "linux";

type CardState =
  | { kind: "loading" }
  | { kind: "ready"; manifest: SetupManifest }
  | { kind: "missing" }; // 无 release（404）或清单拉取失败

/**
 * 单个频道的下载卡片。/installer.json 是公开端点（无需 JWT），不走 api()
 * 封装；fetch 失败只把本卡片置为"暂无发布"，不影响另一张卡片。platform
 * 参数选制品（windows = setup.exe；linux = xnc-linux-amd64.tar.gz）。
 */
function ChannelCard({
  channel,
  platform,
  title,
  note,
}: {
  channel: string;
  platform: Platform;
  title: string;
  note: string;
}) {
  const [state, setState] = useState<CardState>({ kind: "loading" });
  // "copied" 复制成功；"select" 剪贴板不可用 → 已选中文本等待手动 Ctrl+C。
  const [copyHint, setCopyHint] = useState<"copied" | "select" | null>(null);
  const shaRef = useRef<HTMLSpanElement>(null);

  const manifestQ = platform === "linux" ? `&platform=linux` : "";
  const installerHref = `/installer?channel=${channel}${manifestQ}`;

  useEffect(() => {
    let cancelled = false;
    fetch(`/installer.json?channel=${channel}${manifestQ}`)
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
  }, [channel, manifestQ]);

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
          <a className="download-btn" href={installerHref}>
            Download {platform === "linux" ? "tarball" : "installer"}
          </a>
        </>
      )}
    </div>
  );
}

/** 公开下载页：无侧栏的独立布局（与 Login 同级，在 LoginGuard 之外）。 */
export default function Download() {
  const { user } = useAuth();
  const [platform, setPlatform] = useState<Platform>("windows");
  const installOneLiner = `curl -L ${window.location.origin}/install.sh | sudo bash`;

  return (
    <div className="download-page">
      <header className="download-head">
        <span className="download-brand">XNC</span>
        <Link to={user ? "/nodes" : "/login"}>{user ? "Back to console" : "Sign in to console"}</Link>
      </header>
      <main className="download-main">
        <h1>Download the installer</h1>
        {/* 平台选择：windows 安装器 / linux tarball + 一键脚本。 */}
        <div className="download-platform-toggle" role="tablist">
          <button
            type="button"
            role="tab"
            aria-selected={platform === "windows"}
            className={platform === "windows" ? "active" : ""}
            onClick={() => setPlatform("windows")}
          >
            Windows
          </button>
          <button
            type="button"
            role="tab"
            aria-selected={platform === "linux"}
            className={platform === "linux" ? "active" : ""}
            onClick={() => setPlatform("linux")}
          >
            Linux
          </button>
        </div>
        {platform === "windows" ? (
          <>
            <p className="download-sub dim">
              Windows installer for XNC nodes — the xnc CLI ships inside.
            </p>
            <div className="download-channels">
              <ChannelCard channel="stable" platform="windows" title="Stable" note="Recommended" />
              <ChannelCard channel="dev" platform="windows" title="Dev" note="Latest builds" />
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
          </>
        ) : (
          <>
            <p className="download-sub dim">
              Linux (Ubuntu/Debian, systemd) — one-line install; the xnc CLI
              ships inside.
            </p>
            <section className="card download-steps">
              <h2>One-line install</h2>
              <p>
                <code className="mono cmd">{installOneLiner}</code>
              </p>
              <p className="dim">
                The script fetches the latest tarball from this site, verifies
                its sha256, and runs the bundled install.sh (idempotent:
                re-run to upgrade; <code className="mono">--uninstall</code>{" "}
                to remove).
              </p>
            </section>
            <div className="download-channels">
              <ChannelCard channel="stable" platform="linux" title="Stable" note="Recommended" />
              <ChannelCard channel="dev" platform="linux" title="Dev" note="Latest builds" />
            </div>
            <section className="card download-steps">
              <h2>Then</h2>
              <ol>
                <li>
                  <strong>Register the node</strong> — run{" "}
                  <code className="mono">sudo xnc register</code>: sign in,
                  pick a cluster, and the node comes online (control agentctl
                  socket is root-only).
                </li>
                <li>
                  <strong>Manage from the console</strong> — exec/shell/file
                  sessions work over the same relay data plane as Windows
                  nodes; remote desktop is Windows-only for now.
                </li>
              </ol>
            </section>
          </>
        )}
      </main>
    </div>
  );
}
