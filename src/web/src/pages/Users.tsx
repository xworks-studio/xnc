import { useCallback, useEffect, useState, type FormEvent } from "react";
import { api, APIError } from "../api";
import type { UserDTO } from "../types";

/**
 * User management (admin-only, users.is_admin since 0005; enforced
 * server-side). GET /api/users → 403 FORBIDDEN for non-admins, rendered as
 * "Admin access required" instead of the page.
 *
 * Two sections: user list (with is_admin toggle — last admin protected
 * server-side, 400 LAST_ADMIN) and create-user form (no self-registration;
 * new users get a personal default cluster automatically). Cluster
 * membership management lives on the Clusters page (by email, owner-only).
 */
export default function Users() {
  const [users, setUsers] = useState<UserDTO[] | null>(null);
  const [forbidden, setForbidden] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notice, setNotice] = useState<string | null>(null);

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

  // Create-user form
  const [email, setEmail] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [password, setPassword] = useState("");
  const [createBusy, setCreateBusy] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);

  async function onCreateUser(e: FormEvent) {
    e.preventDefault();
    setCreateBusy(true);
    setCreateError(null);
    setNotice(null);
    try {
      const created = await api<UserDTO>("/api/users", {
        method: "POST",
        body: JSON.stringify({ email, display_name: displayName, password }),
      });
      setNotice(`User ${created.email} created (personal default cluster ready)`);
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
      <h1>Users</h1>
      {error && <div className="form-error" role="alert">{error}</div>}
      {notice && <div className="notice" role="status">{notice}</div>}

      {users && (
        <div className="card flush">
          <table>
            <thead>
              <tr>
                <th>Email</th>
                <th>Display name</th>
                <th>Admin</th>
                <th>ID</th>
              </tr>
            </thead>
            <tbody>
              {users.map((u) => (
                <tr key={u.id}>
                  <td>{u.email}</td>
                  <td className="dim">{u.display_name}</td>
                  <td>
                    <button type="button" onClick={() => void onToggleAdmin(u)}>
                      {u.is_admin ? "Revoke admin" : "Make admin"}
                    </button>
                  </td>
                  <td className="mono dim">{u.id}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {users.length === 0 && <div className="empty">No users.</div>}
        </div>
      )}

      <h2>Create user</h2>
      <div className="card">
        <form className="form-stack" onSubmit={onCreateUser}>
          <label>
            Email
            <input
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
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
          {createError && <div className="form-error" role="alert">{createError}</div>}
          <button type="submit" disabled={createBusy}>
            {createBusy ? "Creating…" : "Create user"}
          </button>
        </form>
      </div>
    </div>
  );
}
