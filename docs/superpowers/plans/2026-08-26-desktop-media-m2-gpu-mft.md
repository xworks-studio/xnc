# Desktop Media M2 GPU/MFT Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace CPU readback on the normal path with an owned LatestSurface, D3D11 VideoProcessor conversion, and D3D11-aware hardware MFT input.

**Architecture:** One media GPU thread owns the immediate context. It copies the acquired desktop into a persistent BGRA texture, converts only the latest content into one of three leased NV12 textures, and submits those textures to a probed MFT encoder.

**Tech Stack:** C++17, D3D11, DXGI Desktop Duplication, Media Foundation, Windows H.264 MFT.

**Spec:** `docs/superpowers/specs/2026-08-26-desktop-media-pipeline-design.md`

## Global Constraints

- No GPU→CPU readback on the healthy DXGI/hardware-MFT path.
- Only `MediaGpuThread` may call the D3D11 immediate context.
- Three NV12 slots maximum; a submitted slot cannot be reused before its output or retirement.
- Software MFT and GDI remain working fallbacks.
- M0/M1 tests remain green.

---

### Task 1: Introduce LatestSurface and lease state machines

**Files:**
- Create: `native/desktop/gpu_surface.h`
- Create: `native/desktop/gpu_surface.cpp`
- Modify: `native/desktop/build.bat`
- Test: `native/desktop/desktop_selftest.cpp`

**Interfaces:**
- Produces: `LatestSurface::CopyFrom`, `Nv12SurfacePool::Acquire`, `SurfaceLease::Submit`, `SurfaceLease::Release`, `RetireAll`.

- [x] **Step 1: Add pure lease transition tests**

```cpp
xnc::SurfaceLeaseModel m(3);
auto a = m.Acquire(); auto b = m.Acquire(); auto c = m.Acquire();
CHECK("pool-bounded", a && b && c && !m.Acquire());
a->Submit(10);
CHECK("submitted-not-reusable", !m.ReleaseFree(a->index()));
CHECK("output-releases", m.Complete(10) && m.Acquire());
```

- [x] **Step 2: Build and confirm missing types**

Run: `cmd /c native\desktop\build.bat`

- [x] **Step 3: Implement GPU wrappers**

```cpp
class LatestSurface {
 public:
  bool Init(ID3D11Device*, uint32_t w, uint32_t h, std::string*);
  bool CopyFrom(ID3D11DeviceContext*, ID3D11Texture2D*, const FrameIdentity&, std::string*);
  bool Snapshot(FrameIdentity*, ID3D11Texture2D**);
  void Invalidate();
};
```

`Snapshot` returns an AddRef'd texture and identity; it fails after reset until a new copy completes. Implement the three-slot pool with explicit FREE/CONVERTING/SUBMITTED/RETIRED transitions.

- [x] **Step 4: Enable D3D11 debug-layer test mode**

Add `XNC_D3D_DEBUG=1`; selftest fails on D3D severity ERROR/CORRUPTION and asserts zero live pool leases after teardown.

- [x] **Step 5: Run selftest and commit**

Run: `cmd /c native\desktop\build.bat && bin\xnc-desktop.exe --selftest`

```bash
git add native/desktop/gpu_surface.h native/desktop/gpu_surface.cpp native/desktop/build.bat native/desktop/desktop_selftest.cpp
git commit -m "feat(desktop): add owned GPU surface leases"
```

### Task 2: Make DXGI publish an owned GPU surface

**Files:**
- Modify: `native/desktop/capture.h`
- Modify: `native/desktop/dxgi_capture.h`
- Modify: `native/desktop/dxgi_capture.cpp`
- Modify: `native/desktop/gdi_capture.cpp`
- Test: `native/desktop/desktop_selftest.cpp`

**Interfaces:**
- Produces: `CapturedSurface { FrameIdentity id; ID3D11Texture2D* texture; bool changed; }`.
- Preserves: CPU `FrameBlob` path only for software/GDI fallback.

- [x] **Step 1: Add a fake captured-surface ownership test**

Assert `ReleaseFrame` occurs immediately after `CopyResource`, before any encoder callback, and cursor-only `LastPresentTime==0` does not increment contentId.

