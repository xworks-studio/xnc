import { useCallback, useEffect, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { api } from "../api";
import type { ClusterDTO, NodeDTO } from "../types";
import StatusBadge from "../components/StatusBadge";

const REFRESH_MS = 10_000;

/** "42s ago" / "5m ago" / "3h ago" / local datetime — null means never seen. */
function lastSeen(iso: string | null): string {
  if (!iso) return "never";
  const ms = Date.now() - Date.parse(iso);
  if (Number.isNaN(ms)) return "never";
  if (ms < 60_000) return `${Math.max(1, Math.floor(ms / 1000))}s ago`;
  if (ms < 3_600_000) return `${Math.floor(ms / 60_000)}m ago`;
  if (ms < 86_400_000) return `${Math.floor(ms / 3_600_000)}h ago`;
  return new Date(iso).toLocaleString();
}

/** Node list — 10s auto-refresh, cluster dropdown filter, rows open node detail. */
export default function Nodes() {
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  // The API's clusterId query param accepts the cluster name or UUID.
  const cluster = searchParams.get("cluster") ?? "";

  const [clusters, setClusters] = useState<ClusterDTO[]>([]);
  const [nodes, setNodes] = useState<NodeDTO[] | null>(null);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      const qs = cluster ? `?clusterId=${encodeURIComponent(cluster)}` : "";
      setNodes(await api<NodeDTO[]>(`/api/nodes${qs}`));
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to load nodes");
    }
  }, [cluster]);

  useEffect(() => {
    void load();
    const timer = setInterval(() => void load(), REFRESH_MS);
    return () => clearInterval(timer);
  }, [load]);

  useEffect(() => {
    api<ClusterDTO[]>("/api/clusters")
      .then(setClusters)
      .catch(() => {
        // Filter dropdown stays empty; the node table itself still renders.
      });
  }, []);

  function onFilterChange(value: string) {
    setSearchParams(value ? { cluster: value } : {});
  }

  return (
    <div>
      <h1>Nodes</h1>
      <div className="toolbar">
        <label className="inline-label">
          Cluster
          <select value={cluster} onChange={(e) => onFilterChange(e.target.value)}>
            <option value="">All clusters</option>
            {clusters.map((c) => (
              <option key={c.id} value={c.name}>
                {c.name}
              </option>
            ))}
          </select>
        </label>
        {nodes && <span className="dim">{nodes.length} node{nodes.length === 1 ? "" : "s"}</span>}
      </div>
      {error && <div className="form-error" role="alert">{error}</div>}
      {nodes && (
        <div className="card flush">
          <table>
            <thead>
              <tr>
                <th>Name</th>
                <th>Cluster</th>
                <th>Status</th>
                <th>Agent</th>
                <th>Last seen</th>
              </tr>
            </thead>
            <tbody>
              {nodes.map((n) => (
                <tr key={n.id} className="clickable" onClick={() => navigate(`/nodes/${n.id}`)}>
                  <td>{n.name}</td>
                  <td className="dim">{n.cluster}</td>
                  <td><StatusBadge status={n.status} /></td>
                  <td className="mono dim">{n.agent_version}</td>
                  <td className="dim">{lastSeen(n.last_seen_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {nodes.length === 0 && <div className="empty">No nodes.</div>}
        </div>
      )}
    </div>
  );
}
