// gdi_capture.h - GDI fallback capture backend (M2-Slice1 Task 3, spec
// §7.6). Chain: GetDC(NULL) -> CreateCompatibleDC/Bitmap ->
// BitBlt(SRCCOPY|CAPTUREBLT) -> GetDIBits(32bpp top-down BI_RGB) -> compact
// BGRA FrameBlob. GetSystemMetrics(SM_CXSCREEN/CYSCREEN) fixes the primary
// screen size: GetDC(NULL) and GetSystemMetrics share the same
// DPI-virtualization context inside one process, so the pair is always
// self-consistent (deliberately simple per the plan; no per-monitor DPI
// work). CAPTUREBLT pulls layered windows in; the known cost is a possible
// cursor flicker - the real cursor rides the independent 0x0109 channel
// (spec §7.6: GDI 模式下光标走独立通道, 视频不烧录光标).
//
// Interface parity with DxgiCapture (capture.h contract):
//   true                   frame (first after Init/Rebuild is always a frame)
//   false + "err_timeout"  static screen: sampled-CRC detected no change -
//                          GDI has no present notification, so the CRC IS
//                          the "no change" signal; the pipeline encodes
//                          nothing, exactly like DXGI WAIT_TIMEOUT
//   false + "err_rebuilt"  transient BitBlt/GetDIBits failure healed by an
//                          in-place re-init, or screen metrics drifted
//                          (resolution change): retry, no frame this call
//   false + "err_access_lost" persistently broken after the internal
//                          re-init failed: the unified CaptureReset owns
//                          the retrying (same never-fatal semantics DXGI
//                          has post-T2)
//
// 15 fps cap (spec §7.6: GDI 上限 15fps): Acquire internally paces to at
// most one BitBlt per kGdiFrameIntervalMs regardless of the pipeline's
// target fps.
//
// The pure helpers (Fnv1a64Update / GdiSampledRowY / GdiSampledCrc) are
// header-only on purpose: desktop_selftest covers them without a desktop.
// Win32 plumbing lives in gdi_capture.cpp (pimpl, header needs no windows.h).
#ifndef XNC_NATIVE_DESKTOP_GDI_CAPTURE_H_
#define XNC_NATIVE_DESKTOP_GDI_CAPTURE_H_

#include <cstddef>
#include <cstdint>
#include <memory>
#include <string>

#include "capture.h"       // ICapture, ICaptureSurface, FrameBlob
#include "dxgi_capture.h"  // Fnv1a64, BgraBytes
#include "gpu_surface.h"   // LatestSurface (AcquireSurface destination)

namespace xnc {

// Hard frame-period cap: 67 ms > 1000/15, so the effective rate never
// exceeds 15 fps even when the caller polls faster.
inline constexpr uint32_t kGdiFrameIntervalMs = 67;

// FNV-1a continuation: folds n bytes into a running state (Fnv1a64 is the
// one-shot over a single buffer; the sampled CRC chains several row
// segments plus the dimensions).
inline uint64_t Fnv1a64Update(uint64_t h, const uint8_t* d, size_t n) {
  for (size_t i = 0; i < n; ++i) {
    h ^= d[i];
    h *= 1099511628211ull;
  }
  return h;
}

// y coordinate of sampled row i of `rows` evenly spread rows: row MIDPOINTS
// ((2i+1)*h)/(2*rows), clamped to h-1. Midpoints instead of edge rows
// avoid biasing the sample on caption-only / taskbar-only rows.
inline uint32_t GdiSampledRowY(uint32_t h, uint32_t i, uint32_t rows) {
  if (h == 0 || rows == 0) return 0;
  const uint64_t y =
      (static_cast<uint64_t>(h) * (2ull * static_cast<uint64_t>(i) + 1ull)) /
      (2ull * static_cast<uint64_t>(rows));
  return y >= h ? h - 1 : static_cast<uint32_t>(y);
}

// Sampled change-detection CRC (plan Task 3: detect 静止用 CRC 行采样, 8 rows
// sampled): FNV-1a over `rows` evenly spread FULL rows, with w/h mixed in
// first so a mode change can never collide with identical pixels at a
// different geometry. GDI has no present notification - this is how a static
// desktop surfaces as "err_timeout" so the pipeline treats the GDI backend
// exactly like DXGI (interface parity). Documented approximation: a change
// confined strictly BETWEEN two sampled rows (e.g. a blinking caret) can be
// missed until the next change touching a sampled row; cursor motion still
// flows via the independent cursor channel.
inline uint64_t GdiSampledCrc(const uint8_t* bgra, uint32_t w, uint32_t h,
                              uint32_t rows = 8) {
  if (bgra == nullptr || w == 0 || h == 0 || rows == 0) return 0;
  const uint32_t dims[2] = {w, h};
  uint64_t hash = Fnv1a64(reinterpret_cast<const uint8_t*>(dims), sizeof(dims));
  const size_t row_bytes = static_cast<size_t>(w) * 4;
  for (uint32_t i = 0; i < rows; ++i) {
    const uint32_t y = GdiSampledRowY(h, i, rows);
    hash = Fnv1a64Update(hash, bgra + static_cast<size_t>(y) * row_bytes, row_bytes);
  }
  return hash;
}

// GDI fallback backend (spec §7.6 rung below DXGI). Single-threaded use
// only (pipeline thread), same as DxgiCapture. M2 Task 2: also implements
// ICaptureSurface - the BitBlt/DIB acquisition is kept and the compact BGRA
// is uploaded into the caller-owned LatestSurface (UpdateSubresource) on an
// internal D3D device (hardware -> WARP), so even the fallback rung can
// feed the GPU pipeline. The CPU FrameBlob path REMAINS (the software
// fallback pipeline keeps using it - Task 4 wires which one runs).
class GdiCapture final : public ICapture, public ICaptureSurface {
 public:
  GdiCapture();
  ~GdiCapture() override;
  GdiCapture(const GdiCapture&) = delete;
  GdiCapture& operator=(const GdiCapture&) = delete;