- [x] **Step 2: Add `ICaptureSurface`**

```cpp
class ICaptureSurface {
 public:
  virtual CaptureStatus AcquireSurface(LatestSurface&, uint32_t timeout_ms,
                                       FrameIdentity*, std::string*) = 0;
};
```

The DXGI implementation performs full-resource GPU copy. Do not reconstruct from dirty/move rectangles in M2.

- [x] **Step 3: Adapt GDI fallback**

Keep BitBlt/DIB acquisition, then upload the compact BGRA buffer to LatestSurface with `UpdateSubresource` on the media GPU thread.

- [x] **Step 4: Run tests and commit**

Run: `cmd /c native\desktop\build.bat && bin\xnc-desktop.exe --selftest`

```bash
git add native/desktop/capture.h native/desktop/dxgi_capture.h native/desktop/dxgi_capture.cpp native/desktop/gdi_capture.cpp native/desktop/desktop_selftest.cpp
git commit -m "feat(desktop): retain the latest desktop on GPU"
```

### Task 3: Add a D3D11-aware MFT encoder session

**Files:**
- Create: `native/desktop/mf_gpu_encoder.h`
- Create: `native/desktop/mf_gpu_encoder.cpp`
- Modify: `native/desktop/mf_encoder.h`
- Modify: `native/desktop/mf_encoder.cpp`
- Modify: `native/desktop/build.bat`
- Test: `native/desktop/desktop_selftest.cpp`

**Interfaces:**
- Produces: `IEncoderSession`, `MfGpuEncoder`, `MfCpuEncoder`, `EncoderOutput`.

- [x] **Step 1: Define the interface and a failing fake-session test**

```cpp
class IEncoderSession {
 public:
  virtual SubmitResult Submit(const FrameIdentity&, SurfaceLease&&, bool force_idr) = 0;
  virtual bool TakeOutput(EncoderOutput*, uint32_t timeout_ms) = 0;
  virtual bool Reconfigure(uint32_t bitrate, uint32_t fps) = 0;
  virtual void Shutdown(ShutdownMode) = 0;
};
```

The fake delays outputs by 17 submissions; assert every returned identity and lease matches its input.

- [x] **Step 2: Configure D3D11-aware hardware MFT**

Create `IMFDXGIDeviceManager`, call `ResetDevice`, set `MF_SA_D3D11_AWARE`, use DXGI-surface-backed `IMFMediaBuffer`, and set low-latency/B-frame/rate-control properties before streaming messages.

Configure VideoProcessor and media types as BT.709 limited range at 720p and above, otherwise BT.601 limited range; add a selftest that the selected matrix and SPS/VUI metadata agree.

- [x] **Step 3: Track output sample time and release the exact lease**

Use strictly increasing 100ns sample times keyed to `encodeSeq`. `TakeOutput` must fail with `encoder_identity_mismatch` when output time is missing, duplicated or unknown.

- [x] **Step 4: Add the eight-frame pixel probe**

Require first output within two inputs or 100ms, decode IDR through a decoder MFT, and compare keyed luma signatures. Re-activate a pristine MFT after probing.

- [x] **Step 5: Run tests and commit**

Run: `cmd /c native\desktop\build.bat && bin\xnc-desktop.exe --selftest`

```bash
git add native/desktop/mf_gpu_encoder.h native/desktop/mf_gpu_encoder.cpp native/desktop/mf_encoder.h native/desktop/mf_encoder.cpp native/desktop/build.bat native/desktop/desktop_selftest.cpp
git commit -m "feat(desktop): submit D3D11 textures to hardware MFT"
```

### Task 4: Build MediaPipelineV2 around a depth-one mailbox

**Files:**
- Create: `native/desktop/media_pipeline_v2.h`
- Create: `native/desktop/media_pipeline_v2.cpp`
- Modify: `native/desktop/xnc-desktop.cpp`
- Modify: `native/desktop/rt_pipe_server.cpp`
- Modify: `native/desktop/build.bat`
- Test: `native/desktop/desktop_selftest.cpp`

