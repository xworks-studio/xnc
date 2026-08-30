# Desktop Media M3 RTP/QoS/Browser Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add per-viewer recovery, conservative shared-stream QoS, and end-to-end frame provenance through the browser.

**Architecture:** A shared immutable AU hub feeds independent ViewerSenders. Each sender owns RTP/RTCP state and WAIT_IDR recovery; a global controller adapts the one encoder from prioritized feedback, while an unreliable metadata channel makes displayed content observable.

**Tech Stack:** Go, Pion WebRTC v4.2.18, React/TypeScript, Browser WebRTC Stats and requestVideoFrameCallback.

**Spec:** `docs/superpowers/specs/2026-08-26-desktop-media-pipeline-design.md`

## Global Constraints

- One encoded stream, maximum four viewers.
- Controller QoS has priority; hidden viewers do not affect global encoding.
- No arbitrary post-encode P-frame sampling.
- RTP send queue target <50ms and hard maximum 100ms.
- Media remains relay-only over TURN/TLS/TCP 443.

---

### Task 1: Extract ViewerSender and enforce WAIT_IDR

**Files:**
- Create: `agent/desktop/viewer_sender.go`
- Create: `agent/desktop/viewer_sender_test.go`
- Modify: `agent/desktop/transport.go`
- Modify: `agent/desktop/frames.go`

**Interfaces:**
- Produces: `ViewerSender.Enqueue(Frame)`, `Discontinuity`, `Pause`, `Resume`, `Stats`, `Close`.

- [x] **Step 1: Add state-machine tests**

```go
s := newFakeViewerSender()
s.Enqueue(delta(1)); if s.sent() != 0 { t.Fatal("delta before IDR") }
s.Enqueue(idr(2)); if s.state() != stateLive { t.Fatal("IDR did not start stream") }
s.overflow(); s.Enqueue(delta(3)); if s.sent() != 1 { t.Fatal("sent broken delta") }
s.Enqueue(idr(4)); if s.state() != stateLive { t.Fatal("recovery IDR failed") }
```

- [x] **Step 2: Implement a one-frame/time-bounded queue**

Use states `waitIDR`, `live`, `paused`, `closed`. Queue age above 100ms flushes packets, clears retransmission state, enters waitIDR and calls the merged keyframe callback once. Pace RTP packets with a per-viewer token bucket set to 85% of its current budget; packets whose deadline would exceed 100ms are not queued.

- [x] **Step 3: Keep packetization private per viewer**

Each sender owns RTP clock, sequence, SSRC, TWCC and packet queue. It receives immutable `Frame.AU` bytes but creates independent RTP packets.

- [x] **Step 4: Run and commit**

Run: `cd agent; go test ./desktop -run ViewerSender -count=1`

```bash
git add agent/desktop/viewer_sender.go agent/desktop/viewer_sender_test.go agent/desktop/transport.go agent/desktop/frames.go
git commit -m "feat(agent): isolate each desktop viewer sender"
```

### Task 2: Add KeyframeCoordinator

**Files:**
- Create: `agent/desktop/keyframe_coordinator.go`
- Create: `agent/desktop/keyframe_coordinator_test.go`
- Modify: `agent/desktop/qos_min.go`
- Modify: `agent/desktop/session.go`

**Interfaces:**
- Produces: `Request(reason, urgent)`, `OnIDR(codecEpoch, encodeSeq)`, `Pending()`.

- [x] **Step 1: Test merging and cooldown**

With a fake clock, send PLI/FIR/overflow inside 250ms and assert one Host request. Assert new-subscriber and epoch-change urgent requests bypass cooldown but never duplicate an already pending request.

- [x] **Step 2: Implement coordinator**

Track pending reason set, last request time and requested epoch. Clear only when a matching/newer IDR is observed. At 250ms or two frame periods without IDR, emit `encoder_idr_timeout` state.

- [x] **Step 3: Route all keyframe sources through it**

Replace direct `Source.RequestKeyframe` calls from RTCP, connection-ready and overflow paths.

- [x] **Step 4: Run and commit**

Run: `cd agent; go test ./desktop -run Keyframe -count=1`

```bash
git add agent/desktop/keyframe_coordinator.go agent/desktop/keyframe_coordinator_test.go agent/desktop/qos_min.go agent/desktop/session.go
git commit -m "feat(agent): merge desktop keyframe requests"
```

### Task 3: Implement shared-stream QoS decisions

**Files:**
- Create: `agent/desktop/qos_controller.go`
- Create: `agent/desktop/qos_controller_test.go`
- Modify: `agent/desktop/qos_min.go`
- Modify: `agent/desktop/source.go`
- Modify: `agent/desktoppipe/client.go`
- Modify: `agent/desktop/session.go`

**Interfaces:**
- Produces: `QoSController.Observe(ViewerFeedback) []Action` and pipe `SET_VIDEO_CONFIG`.

- [x] **Step 1: Encode the decision table as failing tests**

Test immediate 30% downshift on queueAge>100ms, no upshift before 10 stable seconds, hidden viewer exclusion, controller priority, and spectator pause below 35% controller bandwidth.

- [x] **Step 2: Implement exact ladders**

