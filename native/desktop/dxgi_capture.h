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

#include "capture.h"  // ICapture, FrameBlob, TryCreateDxgiCapture

namespace xnc {

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

// DXGI Desktop Duplication with CPU readback. Single-threaded use only.
class DxgiCapture final : public ICapture {
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
  uint32_t Width() const override { return w_; }
  uint32_t Height() const override { return h_; }
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

  struct Impl;  // COM pointers (d3d11.h stays out of this header)
  Impl* impl_;
  uint32_t w_ = 0, h_ = 0;
  uint32_t rebuilds_ = 0;
  uint32_t consecutive_rebuild_failures_ = 0;
  bool have_base_frame_ = false;  // first frame after create/rebuild is base
};

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_DXGI_CAPTURE_H_
