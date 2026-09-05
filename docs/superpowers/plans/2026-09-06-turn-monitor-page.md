# TURN 监控页 + DesktopLive 信息补齐 — 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 落地 spec 三件：`GET /api/turn/status`（用户 JWT）、登录可见的 Monitor 页（TURN 状态 + 回环探测时延 + 会话用量聚合）、DesktopLive 统计补齐（e2e/rtt/bitrate/via TURN）与状态栏分组。

**Architecture:** server 侧给 TurnPoolManager/session.Manager 各加一个只读快照方法，新 handler 挂既有用户 JWT 组；web 侧新增探测库（可注入 RTCPeerConnection 工厂）与 Monitor 页；DesktopLive 把既有 1s 反馈环里已计算的指标提升为 state 展示，中继路径解析提取纯函数。

**Tech Stack:** Go（chi + sqlc 无关、testcontainers 真 PG）/ React 19 + Vite + vitest（happy-dom）/ WebRTC（RTCPeerConnection loopback probe、getStats）。

**Spec:** `docs/superpowers/specs/2026-09-06-turn-monitor-page-design.md`

## Global Constraints

- 代码注释中文、提交信息英文 conventional commits；每个任务独立提交。
- server 测试真 PG（testcontainers，Docker 需运行）：`cd server && go test ./internal/api/ -run <Test> -count=1`。
- web 测试：`cd web && npm run test`（vitest run，happy-dom；React 19 act 环境照 Download.test.tsx 模式）。
- 构建验证逐模块：`cd server && go build ./...`、`cd web && npx tsc -b`。
- 凭据纪律：username/credential 只出现在 JWT 保护端点的 JSON body（与会话下发同源），绝不入日志、不入公开端点。
- DesktopLive 布局机制不动（无折叠/全屏）；`?framediag` 诊断模式不动。
- proto/agent 零改动。

---

### Task 1: server — `GET /api/turn/status` + 两个只读快照方法

**Files:**
- Modify: `server/internal/api/turnpool.go`（加 `turnServerStatus` 类型 + `Status()` 方法）
- Modify: `server/internal/session/manager.go`（加 `CountActive(kind)`）
- Create: `server/internal/api/turn_handlers.go`
- Modify: `server/internal/api/router.go`（/api/turn 路由组，/api/users 块之后）
- Test: `server/internal/api/turnpool_test.go`（追加 Status 用例）
- Test: `server/internal/session/manager_test.go`（追加 CountActive 用例；无此文件则建）
- Test: `server/internal/api/turn_handlers_test.go`（新建）

**Interfaces:**
- Consumes: 既有 `TurnPoolManager`（`servers []*turnServer`、`mu sync.Mutex`、`turnURLs()`）；既有 `Manager.sessions map[string]*session` + `countByNodeLocked` 风格；`handlers` 结构的 `turnPool *TurnPoolManager`、`sess *session.Manager`、`cfg config.Config`；`proto.KindDesktop`。
- Produces: `GET /api/turn/status`（用户 JWT）→ §spec 3.1 JSON；`(*TurnPoolManager).Status() []turnServerStatus`；`(*session.Manager).CountActive(kind string) int`。Task 2 的 web 消费此 JSON 契约。

- [ ] **Step 1: TurnPoolManager.Status()（先写测试）**

`server/internal/api/turnpool_test.go` 追加：

```go
// TestTurnPoolStatusSnapshot — Status 返回池内容的锁内快照（ip/port/
// urls(udp+tcp)/healthy），与构造输入一致；空池返回空切片。
func TestTurnPoolStatusSnapshot(t *testing.T) {
	m := NewTurnPoolManager([]string{"1.2.3.4", "5.6.7.8:443", "bad-entry"}, "u", "p")
	snaps := m.Status()
	if len(snaps) != 2 {
		t.Fatalf("pool size = %d, want 2 (bad-entry skipped)", len(snaps))
	}
	if snaps[0].IP != "1.2.3.4" || snaps[0].Port != 3478 || !snaps[0].Healthy {
		t.Fatalf("snaps[0] = %+v, want 1.2.3.4:3478 healthy", snaps[0])
	}
	wantURLs := []string{
		"turn:1.2.3.4:3478?transport=udp",
		"turn:1.2.3.4:3478?transport=tcp",
	}
	if len(snaps[0].URLs) != 2 || snaps[0].URLs[0] != wantURLs[0] || snaps[0].URLs[1] != wantURLs[1] {
		t.Fatalf("snaps[0].URLs = %v, want %v", snaps[0].URLs, wantURLs)
	}
	if snaps[1].Port != 443 {
		t.Fatalf("snaps[1].Port = %d, want 443", snaps[1].Port)
	}
	if got := len((NewTurnPoolManager(nil, "u", "p")).Status()); got != 0 {
		t.Fatalf("empty pool Status size = %d, want 0", got)
	}
}
```

Run: `cd server && go test ./internal/api/ -run TestTurnPoolStatusSnapshot -count=1`
Expected: FAIL——`m.Status undefined`。

`server/internal/api/turnpool.go` 追加（类型放在 `turnServer` 定义后）：