```go
var fpsLadder = []uint32{60, 30, 20, 15, 10, 5}
var heightLadder = []uint32{1440, 1080, 900, 720}
```

Use 85% of estimated bandwidth, bitrate range 500kbps–15Mbps, down decisions at most once per second and up decisions at most once per three seconds after stability.

- [x] **Step 3: Add config control to Source/Pipe**

```go
type VideoConfig struct { Bitrate, FPS, MaxW uint32 }
type Source interface { SetVideoConfig(VideoConfig) error /* existing methods remain */ }
```

Bitrate/FPS update hot; MaxW change triggers codec epoch reset.

Parse control message `{"type":"viewer_feedback","visible":bool,"estimatedBps":uint64,"queueMs":number,"decodeQueue":number,"rttMs":number}` in `session.go` and feed it to the controller. A `PauseSpectator` action sends stable state `spectator_network_paused` to that viewer and calls `ViewerSender.Pause()`.

- [x] **Step 4: Run and commit**

Run: `cd agent; go test ./desktop ./desktoppipe -count=1`

```bash
git add agent/desktop/qos_controller.go agent/desktop/qos_controller_test.go agent/desktop/qos_min.go agent/desktop/source.go agent/desktoppipe/client.go
git commit -m "feat(desktop): close the shared-stream QoS loop"
```

### Task 4: Add frame-meta telemetry channel

**Files:**
- Create: `agent/desktop/frame_meta.go`
- Create: `agent/desktop/frame_meta_test.go`
- Modify: `agent/desktop/transport.go`
- Modify: `agent/desktop/session.go`

**Interfaces:**
- Produces binary `FrameMetaV1` with codecEpoch, contentId, encodeSeq, RTP timestamp, sourceMonoUs and keyed hash64.

- [x] **Step 1: Add byte-exact codec tests**

Assert fixed 56-byte encoding, little-endian fields, version rejection and deterministic session-keyed hash.

- [x] **Step 2: Create unordered no-retransmit channel**

Create `frame-meta` with `Ordered=false` and `MaxRetransmits=0`. Send metadata after the last RTP packet of each frame; failure increments telemetryDrops and never blocks media.

- [x] **Step 3: Run and commit**

Run: `cd agent; go test ./desktop -run FrameMeta -count=1`

```bash
git add agent/desktop/frame_meta.go agent/desktop/frame_meta_test.go agent/desktop/transport.go agent/desktop/session.go
git commit -m "feat(desktop): expose frame provenance metadata"
```

### Task 5: Correlate displayed frames in React

**Files:**
- Create: `web/src/lib/desktopFrameMeta.ts`
- Create: `web/src/lib/desktopFrameMeta.test.ts`
- Modify: `web/src/pages/DesktopLive.tsx`
- Modify: `web/package.json`
- Modify: `web/package-lock.json`

**Interfaces:**
- Produces: `decodeFrameMeta(ArrayBuffer)`, bounded RTP-timestamp metadata map, one-second viewer feedback.

- [x] **Step 1: Add TypeScript golden-vector tests**

Decode the Go golden vector and assert every uint64 value using `bigint`. Reject wrong version/length without throwing from the DataChannel handler.

Install the test runner and add `"test": "vitest run"` to package scripts.

Run: `cd web; npm install --save-dev vitest`

- [x] **Step 2: Extend rVFC metadata typing**

Add optional `rtpTimestamp`, `captureTime`, `receiveTime`, and `processingDuration`. On each presented frame, look up frame-meta by RTP timestamp and assert codecEpoch/contentId do not regress.

- [x] **Step 3: Send one-second feedback**

Report page visibility, decoded/dropped deltas, jitter-buffer average delta, RTT, freezes and rVFC cadence through the existing control channel. Do not send when disposed.

- [x] **Step 4: Run and commit**

Run: `cd web; npm run test`

Run: `cd web; npm run build`

```bash
git add web/src/lib/desktopFrameMeta.ts web/src/lib/desktopFrameMeta.test.ts web/src/pages/DesktopLive.tsx web/package.json web/package-lock.json
git commit -m "feat(web): correlate rendered desktop frame identity"
```

### Task 6: TURN/TCP congestion E2E

**Files:**
- Modify: `tools/e2eviewer/main.go`
- Modify: `tools/e2eviewer/main_test.go`
- Create: `scripts/desktop-media/run-network-matrix.ps1`

- [x] **Step 1: Add report fields**

Record queueAge p50/p95/max, RTP timestamp regressions, contentId regressions, PLI-to-IDR and PLI-to-present, per-viewer paused intervals and QoS actions.

- [x] **Step 2: Add deterministic network cases**

Run relay-only cases at 15Mbps/30ms, 5Mbps/100ms and 1Mbps/250ms. Assert queue hard max 100ms, controller remains LIVE when a spectator is throttled, and every recovery begins with IDR.

- [x] **Step 3: Run Agent and Web suites**

Run: `cd agent; go test ./desktop ./desktoppipe -count=1`

Run: `cd web; npm run test`

Run: `cd web; npm run build`

- [x] **Step 4: Commit**

```bash
git add tools/e2eviewer/main.go tools/e2eviewer/main_test.go scripts/desktop-media/run-network-matrix.ps1
git commit -m "test(desktop): validate TURN congestion recovery"
```
