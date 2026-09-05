# Web 个人信息 / 版本展示 / CLI 固定 server — 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 落地 spec 四项：登录态 Download 入口、侧栏 server 版本小字、个人信息页（改 display_name/密码）、CLI server 固定 xnc.app。

**Architecture:** server 侧在既有 `/api/auth` JWT 组内加两个自助端点（PATCH /me、POST /password，sqlc 两条 UPDATE）；web 侧加 `/profile` 页 + Sidebar 两处增强；cli 侧 `resolveServer` 加默认值兜底、`--server` 隐藏、登录编排去 server prompt。全部为既有流程内的增量改动，无迁移、无破坏性变更。

**Tech Stack:** Go（chi + sqlc + pgx，testcontainers 真 PG）/ React 19 + Vite + vitest（happy-dom）/ cobra CLI。

**Spec:** `docs/superpowers/specs/2026-09-05-web-profile-server-version-cli-fixed-server-design.md`

## Global Constraints

- 代码注释中文、提交信息英文 conventional commits；每个任务独立提交。
- server 测试走真 PG（testcontainers 自动拉起，需本机 Docker 运行）；命令 `cd server && go test ./internal/api/ -run <Test> -count=1`。
- web 测试：`cd web && npm run test`（vitest run，happy-dom）。
- 构建验证逐模块：`cd server && go build ./...`、`cd cli && go build .`（根目录不是 Go 模块）。
- sqlc 重新生成：`make sqlc`（等价 `cd server && sqlc generate`）；生成的 `server/internal/db/sqlc/*.go` 一并提交。
- CLI `--server` flag 只 MarkHidden 不删除（存量测试零改动，spec §3.4）。
- 改密后 JWT 保持有效（不吊销，spec §2 决策）；email 只读。
- 审计动作命名对齐既有 `域.动作` 惯例：`user.password_change`。
- 密码新规则（≥8 字符）只在改密端点执行；`POST /api/users` 创建端点不动（spec §2 非目标）。

---

### Task 1: server 自助档案端点（PATCH /api/auth/me + POST /api/auth/password）

**Files:**
- Modify: `server/internal/db/queries/users.sql`（追加两条查询）
- Regenerate: `server/internal/db/sqlc/`（`make sqlc`）
- Modify: `server/internal/api/auth_handlers.go`（两个 handler + 路由注册处同文件不改，路由在 `router.go`）
- Modify: `server/internal/api/router.go:96-99`（JWT 组内注册两路由）
- Test: `server/internal/api/auth_handlers_test.go`（追加）

**Interfaces:**
- Consumes: `auth.UserFrom(r.Context())` 返回 `sqlc.User`（中间件已加载全行，含 `PasswordHash`；`ID` 为 `uuid.UUID`）；`auth.HashPassword(p) (string, error)`、`auth.VerifyPassword(hash, p) bool`；`pgUUID(uuid)`、`auditInsertTimeout`、`mustJSON`（api 包既有）；`doJSON`/`decodeJSON`/`NewTestEnv`/`env.AdminToken`（测试既有，见 `user_handlers_test.go:14`、`testenv_test.go:40`）。
- Produces: `PATCH /api/auth/me` → `200 {"user":{id,email,display_name}}`（TrimSpace，≤64 rune，空=清除；非法 400；无 JWT 401）；`POST /api/auth/password` → `204`（current 错 401 "invalid credentials"；new <8 字符 400；缺字段 400）；审计行 `user.password_change`。web Task 2 消费此契约。

- [ ] **Step 1: sqlc 查询（先写，测试依赖生成的函数）**

在 `server/internal/db/queries/users.sql` 末尾追加：

```sql
-- name: UpdateUserDisplayName :execrows
UPDATE users SET display_name = $2 WHERE id = $1;

-- name: UpdateUserPassword :execrows
UPDATE users SET password_hash = $2 WHERE id = $1;
```

运行 `make sqlc`，确认 `server/internal/db/sqlc/users.sql.go` 生成
`UpdateUserDisplayName(ctx, UpdateUserDisplayNameParams{ID uuid.UUID, DisplayName string}) (int64, error)`
与 `UpdateUserPassword(ctx, UpdateUserPasswordParams{ID uuid.UUID, PasswordHash string}) (int64, error)`。

- [ ] **Step 2: 写失败测试**

`server/internal/api/auth_handlers_test.go` 追加（imports 需补 `context`、`strings`）：

