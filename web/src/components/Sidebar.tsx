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
      </nav>
      <div className="side-foot">
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