```go
// turnServerStatus Status() 的对外快照形态（监控端点用；同包直读）。
type turnServerStatus struct {
	IP      string
	Port    int
	URLs    []string
	Healthy bool
}

// Status 返回池内各台的只读快照（ip/port/urls/healthy）。锁内浅拷贝；
// 探测 goroutine 的状态更新与读方互不阻塞。
func (m *TurnPoolManager) Status() []turnServerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]turnServerStatus, 0, len(m.servers))
	for _, s := range m.servers {
		out = append(out, turnServerStatus{
			IP: s.IP, Port: s.Port, URLs: s.turnURLs(), Healthy: s.Healthy,
		})
	}
	return out
}
```

- [ ] **Step 2: Manager.CountActive（先写测试）**

`server/internal/session/manager_test.go`（若不存在则新建，package session；既有测试风格用 testify 则 require）：

```go
// TestCountActive — 全表按 kind 计数：空表 0；混 kind 只数目标 kind。
func TestCountActive(t *testing.T) {
	m := New(nil, slog.Default())
	if got := m.CountActive("desktop"); got != 0 {
		t.Fatalf("empty CountActive = %d, want 0", got)
	}
	n1 := uuid.New()
	n2 := uuid.New()
	if _, err := m.Create(n1, uuid.New(), "desktop", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("create desktop: %v", err)
	}
	if _, err := m.Create(n1, uuid.New(), "desktop", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("create desktop 2: %v", err)
	}
	if _, err := m.Create(n2, uuid.New(), "shell", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("create shell: %v", err)
	}
	if got := m.CountActive("desktop"); got != 2 {
		t.Fatalf("CountActive(desktop) = %d, want 2", got)
	}
	if got := m.CountActive("shell"); got != 1 {
		t.Fatalf("CountActive(shell) = %d, want 1", got)
	}
}
```

（imports：`encoding/json`、`log/slog`、`testing`、`github.com/google/uuid`。注意 Create 的 shell/desktop 治理默认值在 New 里已有；若 Create 需要非 nil registry 也传 nil 可行——registry 仅在 attach/close 时用。若编译发现 Create 签名不同，以 manager.go:147 实签名为准调整调用。）

Run: `cd server && go test ./internal/session/ -run TestCountActive -count=1`
Expected: FAIL——`m.CountActive undefined`。

`server/internal/session/manager.go` 追加（`SessionsOf` 附近）：

```go
// CountActive 返回当前表中指定 kind 的会话数（监控页聚合口径；锁内遍历，
// 与 countByNodeLocked 同型）。
func (m *Manager) CountActive(kind string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.sessions {
		if s.Kind == kind {
			n++
		}
	}
	return n
}
```

- [ ] **Step 3: 端点（先写集成测试）**

`server/internal/api/turn_handlers_test.go`（新建；用既有 `doJSON`/`decodeJSON`/`NewTestEnv`/`newTestEnvWithCfg`）：

```go
package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/google/uuid"

	"xnc/proto"
)

// TestTurnStatus — GET /api/turn/status：默认 TestEnv（TurnURLs 注入）=
// urls 模式；池注入 = pool 模式带健康快照；清空 = unconfigured；
// 未认证 401；desktop 会话计数生效；凭据字段在场（探测用）。
func TestTurnStatus(t *testing.T) {
	env := NewTestEnv(t)
	token := env.AdminToken(t)

	// 未认证 → 401（getTurnStatus：本文件 helper，见下）。
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	resp := doJSON(t, srv.URL, "GET", "/api/turn/status", "", "")
	assert.Equal(t, 401, resp.StatusCode)

	// 默认（TurnURLs 配置、无池）→ urls 模式。
	s := getTurnStatus(t, token, env)
	assert.Equal(t, "urls", s["mode"])
	assert.Equal(t, "relay", s["icePolicy"])
	assert.NotEmpty(t, s["fallbackUrls"])
	assert.Empty(t, s["pool"])
	assert.Equal(t, "testuser", s["username"])   // TestEnv 注入值
	assert.Equal(t, "testcred", s["credential"]) // 与会话下发同源
}

// getTurnStatus — helper：一次性 httptest server 包 env.Router，带 token
// GET /api/turn/status 并解码为 map（勿改 testenv）。
func getTurnStatus(t *testing.T, token string, env *TestEnv) map[string]any {
	t.Helper()
	srv := httptest.NewServer(env.Router)
	t.Cleanup(srv.Close)
	resp := doJSON(t, srv.URL, "GET", "/api/turn/status", token, "")
	require.Equal(t, 200, resp.StatusCode)
	var s map[string]any
	require.NoError(t, decodeJSON(resp.Body, &s))
	return s
}
```

（imports 补 `net/http/httptest`。）

pool/unconfigured 用 `newTestEnvWithCfg`：

