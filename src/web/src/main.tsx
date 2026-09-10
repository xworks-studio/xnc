import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom";
import "./styles.css";
import App from "./App";
import { AuthProvider, LoginGuard } from "./auth";
import Login from "./pages/Login";
import Download from "./pages/Download";
import Clusters from "./pages/Clusters";
import Nodes from "./pages/Nodes";
import NodeDetail from "./pages/NodeDetail";
import Terminal from "./pages/Terminal";
import Users from "./pages/Users";
import Profile from "./pages/Profile";
import Monitor from "./pages/Monitor";
import DesktopLive from "./pages/DesktopLive";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <BrowserRouter>
      <AuthProvider>
        <Routes>
          <Route path="/login" element={<Login />} />
          {/* 公开安装器下载页（LoginGuard 之外，无侧栏独立布局） */}
          <Route path="/download" element={<Download />} />
          <Route
            path="/"
            element={
              <LoginGuard>
                <App />
              </LoginGuard>
            }
          >
            <Route index element={<Navigate to="/nodes" replace />} />
            <Route path="/clusters" element={<Clusters />} />
            <Route path="/nodes" element={<Nodes />} />
            <Route path="/nodes/:id" element={<NodeDetail />} />
            <Route path="/terminal/:id" element={<Terminal />} />
            <Route path="/users" element={<Users />} />
            <Route path="/profile" element={<Profile />} />
            <Route path="/monitor" element={<Monitor />} />
          </Route>
          {/* 远程桌面：沉浸式独立布局（无侧栏，占满视口），LoginGuard 保护 */}
          <Route
            path="/screen/:nodeId"
            element={
              <LoginGuard>
                <DesktopLive />
              </LoginGuard>
            }
          />
          <Route
            path="/desktop/:nodeId"
            element={
              <LoginGuard>
                <DesktopLive />
              </LoginGuard>
            }
          />
        </Routes>
      </AuthProvider>
    </BrowserRouter>
  </StrictMode>,
);
