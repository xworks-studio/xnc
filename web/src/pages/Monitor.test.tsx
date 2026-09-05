// @vitest-environment happy-dom
/**
 * Monitor 页测试：三态渲染（pool 模式表行 / unconfigured 占位 / 拉取
 * 失败）、探测按钮触发注入的假 probeTurn、结果与 timeout 呈现。
 */
import { act } from "react";
import { createElement } from "react";
import { createRoot, type Root } from "react-dom/client";
import { MemoryRouter } from "react-router-dom";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { AuthProvider } from "../auth";
import Monitor from "./Monitor";

vi.mock("../lib/turnProbe", () => ({
  probeTurn: vi.fn(async () => 12.3),
}));
import { probeTurn } from "../lib/turnProbe";

(globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;

const STATUS_POOL = {
  mode: "pool",
  icePolicy: "relay",
  pool: [
    { ip: "1.2.3.4", port: 3478, urls: ["turn:1.2.3.4:3478?transport=udp", "turn:1.2.3.4:3478?transport=tcp"], healthy: true },
    { ip: "5.6.7.8", port: 3478, urls: ["turn:5.6.7.8:3478?transport=udp", "turn:5.6.7.8:3478?transport=tcp"], healthy: false },
  ],
  fallbackUrls: ["turn:xnc.app:3478?transport=tcp"],
  username: "u",
  credential: "p",
  activeDesktopSessions: 3,
};

function jsonRes(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

let container: HTMLDivElement | null = null;
let root: Root | null = null;

beforeEach(() => {
  localStorage.setItem("xnc_token", "t");
  localStorage.setItem("xnc_user", JSON.stringify({ id: "u1", email: "a@t.local", display_name: "" }));
  vi.mocked(probeTurn).mockClear();
  vi.mocked(probeTurn).mockImplementation(async () => 12.3);
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

async function renderMonitor(fetchImpl: () => Promise<Response>) {
  vi.stubGlobal("fetch", vi.fn(fetchImpl));
  container = document.createElement("div");
  document.body.appendChild(container);
  const r = createRoot(container);
  root = r;
  await act(async () => {
    r.render(createElement(MemoryRouter, null, createElement(AuthProvider, null, createElement(Monitor))));
  });
  // 10s 自动刷新的 interval 在测试里不推进（fake timers 不用，真定时器
  // 100ms 后也不会触发 fetch 断言冲突）。
  await act(async () => {});
}

describe("Monitor", () => {
  it("renders pool members + fallback rows and probes them", async () => {
    await renderMonitor(async () => jsonRes(STATUS_POOL));
    expect(container!.textContent).toContain("pool");
    expect(container!.textContent).toContain("1.2.3.4:3478");
    expect(container!.textContent).toContain("5.6.7.8:3478");
    expect(container!.textContent).toContain("turn:xnc.app:3478?transport=tcp");
    expect(container!.textContent).toContain("3"); // activeDesktopSessions
    // 自动探测：3 个目标（2 池 + 1 fallback）串行各一次。
    await act(async () => {});
    expect(vi.mocked(probeTurn).mock.calls.length).toBeGreaterThanOrEqual(3);
    expect(container!.textContent).toContain("12.3");
  });

  it("shows probe timeout state", async () => {
    vi.mocked(probeTurn).mockImplementation(async () => {
      throw new Error("timeout");
    });
    await renderMonitor(async () => jsonRes(STATUS_POOL));
    await act(async () => {});
    await act(async () => {});
    expect(container!.textContent).toContain("timeout");
  });

  it("renders unconfigured placeholder without probe", async () => {
    await renderMonitor(async () =>
      jsonRes({ mode: "unconfigured", icePolicy: "relay", pool: [], fallbackUrls: [], username: "", credential: "", activeDesktopSessions: 0 }),
    );
    expect(container!.textContent!.toLowerCase()).toContain("not configured");
    expect(probeTurn).not.toHaveBeenCalled();
  });

  it("renders fetch-failure placeholder", async () => {
    await renderMonitor(async () => jsonRes({ error: { code: "INTERNAL", message: "boom" } }, 500));
    expect(container!.textContent).toContain("Failed to load");
  });
});