```go
func TestTurnStatusModes(t *testing.T) {
	// pool 模式：注入池（127.0.0.1 无 coturn 也无害——初始 healthy=true，
	// 2 次探测失败才会翻转，测试窗口内不会）。
	poolEnv := newTestEnvWithCfg(t, func(c *config.Config) {
		c.TurnPool = []string{"127.0.0.1:3478"}
		c.TurnUsername = "u1"
		c.TurnCredential = "p1"
	})
	s := getTurnStatus(t, poolEnv.AdminToken(t), poolEnv)
	require.Equal(t, "pool", s["mode"])
	pool := s["pool"].([]any)
	require.Len(t, pool, 1)
	m := pool[0].(map[string]any)
	assert.Equal(t, "127.0.0.1", m["ip"])
	assert.Equal(t, float64(3478), m["port"])
	assert.Equal(t, true, m["healthy"])
	assert.Equal(t, "u1", s["username"])

	// unconfigured：清空 TurnURLs 且无池。
	offEnv := newTestEnvWithCfg(t, func(c *config.Config) {
		c.TurnURLs = nil
	})
	s = getTurnStatus(t, offEnv.AdminToken(t), offEnv)
	assert.Equal(t, "unconfigured", s["mode"])
	assert.Equal(t, "", s["username"])
}

func TestTurnStatusCountsDesktopSessions(t *testing.T) {
	env := NewTestEnv(t)
	if _, err := env.Sess.Create(uuid.New(), uuid.New(), proto.KindDesktop, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("create desktop: %v", err)
	}
	s := getTurnStatus(t, env.AdminToken(t), env)
	assert.Equal(t, float64(1), s["activeDesktopSessions"])
}
```

（`getTurnStatus` 为本文件 helper：GET /api/turn/status + decodeJSON 为 `map[string]any`；`serveJSON` 同。imports 补 `encoding/json`、`net/http/httptest`、`xnc/server/internal/config`。）

Run: `cd server && go test ./internal/api/ -run 'TestTurnStatus' -count=1`
Expected: FAIL——404（路由不存在）。

- [ ] **Step 4: 实现 handler + 路由**

`server/internal/api/turn_handlers.go`（新建）：

```go
package api

// turn_handlers.go — GET /api/turn/status（用户 JWT）：TURN 服务状态 +
// 聚合用量（设计 §3.1）。username/credential 与会话下发同源，仅登录可见
//（浏览器回环探测分配 relay 候选用）；绝不入日志。

import (
	"net/http"

	"xnc/proto"
)

// turnStatus — GET /api/turn/status。
func (h *handlers) turnStatus(w http.ResponseWriter, r *http.Request) {
	mode := "urls"
	var pool []map[string]any
	if h.turnPool != nil {
		for _, s := range h.turnPool.Status() {
			mode = "pool"
			pool = append(pool, map[string]any{
				"ip": s.IP, "port": s.Port, "urls": s.URLs, "healthy": s.Healthy,
			})
		}
	}
	if pool == nil {
		pool = []map[string]any{}
	}
	fallback := h.cfg.TurnURLs
	if fallback == nil {
		fallback = []string{}
	}
	if mode == "urls" && len(fallback) == 0 {
		mode = "unconfigured"
	}
	// ICE 策略归一化（与 config.icePolicy 同法 fail-closed）：TestEnv 等直构
	// config 不经 Load()，零值 "" 必须归到 "relay"，端点契约恒定。
	policy := h.cfg.DesktopICEPolicy
	if policy != "all" {
		policy = "relay"
	}
	respondJSON(w, http.StatusOK, map[string]any{
		"mode":                   mode,
		"icePolicy":              policy,
		"pool":                   pool,
		"fallbackUrls":           fallback,
		"username":               h.cfg.TurnUsername,
		"credential":             h.cfg.TurnCredential,
		"activeDesktopSessions":  h.sess.CountActive(proto.KindDesktop),
	})
}
```

`server/internal/api/router.go`（`/api/users` 路由块之后追加）：

```go
	// TURN 服务状态（监控页）：用户 JWT；凭据与会话下发同源（设计 §3.1）。
	r.Route("/api/turn", func(tr chi.Router) {
		tr.Use(auth.Middleware(cfg.JWTSecret, st))
		tr.Get("/status", h.turnStatus)
	})
```

- [ ] **Step 5: 全量验证**

Run: `cd server && go build ./... && go vet ./... && go test ./internal/api/ ./internal/session/ -count=1`
Expected: PASS（含既有回归）。

- [ ] **Step 6: 提交**

```bash
git add server/internal/api/turnpool.go server/internal/api/turnpool_test.go server/internal/api/turn_handlers.go server/internal/api/turn_handlers_test.go server/internal/api/router.go server/internal/session/manager.go server/internal/session/manager_test.go
git commit -m "feat(server): GET /api/turn/status - pool snapshot, aggregate session count, probe credentials"
```

---

### Task 2: web — turnProbe 探测库 + Monitor 页

**Files:**
- Create: `web/src/lib/turnProbe.ts`
- Test: `web/src/lib/turnProbe.test.ts`
- Create: `web/src/pages/Monitor.tsx`
- Test: `web/src/pages/Monitor.test.tsx`
- Modify: `web/src/main.tsx`（App children 加 `/monitor`）
- Modify: `web/src/components/Sidebar.tsx`（nav 加 Monitor，Download 之后）

