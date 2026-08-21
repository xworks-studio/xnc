import { NavLink, Outlet } from "react-router-dom";

/** Layout shell: sidebar navigation + routed content area. */
export default function App() {
  return (
    <div className="shell">
      <aside className="sidebar">
        <div className="brand">XNC</div>
        <nav>
          <NavLink to="/clusters">Clusters</NavLink>
          <NavLink to="/nodes">Nodes</NavLink>
          <NavLink to="/users">Users</NavLink>
        </nav>
      </aside>
      <main className="content">
        <Outlet />
      </main>
    </div>
  );
}
