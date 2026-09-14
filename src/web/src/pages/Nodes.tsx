import { useCallback, useEffect, useState, type FormEvent } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { api } from "../api";
import Modal from "../components/Modal";
import StatusBadge from "../components/StatusBadge";
import type { ClusterDTO, NodeDTO } from "../types";

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

/**
 * Node list（2026-09-14 四轮迭代）：整行 hover/点击进详情（恢复，操作按钮
 * stopPropagation）；行尾 "Manage…" 收纳全部管理动作——改名（PATCH）、
 * 启停（disable/enable）、移动集群（move，双 owner 服务端强制）、删除
 * （admin-only，两步确认）。10s 自动刷新与 cluster 过滤不变。
 */
export default function Nodes() {
  const navigate = useNavigate();
  const [searchParams, setSearchParams] = useSearchParams();
  // The API's clusterId query param accepts the cluster name or UUID.
  const cluster = searchParams.get("cluster") ?? "";

  const [clusters, setClusters] = useState<ClusterDTO[]>([]);
  const [nodes, setNodes] = useState<NodeDTO[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);
  const [isAdmin, setIsAdmin] = useState(false);

  // Manage 对话框状态。
  const [target, setTarget] = useState<NodeDTO | null>(null);
  const [nameValue, setNameValue] = useState("");
  const [moveTarget, setMoveTarget] = useState("");
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [dialogBusy, setDialogBusy] = useState(false);
  const [dialogError, setDialogError] = useState<string | null>(null);
  const [dialogNotice, setDialogNotice] = useState<string | null>(null);

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
    api<{ user: { is_admin?: boolean } }>("/api/auth/me")
      .then((me) => setIsAdmin(me.user.is_admin === true))
      .catch(() => {
        // 非 admin 视图只是少一个删除按钮，不影响其余功能。
      });
  }, []);

  function onFilterChange(value: string) {
    setSearchParams(value ? { cluster: value } : {});
  }

  /** 该节点所在 cluster 上我的角色（按名字匹配集群下拉数据）。 */
  function roleIn(clusterName: string): string | undefined {
    return clusters.find((c) => c.name === clusterName)?.role;
  }

  function openManage(n: NodeDTO) {
    setTarget(n);
    setNameValue(n.name);
    setMoveTarget("");
    setConfirmDelete(false);
    setDialogError(null);
    setDialogNotice(null);
  }

  async function refreshAndSync(updated: Partial<NodeDTO>) {
    await load();
    setTarget((t) => (t ? { ...t, ...updated } : t));
  }

  async function onRename(e: FormEvent) {
    e.preventDefault();
    if (!target) return;
    setDialogBusy(true);
    setDialogError(null);
    setDialogNotice(null);
    try {
      const resp = await api<{ name: string }>(`/api/nodes/${target.id}`, {
        method: "PATCH",
        body: JSON.stringify({ name: nameValue }),
      });
      setDialogNotice("Name saved");
      await refreshAndSync({ name: resp.name });
    } catch (err) {
      setDialogError(err instanceof Error ? err.message : "failed to rename node");
    } finally {
      setDialogBusy(false);
    }
  }

  async function onToggleStatus() {
    if (!target) return;
    const action = target.status === "disabled" ? "enable" : "disable";
    setDialogBusy(true);
    setDialogError(null);
    setDialogNotice(null);
    try {
      await api(`/api/nodes/${target.id}/${action}`, { method: "POST" });
      setDialogNotice(action === "disable" ? "Node disabled" : "Node re-enabled");
      // enable → offline（online 由连接驱动），disable → disabled。
      await refreshAndSync({
        status: action === "disable" ? "disabled" : "offline",
      });
    } catch (err) {
      setDialogError(err instanceof Error ? err.message : "request failed");
    } finally {
      setDialogBusy(false);
    }
  }

  async function onMove() {
    if (!target || !moveTarget) return;
    setDialogBusy(true);
    setDialogError(null);
    setDialogNotice(null);
    try {
      const resp = await api<{ name: string }>(`/api/nodes/${target.id}/move`, {
        method: "POST",
        body: JSON.stringify({ target: moveTarget }),
      });
      const dest = clusters.find((c) => c.id === moveTarget);
      setDialogNotice(
        resp.name && resp.name !== target.name
          ? `Moved to ${dest?.name ?? moveTarget} (renamed to ${resp.name}: name taken)`
          : `Moved to ${dest?.name ?? moveTarget}`,
      );
      setMoveTarget("");
      await refreshAndSync({ cluster: dest?.name ?? target.cluster, name: resp.name });
    } catch (err) {
      setDialogError(err instanceof Error ? err.message : "failed to move node");
    } finally {
      setDialogBusy(false);
    }
  }

  async function onDelete() {
    if (!target) return;
    setDialogBusy(true);
    setDialogError(null);
    try {
      await api(`/api/nodes/${target.id}`, { method: "DELETE" });
      setNotice(`Node ${target.name} deleted`);
      setTarget(null);
      await load();
    } catch (err) {
      setDialogError(err instanceof Error ? err.message : "failed to delete node");
      setConfirmDelete(false);
    } finally {
      setDialogBusy(false);
    }
  }

  const manageOwner = target ? roleIn(target.cluster) === "owner" : false;
  const moveTargets = target
    ? clusters.filter((c) => c.role === "owner" && c.name !== target.cluster)
    : [];

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
              {nodes.map((n) => (
                <tr
                  key={n.id}
                  className="clickable"
                  onClick={() => navigate(`/nodes/${n.id}`)}
                >
                  <td>{n.name}</td>
                  <td className="dim">{n.cluster}</td>
                  <td><StatusBadge status={n.status} /></td>
                  <td className="mono dim">{n.agent_version}</td>
                  <td className="dim">{lastSeen(n.last_seen_at)}</td>
                  <td className="cell-actions" onClick={(e) => e.stopPropagation()}>
                    <button className="secondary small" onClick={() => openManage(n)}>
                      Manage…
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {nodes.length === 0 && <div className="empty">No nodes.</div>}
        </div>
      )}

      {target && (
        <Modal title="Manage node" onClose={() => setTarget(null)}>
          <div className="manage-head">
            <span className="user-email">{target.name}</span>
            <StatusBadge status={target.status} />
            <span className="dim">{target.cluster}</span>
            <span className="dim mono">{target.agent_version}</span>
            <span className="dim">seen {lastSeen(target.last_seen_at)}</span>
          </div>

          {manageOwner ? (
            <>
              <form className="manage-section" onSubmit={onRename}>
                <label>
                  Name
                  <input
                    value={nameValue}
                    onChange={(e) => setNameValue(e.target.value)}
                    autoComplete="off"
                  />
                </label>
                <button type="submit" className="secondary small" disabled={dialogBusy}>
                  Save
                </button>
              </form>

              <div className="manage-section">
                <div className="manage-line">
                  <span>
                    Status
                    <span className="dim" style={{ marginLeft: 8 }}>
                      {target.status === "disabled"
                        ? "sessions blocked (403 NODE_DISABLED)"
                        : "normal"}
                    </span>
                  </span>
                  <button
                    className="secondary small"
                    disabled={dialogBusy}
                    onClick={() => void onToggleStatus()}
                  >
                    {target.status === "disabled" ? "Enable" : "Disable"}
                  </button>
                </div>
              </div>

              {moveTargets.length > 0 && (
                <div className="manage-section">
                  <label>
                    Move to cluster
                    <select
                      value={moveTarget}
                      onChange={(e) => setMoveTarget(e.target.value)}
                    >
                      <option value="">Target…</option>
                      {moveTargets.map((c) => (
                        <option key={c.id} value={c.id}>{c.name}</option>
                      ))}
                    </select>
                  </label>
                  <button
                    className="secondary small"
                    disabled={dialogBusy || !moveTarget}
                    onClick={() => void onMove()}
                  >
                    {dialogBusy ? "Moving…" : "Move"}
                  </button>
                </div>
              )}
            </>
          ) : (
            <p className="dim" style={{ margin: 0, fontSize: 13 }}>
              Cluster owner role required to manage this node.
            </p>
          )}

          {isAdmin && (
            <div className="manage-section manage-danger">
              <div className="manage-line">
                <span className="dim" style={{ fontSize: 13 }}>
                  Delete node — removes the machine registration (admin only).
                </span>
                {confirmDelete ? (
                  <button className="danger small" disabled={dialogBusy} onClick={() => void onDelete()}>
                    {dialogBusy ? "Deleting…" : "Confirm delete"}
                  </button>
                ) : (
                  <button className="danger small" disabled={dialogBusy} onClick={() => setConfirmDelete(true)}>
                    Delete…
                  </button>
                )}
              </div>
            </div>
          )}

          {dialogError && <div className="form-error" role="alert">{dialogError}</div>}
          {dialogNotice && <div className="notice" role="status">{dialogNotice}</div>}
        </Modal>
      )}
    </div>
  );
}
