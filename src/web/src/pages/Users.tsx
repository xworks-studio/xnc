import { useCallback, useEffect, useState, type FormEvent } from "react";
import { api, APIError } from "../api";
import type { ClusterDTO, MemberDTO, UserDTO } from "../types";

const ROLES: MemberDTO["role"][] = ["viewer", "operator", "owner"];

/**
 * User management (admin-only — "admin" = owner of at least one cluster,
 * enforced server-side). GET /api/users → 403 FORBIDDEN for non-admins,
 * rendered as "Admin access required" instead of the page.
 *
 * Three sections: user list, create-user form (no self-registration),
 * cluster membership assignment (owner-only server-side).
 */
export default function Users() {
  const [users, setUsers] = useState<UserDTO[] | null>(null);
  const [forbidden, setForbidden] = useState(false);
  const [error, setError] = useState<string | null>(null);

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
  const [notice, setNotice] = useState<string | null>(null);

  // Membership assignment
  const [clusters, setClusters] = useState<ClusterDTO[]>([]);
  const [memberUserId, setMemberUserId] = useState("");
  const [memberClusterId, setMemberClusterId] = useState("");
  const [memberRole, setMemberRole] = useState<MemberDTO["role"]>("viewer");
  const [memberBusy, setMemberBusy] = useState(false);
  const [memberError, setMemberError] = useState<string | null>(null);
  const [members, setMembers] = useState<MemberDTO[] | null>(null);

  const loadClusters = useCallback(async () => {
    try {
      setClusters(await api<ClusterDTO[]>("/api/clusters"));
    } catch {
      // Dropdown stays empty; membership form is unusable but list still works.
    }
  }, []);

  useEffect(() => {
    void loadClusters();
  }, [loadClusters]);

  const loadMembers = useCallback(async (clusterId: string) => {
    if (!clusterId) {
      setMembers(null);
      return;
    }
    try {
      setMembers(await api<MemberDTO[]>(`/api/clusters/${clusterId}/members`));
    } catch {
      setMembers(null);
    }
  }, []);

  useEffect(() => {
    void loadMembers(memberClusterId);
  }, [memberClusterId, loadMembers]);

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
      setNotice(`User ${created.email} created`);
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

  async function onAssignMember(e: FormEvent) {
    e.preventDefault();
    setMemberBusy(true);
    setMemberError(null);
    setNotice(null);
    try {
      await api<unknown>(`/api/clusters/${memberClusterId}/members`, {
        method: "POST",
        body: JSON.stringify({ user_id: memberUserId, role: memberRole }),
      });
      const user = users?.find((u) => u.id === memberUserId);
      const cluster = clusters.find((c) => c.id === memberClusterId);
      setNotice(`${user?.email ?? memberUserId} is now ${memberRole} of ${cluster?.name ?? memberClusterId}`);
      setMemberUserId("");
      await loadMembers(memberClusterId);
    } catch (err) {
      if (err instanceof APIError && err.code === "FORBIDDEN") {
        setMemberError("permission denied (cluster owner required)");
      } else {
        setMemberError(err instanceof Error ? err.message : "failed to assign member");
      }
    } finally {
      setMemberBusy(false);
    }
  }

  if (forbidden) {
    return (
      <div>
        <h1>Users</h1>
        <div className="card">
          <p className="dim" style={{ margin: 0 }}>
            Admin access required. Ask a cluster owner to manage users.
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
                <th>ID</th>
              </tr>
            </thead>
            <tbody>
              {users.map((u) => (
                <tr key={u.id}>
                  <td>{u.email}</td>
                  <td className="dim">{u.display_name}</td>
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

      <h2>Cluster membership</h2>
      <div className="card">
        <form className="form-inline" onSubmit={onAssignMember}>
          <label>
            User
            <select value={memberUserId} onChange={(e) => setMemberUserId(e.target.value)} required>
              <option value="">Select user…</option>
              {users?.map((u) => (
                <option key={u.id} value={u.id}>
                  {u.email}
                </option>
              ))}
            </select>
          </label>
          <label>
            Cluster
            <select value={memberClusterId} onChange={(e) => setMemberClusterId(e.target.value)} required>
              <option value="">Select cluster…</option>
              {clusters.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
            </select>
          </label>
          <label>
            Role
            <select value={memberRole} onChange={(e) => setMemberRole(e.target.value as MemberDTO["role"])}>
              {ROLES.map((r) => (
                <option key={r} value={r}>
                  {r}
                </option>
              ))}
            </select>
          </label>
          <button type="submit" disabled={memberBusy || !memberUserId || !memberClusterId}>
            {memberBusy ? "Assigning…" : "Assign"}
          </button>
        </form>
        {memberError && <div className="form-error" role="alert" style={{ marginTop: 12 }}>{memberError}</div>}
        {members && (
          <table style={{ marginTop: 16 }}>
            <thead>
              <tr>
                <th>Email</th>
                <th>Display name</th>
                <th>Role</th>
              </tr>
            </thead>
            <tbody>
              {members.map((m) => (
                <tr key={m.user_id}>
                  <td>{m.email}</td>
                  <td className="dim">{m.display_name}</td>
                  <td className="dim">{m.role}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}
