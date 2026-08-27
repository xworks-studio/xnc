// dxgi_capture.h - DXGI Desktop Duplication backend for xnc-desktop
// (Task 3, spec §7.4, CPU readback path). Chain: D3D11CreateDevice →
// IDXGIOutput5::DuplicateOutput1(B8G8R8A8) with fallback to
// IDXGIOutput1::DuplicateOutput → AcquireNextFrame → CopyResource to a
// persistent staging texture → Map → compact rows into FrameBlob.
//
// The pure helpers (BgraBytes / CompactBgraRows / Fnv1a64 /
// SamplePointsNotUniform) are header-only on purpose: desktop_selftest.cpp
// covers them without a desktop, GPU, or d3d11.lib. The COM plumbing lives in
// dxgi_capture.cpp (pimpl, so this header needs no D3D includes).
#ifndef XNC_NATIVE_DESKTOP_DXGI_CAPTURE_H_
#define XNC_NATIVE_DESKTOP_DXGI_CAPTURE_H_

#include <cstddef>
#include <cstdint>
#include <cstring>
#include <string>

#include "capture.h"  // ICapture, ICaptureSurface, FrameBlob, TryCreateDxgiCapture
#include "gpu_surface.h"  // LatestSurface (AcquireSurface destination)

namespace xnc {

// The CPU must map the same staging texture that the current acquire copied.
inline constexpr uint32_t StagingReadIndex(uint32_t write_index) {
  return write_index;
}

// Byte size of a tightly packed w*h BGRA buffer (== FrameBlob::bgra.size()).
// Returns 0 for absurd dimensions instead of overflowing/wrapping.
inline size_t BgraBytes(uint32_t w, uint32_t h) {
  const uint64_t bytes = static_cast<uint64_t>(w) * static_cast<uint64_t>(h) * 4ull;
  // 8 GiB cap: any real desktop frame is far below this; anything larger is
  // garbage input (or uint32 overflow) and must not reach a resize().
  if (bytes > (1ull << 33)) return 0;
  return static_cast<size_t>(bytes);
}

// Copies h rows of w*4 bytes from a pitched Map() source (RowPitch >= w*4)
// into tightly packed dst. This is where row pitch is dropped - FrameBlob is
// always compact.
inline void CompactBgraRows(const uint8_t* src, size_t src_pitch, uint8_t* dst,
                            uint32_t w, uint32_t h) {
  const size_t row_bytes = static_cast<size_t>(w) * 4;
  for (uint32_t r = 0; r < h; ++r) {
    std::memcpy(dst + static_cast<size_t>(r) * row_bytes,
                src + static_cast<size_t>(r) * src_pitch, row_bytes);
  }
}

// Copies a pitched NV12 staging Map() into tightly packed dst
// (w*h + w*h/2 bytes). D3D11 quirk (verified 2026-08-25 on Intel): an NV12
// staging texture is ONE subresource - Map(0) returns the Y plane rows then
// the interleaved UV plane rows packed contiguously with the SAME RowPitch
// (uv rows start at y_pitch*h; Map(subresource 1) fails E_INVALIDARG).
// w/h must be even; mirrors CompactBgraRows' role.
inline void CompactNv12Rows(const uint8_t* src, size_t pitch, uint8_t* dst,
                            uint32_t w, uint32_t h) {
  const size_t row = static_cast<size_t>(w);
  for (uint32_t r = 0; r < h; ++r)
    std::memcpy(dst + static_cast<size_t>(r) * row,
                src + static_cast<size_t>(r) * pitch, row);
  const uint8_t* uv = src + pitch * h;
  uint8_t* uv_dst = dst + static_cast<size_t>(w) * h;
  for (uint32_t r = 0; r < h / 2; ++r)
    std::memcpy(uv_dst + static_cast<size_t>(r) * row,
                uv + static_cast<size_t>(r) * pitch, row);
}

// NV12 Y-plane analogue of SamplePointsNotUniform: samples 256 evenly spread
// Y values; true when they are not all identical (the "not black / not
// garbage-uniform" first-frame diag for the GPU NV12 path).
inline bool Nv12YNotUniform(const uint8_t* nv12, uint32_t w, uint32_t h) {
  if (!nv12 || w == 0 || h == 0) return false;
  for (uint32_t i = 1; i < 256; ++i) {
    const uint32_t x = static_cast<uint32_t>((static_cast<uint64_t>(i) * w) / 256);
    const uint32_t y = static_cast<uint32_t>((static_cast<uint64_t>(i) * h) / 256);
    if (nv12[static_cast<size_t>(y) * w + x] != nv12[0]) return true;
  }
  return false;
}

// FNV-1a 64 over n bytes (diag: hash of the first 64 frame bytes).
// Known vectors: "" -> 0xcbf29ce484222325, "a" -> 0xaf63dc4c8601ec8c,
// "foobar" -> 0x85944171f73967e8.
inline uint64_t Fnv1a64(const uint8_t* data, size_t n) {
  uint64_t h = 14695981039346656037ull;
  for (size_t i = 0; i < n; ++i) {
    h ^= data[i];
    h *= 1099511628211ull;
  }
  return h;
}

// Samples 256 evenly spread pixels; true when they are not all identical.
// The cheap "not all black / not garbage-uniform" check for diag (plan:
// 采样 256 点不全等). Byte-compares, no alignment assumptions.
inline bool SamplePointsNotUniform(const uint8_t* bgra, uint32_t w, uint32_t h) {
  if (!bgra || w == 0 || h == 0) return false;
  for (uint32_t i = 1; i < 256; ++i) {  // i=0 is the reference pixel (0,0)
    const uint32_t x = static_cast<uint32_t>((static_cast<uint64_t>(i) * w) / 256);
    const uint32_t y = static_cast<uint32_t>((static_cast<uint64_t>(i) * h) / 256);
    const uint8_t* p = bgra + (static_cast<size_t>(y) * w + x) * 4;
    if (p[0] != bgra[0] || p[1] != bgra[1] || p[2] != bgra[2] || p[3] != bgra[3])
      return true;
  }
  return false;
}

// True when a TryCreateDxgiCapture/Acquire error string indicates the
// process cannot access the interactive desktop (expected when run as
// SYSTEM in session 0 - the Task 6 session bridge fixes this). Matches
// E_ACCESSDENIED / DXGI_ERROR_UNSUPPORTED / DXGI_ERROR_NOT_CURRENTLY_AVAILABLE /
// DXGI_ERROR_SESSION_DISCONNECTED carried in the "<step>: hr=0x%08X" format.
bool DxgiErrIsDesktopAccessDenied(const std::string& err);

// ---- M2-Slice3 Task 5: multi-display enumeration ----
//
// One attached desktop output. idx is the STABLE table index (GDI monitor
// order - see BuildDisplayTable); origin is the desktop-coordinate position
// of the output's top-left corner; primary mirrors MONITORINFOF_PRIMARY.
// monitor_id is the internal dedupe/order key (the HMONITOR value; 0 when
// unknown - such entries append at the table tail).
struct DisplayInfo {
  uint32_t idx = 0;
  int32_t origin_x = 0, origin_y = 0;
  uint32_t w = 0, h = 0;
  uint8_t primary = 0;
  uint64_t monitor_id = 0;
};

// One raw enumerated output (adapter-walk order), the injectable seam the
// selftest feeds with fake outputs. attached mirrors DXGI
// OUTPUT_DESC.AttachedToDesktop; x/y/w/h the DesktopCoordinates rect;
// primary from GetMonitorInfo; monitor_id the HMONITOR (0 = unknown).
struct RawDisplayOutput {
  uint64_t monitor_id = 0;
  bool attached = true;
  int32_t x = 0, y = 0;
  uint32_t w = 0, h = 0;
  bool primary = false;
};

// Builds the stable displays table from the adapter-walk outputs:
//   1. dedupe by monitor_id (keep the first occurrence; id 0 never dedupes);
//   2. index in GDI-order first (the ids in gdi_order, in that order -
//      EnumDisplayMonitors callback order aligns with EnumDisplayDevices,
//      M0 repo knowledge), then any outputs GDI did not list (adapter-walk
//      order, including monitor_id == 0) appended at the tail.
// Output DisplayInfo.idx == position in the returned vector.
inline std::vector<DisplayInfo> BuildDisplayTable(
    const std::vector<RawDisplayOutput>& adapter_order,
    const std::vector<uint64_t>& gdi_order) {
  std::vector<DisplayInfo> deduped;
  for (const auto& r : adapter_order) {
    if (!r.attached) continue;
    if (r.monitor_id != 0) {
      bool dup = false;
      for (const auto& d : deduped)
        if (d.monitor_id == r.monitor_id) { dup = true; break; }
      if (dup) continue;
    }
    DisplayInfo d;
    d.origin_x = r.x; d.origin_y = r.y; d.w = r.w; d.h = r.h;
    d.primary = r.primary ? 1 : 0;
    d.monitor_id = r.monitor_id;
    deduped.push_back(d);
  }
  std::vector<DisplayInfo> out;
  std::vector<bool> used(deduped.size(), false);
  for (const uint64_t id : gdi_order) {
    if (id == 0) continue;
    for (size_t i = 0; i < deduped.size(); ++i) {
      if (!used[i] && deduped[i].monitor_id == id) {
        used[i] = true;
        out.push_back(deduped[i]);
        break;
      }
    }
  }
  for (size_t i = 0; i < deduped.size(); ++i)
    if (!used[i]) out.push_back(deduped[i]);
  for (size_t i = 0; i < out.size(); ++i) out[i].idx = static_cast<uint32_t>(i);
  return out;
}

// Selection sentinel: "auto" = primary display if one is attached, else
// table entry 0 (the pre-Task-5 behavior picked the first duplicable output;
// primary-first keeps single-display nodes identical).
inline constexpr uint32_t kDisplaySelectAuto = 0xFFFFFFFFu;

// Resolves the index the NEXT Init binds: an explicit in-range selection
// wins; a STALE selection (desired >= table.size() - monitor unplugged /
// table shrank / session rebuild lost outputs) falls back to primary, else
// table entry 0. *stale reports the fallback (Init logs + resets to auto).
inline uint32_t ResolveDisplayIndex(uint32_t desired,
                                    const std::vector<DisplayInfo>& table,
                                    bool* stale = nullptr) {
  if (stale) *stale = false;
  if (table.empty()) return 0;
  if (desired != kDisplaySelectAuto && desired < table.size()) return desired;
  if (stale) *stale = desired != kDisplaySelectAuto;
  for (const auto& d : table)
    if (d.primary) return d.idx;
  return 0;
}

// Snapshot of the process-global displays table (refreshed on every
// DxgiCapture::Init; empty when DXGI enumeration never ran). Thread-safe.
std::vector<DisplayInfo> DxgiDisplaysSnapshot();

// Validates idx against the CURRENT table (refreshing it if empty) and
// records it as the desired selection - consumed by the NEXT Init/rebuild
// (switch = set this + RequestReset("switch")). Returns false for an out-of
// -range index or an empty table.
bool DxgiSelectDisplay(uint32_t idx);

// Monotonic-clock microseconds since boot (QueryPerformanceCounter), the
// FrameBlob::mono_us clock. Shared by the capture backend (stamps frames)
// and the pipeline's encode thread (pipe_latency_ms measurement).
uint64_t NowMonoUs();

// ---- gpu-readback task: GPU downscale + NV12 path ----
//
// DxgiCapture gains a GPU-scale mode: when Init'ed with a max width > 0, the
// acquired desktop texture is NOT read back as full-resolution BGRA. Instead
// a D3D11 VideoProcessor scales it to max_w (aspect preserved) AND converts
// to NV12 in one VideoProcessorBlt (rotation applied in the same pass via
// ID3D11VideoContext1::VideoProcessorSetStreamRotation), then a small NV12
// staging texture is read back (~4x less data than full BGRA). The FrameBlob
// then carries Pixfmt::kNv12 at the SCALED dims - the pipeline feeds the
// encoder's NV12 entry directly (no CPU downscale, no CPU BGRA->NV12).
// max_w == 0 keeps the legacy full-BGRA CPU readback path.

// Rotation of the duplicated output (maps 1:1 to DXGI_OUTDUPL_DESC.Rotation
// and to D3D11_VIDEO_PROCESSOR_ROTATION for the in-pass VP rotation).
enum class Rotate : uint8_t { kNone = 0, k90 = 1, k180 = 2, k270 = 3 };

inline Rotate RotateFromDxgi(int dxgi_rotation) {
  switch (dxgi_rotation) {
    case 2: return Rotate::k90;   // DXGI_MODE_ROTATION_ROTATE90
    case 3: return Rotate::k180;  // DXGI_MODE_ROTATION_ROTATE180
    case 4: return Rotate::k270;  // DXGI_MODE_ROTATION_ROTATE270
    default: return Rotate::kNone;  // IDENTITY(1) / UNSPECIFIED(0)
  }
}

// VideoProcessor output dims for a source w*h under `rot` and a max width:
// 90/270 rotations swap the dims first, then max_w scaling (aspect
// preserved) with a %4 snap (D3D11 requires NV12 texture widths %4; the
// encoder needs even - %4 covers both, ≤3px off the true aspect). Identity
// when w <= max_w (or max_w == 0). Returns false on degenerate input
// (zero dims after snap, or max_w would upscale - the VP path never
// upscales). Pure - the selftest covers it without a GPU.
inline bool GpuScaledDims(uint32_t w, uint32_t h, Rotate rot, uint32_t max_w,
                          uint32_t* ow, uint32_t* oh) {
  if (ow == nullptr || oh == nullptr) return false;
  if (w == 0 || h == 0) return false;
  uint32_t dw = w, dh = h;
  if (rot == Rotate::k90 || rot == Rotate::k270) {
    dw = h;
    dh = w;
  }
  if (max_w > 0 && dw > max_w) {
    uint32_t nh = static_cast<uint32_t>(((static_cast<uint64_t>(dh) * max_w + dw - 1) / dw));
    dw = max_w;
    dh = nh;
  }
  if (dw == 0 || dh == 0) return false;
  dw -= dw % 4;  // NV12 %4 snap (covers the encoder's even requirement)
  dh -= dh % 4;
  if (dw == 0 || dh == 0) return false;
  *ow = dw;
  *oh = dh;
  return true;
}

// DXGI Desktop Duplication with CPU readback. Single-threaded use only.
// M2 Task 2: also implements ICaptureSurface - AcquireSurface publishes the
// newest desktop into a caller-owned LatestSurface with a full-resource GPU
// copy (no readback), the GPU twin of the Acquire CPU path.
class DxgiCapture final : public ICapture, public ICaptureSurface {
 public:
  DxgiCapture();
  ~DxgiCapture() override;
  DxgiCapture(const DxgiCapture&) = delete;
  DxgiCapture& operator=(const DxgiCapture&) = delete;

