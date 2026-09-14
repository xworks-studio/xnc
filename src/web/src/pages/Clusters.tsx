import { Fragment, useCallback, useEffect, useState, type FormEvent } from "react";
import { useNavigate } from "react-router-dom";
import { api } from "../api";
import type { ClusterDTO, MemberDTO } from "../types";

const ROLES: MemberDTO["role"][] = ["viewer", "operator", "owner"];

/**
 * Cluster management (0005)：建/改名/删 + 成员面板（按 email 加、改角色、
 * 移除）。写操作 owner-only（服务端强制；非 owner 按钮 403 报错内联显示）。
 * 行点击名称区跳转按该 cluster 过滤的节点列表。
 */
export default function Clusters() {
  const navigate = useNavigate();
  const [clusters, setClusters] = useState<ClusterDTO[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  // Members panel state（当前展开的 cluster id；null = 收起）。
  const [openId, setOpenId] = useState<string | null>(null);
  const [members, setMembers] = useState<MemberDTO[] | null>(null);
  const [memberEmail, setMemberEmail] = useState("");
  const [memberRole, setMemberRole] = useState<MemberDTO["role"]>("operator");
  const [panelBusy, setPanelBusy] = useState(false);
  const [panelError, setPanelError] = useState<string | null>(null);

  // Create form。
  const [newName, setNewName] = useState("");
  const [createBusy, setCreateBusy] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);

  // Rename inline（正在改名的 cluster id + 输入值）。
  const [renameId, setRenameId] = useState<string | null>(null);
  const [renameValue, setRenameValue] = useState("");

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

  function togglePanel(c: ClusterDTO) {
    if (openId === c.id) {
      setOpenId(null);
      setMembers(null);
      return;
    }
    setOpenId(c.id);
    setPanelError(null);
    setMemberEmail("");
    void loadMembers(c.id);
  }

  async function onCreate(e: FormEvent) {
    e.preventDefault();
    setCreateBusy(true);
    setCreateError(null);
    setNotice(null);
    try {
      await api<ClusterDTO>("/api/clusters", {
        method: "POST",
        body: JSON.stringify({ name: newName }),
      });
      setNotice(`Cluster ${newName} created`);
      setNewName("");
      await load();
    } catch (err) {
      setCreateError(err instanceof Error ? err.message : "failed to create cluster");
    } finally {
      setCreateBusy(false);
    }
  }

  async function onRename(c: ClusterDTO) {
    if (!renameValue.trim()) return;
    try {
      await api(`/api/clusters/${c.id}`, {
        method: "PATCH",
        body: JSON.stringify({ name: renameValue.trim() }),
      });
      setNotice(`Renamed to ${renameValue.trim()}`);
      setRenameId(null);
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to rename cluster");
    }
  }

  async function onDelete(c: ClusterDTO) {
    if (!window.confirm(`Delete cluster ${c.name}? (must be empty)`)) return;
    try {
      await api(`/api/clusters/${c.id}`, { method: "DELETE" });
      setNotice(`Cluster ${c.name} deleted`);
      setOpenId(null);
      await load();
    } catch (err) {
      // 409 CLUSTER_NOT_EMPTY 常见：提示引导先移出/删除节点。
      setError(err instanceof Error ? err.message : "failed to delete cluster");
    }
  }

  async function onAddMember(e: FormEvent, c: ClusterDTO) {
    e.preventDefault();
    setPanelBusy(true);
    setPanelError(null);
    try {
      await api(`/api/clusters/${c.id}/members`, {
        method: "POST",
        body: JSON.stringify({ email: memberEmail, role: memberRole }),
      });
      setMemberEmail("");
      await loadMembers(c.id);
    } catch (err) {
      setPanelError(err instanceof Error ? err.message : "failed to add member");
    } finally {
      setPanelBusy(false);
    }
  }

  async function onRoleChange(c: ClusterDTO, m: MemberDTO, role: MemberDTO["role"]) {
    setPanelError(null);
    try {
      await api(`/api/clusters/${c.id}/members/${m.user_id}`, {
        method: "PATCH",
        body: JSON.stringify({ role }),
      });
      await loadMembers(c.id);
    } catch (err) {
      setPanelError(err instanceof Error ? err.message : "failed to change role");
    }
  }

  async function onRemoveMember(c: ClusterDTO, m: MemberDTO) {
    setPanelError(null);
    try {
      await api(`/api/clusters/${c.id}/members/${m.user_id}`, { method: "DELETE" });
      await loadMembers(c.id);
    } catch (err) {
      setPanelError(err instanceof Error ? err.message : "failed to remove member");
    }
  }

  return (
    <div>
      <h1>Clusters</h1>
      {error && <div className="form-error" role="alert">{error}</div>}
      {notice && <div className="notice" role="status">{notice}</div>}

      <h2>Create cluster</h2>
      <div className="card">
        <form className="form-inline" onSubmit={onCreate}>
          <label>
            Name
            <input
              type="text"
              value={newName}
              onChange={(e) => setNewName(e.target.value)}
              autoComplete="off"
              required
            />
          </label>
          <button type="submit" disabled={createBusy || !newName.trim()}>
            {createBusy ? "Creating…" : "Create"}
          </button>
        </form>
        {createError && <div className="form-error" role="alert" style={{ marginTop: 12 }}>{createError}</div>}
      </div>

      {clusters && (
        <div className="card flush">
          <table>
            <thead>
              <tr>
                <th>Name</th>
                <th>My role</th>
                <th>ID</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {clusters.map((c) => (
                <Fragment key={c.id}>
                  <tr>
                    <td>
                      <span
                        className="clickable"
                        onClick={() => navigate(`/nodes?cluster=${encodeURIComponent(c.name)}`)}
                      >
                        {c.name}
                      </span>
                      {c.personal && <span className="dim" style={{ marginLeft: 8 }}>(personal)</span>}
                    </td>
                    <td className="dim">{c.role ?? "—"}</td>
                    <td className="mono dim">{c.id}</td>
                    <td>
                      {c.role === "owner" && (
                        <span className="row-actions">
                          <button type="button" onClick={() => togglePanel(c)}>
                            {openId === c.id ? "Close members" : "Members"}
                          </button>
                          {renameId === c.id ? (
                            <>
                              <input
                                type="text"
                                value={renameValue}
                                onChange={(e) => setRenameValue(e.target.value)}
                                autoFocus
                                style={{ width: 140 }}
                              />
                              <button type="button" onClick={() => void onRename(c)}>Save</button>
                              <button type="button" onClick={() => setRenameId(null)}>Cancel</button>
                            </>
                          ) : (
                            <button
                              type="button"
                              onClick={() => {
                                setRenameId(c.id);
                                setRenameValue(c.name);
                              }}
                            >
                              Rename
                            </button>
                          )}
                          <button type="button" onClick={() => void onDelete(c)}>Delete</button>
                        </span>
                      )}
                    </td>
                  </tr>
                  {openId === c.id && (
                    <tr>
                      <td colSpan={4}>
                        <div style={{ padding: "4px 0" }}>
                          <form className="form-inline" onSubmit={(e) => void onAddMember(e, c)}>
                            <label>
                              Add member by email
                              <input
                                type="email"
                                value={memberEmail}
                                onChange={(e) => setMemberEmail(e.target.value)}
                                autoComplete="off"
                                required
                              />
                            </label>
                            <label>
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
                          {panelError && (
                            <div className="form-error" role="alert" style={{ marginTop: 8 }}>
                              {panelError}
                            </div>
                          )}
                          {members && (
                            <table style={{ marginTop: 12 }}>
                              <thead>
                                <tr>
                                  <th>Email</th>
                                  <th>Display name</th>
                                  <th>Role</th>
                                  <th></th>
                                </tr>
                              </thead>
                              <tbody>
                                {members.map((m) => (
                                  <tr key={m.user_id}>
                                    <td>{m.email}</td>
                                    <td className="dim">{m.display_name}</td>
                                    <td>
                                      <select
                                        value={m.role}
                                        onChange={(e) =>
                                          void onRoleChange(c, m, e.target.value as MemberDTO["role"])
                                        }
                                      >
                                        {ROLES.map((r) => (
                                          <option key={r} value={r}>{r}</option>
                                        ))}
                                      </select>
                                    </td>
                                    <td>
                                      <button type="button" onClick={() => void onRemoveMember(c, m)}>
                                        Remove
                                      </button>
                                    </td>
                                  </tr>
                                ))}
                              </tbody>
                            </table>
                          )}
                        </div>
                      </td>
                    </tr>
                  )}
                </Fragment>
              ))}
            </tbody>
          </table>
          {clusters.length === 0 && <div className="empty">No clusters — create one above.</div>}
        </div>
      )}
    </div>
  );
}
