import { useState, type FormEvent } from "react";
import { api } from "../api";
import { useAuth } from "../auth";
import type { UserDTO } from "../types";

/**
 * 个人信息页（设计 §3.3）：账号卡（email 只读 + display_name 自助修改）
 * 与改密卡（current/new/confirm）。改密成功不登出——JWT 保持有效。
 */
export default function Profile() {
  const { user, updateUser } = useAuth();
  const [displayName, setDisplayName] = useState(user?.display_name ?? "");
  const [savingName, setSavingName] = useState(false);
  const [nameNotice, setNameNotice] = useState("");
  const [nameError, setNameError] = useState("");

  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [savingPw, setSavingPw] = useState(false);
  const [pwNotice, setPwNotice] = useState("");
  const [pwError, setPwError] = useState("");

  async function saveProfile(e: FormEvent) {
    e.preventDefault();
    setSavingName(true);
    setNameNotice("");
    setNameError("");
    try {
      const res = await api<{ user: UserDTO }>("/api/auth/me", {
        method: "PATCH",
        body: JSON.stringify({ display_name: displayName }),
      });
      updateUser(res.user);
      setNameNotice("Saved");
    } catch (err) {
      setNameError(err instanceof Error ? err.message : String(err));
    } finally {
      setSavingName(false);
    }
  }

  async function changePassword(e: FormEvent) {
    e.preventDefault();
    if (newPassword !== confirmPassword) {
      setPwError("new passwords do not match");
      setPwNotice("");
      return;
    }
    setSavingPw(true);
    setPwError("");
    setPwNotice("");
    try {
      await api("/api/auth/password", {
        method: "POST",
        // 401=当前密码错误（服务端防枚举）而非会话过期：豁免自动登出，
        // 由 catch 走 setPwError 页内展示服务端错误
        on401: "throw",
        body: JSON.stringify({
          current_password: currentPassword,
          new_password: newPassword,
        }),
      });
      setPwNotice("Password updated");
      setCurrentPassword("");
      setNewPassword("");
      setConfirmPassword("");
    } catch (err) {
      setPwError(err instanceof Error ? err.message : String(err));
    } finally {
      setSavingPw(false);
    }
  }

  if (!user) return null; // LoginGuard 兜底，类型收窄

  return (
    <div>
      <h1>Profile</h1>

      <section className="card">
        <h2>Account</h2>
        <dl className="detail-grid">
          <dt>Email</dt>
          <dd>{user.email}</dd>
        </dl>
        <form className="form-stack" onSubmit={(e) => void saveProfile(e)}>
          <label>
            Display name
            <input
              name="display_name"
              value={displayName}
              onChange={(e) => setDisplayName(e.target.value)}
              placeholder="Shown next to your email"
            />
          </label>
          <button type="submit" disabled={savingName}>
            {savingName ? "Saving…" : "Save"}
          </button>
          {nameNotice && <span className="notice">{nameNotice}</span>}
          {nameError && <span className="form-error">{nameError}</span>}
        </form>
      </section>

      <section className="card">
        <h2>Change password</h2>
        <form className="form-stack" onSubmit={(e) => void changePassword(e)}>
          <label>
            Current password
            <input
              name="current_password"
              type="password"
              value={currentPassword}
              onChange={(e) => setCurrentPassword(e.target.value)}
              autoComplete="current-password"
            />
          </label>
          <label>
            New password
            <input
              name="new_password"
              type="password"
              value={newPassword}
              onChange={(e) => setNewPassword(e.target.value)}
              autoComplete="new-password"
            />
          </label>
          <label>
            Confirm new password
            <input
              name="confirm_password"
              type="password"
              value={confirmPassword}
              onChange={(e) => setConfirmPassword(e.target.value)}
              autoComplete="new-password"
            />
          </label>
          <button type="submit" disabled={savingPw}>
            {savingPw ? "Updating…" : "Update password"}
          </button>
          {pwNotice && <span className="notice">{pwNotice}</span>}
          {pwError && <span className="form-error">{pwError}</span>}
        </form>
      </section>
    </div>
  );
}
