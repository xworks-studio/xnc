import { useCallback, useEffect, useState, type FormEvent } from "react";
import { Link } from "react-router-dom";
import { api } from "../api";
import Modal from "../components/Modal";
import type { ClusterDTO, MemberDTO } from "../types";

const ROLES: MemberDTO["role"][] = ["viewer", "operator", "owner"];

/**
 * Cluster 管理（主从布局，2026-09-14 重设计）：左侧列表（搜索 + 选中态 +
 * 计数/徽标），右侧详情（改名/删除/看节点 + 成员面板）。低频的"创建"收进
 * 页头主按钮 + 对话框；破坏性操作（删除）用对话框确认代替 window.confirm。
 * 写操作 owner-only（服务端强制）；viewer 只读成员表。
 */
export default function Clusters() {
  const [clusters, setClusters] = useState<ClusterDTO[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const [query, setQuery] = useState("");
  const [selectedId, setSelectedId] = useState<string | null>(null);

  // 成员面板（跟随选中 cluster）。
  const [members, setMembers] = useState<MemberDTO[] | null>(null);
  const [memberEmail, setMemberEmail] = useState("");
  const [memberRole, setMemberRole] = useState<MemberDTO["role"]>("operator");
  const [panelBusy, setPanelBusy] = useState(false);
  const [panelError, setPanelError] = useState<string | null>(null);

  // 对话框：创建 / 改名 / 删除。
  const [createOpen, setCreateOpen] = useState(false);
  const [newName, setNewName] = useState("");
  const [renameTarget, setRenameTarget] = useState<ClusterDTO | null>(null);
  const [renameValue, setRenameValue] = useState("");
  const [deleteTarget, setDeleteTarget] = useState<ClusterDTO | null>(null);
  const [dialogBusy, setDialogBusy] = useState(false);
  const [dialogError, setDialogError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      setClusters(await api<ClusterDTO[]>("/api/clusters"));
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to load clusters");
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const loadMembers = useCallback(async (clusterId: string) => {
    try {
      setMembers(await api<MemberDTO[]>(`/api/clusters/${clusterId}/members`));
    } catch {
      setMembers(null);
    }
  }, []);

  const clustersList = clusters ?? [];
  const filtered = query.trim()
    ? clustersList.filter((c) => c.name.toLowerCase().includes(query.trim().toLowerCase()))
    : clustersList;
  // 选中项被过滤掉时回落到第一项，详情区始终有内容。
  const selected =
    filtered.find((c) => c.id === selectedId) ?? filtered[0] ?? null;

  useEffect(() => {
    setMembers(null);
    setPanelError(null);
    setMemberEmail("");
    if (selected) void loadMembers(selected.id);
  }, [selected?.id, loadMembers]);

  async function onCreate(e: FormEvent) {
    e.preventDefault();
    setDialogBusy(true);
    setDialogError(null);
    try {
      const created = await api<ClusterDTO>("/api/clusters", {
        method: "POST",
        body: JSON.stringify({ name: newName }),
      });
      setNotice(`Cluster ${created.name} created`);
      setCreateOpen(false);
      setNewName("");
      setSelectedId(created.id);
      await load();
    } catch (err) {
      setDialogError(err instanceof Error ? err.message : "failed to create cluster");
    } finally {
      setDialogBusy(false);
    }
  }

  async function onRename(e: FormEvent) {
    e.preventDefault();
    if (!renameTarget) return;
    setDialogBusy(true);
    setDialogError(null);
    try {
      await api(`/api/clusters/${renameTarget.id}`, {
        method: "PATCH",
        body: JSON.stringify({ name: renameValue.trim() }),
      });
      setNotice(`Renamed to ${renameValue.trim()}`);
      setRenameTarget(null);
      await load();
    } catch (err) {
      setDialogError(err instanceof Error ? err.message : "failed to rename cluster");
    } finally {
      setDialogBusy(false);
    }
  }

  async function onDelete() {
    if (!deleteTarget) return;
    setDialogBusy(true);
    setDialogError(null);
    try {
      await api(`/api/clusters/${deleteTarget.id}`, { method: "DELETE" });
      setNotice(`Cluster ${deleteTarget.name} deleted`);
      if (selectedId === deleteTarget.id) setSelectedId(null);
      setDeleteTarget(null);
      await load();
    } catch (err) {
      // 409 CLUSTER_NOT_EMPTY 常见：文案引导先移出/删除节点。
      setDialogError(err instanceof Error ? err.message : "failed to delete cluster");
    } finally {
      setDialogBusy(false);
    }
  }

  async function onAddMember(e: FormEvent) {
    e.preventDefault();
    if (!selected) return;
    setPanelBusy(true);
    setPanelError(null);
    try {
      await api(`/api/clusters/${selected.id}/members`, {
        method: "POST",
        body: JSON.stringify({ email: memberEmail, role: memberRole }),
      });
      setMemberEmail("");
      await Promise.all([loadMembers(selected.id), load()]);
    } catch (err) {
      setPanelError(err instanceof Error ? err.message : "failed to add member");
    } finally {
      setPanelBusy(false);
    }
  }

  async function onRoleChange(m: MemberDTO, role: MemberDTO["role"]) {
    if (!selected) return;
    setPanelError(null);
    try {
      await api(`/api/clusters/${selected.id}/members/${m.user_id}`, {
        method: "PATCH",
        body: JSON.stringify({ role }),
      });
      await loadMembers(selected.id);
    } catch (err) {
      setPanelError(err instanceof Error ? err.message : "failed to change role");
    }
  }

  async function onRemoveMember(m: MemberDTO) {
    if (!selected) return;
    setPanelError(null);
    try {
      await api(`/api/clusters/${selected.id}/members/${m.user_id}`, {
        method: "DELETE",
      });
      await Promise.all([loadMembers(selected.id), load()]);
    } catch (err) {
      setPanelError(err instanceof Error ? err.message : "failed to remove member");
    }
  }

  const isOwner = selected?.role === "owner";

  return (
    <div>
      <div className="page-head">
        <div>
          <h1>Clusters</h1>
          <p className="page-sub">
            {clusters ? `${clusters.length} cluster${clusters.length === 1 ? "" : "s"}` : "…"}
          </p>
        </div>
        <div className="page-actions">
          <input
            className="search-box"
            type="search"
            placeholder="Search clusters…"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            aria-label="Search clusters"
          />
          <button onClick={() => { setCreateOpen(true); setDialogError(null); }}>
            New cluster
          </button>
        </div>
      </div>

      {error && <div className="form-error" role="alert">{error}</div>}
      {notice && <div className="notice" role="status">{notice}</div>}

      {clusters && (
        <div className="cluster-layout">
          <div className="cluster-list">
            {filtered.map((c) => (
              <div
                key={c.id}
                className={`cluster-item${selected?.id === c.id ? " active" : ""}`}
                onClick={() => setSelectedId(c.id)}
              >
                <div className="cluster-item-top">
                  <span className="cluster-name">{c.name}</span>
                  {c.personal && <span className="tag tag-personal">personal</span>}
                </div>
                <div className="cluster-meta">
                  <span className="role-chip">{c.role ?? "—"}</span>
                  <span>{c.memberCount ?? "–"} member{(c.memberCount ?? 0) === 1 ? "" : "s"}</span>
                  <span>{c.nodeCount ?? "–"} node{(c.nodeCount ?? 0) === 1 ? "" : "s"}</span>
                </div>
              </div>
            ))}
            {filtered.length === 0 && (
              <div className="empty">{query ? "No match." : "No clusters yet."}</div>
            )}
          </div>

          {selected ? (
            <div className="card">
              <div className="detail-head">
                <div className="detail-title">
                  <h2>{selected.name}</h2>
                  {selected.personal && <span className="tag tag-personal">personal</span>}
                </div>
                <div className="detail-actions">
                  <Link className="btn-link" to={`/nodes?cluster=${encodeURIComponent(selected.name)}`}>
                    View nodes →
                  </Link>
                  {isOwner && (
                    <>
                      <button
                        className="secondary small"
                        onClick={() => {
                          setRenameTarget(selected);
                          setRenameValue(selected.name);
                          setDialogError(null);
                        }}
                      >
                        Rename
                      </button>
                      <button
                        className="danger small"
                        onClick={() => { setDeleteTarget(selected); setDialogError(null); }}
                      >
                        Delete
                      </button>
                    </>
                  )}
                </div>
              </div>
              <div className="detail-meta">
                <span className="role-chip">my role: {selected.role ?? "—"}</span>
                <span className="mono dim">{selected.id}</span>
              </div>

              <h2>Members</h2>
              {isOwner && (
                <form className="member-add" onSubmit={onAddMember}>
                  <label>
                    Add by email
                    <input
                      type="email"
                      value={memberEmail}
                      onChange={(e) => setMemberEmail(e.target.value)}
                      placeholder="teammate@example.com"
                      autoComplete="off"
                      required
                    />
                  </label>
                  <label className="role-sel">
                    Role
                    <select
                      value={memberRole}
                      onChange={(e) => setMemberRole(e.target.value as MemberDTO["role"])}
                    >
                      {ROLES.map((r) => (
                        <option key={r} value={r}>{r}</option>
                      ))}
                    </select>
                  </label>
                  <button type="submit" disabled={panelBusy || !memberEmail}>
                    {panelBusy ? "Adding…" : "Add"}
                  </button>
                </form>
              )}
              {panelError && (
                <div className="form-error" role="alert" style={{ marginBottom: 12 }}>
                  {panelError}
                </div>
              )}
              {members ? (
                <table>
                  <thead>
                    <tr>
                      <th>Email</th>
                      <th>Display name</th>
                      <th>Role</th>
                      {isOwner && <th></th>}
                    </tr>
                  </thead>
                  <tbody>
                    {members.map((m) => (
                      <tr key={m.user_id}>
                        <td>{m.email}</td>
                        <td className="dim">{m.display_name}</td>
                        <td>
                          {isOwner ? (
                            <select
                              value={m.role}
                              aria-label={`Role of ${m.email}`}
                              onChange={(e) =>
                                void onRoleChange(m, e.target.value as MemberDTO["role"])
                              }
                            >
                              {ROLES.map((r) => (
                                <option key={r} value={r}>{r}</option>
                              ))}
                            </select>
                          ) : (
                            <span className="role-chip">{m.role}</span>
                          )}
                        </td>
                        {isOwner && (
                          <td>
                            <button className="secondary small" onClick={() => void onRemoveMember(m)}>
                              Remove
                            </button>
                          </td>
                        )}
                      </tr>
                    ))}
                  </tbody>
                </table>
              ) : (
                <div className="loading">Loading members…</div>
              )}
            </div>
          ) : (
            <div className="card">
              <div className="empty" style={{ padding: "40px 24px" }}>
                {query ? "No cluster matches your search." : "Create your first cluster."}
              </div>
            </div>
          )}
        </div>
      )}

      {createOpen && (
        <Modal title="New cluster" onClose={() => setCreateOpen(false)}>
          <form className="form-stack" onSubmit={onCreate} style={{ maxWidth: "none" }}>
            <label>
              Name
              <input
                value={newName}
                onChange={(e) => setNewName(e.target.value)}
                autoFocus
                autoComplete="off"
                required
              />
            </label>
            {dialogError && <div className="form-error" role="alert">{dialogError}</div>}
            <div className="modal-foot">
              <button type="button" className="secondary" onClick={() => setCreateOpen(false)}>
                Cancel
              </button>
              <button type="submit" disabled={dialogBusy || !newName.trim()}>
                {dialogBusy ? "Creating…" : "Create"}
              </button>
            </div>
          </form>
        </Modal>
      )}

      {renameTarget && (
        <Modal title="Rename cluster" onClose={() => setRenameTarget(null)}>
          <form className="form-stack" onSubmit={onRename} style={{ maxWidth: "none" }}>
            <label>
              New name
              <input
                value={renameValue}
                onChange={(e) => setRenameValue(e.target.value)}
                autoFocus
                autoComplete="off"
                required
              />
            </label>
            {dialogError && <div className="form-error" role="alert">{dialogError}</div>}
            <div className="modal-foot">
              <button type="button" className="secondary" onClick={() => setRenameTarget(null)}>
                Cancel
              </button>
              <button type="submit" disabled={dialogBusy || !renameValue.trim()}>
                {dialogBusy ? "Saving…" : "Save"}
              </button>
            </div>
          </form>
        </Modal>
      )}

      {deleteTarget && (
        <Modal title="Delete cluster" onClose={() => setDeleteTarget(null)}>
          <p style={{ margin: 0 }}>
            Delete <strong>{deleteTarget.name}</strong>? The cluster must be empty —
            move or remove its nodes first. This cannot be undone.
          </p>
          {dialogError && <div className="form-error" role="alert">{dialogError}</div>}
          <div className="modal-foot">
            <button className="secondary" onClick={() => setDeleteTarget(null)}>
              Cancel
            </button>
            <button className="danger" disabled={dialogBusy} onClick={() => void onDelete()}>
              {dialogBusy ? "Deleting…" : "Delete cluster"}
            </button>
          </div>
        </Modal>
      )}
    </div>
  );
}
