# Desktop Media M0 Correctness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Eliminate every known stale-frame and timestamp-misattribution path while retaining the current CPU-readable pipeline.

**Architecture:** Make staging reads synchronous and content-correct, make the idle cache track the latest frame, pair every delayed MFT AU with its submission, and replace implicit Pion sample durations with an explicit 90 kHz RTP clock.

**Tech Stack:** C++17, D3D11/DXGI, Media Foundation, Go 1.x, Pion WebRTC v4.2.18.

**Spec:** `docs/superpowers/specs/2026-08-26-desktop-media-pipeline-design.md`

## Global Constraints

- Preserve the current pipe wire in M0; Pipe v2 starts in M1.
- Do not add FFmpeg or vendor codec SDKs.
- Queues remain bounded and drop-oldest before encode.
- Every regression test must validate decoded content or immutable frame identity, not only an IDR flag.
- Preserve unrelated dirty-worktree changes.

---

### Task 1: Make each DXGI acquire return the frame copied in that acquire

**Files:**
- Modify: `native/desktop/dxgi_capture.h`
- Modify: `native/desktop/dxgi_capture.cpp`
- Test: `native/desktop/desktop_selftest.cpp`

**Interfaces:**
- Produces: `constexpr uint32_t StagingReadIndex(uint32_t write_index)` returning `write_index`.
- Preserves: `ICapture::Acquire(FrameBlob&, std::string*, uint32_t)`.

- [x] **Step 1: Add a failing staging identity test**

```cpp
CHECK("staging-read-is-current-0", xnc::StagingReadIndex(0) == 0);
CHECK("staging-read-is-current-1", xnc::StagingReadIndex(1) == 1);
```

- [x] **Step 2: Build and confirm the helper is missing**

Run: `cmd /c native\desktop\build.bat`

Expected: compile failure naming `StagingReadIndex`.

- [x] **Step 3: Implement current-buffer mapping**

Add to `dxgi_capture.h`:

```cpp
inline constexpr uint32_t StagingReadIndex(uint32_t write_index) {
  return write_index;
}
```

In both NV12 and BGRA paths, set `read_buf = StagingReadIndex(cur)`, issue `CopyResource`, map that buffer, then toggle `staging_cur_`. Remove comments claiming N+1 overlaps N readback.

- [x] **Step 4: Run native tests**

Run: `cmd /c native\desktop\build.bat`

Run: `bin\xnc-desktop.exe --selftest`

Expected: exit 0 and both staging assertions pass.

- [x] **Step 5: Commit**

```bash
git add native/desktop/dxgi_capture.h native/desktop/dxgi_capture.cpp native/desktop/desktop_selftest.cpp
git commit -m "fix(desktop): return the current DXGI staging frame"
```

### Task 2: Replace generation-first cache semantics with LatestFrame

**Files:**
- Modify: `native/desktop/pipeline.h`
- Modify: `native/desktop/pipeline.cpp`
- Test: `native/desktop/desktop_selftest.cpp`

**Interfaces:**
- Produces: `LatestFrameStore::Update(const FrameBlob&)`, `Snapshot(FrameBlob*) const`, `Invalidate()`.
- Consumes: existing `FrameBlob` value type.

- [x] **Step 1: Add a failing A/B/C latest-frame test**

```cpp
xnc::LatestFrameStore latest;
latest.Update(MakeSolidFrame(1, 0x11));
latest.Update(MakeSolidFrame(2, 0x22));
latest.Update(MakeSolidFrame(3, 0x33));
xnc::FrameBlob got;
CHECK("latest-frame-snapshot", latest.Snapshot(&got) &&
      got.mono_us == 3 && got.bgra[0] == 0x33);
```

- [x] **Step 2: Build and confirm `LatestFrameStore` is missing**

Run: `cmd /c native\desktop\build.bat`

Expected: compile failure naming `LatestFrameStore`.

- [x] **Step 3: Implement the store and replace `PipelineShared::base`**

```cpp
class LatestFrameStore {
 public:
  void Update(const FrameBlob& f);
  bool Snapshot(FrameBlob* out) const;
  void Invalidate();
 private:
  mutable std::mutex mu_;
  FrameBlob frame_;
  bool valid_ = false;
};
```