  // ICapture - see capture.h for the err contract ("err_timeout" /
  // "err_rebuilt" / "err_access_lost" retryables, anything else fatal).
  // Task 3 CPU path: every content frame is a full CopyResource→staging→Map
  // readback; dirty-rect incremental frames are a Task 5/M4 concern and
  // never applied before the base frame exists. timeout_ms == 0 uses the
  // backend default (100 ms); the pipeline passes spf so static screens
  // wake at the target frame cadence.
  bool Acquire(FrameBlob& blob, std::string* err = nullptr,
               uint32_t timeout_ms = 0) override;
  // ICaptureSurface (M2 Task 2): full-resource GPU CopyResource of the
  // acquired desktop texture into `latest` (ruling 2: NO dirty/move-rect
  // reconstruction in M2 - spec §3.1 simplicity trade), then ReleaseFrame
  // IMMEDIATELY, before the method returns / any encoder-visible work
  // (ruling 1c). Cursor-only frames (LastPresentTime == 0) never copy:
  // kNoChange, surface + identity untouched, no content increment. Operates
  // at the duplication's NATIVE dims (w_ x h_, pre-rotation - the later
  // convert task applies rotation) and is orthogonal to the GPU-NV12 blob
  // mode: AcquireSurface always copies the full-resolution BGRA. See
  // capture.h for the identity authority, LatestSurface re-Init rules and
  // the status/err vocabulary.
  CaptureStatus AcquireSurface(LatestSurface& latest, uint32_t timeout_ms,
                               FrameIdentity* id, std::string* err) override;
  // GPU path: the VideoProcessor OUTPUT (scaled/rotated) dims - the stream,
  // encoder Init and HOST_HELLO all live in that (scaled) space. BGRA path:
  // the duplication's native dims.
  uint32_t Width() const override { return gpu_max_w_ > 0 ? out_w_ : w_; }
  uint32_t Height() const override { return gpu_max_w_ > 0 ? out_h_ : h_; }
  uint32_t RebuildCount() const override { return rebuilds_; }

