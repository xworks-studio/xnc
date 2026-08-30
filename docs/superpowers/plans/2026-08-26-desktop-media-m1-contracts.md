# Desktop Media M1 Contracts Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Introduce immutable frame identity, Pipe v2 validation, and explicit discontinuity semantics across C++ Host and Go Agent.

**Architecture:** Define one `EncodedAU` schema at the encoder boundary, serialize it with a versioned fixed header and CRC32C, and make every consumer enforce epoch monotonicity and WAIT_IDR recovery.

**Tech Stack:** C++17, Go, named pipes, Annex-B H.264, CRC32C Castagnoli.

**Spec:** `docs/superpowers/specs/2026-08-26-desktop-media-pipeline-design.md`

## Global Constraints

- M0 tests must remain green.
- Pipe v2 AU maximum is 8 MiB.
- Host and Agent upgrade together; v1 remains only behind `desktop_pipeline_v2=false` until M4.
- IDs are unsigned 64-bit and monotonic inside their declared epoch.

---

### Task 1: Define immutable Native media types

**Files:**
- Create: `native/desktop/media_types.h`
- Modify: `native/desktop/pipeline.h`
- Modify: `native/desktop/pipeline.cpp`
- Test: `native/desktop/desktop_selftest.cpp`

**Interfaces:**
- Produces: `FrameIdentity`, `EncodedAU`, `AuFlags`, `FrameIdentityLedger`.

- [x] **Step 1: Add compile-time and monotonicity tests**

```cpp
xnc::FrameIdentity id{1, 2, 3, 4, 500, 600};
CHECK("identity-fields", id.capture_epoch == 1 && id.encode_seq == 4);
xnc::FrameIdentityLedger l;
CHECK("identity-accept", l.Accept(id));
CHECK("identity-reject-repeat", !l.Accept(id));
```

- [x] **Step 2: Build and confirm types are missing**

Run: `cmd /c native\desktop\build.bat`

- [x] **Step 3: Add exact types**

```cpp
struct FrameIdentity {
  uint64_t capture_epoch, codec_epoch, content_id, encode_seq;
  uint64_t source_mono_us, present_mono_us;
};
struct EncodedAU {
  FrameIdentity id;
  uint32_t width, height;
  uint32_t flags;
  std::shared_ptr<const std::vector<uint8_t>> annexb;
};
```

Change `AuSink::OnAu` to `virtual const char* OnAu(const EncodedAU&) = 0`. Update File/Tee/RtServer sinks and make pipeline assign IDs at capture and successful encoder submission.

- [x] **Step 4: Run native selftest**

Run: `cmd /c native\desktop\build.bat && bin\xnc-desktop.exe --selftest`

- [x] **Step 5: Commit**

```bash
git add native/desktop/media_types.h native/desktop/pipeline.h native/desktop/pipeline.cpp native/desktop/desktop_selftest.cpp
git commit -m "feat(desktop): add immutable encoded frame identity"
```

### Task 2: Implement Pipe v2 codec with CRC32C

**Files:**
- Create: `native/common/crc32c.h`
- Modify: `native/desktop/rt_pipe_server.h`
- Modify: `native/desktop/rt_pipe_server.cpp`
- Test: `native/desktop/desktop_selftest.cpp`

**Interfaces:**
- Produces: `EncodeFrameEventV2(const EncodedAU&)`, `DecodeFrameEventV2`.
- Wire: little-endian fixed header followed by Annex-B payload and CRC32C over header-with-zero-crc plus payload.

- [x] **Step 1: Add golden-vector tests**

Construct an AU with IDs 1–6, 1920x1080, key flag and `{0,0,0,1,0x65}` payload. Assert exact offsets, round-trip equality, CRC rejection after one flipped byte, truncated-header rejection and 8 MiB+1 rejection.

- [x] **Step 2: Build and confirm v2 functions are missing**

Run: `cmd /c native\desktop\build.bat`

- [x] **Step 3: Implement Castagnoli CRC and frame codec**

Use message type `0x0205`. Include `header_bytes`, all six identity fields, dimensions, flags, payload length and CRC. Decode with checked `size_t` addition before allocation.

- [x] **Step 4: Add protocol capability to HOST_HELLO**

Append `u32 media_protocol=2` to the extended hello. When `desktop_pipeline_v2=false`, emit the existing hello and `0x0105` frames.

- [x] **Step 5: Run selftest and commit**

Run: `cmd /c native\desktop\build.bat && bin\xnc-desktop.exe --selftest`

