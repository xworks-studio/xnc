import { NavLink, Outlet, useNavigate } from "react-router-dom";
import { useAuth } from "./auth";

/** Layout shell: sidebar navigation + routed content area. */
export default function App() {
  const { user, logout } = useAuth();
  const navigate = useNavigate();

  function onLogout() {
    logout();
    navigate("/login");
  }

  return (
    <div className="shell">
      <aside className="sidebar">
        <div className="brand">XNC</div>
        <nav>
          <NavLink to="/clusters">Clusters</NavLink>
          <NavLink to="/nodes">Nodes</NavLink>
          <NavLink to="/users">Users</NavLink>
        </nav>
        <div className="side-foot">
          {user && <div className="email" title={user.email}>{user.email}</div>}
          <button type="button" className="secondary" onClick={onLogout}>
            Sign out
          </button>
        </div>
      </aside>
      <main className="content">
        <Outlet />
      </main>
    </div>
  );
}
