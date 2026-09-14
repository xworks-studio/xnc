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

/** Node list — 10s auto-refresh, cluster dropdown filter, rows open node detail.
 *  0005：行内 "Move"（我是该 cluster 的 owner 时显示）把节点搬去我 owner 的
 *  其他 cluster（服务端要求双边 owner；nodeId/在线连接保留）。 */
export default function Nodes() {
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  // The API's clusterId query param accepts the cluster name or UUID.
  const cluster = searchParams.get("cluster") ?? "";

  const [clusters, setClusters] = useState<ClusterDTO[]>([]);
  const [nodes, setNodes] = useState<NodeDTO[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  // Move 行内状态：正在搬的节点 id + 目标 cluster id。
  const [moveRow, setMoveRow] = useState<string | null>(null);
  const [moveTarget, setMoveTarget] = useState("");
  const [moveBusy, setMoveBusy] = useState(false);

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

  /** 该节点所在 cluster 上我的角色（按名字匹配集群下拉数据）。 */
  function roleIn(clusterName: string): string | undefined {
    return clusters.find((c) => c.name === clusterName)?.role;
  }

  async function onMove(node: NodeDTO) {
    if (!moveTarget) return;
    setMoveBusy(true);
    setError(null);
    try {
      const resp = await api<{ name: string }>(`/api/nodes/${node.id}/move`, {
        method: "POST",
        body: JSON.stringify({ target: moveTarget }),
      });
      const target = clusters.find((c) => c.id === moveTarget);
      setNotice(
        resp.name && resp.name !== node.name
          ? `Moved ${node.name} to ${target?.name ?? moveTarget} (renamed to ${resp.name}: name taken)`
          : `Moved ${node.name} to ${target?.name ?? moveTarget}`,
      );
      setMoveRow(null);
      setMoveTarget("");
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to move node");
    } finally {
      setMoveBusy(false);
    }
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
      {notice && <div className="notice" role="status">{notice}</div>}
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
                <th></th>
              </tr>
            </thead>
            <tbody>
              {nodes.map((n) => {
                const owner = roleIn(n.cluster) === "owner";
                const targets = clusters.filter((c) => c.role === "owner" && c.name !== n.cluster);
                return (
                  <tr key={n.id}>
                    <td className="clickable" onClick={() => navigate(`/nodes/${n.id}`)}>{n.name}</td>
                    <td className="dim">{n.cluster}</td>
                    <td><StatusBadge status={n.status} /></td>
                    <td className="mono dim">{n.agent_version}</td>
                    <td className="dim">{lastSeen(n.last_seen_at)}</td>
                    <td>
                      {owner && targets.length > 0 && (
                        moveRow === n.id ? (
                          <span className="row-actions">
                            <select value={moveTarget} onChange={(e) => setMoveTarget(e.target.value)}>
                              <option value="">Target…</option>
                              {targets.map((c) => (
                                <option key={c.id} value={c.id}>{c.name}</option>
                              ))}
                            </select>
                            <button
                              type="button"
                              disabled={moveBusy || !moveTarget}
                              onClick={() => void onMove(n)}
                            >
                              {moveBusy ? "Moving…" : "Go"}
                            </button>
                            <button type="button" onClick={() => setMoveRow(null)}>Cancel</button>
                          </span>
                        ) : (
                          <button type="button" onClick={() => { setMoveRow(n.id); setMoveTarget(""); }}>
                            Move
                          </button>
                        )
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
          {nodes.length === 0 && <div className="empty">No nodes.</div>}
        </div>
      )}
    </div>
  );
}