```go
// TestProfileUpdateAndChangePassword — 自助档案端点（设计 §3.3）：
// display_name 自助修改（TrimSpace/长度/清除/鉴权）与改密（current 校验、
// 强度、生效后新旧密码翻转、审计行）。
func TestProfileUpdateAndChangePassword(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t) // bootstrap admin，初始密码 pw-123456

	// PATCH /api/auth/me：TrimSpace 生效。
	resp := doJSON(t, srv.URL, "PATCH", "/api/auth/me", admin,
		`{"display_name":"  Boss  "}`)
	require.Equal(t, 200, resp.StatusCode)
	var out struct {
		User struct {
			Email       string `json:"email"`
			DisplayName string `json:"display_name"`
		} `json:"user"`
	}
	require.NoError(t, decodeJSON(resp.Body, &out))
	assert.Equal(t, "Boss", out.User.DisplayName)

	// >64 字符 → 400。
	resp = doJSON(t, srv.URL, "PATCH", "/api/auth/me", admin,
		`{"display_name":"`+strings.Repeat("x", 65)+`"}`)
	assert.Equal(t, 400, resp.StatusCode)

	// 空 display_name 合法（清除显示名）。
	resp = doJSON(t, srv.URL, "PATCH", "/api/auth/me", admin, `{"display_name":""}`)
	require.Equal(t, 200, resp.StatusCode)

	// 未认证 → 401。
	resp = doJSON(t, srv.URL, "PATCH", "/api/auth/me", "", `{"display_name":"x"}`)
	assert.Equal(t, 401, resp.StatusCode)

	// POST /api/auth/password：current 错 → 401（与 login 同文案）。
	resp = doJSON(t, srv.URL, "POST", "/api/auth/password", admin,
		`{"current_password":"wrong-password","new_password":"pw-654321"}`)
	assert.Equal(t, 401, resp.StatusCode)

	// 新密码 <8 → 400。
	resp = doJSON(t, srv.URL, "POST", "/api/auth/password", admin,
		`{"current_password":"pw-123456","new_password":"short"}`)
	assert.Equal(t, 400, resp.StatusCode)

	// 缺字段 → 400。
	resp = doJSON(t, srv.URL, "POST", "/api/auth/password", admin,
		`{"current_password":"pw-123456"}`)
	assert.Equal(t, 400, resp.StatusCode)

	// 正确 current → 204；旧密码 login 401、新密码 login 200。
	resp = doJSON(t, srv.URL, "POST", "/api/auth/password", admin,
		`{"current_password":"pw-123456","new_password":"pw-654321"}`)
	require.Equal(t, 204, resp.StatusCode)

	respOld, err := http.Post(srv.URL+"/api/auth/login", "application/json",
		strings.NewReader(`{"email":"admin@t.local","password":"pw-123456"}`))
	require.NoError(t, err)
	defer respOld.Body.Close()
	assert.Equal(t, 401, respOld.StatusCode)

	respNew, err := http.Post(srv.URL+"/api/auth/login", "application/json",
		strings.NewReader(`{"email":"admin@t.local","password":"pw-654321"}`))
	require.NoError(t, err)
	defer respNew.Body.Close()
	assert.Equal(t, 200, respNew.StatusCode)

	// 审计行（background-ctx 写入，稍候轮询到即可）。
	require.Eventually(t, func() bool {
		var n int
		err := env.Store.Pool().QueryRow(context.Background(),
			`SELECT count(*) FROM audit_logs WHERE action='user.password_change'`).Scan(&n)
		return err == nil && n == 1
	}, 5*time.Second, 100*time.Millisecond)
}
```

- [ ] **Step 3: 跑测试确认失败**

Run: `cd server && go test ./internal/api/ -run TestProfileUpdateAndChangePassword -count=1 -v`
Expected: FAIL——路由不存在，PATCH /api/auth/me 返回 404/405（chi 默认），首个 `require.Equal(t, 200, ...)` 失败。

- [ ] **Step 4: 实现 handler**

`server/internal/api/auth_handlers.go`——imports 补 `"context"`、`"strings"`、`"unicode/utf8"`、`"xnc/server/internal/db/sqlc"`；文件末尾追加：

