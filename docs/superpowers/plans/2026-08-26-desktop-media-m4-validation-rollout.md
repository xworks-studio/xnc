# Desktop Media M4 Validation/Rollout Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prove the redesigned pipeline across hardware, desktop transitions, long idle periods and constrained TURN networks, then remove v1 only after canary gates pass.

**Architecture:** A repeatable validation harness produces machine-readable evidence for correctness, latency, resource use and recovery. Server-controlled rollout flags permit instant fallback without mixing media protocols inside one live session.

**Tech Stack:** PowerShell, Go diagnostics, Windows Performance Counters, Chrome/Edge WebRTC, existing XNC dev topology.

**Spec:** `docs/superpowers/specs/2026-08-26-desktop-media-pipeline-design.md`

## Global Constraints

- Never collect or persist raw desktop pixels in production telemetry.
- A live session uses exactly one media protocol version from start to finish.
- Any stale-content, identity-regression, unbounded-queue or unrecovered-freeze result blocks rollout.
- Preserve `desktop_pipeline_v2=false` until every mandatory gate passes.

---

### Task 1: Build the repeatable validation harness

**Files:**
- Create: `scripts/desktop-media/run-soak.ps1`
- Create: `scripts/desktop-media/run-device-matrix.ps1`
- Create: `scripts/desktop-media/collect-metrics.ps1`
- Create: `tools/desktopreport/main.go`
- Create: `tools/desktopreport/go.mod`

**Interfaces:**
- Produces: one JSON result per run and one merged Markdown table.

- [x] **Step 1: Define the result schema in Go**

```go
type RunResult struct {
    Host, GPU, Driver, OS, Browser string
    Resolution string
    TargetFPS uint32
    DurationSec uint64
    OldFrameRegressions, EpochRegressions uint64
    UnrecoveredFreezes uint64
    CaptureToAUP95Ms, InputToPresentP95Ms float64
    QueueP95Ms, QueueMaxMs, CPUPercent, WorkingSetMB float64
    Verdict string
}
```

- [x] **Step 2: Add schema tests and merger**

Feed passing and failing JSON fixtures. `desktopreport` must return nonzero if any P0 counter is nonzero or a numeric gate is exceeded.

- [x] **Step 3: Implement bounded PowerShell runners**

Resolve every output path under `artifacts/desktop-media/<timestamp>` before starting. Use `Start-Process -WindowStyle Hidden`, explicit process IDs and `try/finally` cleanup. Do not kill by executable name.

- [x] **Step 4: Run tool tests and commit**

Run: `cd tools\desktopreport; go test ./... -count=1`

```bash
git add scripts/desktop-media/run-soak.ps1 scripts/desktop-media/run-device-matrix.ps1 scripts/desktop-media/collect-metrics.ps1 tools/desktopreport
git commit -m "test(desktop): add media validation harness"
```

### Task 2: Execute correctness and recovery gates

**Files:**
- Create: `docs/superpowers/plans/2026-08-26-desktop-media-m4-results.md`

- [ ] **Step 1: Run the 8-hour A/B/C soak**

Alternate motion for 60s and idle for randomized 1–10 minute windows. During idle issue new-viewer, PLI, queue-overflow and reconnect events. Required: zero contentId/hash regressions and zero unrecovered freezes.

- [ ] **Step 2: Run 100 reset cycles**

Cover lock/unlock, UAC, resolution change, display switch and injected ACCESS_LOST. Required: exactly one epoch increment per reset execution and recovery within 2s for at least 99 cycles; any stale frame fails regardless of percentage.

- [x] **Step 3: Run encoder contract failures**

Inject delayed, missing and unknown-timestamp outputs. Required: no invalid AU reaches pipe; hardware fails over to software after the configured threshold without oscillation.

- [x] **Step 4: Record exact commands, artifacts and verdicts**

Write the result document with hardware/driver versions, artifact paths and numeric values. Do not summarize a failed mandatory gate as pass.

- [x] **Step 5: Commit evidence**

```bash
git add docs/superpowers/plans/2026-08-26-desktop-media-m4-results.md
git commit -m "docs(desktop): record media reliability gates"
```

### Task 3: Execute performance and network gates

**Files:**
- Modify: `docs/superpowers/plans/2026-08-26-desktop-media-m4-results.md`

- [x] **Step 1: Run hardware matrix**