  // Unified CaptureReset entry (M2-Slice1 Task 2): full re-creation
  // (device + output enumeration + duplication + staging). Resets the
  // refused-rebuild streak; on success the next content frame is the new
  // base frame.
  bool Rebuild(std::string* err) override {
    const bool ok = Init(err);
    if (ok) {
      consecutive_rebuild_failures_ = 0;
      have_base_frame_ = false;
    }
    return ok;
  }

  // Full (re)creation: device + output enumeration + duplication + staging.
  // Public for the TryCreateDxgiCapture factory; also the DEVICE_REMOVED
  // rebuild path. On failure *err is "<step>: hr=0x%08X" (or a message).
  bool Init(std::string* err);

  // gpu-readback task: same as Init, but with the GPU-scale+NV12 pipeline
  // enabled (max_w > 0 = scale/convert to NV12 at max_w in the VideoProcessor
  // and read the small NV12 frame back; max_w == 0 behaves exactly like
  // Init). When the device/video processor cannot do the NV12 conversion the
  // backend DEGRADES IN PLACE to the legacy full-BGRA path (logged, never a
  // hard failure) - ScaledCapture then does the CPU downscale as before.
  bool InitGpuScale(uint32_t max_w, std::string* err);

 private:
  // Cheap rebuild: re-DuplicateOutput on the existing device/output and
  // resize staging if the mode changed. Falls back to Init on hard errors.
  bool Reduplicate(std::string* err);
  // ACCESS_LOST / DEVICE_REMOVED handler: one in-place rebuild attempt,
  // counted. Success -> "err_rebuilt"; a refusal (secure desktop up, T1
  // evidence) -> "err_access_lost" for the unified reset - never fatal.
  // (hr param is HRESULT, kept as long so this header needs no windows.h)
  bool HandleAccessLost(std::string* err, long hr);
  // (Re)creates the CPU-readable staging texture for current w_/h_.
  bool MakeStaging(std::string* err);
  // gpu-readback: (re)creates the VideoProcessor (enumerator for the current
  // source dims/format + NV12 output) and the NV12 output + double-buffered
  // staging textures for out_w_ x out_h_. Must be called after w_/h_/rot_/
  // out_w_/out_h_ are set. On failure the caller degrades to the BGRA path.
  bool MakeGpuPipeline(std::string* err);

