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