```go
// updateMe — PATCH /api/auth/me：自助修改 display_name（email 只读，设计
// §3.3）。TrimSpace 后 ≤64 rune；空串合法（清除显示名）。
func (h *handlers) updateMe(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	var req struct {
		DisplayName string `json:"display_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "bad request"))
		return
	}
	name := strings.TrimSpace(req.DisplayName)
	if utf8.RuneCountInString(name) > 64 {
		respondError(w, proto.Err(400, proto.CodeInternal, "display_name too long (max 64)"))
		return
	}
	if _, err := h.st.Q().UpdateUserDisplayName(r.Context(), sqlc.UpdateUserDisplayNameParams{
		ID: u.ID, DisplayName: name,
	}); err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "update profile"))
		return
	}
	respondJSON(w, 200, map[string]any{"user": newUserDTO(u.ID.String(), u.Email, name)})
}

// changePassword — POST /api/auth/password：current 校验 → 新密码强度 →
// bcrypt 落库。current 不符 401（与 login 同文案，防枚举）；JWT 无状态、
// 存量 token 保持有效（spec §2 决策）。审计 user.password_change。
func (h *handlers) changePassword(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context()) // sqlc.User 全行，含 PasswordHash
	var req struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil ||
		req.CurrentPassword == "" || req.NewPassword == "" {
		respondError(w, proto.Err(400, proto.CodeInternal, "current_password and new_password required"))
		return
	}
	if !auth.VerifyPassword(u.PasswordHash, req.CurrentPassword) {
		respondError(w, proto.Err(401, proto.CodeUnauthorized, "invalid credentials"))
		return
	}
	if len(req.NewPassword) < 8 {
		respondError(w, proto.Err(400, proto.CodeInternal, "new password must be at least 8 characters"))
		return
	}
	hash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "hash password"))
		return
	}
	if _, err := h.st.Q().UpdateUserPassword(r.Context(), sqlc.UpdateUserPasswordParams{
		ID: u.ID, PasswordHash: hash,
	}); err != nil {
		respondError(w, proto.Err(500, proto.CodeInternal, "update password"))
		return
	}
	// 审计沿用 user.create 的 background-ctx 模式（不受客户端断连影响）。
	actx, acancel := context.WithTimeout(context.Background(), auditInsertTimeout)
	defer acancel()
	_ = h.st.Q().InsertAuditLog(actx, sqlc.InsertAuditLogParams{
		UserID: pgUUID(u.ID), Action: "user.password_change", Metadata: []byte("{}"),
	})
	w.WriteHeader(http.StatusNoContent)
}
```

`server/internal/api/router.go` JWT 组（现 `g.Get("/me", h.me)` 处）追加：

```go
			g.Patch("/me", h.updateMe)
			g.Post("/password", h.changePassword)
```

- [ ] **Step 5: 跑测试确认通过**

Run: `cd server && go test ./internal/api/ -run TestProfileUpdateAndChangePassword -count=1 -v`
Expected: PASS
再跑全包回归：`cd server && go test ./internal/api/ -count=1`
Expected: PASS（无既有测试回归）

- [ ] **Step 6: 提交**

```bash
git add server/internal/db/queries/users.sql server/internal/db/sqlc/ server/internal/api/auth_handlers.go server/internal/api/auth_handlers_test.go server/internal/api/router.go
git commit -m "feat(server): self-service profile endpoints - PATCH /api/auth/me, POST /api/auth/password"
```

---

### Task 2: web Profile 页 + 路由 + updateUser + 侧栏 email 入口

**Files:**
- Modify: `web/src/auth.tsx`（Auth 接口加 `updateUser`）
- Create: `web/src/pages/Profile.tsx`
- Modify: `web/src/main.tsx`（App children 加 `/profile` 路由）
- Modify: `web/src/components/Sidebar.tsx`（email div → Link）
- Test: Create `web/src/pages/Profile.test.tsx`、Create `web/src/components/Sidebar.test.tsx`

**Interfaces:**
- Consumes: Task 1 端点契约（PATCH `/api/auth/me` → `{user}`；POST `/api/auth/password` → 204/400/401）；`api<T>(path, opts)`（`web/src/api.ts`，自动带 JWT、抛 `APIError`）；`UserDTO {id,email,display_name}`（`web/src/types.ts`）。
- Produces: `useAuth()` 返回值新增 `updateUser(user: UserDTO): void`（更新 state + localStorage `xnc_user`）；路由 `/profile`（App shell 内）；Sidebar email 渲染为 `<Link to="/profile">`。Task 3 的 Sidebar 测试依赖 `updateUser` 存在。

- [ ] **Step 1: 写失败测试（Profile 页）**

`web/src/pages/Profile.test.tsx`（模式照 `Download.test.tsx`；`api()` 从 localStorage 取 token，须预置）：

```tsx
// @vitest-environment happy-dom
/**
 * Profile 页组件测试：display_name 保存走 PATCH /api/auth/me 且成功后
 * updateUser 生效（侧栏 email 数据源刷新）；改密 confirm 不一致前端拦截
 * （不发请求）、服务端错误显示。
 */