Test Intel, NVIDIA, AMD, hybrid GPU and software-only/VM where available on Windows 10 and 11. Record selected MFT, adapter LUID, fallback reason and D3D debug verdict.

- [x] **Step 2: Run 1080p60 performance gate**

Required: GPU copy+convert p95 <3ms, capture→AU p95 <15ms, CPU <15%, working set <350MB and target FPS ≥95% under sufficient bandwidth.

- [x] **Step 3: Run TURN network matrix**

Use 15Mbps/30ms, 5Mbps/100ms and 1Mbps/250ms relay-only cases. Required: queue p95 <50ms, hard max ≤100ms, no latency accumulation, and slow spectator isolation.

- [ ] **Step 4: Run Chrome and Edge**

Required: first visible frame p95 <1s on controlled LAN relay; input-to-present p95 <150ms; PLI-to-present p95 <1s; frame-meta correlates every sampled rVFC timestamp without identity regression.

- [x] **Step 5: Update and commit evidence**

```bash
git add docs/superpowers/plans/2026-08-26-desktop-media-m4-results.md
git commit -m "docs(desktop): record media performance gates"
```

### Task 4: Add server-controlled canary rollout

**Files:**
- Modify: `proto/session.go`
- Modify: `server/internal/api/desktop_handlers.go`
- Modify: `server/internal/api/desktop_handlers_test.go`
- Modify: `agent/desktop/session.go`

**Interfaces:**
- Produces session parameter `mediaProtocol: "v1" | "v2"` and node configuration percentage/allowlist.

- [x] **Step 1: Add server selection tests**

Assert explicit node allowlist beats percentage, an existing live session never changes version, unknown values fail closed to v1, and rollback sets all new sessions to v1.

- [x] **Step 2: Implement immutable per-session selection**

Select version before starting Host/Publisher, pass it through SESSION_OPEN, and reject HostHello whose `media_protocol` differs. Never negotiate down after media starts.

- [x] **Step 3: Run server and Agent tests**

Run: `cd server; go test ./internal/api -count=1`

Run: `cd agent; go test ./desktop ./desktoppipe -count=1`

- [x] **Step 4: Commit**

```bash
git add proto/session.go server/internal/api/desktop_handlers.go server/internal/api/desktop_handlers_test.go agent/desktop/session.go
git commit -m "feat(desktop): gate media v2 rollout per session"
```

### Task 5: Canary, promote, and remove v1

**Files:**
- Modify: `native/desktop/xnc-desktop.cpp`
- Modify: `native/desktop/pipeline.cpp`
- Modify: `agent/desktoppipe/client.go`
- Modify: `agent/desktop/transport.go`
- Modify: `docs/superpowers/plans/2026-08-26-desktop-media-m4-results.md`

- [ ] **Step 1: Enable XIAOXIN-only canary**

Run at least 24 hours. Required: all P0 anomaly counters zero and no fallback rate increase compared with validation runs.

- [ ] **Step 2: Promote to development channel**

Run at least 72 hours. Compare capture→present, queue age, MFT fallback, PLI recovery, CPU and memory against v1. Any mandatory regression triggers the server rollback switch.

- [ ] **Step 3: Make v2 default**

Keep the v1 flag available for one release cycle. Record the exact release and rollback observation window in the result document.

- [ ] **Step 4: Delete v1 after the observation window**

Remove FrameCache idle-base replay, CPU-readback normal path, `MSG_FRAME 0x0105`, `TrackLocalStaticSample`, `frameDuration`, and compatibility branches. Keep GDI/software fallback because they are part of v2.

- [ ] **Step 5: Run the complete gate after deletion**

Run: `cmd /c native\desktop\build.bat && bin\xnc-desktop.exe --selftest --desktop-pipeline-v2`

Run: `cd agent; go test ./... -count=1`

Run: `cd server; go test ./... -count=1`

Run: `cd web; npm run test`

Run: `cd web; npm run build`

Run: `git diff --check`

- [ ] **Step 6: Commit**

```bash
git add native/desktop/xnc-desktop.cpp native/desktop/pipeline.cpp agent/desktoppipe/client.go agent/desktop/transport.go docs/superpowers/plans/2026-08-26-desktop-media-m4-results.md
git commit -m "refactor(desktop): retire media pipeline v1"
```