  // ICapture - see the file header for the GDI err vocabulary ("err_timeout"
  // / "err_rebuilt" / "err_access_lost"; never fatal). timeout_ms is ignored
  // (GDI paces itself to the 15 fps cap).
  bool Acquire(FrameBlob& blob, std::string* err = nullptr,
               uint32_t timeout_ms = 0) override;
  // ICaptureSurface (M2 Task 2, ruling 3): keep the BitBlt/GetDIBits
  // acquisition (Acquire above - unchanged CPU FrameBlob path), then upload
  // the compact BGRA into the caller-owned LatestSurface: UpdateSubresource
  // into a backend-owned DEFAULT-usage BGRA texture on the backend's
  // internal D3D device, followed by the desc-identical CopyFrom that
  // stamps the GIVEN identity (gpu_surface.h has no direct CPU-upload
  // entry - the upload rides the backend texture). There is no
  // duplication-held resource in GDI (no ReleaseFrame equivalent): nothing
  // outlives the call. Statuses/identity semantics: see capture.h.
  CaptureStatus AcquireSurface(LatestSurface& latest, uint32_t timeout_ms,
                               FrameIdentity* id, std::string* err) override;
  uint32_t Width() const override { return w_; }
  uint32_t Height() const override { return h_; }
  uint32_t RebuildCount() const override { return rebuilds_; }

  // Unified CaptureReset entry: destroy + re-create all GDI objects at the
  // current screen metrics (also the internal BitBlt-failure self-heal).
  bool Rebuild(std::string* err) override;

  // Full creation: GetDC(NULL) + compatible DC/bitmap at SM_CX/CYSCREEN.
  bool Init(std::string* err);

 private:
  struct Impl;  // HDC/HBITMAP + the D3D upload device (windows.h/d3d11.h stay
                // out of this header)
  Impl* impl_;
  uint32_t w_ = 0, h_ = 0;
  uint32_t rebuilds_ = 0;
  bool have_frame_ = false;   // first frame after create/rebuild always emits
  uint64_t last_crc_ = 0;     // last frame's sampled CRC (change detection)
  uint64_t last_capture_ms_ = 0;  // 15 fps pacing anchor
  // M2 Task 2 surface state (the D3D device/textures live in Impl):
  // last_surface_id_ mirrors the identity stamped into the caller's
  // LatestSurface (the kNoChange echo); have_surface_base_ logs the first
  // surface frame after (re)create.
  bool EnsureGpuUpload(std::string* err);
  FrameIdentity last_surface_id_{};
  bool have_surface_base_ = false;
};

// Factory (mirrors TryCreateDxgiCapture): null + *err on failure.
std::unique_ptr<ICapture> TryCreateGdiCapture(std::string* err);

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_GDI_CAPTURE_H_