```bash
git add native/common/crc32c.h native/desktop/rt_pipe_server.h native/desktop/rt_pipe_server.cpp native/desktop/desktop_selftest.cpp
git commit -m "feat(desktop): add validated Pipe v2 media frames"
```

### Task 3: Decode and validate Pipe v2 in Go

**Files:**
- Modify: `agent/desktoppipe/client.go`
- Modify: `agent/desktoppipe/client_test.go`
- Modify: `agent/desktop/source.go`
- Modify: `agent/desktop/core_windows.go`

**Interfaces:**
- Produces Go `Frame` fields matching Native `FrameIdentity` exactly.

- [x] **Step 1: Add the Native golden vector to Go tests**

Decode the byte vector from Task 2 and assert every field. Add one-bit CRC corruption, length overflow, unknown version and epoch regression cases.

- [x] **Step 2: Run failing tests**

Run: `cd agent; go test ./desktoppipe -run FrameV2 -count=1`

- [x] **Step 3: Extend the Go frame type**

```go
type Frame struct {
    Key bool
    CaptureEpoch, CodecEpoch uint64
    ContentID, EncodeSeq uint64
    SourceMonoUs, PresentMonoUs uint64
    W, H uint32
    AU []byte
}
```

Use `hash/crc32` with `crc32.MakeTable(crc32.Castagnoli)`. Reject non-monotonic identities before delivering to `FrameCh`.

- [x] **Step 4: Run tests and commit**

Run: `cd agent; go test ./desktoppipe ./desktop -count=1`

```bash
git add agent/desktoppipe/client.go agent/desktoppipe/client_test.go agent/desktop/source.go agent/desktop/core_windows.go
git commit -m "feat(agent): validate Pipe v2 media identity"
```

### Task 4: Enforce discontinuity and WAIT_IDR on both pipe sides

**Files:**
- Modify: `native/desktop/rt_pipe_server.h`
- Modify: `native/desktop/rt_pipe_server.cpp`
- Modify: `agent/desktoppipe/client.go`
- Test: `native/desktop/desktop_selftest.cpp`
- Test: `agent/desktoppipe/client_test.go`

**Interfaces:**
- Produces: `MSG_STREAM_DISCONTINUITY=0x020B` carrying capture epoch, codec epoch and fixed reason.

- [x] **Step 1: Add queue-overflow tests**

Assert a subscriber whose delta queue overflows transitions to WAIT_IDR, receives no later delta, and returns LIVE only after an IDR of the expected epoch.

- [x] **Step 2: Implement Host `SubConn::video_state`**

Use `enum class VideoState { kWaitIdr, kLive, kPaused }`. Flush queued frames and set `kWaitIdr` on join, overflow, discontinuity or epoch change. Never block `OnAu` for a key frame.

- [x] **Step 3: Mirror the same state in Go `Sub.pump`**

Replace the current behavior that blocks while delivering key frames. FrameCh overflow clears buffered frames, marks `needKey`, requests one merged keyframe, and suppresses deltas until a v2 IDR arrives.

- [x] **Step 4: Run cross tests and commit**

Run: `cmd /c native\desktop\build.bat && bin\xnc-desktop.exe --selftest`

Run: `cd agent; go test ./desktoppipe -count=1`

```bash
git add native/desktop/rt_pipe_server.h native/desktop/rt_pipe_server.cpp native/desktop/desktop_selftest.cpp agent/desktoppipe/client.go agent/desktoppipe/client_test.go
git commit -m "fix(desktop): recover pipe consumers only from IDR"
```

### Task 5: M1 cross-language and fuzz verification

**Files:**
- Create: `agent/desktoppipe/frame_v2_fuzz_test.go`
- Modify: `native/desktop/desktop_selftest.cpp`

- [x] **Step 1: Add Go fuzz targets**

```go
func FuzzDecodeFrameV2(f *testing.F) {
    f.Add(nativeGoldenFrameV2())
    f.Fuzz(func(t *testing.T, b []byte) { _, _ = decodeFrameV2(b) })
}
```

- [x] **Step 2: Run bounded fuzzing**

Run: `cd agent; go test ./desktoppipe -run '^$' -fuzz FuzzDecodeFrameV2 -fuzztime 30s`

Expected: no panic, excessive allocation or hang.

- [x] **Step 3: Run full M0/M1 suites**

Run: `cmd /c native\desktop\build.bat && bin\xnc-desktop.exe --selftest`

Run: `cd agent; go test ./desktop ./desktoppipe -count=1`

- [x] **Step 4: Commit**

```bash
git add agent/desktoppipe/frame_v2_fuzz_test.go native/desktop/desktop_selftest.cpp
git commit -m "test(desktop): fuzz Pipe v2 frame decoding"
```
