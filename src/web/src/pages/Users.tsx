import { useCallback, useEffect, useState, type FormEvent } from "react";
import { api, APIError } from "../api";
import Modal from "../components/Modal";
import type { UserDTO } from "../types";

/** "42s ago" / "5m ago" / local datetime — null means never logged in. */
function lastLogin(iso: string | null | undefined): string {
  if (!iso) return "never";
  const ms = Date.now() - Date.parse(iso);
  if (Number.isNaN(ms)) return "never";
  if (ms < 60_000) return `${Math.max(1, Math.floor(ms / 1000))}s ago`;
  if (ms < 3_600_000) return `${Math.floor(ms / 60_000)}m ago`;
  if (ms < 86_400_000) return `${Math.floor(ms / 3_600_000)}h ago`;
  return new Date(iso).toLocaleString();
}

/**
 * User management（2026-09-14 三轮迭代）：列表只呈现事实——显示名（含
 * admin 徽章）/ 邮箱 / 上次登录；全部管理动作（改名、管理员设置、重置密码、
 * 删除用户）收进单一 "Manage" 对话框。admin-only（users.is_admin，服务端
 * 强制）。删除带两步确认；最后 admin / 名下有节点由服务端保护、错误内联。
 */
export default function Users() {
  const [users, setUsers] = useState<UserDTO[] | null>(null);
  const [forbidden, setForbidden] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

  const [query, setQuery] = useState("");

  // 创建对话框。
  const [createOpen, setCreateOpen] = useState(false);
  const [email, setEmail] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [password, setPassword] = useState("");
  const [createBusy, setCreateBusy] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);

  // 管理对话框（单用户全部管理动作）。
  const [target, setTarget] = useState<UserDTO | null>(null);
  const [nameValue, setNameValue] = useState("");
  const [pwdValue, setPwdValue] = useState("");
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [dialogBusy, setDialogBusy] = useState(false);
  const [dialogError, setDialogError] = useState<string | null>(null);
  const [dialogNotice, setDialogNotice] = useState<string | null>(null);

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

  function openManage(u: UserDTO) {
    setTarget(u);
    setNameValue(u.display_name);
    setPwdValue("");
    setConfirmDelete(false);
    setDialogError(null);
    setDialogNotice(null);
  }

  async function onCreateUser(e: FormEvent) {
    e.preventDefault();
    setCreateBusy(true);
    setCreateError(null);
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
      setCreateError(err instanceof Error ? err.message : "failed to create user");
    } finally {
      setCreateBusy(false);
    }
  }

  async function patch(body: Record<string, unknown>, okNotice: string) {
    if (!target) return false;
    setDialogBusy(true);
    setDialogError(null);
    setDialogNotice(null);
    try {
      await api(`/api/users/${target.id}`, {
        method: "PATCH",
        body: JSON.stringify(body),
      });
      setDialogNotice(okNotice);
      await load();
      return true;
    } catch (err) {
      setDialogError(err instanceof Error ? err.message : "request failed");
      return false;
    } finally {
      setDialogBusy(false);
    }
  }

  async function onDeleteUser() {
    if (!target) return;
    setDialogBusy(true);
    setDialogError(null);
    try {
      await api(`/api/users/${target.id}`, { method: "DELETE" });
      setNotice(`User ${target.email} deleted`);
      setTarget(null);
      await load();
    } catch (err) {
      setDialogError(err instanceof Error ? err.message : "failed to delete user");
      setConfirmDelete(false);
    } finally {
      setDialogBusy(false);
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
          <button onClick={() => { setCreateOpen(true); setCreateError(null); }}>
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
                <th>Display name</th>
                <th>Email</th>
                <th>Last login</th>
                <th></th>
              </tr>
            </thead>
            <tbody>
              {filtered.map((u) => (
                <tr key={u.id}>
                  <td>
                    <span className="user-email">{u.display_name || "—"}</span>
                    {u.is_admin && <span className="tag tag-admin">admin</span>}
                  </td>
                  <td>{u.email}</td>
                  <td className="dim" title={u.last_login_at ?? undefined}>
                    {lastLogin(u.last_login_at)}
                  </td>
                  <td className="cell-actions">
                    <button className="secondary small" onClick={() => openManage(u)}>
                      Manage…
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
            {createError && <div className="form-error" role="alert">{createError}</div>}
            <div className="modal-foot">
              <button type="button" className="secondary" onClick={() => setCreateOpen(false)}>
                Cancel
              </button>
              <button type="submit" disabled={createBusy}>
                {createBusy ? "Creating…" : "Create user"}
              </button>
            </div>
          </form>
        </Modal>
      )}

      {target && (
        <Modal title="Manage user" onClose={() => setTarget(null)}>
          <div className="manage-head">
            <span className="user-email">{target.display_name || target.email}</span>
            <span className="dim">{target.email}</span>
            {target.is_admin && <span className="tag tag-admin">admin</span>}
          </div>

          <form
            className="manage-section"
            onSubmit={async (e) => {
              e.preventDefault();
              const ok = await patch({ display_name: nameValue }, "Display name saved");
              if (ok) setTarget({ ...target, display_name: nameValue });
            }}
          >
            <label>
              Display name
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
                Platform admin
                <span className="dim" style={{ marginLeft: 8 }}>
                  {target.is_admin ? "grants full console access" : "not an admin"}
                </span>
              </span>
              <button
                className="secondary small"
                disabled={dialogBusy}
                onClick={async () => {
                  const ok = await patch(
                    { is_admin: !target.is_admin },
                    target.is_admin ? "Admin access revoked" : "Admin access granted",
                  );
                  if (ok) setTarget({ ...target, is_admin: !target.is_admin });
                }}
              >
                {target.is_admin ? "Revoke" : "Make admin"}
              </button>
            </div>
          </div>

          <form
            className="manage-section"
            onSubmit={async (e) => {
              e.preventDefault();
              const ok = await patch({ password: pwdValue }, "Password reset (next login)");
              if (ok) { setPwdValue(""); setTarget(null); }
            }}
          >
            <label>
              Reset password
              <input
                type="password"
                value={pwdValue}
                onChange={(e) => setPwdValue(e.target.value)}
                placeholder="New password"
                autoComplete="new-password"
                required
              />
            </label>
            <button type="submit" className="secondary small" disabled={dialogBusy || !pwdValue}>
              Reset
            </button>
          </form>

          <div className="manage-section manage-danger">
            <div className="manage-line">
              <span className="dim" style={{ fontSize: 13 }}>
                Delete user — their clusters must contain no nodes.
              </span>
              {confirmDelete ? (
                <button className="danger small" disabled={dialogBusy} onClick={() => void onDeleteUser()}>
                  {dialogBusy ? "Deleting…" : "Confirm delete"}
                </button>
              ) : (
                <button className="danger small" disabled={dialogBusy} onClick={() => setConfirmDelete(true)}>
                  Delete…
                </button>
              )}
            </div>
          </div>

          {dialogError && <div className="form-error" role="alert">{dialogError}</div>}
          {dialogNotice && <div className="notice" role="status">{dialogNotice}</div>}
        </Modal>
      )}
    </div>
  );
}