  // M2 Task 2 surface state: device_gen_ bumps on every Init (the D3D device
  // is re-created there); the caller's LatestSurface is re-Init'ed whenever
  // the generation or the duplication mode changed (EnsureLatestSurface -
  // CopyResource demands same-device textures). last_surface_id_ mirrors
  // the identity stamped into that surface (the kNoChange echo).
  bool EnsureLatestSurface(LatestSurface& latest, std::string* err);
  uint64_t device_gen_ = 0;
  uint64_t latest_gen_ = 0;
  FrameIdentity last_surface_id_{};
  bool have_surface_base_ = false;  // first surface frame after (re)create

  struct Impl;  // COM pointers (d3d11.h stays out of this header)
  Impl* impl_;
  uint32_t w_ = 0, h_ = 0;
  uint32_t rebuilds_ = 0;
  uint32_t consecutive_rebuild_failures_ = 0;
  bool have_base_frame_ = false;  // first frame after create/rebuild is base
  // gpu-readback state: gpu_max_w_ > 0 = GPU path active (dims below are the
  // VideoProcessor OUTPUT dims); rot_ is the duplication rotation applied in
  // the same VideoProcessorBlt pass.
  uint32_t gpu_max_w_ = 0;
  Rotate rot_ = Rotate::kNone;
  uint32_t out_w_ = 0, out_h_ = 0;  // scaled/rotated NV12 dims (GPU path)
};

// gpu-readback task: TryCreateDxgiCapture with the GPU-scale+NV12 pipeline.
// max_w > 0 enables it (see InitGpuScale; graceful BGRA degradation when the
// driver cannot do the NV12 conversion); max_w == 0 is exactly
// TryCreateDxgiCapture. Returns null + *err when no output can be duplicated.
std::unique_ptr<ICapture> TryCreateDxgiCaptureGpu(uint32_t max_w, std::string* err);

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_DXGI_CAPTURE_H_
