import { useCallback, useEffect, useState } from "react";
import { api } from "../api";
import { probeTurn } from "../lib/turnProbe";

/** GET /api/turn/status 响应（设计 §3.1）。 */
interface TurnStatus {
  mode: "pool" | "urls" | "unconfigured";
  icePolicy: string;
  pool: { ip: string; port: number; urls: string[]; healthy: boolean }[];
  fallbackUrls: string[];
  username: string;
  credential: string;
  activeDesktopSessions: number;
}

/** 可探测行：池成员（healthy 来自 server 探测）或 fallback URL（无探测）。 */
interface Row {
  key: string;
  label: string;
  urls: string[];
  healthy: boolean | null;
}

type ProbeState = "idle" | "probing" | number | "timeout" | "no rtt" | "error";

const REFRESH_MS = 10_000;

export default function Monitor() {
  const [status, setStatus] = useState<TurnStatus | null>(null);
  const [failed, setFailed] = useState(false);
  const [probes, setProbes] = useState<Record<string, ProbeState>>({});
  const [probing, setProbing] = useState(false);

  const load = useCallback(async () => {
    try {
      setStatus(await api<TurnStatus>("/api/turn/status"));
      setFailed(false);
    } catch {
      setFailed(true);
    }
  }, []);

  useEffect(() => {
    void load();
    const t = window.setInterval(() => void load(), REFRESH_MS);
    return () => window.clearInterval(t);
  }, [load]);

  const rows = (status: TurnStatus): Row[] => [
    ...status.pool.map((p) => ({
      key: `${p.ip}:${p.port}`,
      label: `${p.ip}:${p.port}`,
      urls: p.urls,
      healthy: p.healthy,
    })),
    ...status.fallbackUrls.map((u) => ({ key: u, label: u, urls: [u], healthy: null })),
  ];

  /** 串行探测全部行（并发会互相抬时延，设计 §3.2）。 */
  const runProbes = useCallback(async () => {
    if (!status || probing) return;
    setProbing(true);
    try {
      for (const row of rows(status)) {
        setProbes((p) => ({ ...p, [row.key]: "probing" }));
        try {
          const ms = await probeTurn({ urls: row.urls, username: status.username, credential: status.credential });
          setProbes((p) => ({ ...p, [row.key]: ms }));
        } catch (e) {
          // 区分两类失败：relay 连通但无统计（"no rtt"）≠ 探测超时。
          const msg = e instanceof Error ? e.message : "";
          setProbes((p) => ({ ...p, [row.key]: msg === "no rtt" ? "no rtt" : "timeout" }));
        }
      }
    } finally {
      setProbing(false);
    }
  }, [status, probing]);

  // 状态就绪且未探测过 → 自动跑一轮。
  useEffect(() => {
    if (!status || status.mode === "unconfigured") return;
    if (Object.keys(probes).length > 0 || probing) return;
    void runProbes();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [status]);

  const probeCell = (key: string): string => {
    const s = probes[key];
    if (s === undefined || s === "idle") return "—";
    if (s === "probing") return "probing…";
    if (s === "timeout" || s === "no rtt" || s === "error") return s;
    return `${s} ms`;
  };

  if (failed) {
    return (
      <div className="page">
        <h1>Monitor</h1>
        <div className="empty">Failed to load TURN status.</div>
      </div>
    );
  }
  if (!status) {
    return (
      <div className="page">
        <h1>Monitor</h1>
        <div className="loading">Loading…</div>
      </div>
    );
  }

  return (
    <div className="page">
      <h1>Monitor</h1>

      <section className="card">
        <div className="download-card-head">
          <h2>TURN servers</h2>
          <span className="dim mono">
            {status.mode} · ice {status.icePolicy}
          </span>
          <button type="button" className="secondary" disabled={probing} onClick={() => void runProbes()}>
            {probing ? "Probing…" : "Re-probe"}
          </button>
        </div>
        {status.mode === "unconfigured" ? (
          <div className="empty">TURN is not configured on this server.</div>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Target</th>
                <th>Transports</th>
                <th>Server health</th>
                <th>Browser RTT</th>
              </tr>
            </thead>
            <tbody>
              {rows(status).map((row) => (
                <tr key={row.key}>
                  <td className="mono">{row.label}</td>
                  <td className="dim mono">{row.urls.some((u) => !u.endsWith("?transport=tcp")) ? "udp/tcp" : "tcp"}</td>
                  <td className={row.healthy === false ? "form-error" : "dim"}>
                    {row.healthy === null ? "—" : row.healthy ? "healthy" : "unhealthy"}
                  </td>
                  <td className="mono">{probeCell(row.key)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <section className="card">
        <h2>Usage</h2>
        <dl className="detail-grid">
          <dt>Active desktop sessions</dt>
          <dd className="mono">{status.activeDesktopSessions}</dd>
        </dl>
        <p className="dim">
          Desktop sessions relay through TURN by default; with ICE policy “all” a
          browser on the same LAN may connect directly.
        </p>
      </section>
    </div>
  );
}