**Interfaces:**
- Consumes: Task 1 的 `GET /api/turn/status` JSON（字段名逐一：mode/icePolicy/pool[]{ip,port,urls,healthy}/fallbackUrls/username/credential/activeDesktopSessions）；`api<T>()`（JWT 自动附带）。
- Produces: `probeTurn(target: {urls: string[]; username: string; credential: string}, timeoutMs?: number): Promise<number>`（浏览器→TURN 单向 ms，失败 reject Error("timeout"|"no rtt")）；模块级可注入 `pcFactory.make`。Monitor 页路由 `/monitor` + 侧栏 Monitor 项。

- [ ] **Step 1: turnProbe 单元测试（先写）**

`web/src/lib/turnProbe.test.ts`：

```ts
// @vitest-environment happy-dom
/**
 * turnProbe 单测：假 RTCPeerConnection 驱动回环协商；断言 RTT/2 取整到
 * 0.1ms 与超时拒绝。假实现只覆盖 probeTurn 触碰的面。
 */
import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { probeTurn, pcFactory } from "./turnProbe";
import type { PCFactory } from "./turnProbe";

/** 最小假 PC：两实例经模块级信箱交换 SDP/候选；getStats 返回固定 pair。 */
function makeFakePC(rttSeconds: number) {
  const mailboxes: Array<(msg: unknown) => void> = [];
  const mk = () => {
    const pc: Record<string, unknown> = {
      onicecandidate: null,
      createDataChannel: vi.fn(() => ({ onopen: null as null | (() => void) })),
      createOffer: vi.fn(async () => ({ type: "offer", sdp: "o" })),
      createAnswer: vi.fn(async () => ({ type: "answer", sdp: "a" })),
      setLocalDescription: vi.fn(async () => {
        // 本地描述就绪 = 产出候选（假 relay 候选）。
        const cb = pc.onicecandidate as ((e: { candidate: {} }) => void) | null;
        cb?.({ candidate: {} });
      }),
      setRemoteDescription: vi.fn(async () => {}),
      addIceCandidate: vi.fn(async () => {}),
      getStats: vi.fn(async () => [
        { type: "candidate-pair", nominated: true, currentRoundTripTime: rttSeconds },
      ]),
      close: vi.fn(),
    };
    return pc as unknown as RTCPeerConnection & { createDataChannel: ReturnType<typeof vi.fn> };
  };
  const a = mk();
  const b = mk();
  // 互通：A 的候选直接喂 B（假实现无需真实 ICE）。
  const origA = a.onicecandidate;
  void origA;
  return { a, b, mailboxes };
}

describe("probeTurn", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("resolves with rtt/2 in ms (1 decimal)", async () => {
    const { a, b } = makeFakePC(0.041); // 41ms RTT → 20.5ms 单向
    let n = 0;
    const factory: PCFactory = () => (n++ === 0 ? a : b);
    pcFactory.make = factory;
    const p = probeTurn({ urls: ["turn:1.2.3.4:3478?transport=udp"], username: "u", credential: "p" });
    // 假 PC 的 datachannel onopen 由 setLocalDescription 链路外触发——
    // 直接在微任务后手动开信道。
    await vi.advanceTimersByTimeAsync(0);
    const ch = a.createDataChannel.mock.results[0]?.value as { onopen: null | (() => void) };
    ch.onopen?.();
    await expect(p).resolves.toBe(20.5);
  });

  it("rejects with timeout after timeoutMs", async () => {
    const { a, b } = makeFakePC(1);
    let n = 0;
    pcFactory.make = () => (n++ === 0 ? a : b);
    const p = probeTurn({ urls: ["turn:1.2.3.4:3478?transport=udp"], username: "u", credential: "p" }, 50);
    await expect(p).rejects.toThrow("timeout");
    await vi.advanceTimersByTimeAsync(100);
  });
});
```

（假 PC 的 onopen 触发点若与实现细节不齐，允许调整测试内部触发方式，断言不变：RTT/2 数值与 timeout 拒绝。）

Run: `cd web && npm run test`
Expected: FAIL——模块不存在。

- [ ] **Step 2: 实现 turnProbe.ts**

