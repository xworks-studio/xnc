import { useCallback, useEffect, useState } from "react";
import { api } from "../api";

/** GET /api/rtv/stats 响应。relay-only 形态（XNC_RTV_EMBEDDED=false，
 * 2026-09-11 主站缩减默认）：{enabled:false, relays}——主站不跑媒体，只有
 * 外置 relay 池视图；内嵌形态补 hosts（本地 relay-0 的在服采集端）。 */
interface RtvStats {
  enabled?: boolean;
  uptimeSec?: number;
  hosts?: {
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
  relays?: RelayInfo[];
}

/** 外置 relay（rtvpool 快照元素）。 */
interface RelayInfo {
  id: string;
  region: string;
  status: string;
  online: boolean;
  probeFails: number;
  clockDriftMs: number;
  stats: {
    sessions: number;
    viewers: number;
    mbpsIn: number;
    mbpsOut: number;
  };
  liveSessions: number;
  lastBeat: string;
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

  const relays = stats.relays ?? [];
  const hosts = stats.hosts ?? [];

  return (
    <div className="page">
      <h1>Monitor</h1>

      <section className="card">
        <div className="download-card-head">
          <h2>Relay pool</h2>
          <span className="dim mono">
            {stats.enabled === false ? "embedded relay off (relay-only)" : `uptime ${stats.uptimeSec ?? 0}s`}
          </span>
        </div>
        {relays.length === 0 ? (
          <div className="empty">
            No external relays registered. Desktop sessions require at least
            one approved xnc-relay.
          </div>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Relay</th>
                <th>Region</th>
                <th>Status</th>
                <th>Sessions</th>
                <th>Viewers</th>
                <th>Out</th>
                <th>Probe fails</th>
                <th>Last beat</th>
              </tr>
            </thead>
            <tbody>
              {relays.map((r) => (
                <tr key={r.id}>
                  <td className="mono">{r.id}</td>
                  <td className="dim">{r.region || "—"}</td>
                  <td className="mono">{r.status}</td>
                  <td className="mono">{r.stats?.sessions ?? 0}</td>
                  <td className="mono">{r.stats?.viewers ?? 0}</td>
                  <td className="dim mono">
                    {(r.stats?.mbpsOut ?? 0).toFixed(1)} Mbps
                  </td>
                  <td className="mono">{r.probeFails}</td>
                  <td className="dim mono">
                    {new Date(r.lastBeat).toLocaleTimeString()}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      {stats.enabled !== false && (
        <section className="card">
          <div className="download-card-head">
            <h2>Embedded relay hosts (relay-0)</h2>
            <span className="dim mono">uptime {stats.uptimeSec ?? 0}s</span>
          </div>
          {hosts.length === 0 ? (
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
                {hosts.map((h) => (
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
      )}

      <section className="card">
        <h2>Notes</h2>
        <p className="dim">
          Desktop media relays through xnc-relay servers; this server runs{" "}
          {stats.enabled === false
            ? "relay-only (embedded relay disabled)"
            : "the embedded relay-0"
          }
          . Host leg latency includes clock skew and is trend-only.
        </p>
      </section>
    </div>
  );
}
