// @vitest-environment happy-dom
/**
 * Download 页轻量组件测试（页面是新公开入口；仓库内其余页面无组件
 * 测试惯例，仅此一份）。mock 全局 fetch → 断言渲染版本 / sha256 截断
 * 展示（完整值留在 DOM + title，视觉截断由 CSS 承担）/ 下载按钮直链
 * href；404 → 该频道显示"暂无发布"占位且不影响另一张卡片。
 */
import { act } from "react";
import { createElement } from "react";
import { createRoot, type Root } from "react-dom/client";
import { MemoryRouter } from "react-router-dom";
import { afterEach, describe, expect, it, vi } from "vitest";
import { AuthProvider } from "../auth";
import Download from "./Download";
import type { SetupManifest } from "./Download";

// React 19：直接用 createRoot + act 需要显式开启 act 环境。
(globalThis as Record<string, unknown>).IS_REACT_ACT_ENVIRONMENT = true;

const SHA = "5a2d".repeat(16); // 64 位十六进制

const STABLE: SetupManifest = {
  version: "0.4.5",
  url: "http://xnc.app/installer?channel=stable",
  sha256: SHA,
  size: 12_582_912, // 12.0 MB
  releasedAt: new Date(Date.now() - 3_600_000).toISOString(), // "1h ago"
};

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

/** 挂载整页（MemoryRouter 提供 <Link> 上下文；AuthProvider 提供 useAuth），
 *  并冲刷 fetch 微任务链。 */
async function renderDownload() {
  container = document.createElement("div");
  document.body.appendChild(container);
  const r = createRoot(container);
  root = r;
  await act(async () => {
    r.render(
      createElement(
        MemoryRouter,
        null,
        createElement(AuthProvider, null, createElement(Download)),
      ),
    );
  });
  // useEffect 里的 fetch promise 链在渲染后的微任务里 resolve → 再冲刷一轮。
  await act(async () => {});
}

describe("Download page (mocked /installer.json)", () => {
  it("renders version, relative time, size, truncated sha and download hrefs", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        const manifest =
          url.includes("channel=dev")
            ? { ...STABLE, version: "0.5.0-dev.3" }
            : STABLE;
        return jsonRes(manifest);
      }),
    );

    await renderDownload();
    const text = container!.textContent ?? "";

    expect(text).toContain("Stable");
    expect(text).toContain("Dev");
    expect(text).toContain("0.4.5");
    expect(text).toContain("0.5.0-dev.3");
    expect(text).toContain("12.0 MB");
    expect(text).toContain("1h ago");
    expect(text).toContain("Install in three steps");
    expect(text).toContain("xnc register");

    // sha256：DOM 文本保留完整值（复制用），title 提供悬浮全文；
    // "截断显示"是 .download-sha .mono 的 CSS 溢出省略。
    const shaBtn = container!.querySelector("button.download-sha");
    expect(shaBtn).not.toBeNull();
    expect(shaBtn!.getAttribute("title")).toBe(SHA);
    expect(shaBtn!.textContent).toBe(SHA);
    expect(shaBtn!.querySelector(".mono")).not.toBeNull();

    // 下载按钮 = 浏览器直下 <a>，不走 fetch+blob。
    expect(
      container!.querySelector('a[href="/installer?channel=stable"]'),
    ).not.toBeNull();
    expect(
      container!.querySelector('a[href="/installer?channel=dev"]'),
    ).not.toBeNull();

    // 顶部"登录控制台"入口。
    expect(container!.querySelector('a[href="/login"]')).not.toBeNull();
  });

  it("shows the no-release placeholder on 404 without breaking the other card", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const url = String(input);
        if (url.includes("channel=dev")) {
          // 服务端对无 release 频道返回 {"error":{...}} + 404。
          return jsonRes(
            { error: { code: "NOT_FOUND", message: "no releases" } },
            404,
          );
        }
        return jsonRes(STABLE);
      }),
    );

    await renderDownload();
    const text = container!.textContent ?? "";

    // dev 卡片：占位，且没有下载按钮。
    expect(text).toContain("No release yet");
    expect(
      container!.querySelector('a[href="/installer?channel=dev"]'),
    ).toBeNull();

    // stable 卡片不受影响。
    expect(text).toContain("0.4.5");
    expect(
      container!.querySelector('a[href="/installer?channel=stable"]'),
    ).not.toBeNull();
  });

  it("renders placeholders when the manifest fetch itself fails", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        throw new TypeError("network down");
      }),
    );

    await renderDownload();
    const text = container!.textContent ?? "";
    expect(text.match(/No release yet/g)?.length).toBe(2);
    expect(container!.querySelector("a.download-btn")).toBeNull();
  });

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
});