Update it after every successful capture, not only `is_base`. `IdleFeed` must snapshot it immediately before re-encoding. Reset must call `Invalidate()` and cannot feed until the new base is captured.

- [x] **Step 4: Stamp an idle re-encode with a new presentation time**

Keep the copied pixels but set `feed_frame.mono_us = NowMonoUs()` before `ProcessFrame`. M1 will split source and presentation time explicitly.

- [x] **Step 5: Run the native selftest**

Run: `cmd /c native\desktop\build.bat`

Run: `bin\xnc-desktop.exe --selftest`

Expected: A/B/C snapshot returns C and all existing FrameCache/reset tests pass.

- [x] **Step 6: Commit**

```bash
git add native/desktop/pipeline.h native/desktop/pipeline.cpp native/desktop/desktop_selftest.cpp
git commit -m "fix(desktop): re-encode the latest captured frame"
```

### Task 3: Pair delayed MFT outputs with their actual submissions

**Files:**
- Modify: `native/desktop/pipeline.h`
- Modify: `native/desktop/pipeline.cpp`
- Test: `native/desktop/desktop_selftest.cpp`

**Interfaces:**
- Produces: `SubmissionLedger::Submit(uint64_t)`, `Take(uint64_t*)`, `Pending()`, `Clear()`.
- Consumes: one ledger item per successful encoder input and per output AU.

- [x] **Step 1: Add a failing delayed-output ledger test**

```cpp
xnc::SubmissionLedger ledger;
for (uint64_t t : {100, 200, 300}) ledger.Submit(t);
uint64_t out = 0;
CHECK("ledger-first", ledger.Take(&out) && out == 100);
CHECK("ledger-second", ledger.Take(&out) && out == 200);
CHECK("ledger-third", ledger.Take(&out) && out == 300);
CHECK("ledger-empty", !ledger.Take(&out));
```

- [x] **Step 2: Build and observe the missing type**

Run: `cmd /c native\desktop\build.bat`

Expected: compile failure naming `SubmissionLedger`.

- [x] **Step 3: Add the bounded FIFO ledger**

```cpp
class SubmissionLedger {
 public:
  bool Submit(uint64_t mono_us);       // false above 64 pending inputs
  bool Take(uint64_t* mono_us);        // FIFO
  size_t Pending() const;
  void Clear();
 private:
  std::deque<uint64_t> pending_;
};
```

Register the current input immediately before calling `Encode/EncodeNV12`; remove it on input failure. For every AU returned by `Encode`, `Drain`, or `FlushTail`, call `Take` and pass that timestamp to `OnAu`. A missing ledger entry is `encoder_identity_mismatch` and aborts the stream.

- [x] **Step 4: Remove `PipelineShared::last_mono_us` tail stamping**

`FlushTail` must consume one ledger entry per AU. It must never stamp all tail AUs with the last input timestamp.

- [x] **Step 5: Run native tests**

Run: `cmd /c native\desktop\build.bat`

Run: `bin\xnc-desktop.exe --selftest`

Expected: delayed and tail outputs retain 100/200/300 order; exit 0.

- [x] **Step 6: Commit**

```bash
git add native/desktop/pipeline.h native/desktop/pipeline.cpp native/desktop/desktop_selftest.cpp
git commit -m "fix(desktop): preserve MFT submission timestamps"
```

### Task 4: Replace implicit sample duration with explicit RTP timestamps

**Files:**
- Create: `agent/desktop/rtp_clock.go`
- Create: `agent/desktop/rtp_clock_test.go`
- Modify: `agent/desktop/transport.go`
- Modify: `agent/desktop/session_loopback_test.go`

**Interfaces:**
- Produces: `newRTPClock(base uint32) *rtpClock`, `Timestamp(presentMonoUs uint64) (uint32, error)`.
- Changes: `Publisher.track` to `*webrtc.TrackLocalStaticRTP`.

- [x] **Step 1: Write failing timestamp tests**

```go
func TestRTPClockPlacesGapOnCurrentFrame(t *testing.T) {
    c := newRTPClock(1000)
    a, _ := c.Timestamp(1_000_000)
    b, _ := c.Timestamp(31_000_000)
    if b-a != 30*90000 { t.Fatalf("gap=%d", b-a) }
}

func TestRTPClockRejectsRegression(t *testing.T) {
    c := newRTPClock(0)
    _, _ = c.Timestamp(2)
    if _, err := c.Timestamp(1); err == nil { t.Fatal("accepted regression") }
}
```