**Interfaces:**
- Produces: `MediaPipelineV2::Start`, `RequestIdr`, `Reconfigure`, `Reset`, `Stop`.

- [x] **Step 1: Add a coalescing-mailbox test**

Publish contentIds 1,2,3 while all slots are busy; release one slot and assert only contentId 3 is submitted. Arm IDR during the wait and assert it applies once to contentId 3.

- [x] **Step 2: Implement the single GPU loop**

The loop owns capture, LatestSurface copy, FPS gate, VideoProcessorBlt and encoder submission. Its command mailbox contains only latest-content, sticky-IDR, reconfigure and reset flags; no frame FIFO.

- [x] **Step 3: Route `desktop_pipeline_v2`**

Add a process option/config flag. True selects MediaPipelineV2; false retains M0 pipeline for rollback. Both publish the same Pipe v2 `EncodedAU`.

- [x] **Step 4: Run both modes and commit**

Run: `bin\xnc-desktop.exe --selftest`

Run: `bin\xnc-desktop.exe --selftest --desktop-pipeline-v2`

```bash
git add native/desktop/media_pipeline_v2.h native/desktop/media_pipeline_v2.cpp native/desktop/xnc-desktop.cpp native/desktop/rt_pipe_server.cpp native/desktop/build.bat native/desktop/desktop_selftest.cpp
git commit -m "feat(desktop): add depth-one GPU media pipeline"
```

### Task 5: Unify reset and backend fallback

**Files:**
- Modify: `native/desktop/capture_reset.h`
- Modify: `native/desktop/capture_reset.cpp`
- Modify: `native/desktop/backend_ladder.cpp`
- Modify: `native/desktop/media_pipeline_v2.cpp`
- Test: `native/desktop/desktop_selftest.cpp`

- [x] **Step 1: Add reset-sequence assertions**

Assert `discontinuity → stop submissions → retire leases → rebuild → base → config → IDR → running`, one epoch increment per execution, and exponential backoff for repeated identical reasons.

- [x] **Step 2: Implement serialized reset commands**

Coalesce pending reasons by severity: device-removed > desktop/display change > access-lost > encoder. Reject outputs from retired epochs.

- [x] **Step 3: Implement process-lifetime fallback locks**

After three hardware encoder contract failures, lock to software until process restart. DXGI failure falls to GDI; probe DXGI every 30 seconds and return through the same reset sequence.

- [x] **Step 4: Run full native suite and commit**

Run: `cmd /c native\desktop\build.bat && bin\xnc-desktop.exe --selftest --desktop-pipeline-v2`

```bash
git add native/desktop/capture_reset.h native/desktop/capture_reset.cpp native/desktop/backend_ladder.cpp native/desktop/media_pipeline_v2.cpp native/desktop/desktop_selftest.cpp
git commit -m "feat(desktop): unify GPU pipeline recovery"
```

### Task 6: Measure the GPU path

**Files:**
- Modify: `native/desktop/media_pipeline_v2.cpp`
- Modify: `native/desktop/pipeline.h`
- Modify: `native/desktop/pipeline.cpp`

- [x] **Step 1: Record stage histograms**

Emit p50/p95/p99 for `gpu_copy_us`, `gpu_convert_us`, `mft_submit_to_output_us`, in-flight slots and queue age once per 10 seconds.

- [x] **Step 2: Run a 1080p60 diagnostic capture**

Run: `bin\xnc-desktop.exe --console-diag --desktop-pipeline-v2 --fps 60 --duration 60 --out artifacts\m2.h264`

Expected: GPU copy+convert p95 <3ms, capture→AU p95 <15ms, no CPU readback counter increments.

- [x] **Step 3: Verify the stream**

Run: `cd tools\nalcheck; go run . -- ..\..\artifacts\m2.h264`

Expected: valid Annex-B, IDR contains SPS/PPS, no decoder errors.

- [x] **Step 4: Commit**

```bash
git add native/desktop/media_pipeline_v2.cpp native/desktop/pipeline.h native/desktop/pipeline.cpp
git commit -m "perf(desktop): instrument GPU media latency"
```
