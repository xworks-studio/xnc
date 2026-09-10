import { Outlet } from "react-router-dom";
import Sidebar from "./components/Sidebar";

/** Layout shell: sidebar navigation + routed content area. */
export default function App() {
  return (
    <div className="shell">
      <Sidebar />
      <main className="content">
        <Outlet />
      </main>
    </div>
  );
}
