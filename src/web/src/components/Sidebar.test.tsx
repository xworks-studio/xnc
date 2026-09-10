// @vitest-environment happy-dom
/** Sidebar 组件测试：email 区渲染为 /profile 入口（设计 §3.3）；
 *  nav 含 Download 入口；side-foot 显示 /api/health 的 server 版本小字
 *  （失败静默，设计 §3.2）。 */
import { act } from "react";
import { createElement } from "react";
import { createRoot, type Root } from "react-dom/client";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { AuthProvider } from "../auth";
import Sidebar from "./Sidebar";

(globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;

function jsonRes(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

let container: HTMLDivElement | null = null;
let root: Root | null = null;

// Sidebar 挂载即请求 /api/health：默认给个 500 桩，避免未显式 stub 的
// 用例发起真实网络请求（连接被拒会在 stderr 刷错误日志）；显式
// vi.stubGlobal 的用例会覆盖此默认值。
beforeEach(() => {
  vi.stubGlobal("fetch", vi.fn(async () => new Response("", { status: 500 })));
});

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

  // 失败分支：/api/health 非 2xx → 版本小字静默不渲染，不打扰导航。
  it("stays silent (no version line) when /api/health fails", async () => {
    localStorage.setItem("xnc_token", "t");
    localStorage.setItem(
      "xnc_user",
      JSON.stringify({ id: "u1", email: "admin@t.local", display_name: "" }),
    );
    vi.stubGlobal("fetch", vi.fn(async () => new Response("", { status: 500 })));
    container = document.createElement("div");
    document.body.appendChild(container);
    const r = createRoot(container);
    root = r;
    await act(async () => {
      r.render(createElement(MemoryRouter, null, createElement(AuthProvider, null, createElement(Sidebar))));
    });
    await act(async () => {});
    expect(container.textContent).not.toContain("server v");
  });
});