```ts
/**
 * 浏览器→TURN 回环探测（设计 §3.2）：两条 RTCPeerConnection 经同一 TURN
 * relay 互联，取 selected candidate-pair currentRoundTripTime/2 为单向
 * 时延。RTCPeerConnection 经 pcFactory 注入（happy-dom 无实现，测试替身）。
 */

export interface ProbeTarget {
  urls: string[];
  username: string;
  credential: string;
}

export type PCFactory = (cfg: RTCConfiguration) => RTCPeerConnection;

/** 可注入工厂（测试替身；生产 = window.RTCPeerConnection）。 */
export const pcFactory: { make: PCFactory } = {
  make: (cfg) => new RTCPeerConnection(cfg),
};

const DEFAULT_TIMEOUT_MS = 8000;

/**
 * probeTurn — 返回浏览器→TURN 单向时延（ms，0.1 精度）。
 * 失败：超时 reject Error("timeout")；连通但无 rtt 统计 reject
 * Error("no rtt")。调用方负责串行（并发探测会互相抬时延）。
 */
export async function probeTurn(target: ProbeTarget, timeoutMs = DEFAULT_TIMEOUT_MS): Promise<number> {
  const mk = () =>
    pcFactory.make({
      iceServers: [
        { urls: target.urls, username: target.username, credential: target.credential },
      ],
      iceTransportPolicy: "relay",
    });
  const a = mk();
  const b = mk();
  let timer: ReturnType<typeof setTimeout> | undefined;
  const cleanup = () => {
    if (timer !== undefined) clearTimeout(timer);
    try {
      a.close();
    } catch {
      /* already closed */
    }
    try {
      b.close();
    } catch {
      /* already closed */
    }
  };
  try {
    return await new Promise<number>((resolve, reject) => {
      timer = setTimeout(() => reject(new Error("timeout")), timeoutMs);
      a.onicecandidate = (e) => {
        if (e.candidate) void b.addIceCandidate(e.candidate).catch(() => {});
      };
      b.onicecandidate = (e) => {
        if (e.candidate) void a.addIceCandidate(e.candidate).catch(() => {});
      };
      const ch = a.createDataChannel("probe");
      // 信道开 = ICE（经 TURN relay）已连通；此后轮询 getStats 等
      // currentRoundTripTime 出现（Chrome 需一次 STUN consent 后才填）。
      ch.onopen = async () => {
        for (let i = 0; i < 6; i++) {
          let rtt = 0;
          try {
            const report = await a.getStats();
            report.forEach((s) => {
              const st = s as { type?: string; nominated?: boolean; currentRoundTripTime?: number };
              if (st.type === "candidate-pair" && typeof st.currentRoundTripTime === "number") {
                if (st.nominated || rtt === 0) rtt = st.currentRoundTripTime;
              }
            });
          } catch {
            /* pc closing */
          }
          if (rtt > 0) {
            // 往返/2 = 浏览器→TURN 单程（回环两腿同路径）。
            resolve(Math.round((rtt * 1000 * 10) / 2) / 10);
            return;
          }
          await new Promise((r) => setTimeout(r, 500));
        }
        reject(new Error("no rtt"));
      };
      void (async () => {
        try {
          const offer = await a.createOffer();
          await a.setLocalDescription(offer);
          await b.setRemoteDescription(offer);
          const answer = await b.createAnswer();
          await b.setLocalDescription(answer);
          await a.setRemoteDescription(answer);
        } catch (e) {
          reject(e instanceof Error ? e : new Error(String(e)));
        }
      })();
    });
  } finally {
    cleanup();
  }
}
```

Run: `cd web && npm run test && npx tsc -b`
Expected: probeTurn 两用例 PASS。

- [ ] **Step 3: Monitor 页测试（先写）**

`web/src/pages/Monitor.test.tsx`：

```tsx
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
```

Run: `cd web && npm run test`
Expected: FAIL——Monitor 模块不存在。

- [ ] **Step 4: 实现 Monitor.tsx + 路由 + 侧栏**

`web/src/pages/Monitor.tsx`：

