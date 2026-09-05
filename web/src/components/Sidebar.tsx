import { useEffect, useState } from "react";
import { Link, NavLink, useNavigate } from "react-router-dom";
import { useAuth } from "../auth";

/**
 * Sidebar navigation. The Users link is visible to everyone; the API is the
 * single source of truth for admin rights (GET /api/users → 403 for
 * non-admins, surfaced on the Users page itself).
 */
export default function Sidebar() {
  const { user, logout } = useAuth();
  const navigate = useNavigate();
  const [serverVersion, setServerVersion] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    fetch("/api/health")
      .then(async (res) => {
        if (!res.ok) throw new Error(`HTTP ${res.status}`);
        return (await res.json()) as { version: string };
      })
      .then((h) => {
        if (!cancelled) setServerVersion(h.version);
      })
      .catch(() => {
        /* 版本小字失败静默（设计 §3.2），不打扰导航 */
      });
    return () => {
      cancelled = true;
    };
  }, []);

  function onLogout() {
    logout();
    navigate("/login");
  }

  return (
    <aside className="sidebar">
      <div className="brand">XNC</div>
      <nav>
        <NavLink to="/nodes">Nodes</NavLink>
        <NavLink to="/clusters">Clusters</NavLink>
        <NavLink to="/users">Users</NavLink>
        <NavLink to="/download">Download</NavLink>
        <NavLink to="/monitor">Monitor</NavLink>
      </nav>
      <div className="side-foot">
        {serverVersion && <div className="dim server-version">server v{serverVersion}</div>}
        {user && (
          <Link className="email" to="/profile" title={user.email}>
            {user.email}
          </Link>
        )}
        <button type="button" className="secondary" onClick={onLogout}>
          Sign out
        </button>
      </div>
    </aside>
  );
}
