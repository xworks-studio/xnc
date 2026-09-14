import { useCallback, useEffect, useState, type FormEvent } from "react";
import { api, APIError } from "../api";
import Modal from "../components/Modal";
import type { UserDTO } from "../types";

/**
 * User management（2026-09-14 重设计）：admin-only（users.is_admin，服务端
 * 强制；非 admin 渲染 "Admin access required"）。页头 = 标题 + 计数 + 搜索 +
 * "New user"（低频创建收进对话框）；表格 = 邮箱（下行显示名）/ ADMIN 徽章（
 * 状态与动作分离：徽章示状态，小按钮做授予与撤销）/ 行内改名。最后 admin
 * 保护由服务端 400 LAST_ADMIN 兜底，错误内联展示。
 */
export default function Users() {
  const [users, setUsers] = useState<UserDTO[] | null>(null);
  const [forbidden, setForbidden] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const [query, setQuery] = useState("");

  // 对话框：创建 / 改名。
  const [createOpen, setCreateOpen] = useState(false);
  const [email, setEmail] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [password, setPassword] = useState("");
  const [editTarget, setEditTarget] = useState<UserDTO | null>(null);
  const [editName, setEditName] = useState("");
  const [dialogBusy, setDialogBusy] = useState(false);
  const [dialogError, setDialogError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      setUsers(await api<UserDTO[]>("/api/users"));
      setError(null);
    } catch (err) {
      if (err instanceof APIError && err.code === "FORBIDDEN") setForbidden(true);
      else setError(err instanceof Error ? err.message : "failed to load users");
    }
  }, []);

  useEffect(() => {
    void load();
  }, [load]);

  const usersList = users ?? [];
  const q = query.trim().toLowerCase();
  const filtered = q
    ? usersList.filter(
        (u) =>
          u.email.toLowerCase().includes(q) ||
          u.display_name.toLowerCase().includes(q),
      )
    : usersList;

  async function onCreateUser(e: FormEvent) {
    e.preventDefault();
    setDialogBusy(true);
    setDialogError(null);
    try {
      const created = await api<UserDTO>("/api/users", {
        method: "POST",
        body: JSON.stringify({ email, display_name: displayName, password }),
      });
      setNotice(`User ${created.email} created (personal default cluster ready)`);
      setCreateOpen(false);
      setEmail("");
      setDisplayName("");
      setPassword("");
      await load();
    } catch (err) {
      setDialogError(err instanceof Error ? err.message : "failed to create user");
    } finally {
      setDialogBusy(false);
    }
  }

  async function onEditName(e: FormEvent) {
    e.preventDefault();
    if (!editTarget) return;
    setDialogBusy(true);
    setDialogError(null);
    try {
      await api(`/api/users/${editTarget.id}`, {
        method: "PATCH",
        body: JSON.stringify({ display_name: editName }),
      });
      setNotice(`Renamed ${editTarget.email} to ${editName}`);
      setEditTarget(null);
      await load();
    } catch (err) {
      setDialogError(err instanceof Error ? err.message : "failed to rename user");
    } finally {
      setDialogBusy(false);
    }
  }

  async function onToggleAdmin(u: UserDTO) {
    setError(null);
    setNotice(null);
    try {
      await api(`/api/users/${u.id}`, {
        method: "PATCH",
        body: JSON.stringify({ is_admin: !u.is_admin }),
      });
      setNotice(`${u.email} is ${!u.is_admin ? "now an admin" : "no longer an admin"}`);
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to update admin flag");
    }
  }

  if (forbidden) {
    return (
      <div>
        <h1>Users</h1>
        <div className="card">
          <p className="dim" style={{ margin: 0 }}>
            Admin access required.
          </p>
        </div>
      </div>
    );
  }

  return (
    <div>
      <div className="page-head">
        <div>
          <h1>Users</h1>
          <p className="page-sub">
            {users ? `${users.length} user${users.length === 1 ? "" : "s"}` : "…"}
          </p>
        </div>
        <div className="page-actions">
          <input
            className="search-box"
            type="search"
            placeholder="Search users…"
            value={query}
            onChange={(e) => setQuery(e.target.value)}
            aria-label="Search users"
          />
          <button onClick={() => { setCreateOpen(true); setDialogError(null); }}>
            New user
          </button>
        </div>
      </div>

      {error && <div className="form-error" role="alert">{error}</div>}
      {notice && <div className="notice" role="status">{notice}</div>}

      {users && (
        <div className="card flush">
          <table>
            <thead>
              <tr>
                <th>User</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {filtered.map((u) => (
                <tr key={u.id}>
                  <td>
                    <span className="user-email">{u.email}</span>
                    {u.display_name && (
                      <span className="user-name-inline dim">{u.display_name}</span>
                    )}
                    {u.is_admin && <span className="tag tag-admin">admin</span>}
                  </td>
                  <td className="cell-actions">
                    <button
                      className="secondary small"
                      onClick={() => {
                        setEditTarget(u);
                        setEditName(u.display_name);
                        setDialogError(null);
                      }}
                    >
                      Rename
                    </button>
                    <button
                      className="secondary small"
                      onClick={() => void onToggleAdmin(u)}
                    >
                      {u.is_admin ? "Revoke admin" : "Make admin"}
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {filtered.length === 0 && (
            <div className="empty">{query ? "No user matches your search." : "No users."}</div>
          )}
        </div>
      )}

      {createOpen && (
        <Modal title="New user" onClose={() => setCreateOpen(false)}>
          <form className="form-stack" onSubmit={onCreateUser} style={{ maxWidth: "none" }}>
            <label>
              Email
              <input
                type="email"
                value={email}
                onChange={(e) => setEmail(e.target.value)}
                autoFocus
                autoComplete="off"
                required
              />
            </label>
            <label>
              Display name
              <input
                type="text"
                value={displayName}
                onChange={(e) => setDisplayName(e.target.value)}
                autoComplete="off"
              />
            </label>
            <label>
              Password
              <input
                type="password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                autoComplete="new-password"
                required
              />
            </label>
            <p className="dim" style={{ margin: 0, fontSize: 13 }}>
              A personal default cluster is created automatically.
            </p>
            {dialogError && <div className="form-error" role="alert">{dialogError}</div>}
            <div className="modal-foot">
              <button type="button" className="secondary" onClick={() => setCreateOpen(false)}>
                Cancel
              </button>
              <button type="submit" disabled={dialogBusy}>
                {dialogBusy ? "Creating…" : "Create user"}
              </button>
            </div>
          </form>
        </Modal>
      )}

      {editTarget && (
        <Modal title="Rename user" onClose={() => setEditTarget(null)}>
          <form className="form-stack" onSubmit={onEditName} style={{ maxWidth: "none" }}>
            <p className="dim" style={{ margin: 0, fontSize: 13 }}>
              {editTarget.email}
            </p>
            <label>
              Display name
              <input
                value={editName}
                onChange={(e) => setEditName(e.target.value)}
                autoFocus
                autoComplete="off"
              />
            </label>
            {dialogError && <div className="form-error" role="alert">{dialogError}</div>}
            <div className="modal-foot">
              <button type="button" className="secondary" onClick={() => setEditTarget(null)}>
                Cancel
              </button>
              <button type="submit" disabled={dialogBusy}>
                {dialogBusy ? "Saving…" : "Save"}
              </button>
            </div>
          </form>
        </Modal>
      )}
    </div>
  );
}