```tsx
import { useCallback, useEffect, useState } from "react";
import { api } from "../api";
import { probeTurn } from "../lib/turnProbe";

/** GET /api/turn/status 响应（设计 §3.1）。 */
interface TurnStatus {
  mode: "pool" | "urls" | "unconfigured";
  icePolicy: string;
  pool: { ip: string; port: number; urls: string[]; healthy: boolean }[];
  fallbackUrls: string[];
  username: string;
  credential: string;
  activeDesktopSessions: number;
}

/** 可探测行：池成员（healthy 来自 server 探测）或 fallback URL（无探测）。 */
interface Row {
  key: string;
  label: string;
  urls: string[];
  healthy: boolean | null;
}

type ProbeState = "idle" | "probing" | number | "timeout" | "error";

const REFRESH_MS = 10_000;

export default function Monitor() {
  const [status, setStatus] = useState<TurnStatus | null>(null);
  const [failed, setFailed] = useState(false);
  const [probes, setProbes] = useState<Record<string, ProbeState>>({});
  const [probing, setProbing] = useState(false);

  const load = useCallback(async () => {
    try {
      setStatus(await api<TurnStatus>("/api/turn/status"));
      setFailed(false);
    } catch {
      setFailed(true);
    }
  }, []);

  useEffect(() => {
    void load();
    const t = window.setInterval(() => void load(), REFRESH_MS);
    return () => window.clearInterval(t);
  }, [load]);

  const rows = (status: TurnStatus): Row[] => [
    ...status.pool.map((p) => ({
      key: `${p.ip}:${p.port}`,
      label: `${p.ip}:${p.port}`,
      urls: p.urls,
      healthy: p.healthy,
    })),
    ...status.fallbackUrls.map((u) => ({ key: u, label: u, urls: [u], healthy: null })),
  ];

  /** 串行探测全部行（并发会互相抬时延，设计 §3.2）。 */
  const runProbes = useCallback(async () => {
    if (!status || probing) return;
    setProbing(true);
    try {
      for (const row of rows(status)) {
        setProbes((p) => ({ ...p, [row.key]: "probing" }));
        try {
          const ms = await probeTurn({ urls: row.urls, username: status.username, credential: status.credential });
          setProbes((p) => ({ ...p, [row.key]: ms }));
        } catch {
          setProbes((p) => ({ ...p, [row.key]: "timeout" }));
        }
      }
    } finally {
      setProbing(false);
    }
  }, [status, probing]);

  // 状态就绪且未探测过 → 自动跑一轮。
  useEffect(() => {
    if (!status || status.mode === "unconfigured") return;
    if (Object.keys(probes).length > 0 || probing) return;
    void runProbes();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [status]);

  const probeCell = (key: string): string => {
    const s = probes[key];
    if (s === undefined || s === "idle") return "—";
    if (s === "probing") return "probing…";
    if (s === "timeout" || s === "error") return s;
    return `${s} ms`;
  };

  if (failed) {
    return (
      <div className="page">
        <h1>Monitor</h1>
        <div className="empty">Failed to load TURN status.</div>
      </div>
    );
  }
  if (!status) {
    return (
      <div className="page">
        <h1>Monitor</h1>
        <div className="loading">Loading…</div>
      </div>
    );
  }

  return (
    <div className="page">
      <h1>Monitor</h1>

      <section className="card">
        <div className="download-card-head">
          <h2>TURN servers</h2>
          <span className="dim mono">
            {status.mode} · ice {status.icePolicy}
          </span>
          <button type="button" className="secondary" disabled={probing} onClick={() => void runProbes()}>
            {probing ? "Probing…" : "Re-probe"}
          </button>
        </div>
        {status.mode === "unconfigured" ? (
          <div className="empty">TURN is not configured on this server.</div>
        ) : (
          <table>
            <thead>
              <tr>
                <th>Target</th>
                <th>Transports</th>
                <th>Server health</th>
                <th>Browser RTT</th>
              </tr>
            </thead>
            <tbody>
              {rows(status).map((row) => (
                <tr key={row.key}>
                  <td className="mono">{row.label}</td>
                  <td className="dim mono">{row.urls.some((u) => u.endsWith("udp")) ? "udp/tcp" : "tcp"}</td>
                  <td className={row.healthy === false ? "form-error" : "dim"}>
                    {row.healthy === null ? "—" : row.healthy ? "healthy" : "unhealthy"}
                  </td>
                  <td className="mono">{probeCell(row.key)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      <section className="card">
        <h2>Usage</h2>
        <dl className="detail-grid">
          <dt>Active desktop sessions</dt>
          <dd className="mono">{status.activeDesktopSessions}</dd>
        </dl>
        <p className="dim">
          Desktop sessions relay through TURN by default; with ICE policy “all” a
          browser on the same LAN may connect directly.
        </p>
      </section>
    </div>
  );
}
```

`web/src/main.tsx`：import Monitor；App children（`/profile` 之后）加：

```tsx
            <Route path="/monitor" element={<Monitor />} />
```

`web/src/components/Sidebar.tsx` nav（Download 之后）：

```tsx
        <NavLink to="/monitor">Monitor</NavLink>
```

Run: `cd web && npm run test && npx tsc -b`
Expected: PASS（含既有全部）。

- [ ] **Step 5: 提交**

```bash
git add web/src/lib/turnProbe.ts web/src/lib/turnProbe.test.ts web/src/pages/Monitor.tsx web/src/pages/Monitor.test.tsx web/src/main.tsx web/src/components/Sidebar.tsx
git commit -m "feat(web): monitor page - TURN pool status, browser loopback probe, usage aggregate"
```

---

### Task 3: web — DesktopLive 统计补齐 + 状态栏分组

**Files:**
- Create: `web/src/pages/desktop/turnLabel.ts`
- Test: `web/src/pages/desktop/turnLabel.test.ts`
- Modify: `web/src/pages/DesktopLive.tsx`（net state、readFeedbackStats 扩展、sendFeedback 提升、状态栏分组、统计面板）
- Modify: `web/src/styles.css`（.screen-bar-group）

**Interfaces:**
- Consumes: DesktopLive 既有 `readFeedbackStats`（返回 `{sample, rttMs, availBps}`）、`sendFeedback`（1s 环，`corrSnap.e2eP95Ms`）、`stats` state、状态栏 JSX（~1121-1237）、统计面板 JSX（~1219-1225）。
- Produces: `turnRelayLabel(local: LocalCandLike | undefined): string`——`relay` → `via TURN <ip> (udp|tcp)`，host/srflx → `direct`，缺输入 → `—`。

- [ ] **Step 1: turnRelayLabel 纯函数（先写测试）**

`web/src/pages/desktop/turnLabel.test.ts`：