- [x] **Step 2: Run and confirm failure**

Run: `cd agent; go test ./desktop -run RTPClock -count=1`

Expected: build failure because `newRTPClock` is missing.

- [x] **Step 3: Implement integer 90 kHz mapping**

```go
type rtpClock struct {
    base uint32
    first uint64
    last uint64
    started bool
}

func (c *rtpClock) Timestamp(us uint64) (uint32, error) {
    if !c.started { c.first, c.last, c.started = us, us, true; return c.base, nil }
    if us <= c.last { return 0, errRTPTimeRegression }
    c.last = us
    ticks := ((us-c.first)*9 + 50) / 100
    return c.base + uint32(ticks), nil
}
```

- [x] **Step 4: Packetize Annex-B directly**

Use `TrackLocalStaticRTP`, Pion's H.264 payloader, a random sequencer, MTU 1200, and overwrite every packet's timestamp with `rtpClock.Timestamp(f.MonoUs)`. Set marker only on the final packet. Remove `media.Sample`, `frameDuration`, and `DefaultDuration` from the hot path.

- [x] **Step 5: Add a loopback assertion for a 30-second idle gap**

Capture incoming RTP timestamps in `session_loopback_test.go`; send frames at 1s and 31s and assert the second timestamp delta is 2,700,000 ticks.

- [x] **Step 6: Run Go tests**

Run: `cd agent; go test ./desktop ./desktoppipe -count=1`

Expected: PASS.

- [x] **Step 7: Commit**

```bash
git add agent/desktop/rtp_clock.go agent/desktop/rtp_clock_test.go agent/desktop/transport.go agent/desktop/session_loopback_test.go
git commit -m "fix(desktop): map presentation time directly to RTP"
```

### Task 5: Add the decoded A/B/C end-to-end regression

**Files:**
- Modify: `native/desktop/desktop_selftest.cpp`
- Create: `native/desktop/mf_decoder_probe.h`
- Create: `native/desktop/mf_decoder_probe.cpp`
- Modify: `native/desktop/build.bat`

**Interfaces:**
- Produces: `DecodeAnnexBToLumaHash(const std::vector<uint8_t>&, uint64_t*, std::string*)` for test-only pixel verification.

- [x] **Step 1: Add a failing native idle-recovery scenario**

Feed three solid-color frames A/B/C, then timeouts, then request `sub_join` and `pli`. Assert the AU selected for recovery carries C's `mono_us`, not A's.

- [x] **Step 2: Add a test-only Media Foundation decoder probe**

Feed SPS/PPS+IDR into the Windows H.264 decoder MFT, convert the decoded NV12 Y plane into a deterministic FNV-1a hash, and return the hash to selftest. Decode the post-idle recovery IDR and assert it matches C's expected luma hash.

- [x] **Step 3: Run tests and observe the regression before fixes are applied**

Run: `cmd /c native\desktop\build.bat`

Run: `bin\xnc-desktop.exe --selftest`

Expected before Tasks 1–4: at least one C/latest assertion fails. Expected after Tasks 1–4: all pass.

- [x] **Step 4: Commit**

```bash
git add native/desktop/desktop_selftest.cpp native/desktop/mf_decoder_probe.h native/desktop/mf_decoder_probe.cpp native/desktop/build.bat
git commit -m "test(desktop): detect stale pixels after idle recovery"
```

### Task 6: M0 full verification

**Files:**
- Modify: `docs/superpowers/plans/2026-08-26-desktop-media-m0-correctness.md` only to check completed boxes during execution.

- [x] **Step 1: Run native build and selftest**

Run: `cmd /c native\desktop\build.bat`

Run: `bin\xnc-desktop.exe --selftest`

Expected: exit 0.

- [x] **Step 2: Run Agent tests**

Run: `cd agent; go test ./desktop ./desktoppipe -count=1`

Expected: PASS.

- [x] **Step 3: Run e2eviewer tests**

Run: `cd tools\e2eviewer; go test ./... -count=1`

Expected: PASS.

- [x] **Step 4: Inspect the diff**

Run: `git diff --check`

Expected: no whitespace errors and no unrelated files changed.
