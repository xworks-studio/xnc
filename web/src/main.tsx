import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, Navigate, Route, Routes } from "react-router-dom";
import "./styles.css";
import App from "./App";
import { AuthProvider, LoginGuard } from "./auth";
import Login from "./pages/Login";
import Clusters from "./pages/Clusters";
import Nodes from "./pages/Nodes";
import NodeDetail from "./pages/NodeDetail";
import Terminal from "./pages/Terminal";
import Users from "./pages/Users";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <BrowserRouter>
      <AuthProvider>
        <Routes>
          <Route path="/login" element={<Login />} />
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
          </Route>
        </Routes>
      </AuthProvider>
    </BrowserRouter>
  </StrictMode>,
);