```ts
import { describe, expect, it } from "vitest";
import { turnRelayLabel } from "./turnLabel";

describe("turnRelayLabel", () => {
  it("labels relay candidates with the TURN server address and protocol", () => {
    expect(
      turnRelayLabel({ candidateType: "relay", address: "1.2.3.4", relayProtocol: "udp" }),
    ).toBe("via TURN 1.2.3.4 (udp)");
    expect(
      turnRelayLabel({ candidateType: "relay", address: "5.6.7.8", relayProtocol: "tcp" }),
    ).toBe("via TURN 5.6.7.8 (tcp)");
  });
  it("falls back when relay fields are missing", () => {
    expect(turnRelayLabel({ candidateType: "relay" })).toBe("via TURN (?)");
    expect(turnRelayLabel({ candidateType: "relay", address: "1.2.3.4" })).toBe("via TURN 1.2.3.4 (udp)");
  });
  it("labels non-relay (direct) paths", () => {
    expect(turnRelayLabel({ candidateType: "host", address: "192.168.1.5" })).toBe("direct");
    expect(turnRelayLabel({ candidateType: "srflx", address: "1.1.1.1" })).toBe("direct");
  });
  it("dash when no candidate yet", () => {
    expect(turnRelayLabel(undefined)).toBe("—");
  });
});
```

Run: `cd web && npm run test`
Expected: FAIL——模块不存在。

`web/src/pages/desktop/turnLabel.ts`：

```ts
/**
 * TURN 中继路径标签（设计 §3.3）：selected candidate-pair 的 local
 * candidate 为 relay 时，其 address 即 TURN 服务器地址（TURN 在该地址上
 * 为本端分配 relay 端口）。纯函数，供 DesktopLive 的 stats 轮询调用。
 */
export interface LocalCandLike {
  candidateType?: string;
  address?: string;
  relayProtocol?: string;
}

export function turnRelayLabel(local: LocalCandLike | undefined): string {
  if (!local) return "—";
  if (local.candidateType === "relay") {
    const proto = local.relayProtocol ?? "udp";
    return local.address ? `via TURN ${local.address} (${proto})` : "via TURN (?)";
  }
  return "direct";
}
```

Run: `cd web && npm run test`
Expected: PASS（4 用例）。

- [ ] **Step 2: DesktopLive net state + readFeedbackStats 扩展**

`web/src/pages/DesktopLive.tsx`——文件顶部 import 区加：

```ts
import { turnRelayLabel } from "./desktop/turnLabel";
```

组件 state 区（`const [stats, setStats] = useState(...)` 之后）加：

```tsx
  /** 网络面指标（1s 反馈环提升为 state 供展示；0/null = 尚无样本）。 */
  const [net, setNet] = useState<{
    rttMs: number;
    availBps: number;
    e2eP95Ms: number | null;
    relay: string;
  }>({ rttMs: 0, availBps: 0, e2eP95Ms: null, relay: "—" });
```

`readFeedbackStats`（既有）改造——返回类型加 `relay: string`；report.forEach 里增收集：

```ts
        // local candidate 表 + selected pair：解析 TURN 中继路径（§3.3）。
        const locals = new Map<string, { candidateType?: string; address?: string; relayProtocol?: string }>();
```

（声明放 forEach 之前）forEach 分支追加：

```ts
          } else if (st.type === "local-candidate") {
            const c = st as { id?: string; candidateType?: string; address?: string; relayProtocol?: string };
            if (c.id) locals.set(c.id, c);
          } else if (st.type === "candidate-pair") {
            // 既有 rtt/availBps 逻辑保持；另记 selected pair 的 local 引用。
            const p = st as RTCIceCandidatePairStats;
            if (p.nominated && p.state === "succeeded" && p.localCandidateId) {
              selectedLocalId = p.localCandidateId;
            }
          }
```

（`let selectedLocalId: string | undefined;` 声明于 forEach 前；既有 candidate-pair 分支里 nominated 优先取 rtt 的逻辑不动——新逻辑并入同一分支。）函数末尾：

```ts
      return { sample, rttMs, availBps, relay: turnRelayLabel(locals.get(selectedLocalId ?? "")) };
```

返回类型签名同步加 `relay: string`；`if (!pc)` 早退返回补 `relay: "—"`。

`sendFeedback`（既有）——`const { sample, rttMs, availBps } = await readFeedbackStats();` 改为解构含 `relay`，在 `const corrSnap = correlator.snapshot();` 附近（fb 组装后、send 前后皆可，且**不受 estimatedBps<=0 早退影响**——展示与上报解耦）加：

```ts
      // 展示面提升：无论本轮是否上报 agent，网络指标都刷新 UI。
      setNet((n) => ({
        rttMs: rttMs > 0 ? Math.round(rttMs * 10) / 10 : n.rttMs,
        availBps: availBps > 0 ? Math.round(availBps / 1000) : n.availBps, // kbps 展示
        e2eP95Ms: corrSnap.e2eP95Ms !== null ? Math.round(corrSnap.e2eP95Ms * 10) / 10 : n.e2eP95Ms,
        relay: relay !== "—" ? relay : n.relay,
      }));
```

（注意：该 setNet 放在 `if (estimatedBps <= 0) return;` **之前**，且在 `sample` 空判早退之后仍可达——若 `!sample` 早退发生在前面，net 保持旧值即可接受。`corrSnap` 若原代码在早退后才取，将 `const corrSnap = correlator.snapshot();` 上移到早退前并复用。）