import { act } from "react";
import { createElement } from "react";
import { createRoot, type Root } from "react-dom/client";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AuthProvider } from "../auth";
import type { UserDTO } from "../types";
import Profile from "./Profile";

(globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;

const USER: UserDTO = { id: "u1", email: "admin@t.local", display_name: "Boss" };

function jsonRes(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

let container: HTMLDivElement | null = null;
let root: Root | null = null;

afterEach(() => {
  act(() => {
    root?.unmount();
  });
  container?.remove();
  container = null;
  root = null;
  localStorage.clear();
  vi.unstubAllGlobals();
});

async function renderProfile() {
  localStorage.setItem("xnc_token", "t");
  localStorage.setItem("xnc_user", JSON.stringify(USER));
  container = document.createElement("div");
  document.body.appendChild(container);
  const r = createRoot(container);
  root = r;
  await act(async () => {
    r.render(
      createElement(MemoryRouter, null, createElement(AuthProvider, null, createElement(Profile))),
    );
  });
  await act(async () => {});
}

describe("Profile", () => {
  it("saves display_name via PATCH /api/auth/me and updates auth state", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      expect(String(input)).toContain("/api/auth/me");
      return jsonRes({ user: { ...USER, display_name: "Renamed" } });
    });
    vi.stubGlobal("fetch", fetchMock);
    await renderProfile();

    const input = container!.querySelector('input[name="display_name"]') as HTMLInputElement;
    await act(async () => {
      input.value = "Renamed";
      input.dispatchEvent(new Event("input", { bubbles: true }));
    });
    const form = input.closest("form")!;
    await act(async () => {
      form.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    });
    await act(async () => {});

    const call = fetchMock.mock.calls[0] as unknown as [RequestInfo | URL, RequestInit?];
    expect(call[1]?.method).toBe("PATCH");
    expect(call[1]?.body).toBe(JSON.stringify({ display_name: "Renamed" }));
    // updateUser 生效：localStorage 刷新 + 侧栏数据源（AuthProvider state）更新
    expect(JSON.parse(localStorage.getItem("xnc_user")!).display_name).toBe("Renamed");
  });

  it("blocks password submit when confirm mismatches (no request)", async () => {
    const fetchMock = vi.fn();
    vi.stubGlobal("fetch", fetchMock);
    await renderProfile();

    const set = (name: string, value: string) => {
      const el = container!.querySelector(`input[name="${name}"]`) as HTMLInputElement;
      el.value = value;
      el.dispatchEvent(new Event("input", { bubbles: true }));
    };
    await act(async () => {
      set("current_password", "pw-123456");
      set("new_password", "pw-654321");
      set("confirm_password", "different");
    });
    const forms = container!.querySelectorAll("form");
    const pwForm = forms[forms.length - 1];
    await act(async () => {
      pwForm.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    });
    await act(async () => {});
    expect(fetchMock).not.toHaveBeenCalled();
    expect(container!.textContent).toContain("do not match");
  });

  it("shows server error on password change failure", async () => {
    const fetchMock = vi.fn(async () =>
      jsonRes({ error: { code: "INTERNAL", message: "invalid credentials" } }, 401),
    );
    vi.stubGlobal("fetch", fetchMock);
    await renderProfile();

    const set = (name: string, value: string) => {
      const el = container!.querySelector(`input[name="${name}"]`) as HTMLInputElement;
      el.value = value;
      el.dispatchEvent(new Event("input", { bubbles: true }));
    };
    await act(async () => {
      set("current_password", "wrong");
      set("new_password", "pw-654321");
      set("confirm_password", "pw-654321");
    });
    const forms = container!.querySelectorAll("form");
    const pwForm = forms[forms.length - 1];
    await act(async () => {
      pwForm.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    });
    await act(async () => {});
    expect(fetchMock).toHaveBeenCalled();
    expect(container!.textContent).toContain("invalid credentials");
  });
});
```

注意：`api()` 对非 `/login` 页的 401 会清 token 并 `window.location.assign`——测试 mock 直接返回 401 会触发 happy-dom 导航告警。为避免，第三个用例让 mock 返回 **400**（同样走错误分支显示 message，无重定向副作用）：

```tsx
    const fetchMock = vi.fn(async () =>
      jsonRes({ error: { code: "INTERNAL", message: "weak password" } }, 400),
    );
