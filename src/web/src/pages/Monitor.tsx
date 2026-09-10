import { useCallback, useEffect, useState } from "react";
import { api } from "../api";

/** GET /api/rtv/stats 响应（原 MVP /statsz 的收权版本；RTV 重构后的
 * 监控面——TURN 池视图随栈退役）。 */
interface RtvStats {
  uptimeSec: number;
  hosts: {
    node: string;
    connectedSince: string;
    viewers: number;
    rxPkgs: number;
    rxBytes: number;
    txPkgs: number;
    txBytes: number;
    framesSeen: number;
    hostLegLatencyMs: number;
  }[];
}

const REFRESH_MS = 10_000;

export default function Monitor() {
  const [stats, setStats] = useState<RtvStats | null>(null);
  const [failed, setFailed] = useState(false);

  const load = useCallback(async () => {
    try {
      setStats(await api<RtvStats>("/api/rtv/stats"));
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

  if (failed) {
    return (
      <div className="page">
        <h1>Monitor</h1>
        <div className="empty">Failed to load RTV relay stats.</div>
      </div>
    );
  }
  if (!stats) {
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
          <h2>RTV relay hosts</h2>
          <span className="dim mono">uptime {stats.uptimeSec}s</span>
        </div>
        {stats.hosts.length === 0 ? (
          <div className="empty">No desktop hosts are connected.</div>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Node</th>
                <th>Since</th>
                <th>Viewers</th>
                <th>Rx</th>
                <th>Tx</th>
                <th>Frames</th>
                <th>Host leg</th>
              </tr>
            </thead>
            <tbody>
              {stats.hosts.map((h) => (
                <tr key={h.node}>
                  <td className="mono">{h.node.slice(0, 8)}</td>
                  <td className="dim mono">
                    {new Date(h.connectedSince).toLocaleTimeString()}
                  </td>
                  <td className="mono">{h.viewers}</td>
                  <td className="dim mono">
                    {(h.rxBytes / 1e6).toFixed(1)} MB / {h.rxPkgs} pkts
                  </td>
                  <td className="dim mono">
                    {(h.txBytes / 1e6).toFixed(1)} MB / {h.txPkgs} pkts
                  </td>
                  <td className="mono">{h.framesSeen}</td>
                  <td className="mono">
                    {h.hostLegLatencyMs < 0
                      ? "—"
                      : `${h.hostLegLatencyMs.toFixed(1)} ms`}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <section className="card">
        <h2>Notes</h2>
        <p className="dim">
          Desktop media relays through xnc-server&apos;s QUIC/WT/WS legs (RTV);
          the host leg latency includes clock skew and is trend-only. TURN was
          retired with the 2026-09-08 rewrite.
        </p>
      </section>
    </div>
  );
}