- [ ] **Step 3: 状态栏分组 + 统计面板**

状态栏 JSX（现有 `<div className="screen-bar">` 内）重组为三组（子元素次序调整，控件本体不动）：

```tsx
      <div className="screen-bar">
        <div className="screen-bar-group">
          <strong>{nodeName ?? nodeId}</strong>
          <span className={`screen-state screen-state-${state}`}>{STATE_LABELS[state]}</span>
          <span className="dim mono">ice:{iceState}</span>
          {dims && <span className="dim mono">{dims}</span>}
          {displays.length > 1 && (
            <select
              className="desktop-display-select mono"
              value={displaySel}
              onChange={(e) => sendSwitchDisplay(Number(e.target.value))}
              title="capture display (switch_display)"
            >
              {displays.map((d) => (
                <option key={d.index} value={d.index}>
                  {`#${d.index} ${d.w}x${d.h}${d.primary ? " *" : ""}`}
                </option>
              ))}
            </select>
          )}
          <span className="dim mono">{`h264/${iceModeRef.current ?? "relay"}`}</span>
        </div>
        <div className="screen-bar-group">
          <span className="dim mono">{`fps ${stats.fps}`}</span>
          <span className="dim mono">{`e2e p95 ${net.e2eP95Ms !== null ? `${net.e2eP95Ms}ms` : "—"}`}</span>
          <span className="dim mono">{`rtt ${net.rttMs > 0 ? `${net.rttMs}ms` : "—"}`}</span>
          <span className="dim mono">{net.relay}</span>
        </div>
        <div className="screen-bar-group">
          {agentState && <span className="dim mono">{agentState}</span>}
          <button onClick={sendPli} className="desktop-pli" type="button">PLI</button>
          {/* SAS / lease 按钮、chips、notice、startError 原样移入本组 */}
        </div>
      </div>
```

统计面板（`desktop-stats` overlay）补行：

```tsx
        <div className="desktop-stats mono">
          <div>fps {stats.fps}</div>
          <div>e2e p95 {net.e2eP95Ms !== null ? `${net.e2eP95Ms}ms` : "—"}</div>
          <div>rtt {net.rttMs > 0 ? `${net.rttMs}ms` : "—"}</div>
          <div>rate {net.availBps > 0 ? `${net.availBps}kbps` : "—"}</div>
          <div>{net.relay}</div>
          <div>first frame {stats.firstFrameMs ? `${stats.firstFrameMs}ms` : "—"}</div>
          <div>decoded {stats.framesDecoded}</div>
          <div>key {stats.keyframesDecoded}</div>
          <div>pli sent {stats.plis}</div>
        </div>
```

`web/src/styles.css` 追加（放 screen-bar 相关规则附近）：

```css
/* 状态栏三组（会话信息｜媒体统计｜操作）：flex 分组 + 左分隔线。 */
.screen-bar-group {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
}
.screen-bar-group + .screen-bar-group {
  border-left: 1px solid var(--border, #333);
  padding-left: 10px;
  margin-left: 2px;
}
```

（`--border` 若不存在则用具体色值 #333 与页面暗色基调一致；以 styles.css 实际变量为准。）

- [ ] **Step 4: 验证 + 提交**

Run: `cd web && npm run test && npx tsc -b && npx oxlint`
Expected: PASS；oxlint 无新增警告（对照 HEAD）。

```bash
git add web/src/pages/desktop/turnLabel.ts web/src/pages/desktop/turnLabel.test.ts web/src/pages/DesktopLive.tsx web/src/styles.css
git commit -m "feat(web): desktop live status bar groups, e2e/rtt/bitrate/TURN relay stats surfaced"
```

---

## 自审记录

1. **Spec 覆盖**：§3.1→Task 1；§3.2→Task 2；§3.3→Task 3；§4 契约→Task 1 Step 3/4；§5 测试分布各任务；§6 验收跨三任务终审覆盖。非目标未越界（无 coturn 指标、无会话明细、无深度改版、proto/agent 零改动）。
2. **占位符扫描**：无 TBD/TODO；所有代码步骤含完整代码。Task 1 Step 3 的 testenv server 暴露方式给了两个落地途径（httptest.NewServer 包 Router 或既有导出），实施者二选一——是歧义消解而非占位；Task 3 Step 2 的 corrSnap 上移同理（给出了移动指令）。Task 1 Step 2 对 Create 签名的以实为准注释同。
3. **类型一致性**：`probeTurn(target: ProbeTarget, timeoutMs?)`/`pcFactory.make` 在 Task 2 定义、Monitor.test 经 vi.mock 消费同签名；`turnServerStatus{IP,Port,URLs,Healthy}` 与 handler/测试字段一致；JSON 字段名（mode/icePolicy/pool/fallbackUrls/username/credential/activeDesktopSessions）Task 1 产出与 Task 2 的 TurnStatus 接口逐一对应；`net` state 形状在 Task 3 Step 2/3 两处一致（rttMs/availBps(kbps)/e2eP95Ms/relay）。