```

（断言文本相应改 `toContain("weak password")`。）

- [ ] **Step 2: 跑测试确认失败**

Run: `cd web && npm run test`
Expected: FAIL——`./Profile` 模块不存在（解析错误）。

- [ ] **Step 3: 实现**

`web/src/auth.tsx`——接口与 Provider 扩展：

```tsx
export interface Auth {
  user: UserDTO | null;
  login: (email: string, password: string) => Promise<void>;
  logout: () => void;
  updateUser: (user: UserDTO) => void;
}
```

Provider 内（logout 之后）：

```tsx
  const updateUser = useCallback((user: UserDTO) => {
    localStorage.setItem(USER_KEY, JSON.stringify(user));
    setUser(user);
  }, []);

  const value = useMemo(
    () => ({ user, login, logout, updateUser }),
    [user, login, logout, updateUser],
  );
```

`web/src/pages/Profile.tsx`（新建）：

```tsx
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
    setNameNotice("");
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
    <div className="page">
      <h1>Profile</h1>

      <section className="card">
        <h2>Account</h2>
        <dl className="detail-grid">
          <dt>Email</dt>
          <dd>{user.email}</dd>
        </dl>
        <form onSubmit={(e) => void saveProfile(e)}>
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
          {nameError && <span className="error">{nameError}</span>}
        </form>
      </section>

      <section className="card">
        <h2>Change password</h2>
        <form onSubmit={(e) => void changePassword(e)}>
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
          {pwError && <span className="error">{pwError}</span>}
        </form>
      </section>
    </div>
  );
}
```

（若 `styles.css` 无 `.page`/`.error` 类，用既有页面的容器类名——对照 `Users.tsx` 顶层结构取齐；`.notice` 已在 Download 用过。）

`web/src/main.tsx`——import Profile 并在 App children（`/users` 之后）加：

```tsx
            <Route path="/profile" element={<Profile />} />
```

`web/src/components/Sidebar.tsx`——email div 改链接（import `Link`）：

```tsx
        {user && (
          <Link className="email" to="/profile" title={user.email}>
            {user.email}
          </Link>
        )}
```

- [ ] **Step 4: 写 Sidebar 测试（email 入口）**

`web/src/components/Sidebar.test.tsx`（新建；Task 3 会扩展同一文件）：

```tsx
// @vitest-environment happy-dom
/** Sidebar 组件测试：email 区渲染为 /profile 入口（设计 §3.3）。 */
import { act } from "react";
import { createElement } from "react";
import { createRoot, type Root } from "react-dom/client";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it } from "vitest";
import { AuthProvider } from "../auth";
import Sidebar from "./Sidebar";

(globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;

let container: HTMLDivElement | null = null;
let root: Root | null = null;

afterEach(() => {
  act(() => {
    root?.unmount();
  });
  container?.remove();
  container = null;
  root = null;
  localStorage.clear();
});

describe("Sidebar", () => {
  it("links the signed-in email to /profile", async () => {
    localStorage.setItem("xnc_token", "t");
    localStorage.setItem(
      "xnc_user",
      JSON.stringify({ id: "u1", email: "admin@t.local", display_name: "Boss" }),
    );
    container = document.createElement("div");
    document.body.appendChild(container);
    const r = createRoot(container);
    root = r;
    await act(async () => {
      r.render(createElement(MemoryRouter, null, createElement(AuthProvider, null, createElement(Sidebar))));
    });
    const link = container.querySelector("a.email") as HTMLAnchorElement;
    expect(link).not.toBeNull();
    expect(link.getAttribute("href")).toBe("/profile");
    expect(link.textContent).toBe("admin@t.local");
  });
});
```

- [ ] **Step 5: 跑测试确认通过**

Run: `cd web && npm run test`
Expected: PASS（新测试与既有 Download.test 全绿）

- [ ] **Step 6: 提交**

```bash
git add web/src/auth.tsx web/src/pages/Profile.tsx web/src/pages/Profile.test.tsx web/src/main.tsx web/src/components/Sidebar.tsx web/src/components/Sidebar.test.tsx
git commit -m "feat(web): profile page - display name self-service, password change, sidebar email entry"
```

---

### Task 3: web Download 入口 + 侧栏 server 版本小字

**Files:**
- Modify: `web/src/components/Sidebar.tsx`（nav 加 Download；side-foot 加版本行）
- Modify: `web/src/pages/Download.tsx`（页头链接按登录态切换）
- Test: Modify `web/src/components/Sidebar.test.tsx`（追加两用例）
- Test: Modify `web/src/pages/Download.test.tsx`（渲染助手包 AuthProvider + 追加登录态用例）

**Interfaces:**
- Consumes: `GET /api/health` → `{"status":"ok","version":"X.Y.Z"}`（公开端点，`router.go:90`）；`useAuth().user`；Task 2 的 AuthProvider 测试包装模式。
- Produces: 侧栏 `<NavLink to="/download">Download</NavLink>`；`side-foot` 顶部条件渲染 `<div className="dim server-version">server v{version}</div>`；Download 页头登录态文本 "Back to console" → `/nodes`。

- [ ] **Step 1: 写失败测试（Sidebar 两用例追加）**

`Sidebar.test.tsx` 追加（文件顶部补 `it` 已有；`vi` 需 import）：

```tsx
  it("renders the Download nav entry", async () => {
    localStorage.setItem("xnc_token", "t");
    localStorage.setItem(
      "xnc_user",
      JSON.stringify({ id: "u1", email: "admin@t.local", display_name: "" }),
    );
    container = document.createElement("div");
    document.body.appendChild(container);
    const r = createRoot(container);
    root = r;
    await act(async () => {
      r.render(createElement(MemoryRouter, null, createElement(AuthProvider, null, createElement(Sidebar))));
    });
    const links = [...container.querySelectorAll("nav a")].map((a) => a.getAttribute("href"));
    expect(links).toContain("/download");
  });

  it("shows server version from /api/health and stays silent on failure", async () => {
    localStorage.setItem("xnc_token", "t");
    localStorage.setItem(
      "xnc_user",
      JSON.stringify({ id: "u1", email: "admin@t.local", display_name: "" }),
    );
    const fetchMock = vi.fn(async () => jsonRes({ status: "ok", version: "0.8.1" }));
    vi.stubGlobal("fetch", fetchMock);
    container = document.createElement("div");
    document.body.appendChild(container);
    const r = createRoot(container);
    root = r;
    await act(async () => {
      r.render(createElement(MemoryRouter, null, createElement(AuthProvider, null, createElement(Sidebar))));
    });
    await act(async () => {});
    expect(container.textContent).toContain("server v0.8.1");
  });
```

（需在文件顶部加 `jsonRes` 助手与 `vi` import；`afterEach` 已有 `vi.unstubAllGlobals()`——若无则补。失败分支用 `vi.fn(async () => new Response("", { status: 500 }))`，断言 `container.textContent` 不含 "server v"。）

- [ ] **Step 2: 跑测试确认失败**

Run: `cd web && npm run test`
Expected: FAIL——`/download` 不在 nav 链接里；无 "server v0.8.1" 文本。

- [ ] **Step 3: 实现 Sidebar**

`web/src/components/Sidebar.tsx`——imports 改 `useEffect, useState`；组件内加版本拉取与渲染：

```tsx
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
```

nav 加一项、side-foot 顶部加版本行：

```tsx
      <nav>
        <NavLink to="/nodes">Nodes</NavLink>
        <NavLink to="/clusters">Clusters</NavLink>
        <NavLink to="/users">Users</NavLink>
        <NavLink to="/download">Download</NavLink>
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
```

- [ ] **Step 4: Download 页登录态链接 + 既有测试适配**

`web/src/pages/Download.tsx`——import `useAuth`；组件首行取 `const { user } = useAuth();`；页头链接替换：

```tsx
        <Link to={user ? "/nodes" : "/login"}>{user ? "Back to console" : "Sign in to console"}</Link>
```

`web/src/pages/Download.test.tsx`——`renderDownload` 的 render 调用包一层 AuthProvider（否则 useAuth 抛错）：

```tsx
    r.render(
      createElement(
        MemoryRouter,
        null,
        createElement(AuthProvider, null, createElement(Download)),
      ),
    );
```

（顶部补 `import { AuthProvider } from "../auth";`；未预置 token 时 user 为 null，既有断言不受影响。）追加用例：

```tsx
  it("shows 'Back to console' for signed-in users", async () => {
    localStorage.setItem("xnc_token", "t");
    localStorage.setItem(
      "xnc_user",
      JSON.stringify({ id: "u1", email: "admin@t.local", display_name: "" }),
    );
    vi.stubGlobal("fetch", vi.fn(async () => jsonRes({ status: "ok", version: "0.8.1" })));
    await renderDownload();
    const link = container!.querySelector("header a") as HTMLAnchorElement;
    expect(link.getAttribute("href")).toBe("/nodes");
    expect(link.textContent).toBe("Back to console");
  });
```

（`renderDownload` 若在设置 localStorage 前创建组件无妨——AuthProvider 初始 state 在首次渲染读 localStorage。）

- [ ] **Step 5: 跑测试确认通过**

Run: `cd web && npm run test`
Expected: PASS（全部用例，含既有 Download 用例）

- [ ] **Step 6: 提交**

```bash
git add web/src/components/Sidebar.tsx web/src/components/Sidebar.test.tsx web/src/pages/Download.tsx web/src/pages/Download.test.tsx
git commit -m "feat(web): download nav entry for signed-in users, server version footer"
```

---

### Task 4: CLI server 固定 xnc.app（默认兜底 + flag 隐藏 + 去 prompt）

**Files:**
- Modify: `cli/main.go:62,86-87,149-175`（默认常量、MarkHidden、usage 文本、dial 清理）
- Modify: `cli/termux.go:45-58`（loginDeps 去 promptServer、runLoginFlow 去 server 分支）
- Modify: `cli/cmd_auth.go:34-55`（login 交互 deps、非交互 server 报错）
- Modify: `cli/cmd_register.go:206-215,313-318`（token 分支 prompt、registerLogin deps、非交互报错）
- Test: Modify `cli/cmd_ux_test.go:131,156,170,189`（删 promptServer 行）
- Test: Modify `cli/main_test.go` 或 `cli/cmd_auth_test.go`（追加 resolveServer 默认值测试）

**Interfaces:**
- Consumes: 无新依赖。
- Produces: `const defaultServerURL = "https://xnc.app"`；`resolveServer(cmd, cfg)` 恒非空（flag > env/config > default）。`loginDeps` 不再有 `promptServer` 字段。

- [ ] **Step 1: 写失败测试**

`cli/main_test.go` 追加：

```go
// TestResolveServerDefault — 固定生产控制面兜底（设计 §3.4）：无
// flag/env/config 时解析为 https://xnc.app；config 值仍优先于默认。
func TestResolveServerDefault(t *testing.T) {
	cmd := &cobra.Command{}
	if got := resolveServer(cmd, Config{}); got != "https://xnc.app" {
		t.Fatalf("default server = %q, want https://xnc.app", got)
	}
	if got := resolveServer(cmd, Config{Server: "https://cfg.example"}); got != "https://cfg.example" {
		t.Fatalf("config server = %q, want https://cfg.example", got)
	}
}
```

（`main_test.go` 需 import `"github.com/spf13/cobra"`；未定义 --server flag 的裸命令上 `GetString` 返回空，正合意图。）

Run: `cd cli && go test ./... -run TestResolveServerDefault -count=1`
Expected: FAIL——resolveServer 返回 ""。

- [ ] **Step 2: 实现 main.go**

```go
// defaultServerURL 是 CLI 的固定生产控制面（设计 §3.4）：register/login
// 不再询问 server；--server 与 XNC_SERVER 仅为开发/测试保留（MarkHidden，
// 帮助文本不出现）。
const defaultServerURL = "https://xnc.app"
```

`resolveServer` 加兜底：

```go
// resolveServer/resolveToken apply precedence: flag > env > config file
// > 生产默认（server 恒非空；设计 §3.4）。
func resolveServer(cmd *cobra.Command, cfg Config) string {
	if v, _ := cmd.Flags().GetString("server"); v != "" {
		return v
	}
	if cfg.Server != "" {
		return cfg.Server
	}
	return defaultServerURL
}
```

root flag 注册处（86-87 行附近）注册后追加：

```go
	_ = root.PersistentFlags().MarkHidden("server")
```

usage 示例文本删除 `--server URL    server base (env XNC_SERVER, or config)` 一行（62 行附近）；`dial()` 删除 `if server == ""` 分支（server 不再为空）：

```go
func dial(cmd *cobra.Command, needToken bool) (*Client, string) {
	cfg, _ := LoadConfig()
	token := resolveToken(cmd, cfg)
	if needToken && token == "" {
		return nil, "--token, XNC_TOKEN, or xnc login required"
	}
	return NewClient(resolveServer(cmd, cfg), token), nil
}
```

- [ ] **Step 3: 去 prompt（termux.go / cmd_auth.go / cmd_register.go）**

`cli/termux.go`：

```go
// loginDeps 把登录编排的交互面收窄为可注入的函数，便于单测。
// server 不再交互补齐（固定默认值，设计 §3.4）。
type loginDeps struct {
	promptEmail    func() string
	promptPassword func() (string, error)
	doLogin        func(server, email, password string) (string, userDTO, *proto.APIError)
}

// runLoginFlow 交互式登录编排：缺省提示 → 掩码密码 → 401 重试（≤3 次）。
// server 由调用方解析（恒非空）；返回 token/user。
func runLoginFlow(deps loginDeps, server, email string) (string, string, userDTO, error) {
	if email == "" {
		email = deps.promptEmail()
	}
	// ……（循环体原样保留）
```

`cli/cmd_auth.go` login——交互分支 deps 删除 `promptServer` 字段；非交互分支删除：

```go
				if server == "" {
					return failUsage(cmd, "--server, XNC_SERVER, or config file required")
				}
```

`cli/cmd_register.go`——`token != ""` 分支删除整段 server 询问（保留 remembered email 输出）：

```go
			} else {
				if cfg.RememberedEmail != "" {
					outf(cmd, "using session %s\n", cfg.RememberedEmail)
				}
			}
```

`registerLogin` 交互 deps 删除 `promptServer` 行；非交互分支删除 `if server == ""` 报错块（与 cmd_auth 同型）。

- [ ] **Step 4: 更新既有测试**

`cli/cmd_ux_test.go` 131、156、170、189 四处删除 `promptServer: ...` 行（字段已不存在，编译错引导定位）。全量跑：

Run: `cd cli && go vet ./... && go test ./... -count=1`
Expected: PASS（存量测试经 `--server` 注入假服务器，不受默认值影响）

- [ ] **Step 5: 提交**

```bash
git add cli/main.go cli/termux.go cli/cmd_auth.go cli/cmd_register.go cli/cmd_ux_test.go cli/main_test.go
git commit -m "feat(cli): fixed production server URL - default xnc.app, hidden --server, no interactive prompt"
```

---

### Task 5: 文档对齐（AGENTS.md §6 + README）

**Files:**
- Modify: `AGENTS.md:131-133`（注册条目）
- Modify: `README.md`（register 示例，若含 `--server`）

**Interfaces:**
- Consumes: Task 4 的行为（register/login 固定 xnc.app）。
- Produces: 文档与行为一致。

- [ ] **Step 1: 改 AGENTS.md §6 注册条目**

现文（131 行附近）：

```
- **注册**：`xnc register --server https://xnc.app`（新机首跑需
  --server；TTY 交互登录+选 cluster，非 TTY 用 --email + stdin 密码 +
  --yes）。成功后秒级 online。重装/换机身份 → adopt 沿用原节点。
```

改为：

```
- **注册**：`xnc register`（server 固定 https://xnc.app，不再询问；
  --server/XNC_SERVER 仅为开发保留且已从帮助隐藏。TTY 交互登录+选
  cluster，非 TTY 用 --email + stdin 密码 + --yes）。成功后秒级 online。
  重装/换机身份 → adopt 沿用原节点。
```

- [ ] **Step 2: 改 README**

`grep -n "register --server\|--server https://xnc.app" README.md` 定位所有出现（含"安装后注册"步骤与命令示例），将 `xnc register --server https://xnc.app` 简化为 `xnc register`；如有"新机首跑需 --server"字样一并删除。

- [ ] **Step 3: 全量回归**

Run: `cd server && go build ./... && cd ../cli && go build . && cd ../web && npm run test`
Expected: 全部通过（server/web 与文档无耦合，此处为终检）。

- [ ] **Step 4: 提交**

```bash
git add AGENTS.md README.md
git commit -m "docs: register no longer takes a server URL (fixed xnc.app)"
```

---

## 自审记录

1. **Spec 覆盖**：§3.1→Task 3；§3.2→Task 3；§3.3→Task 1（API）+Task 2（web）；§3.4→Task 4；§5 文档→Task 5；§4 测试计划分布各任务 Step。§2 非目标未越界（无 email 修改、无 token 吊销、无创建端点策略改动）。
2. **占位符扫描**：无 TBD/TODO；所有代码步骤含完整代码。Task 2 Step 3 的样式类名注明"对照 Users.tsx 取齐"——这是对既有样式的引用而非占位。
3. **类型一致性**：`updateUser(user: UserDTO): void` 在 Task 2 定义、Task 3 测试复用 AuthProvider；`defaultServerURL` 仅 Task 4 内部；sqlc 参数结构体名与生成规则一致；Profile 测试选择器 `input[name=…]` 与实现代码 name 属性一一对应（display_name/current_password/new_password/confirm_password）。
