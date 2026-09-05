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

/** React 给受控 input 装了值追踪器（_valueTracker）：直接赋 el.value 会被
 *  判定"值未变化"而不触发 onChange。须用原型 setter 绕过实例属性写值，
 *  再派发 input 事件（与 testing-library fireEvent 同一做法）。 */
function setInputValue(el: HTMLInputElement, value: string) {
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")!.set!;
  setter.call(el, value);
  el.dispatchEvent(new Event("input", { bubbles: true }));
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
      setInputValue(input, "Renamed");
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
      setInputValue(el, value);
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
      jsonRes({ error: { code: "INTERNAL", message: "weak password" } }, 400),
    );
    vi.stubGlobal("fetch", fetchMock);
    await renderProfile();

    const set = (name: string, value: string) => {
      const el = container!.querySelector(`input[name="${name}"]`) as HTMLInputElement;
      setInputValue(el, value);
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
    expect(container!.textContent).toContain("weak password");
  });
});
