// gdi_capture.cpp - Win32 plumbing for the GDI fallback backend (Task 3,
// spec §7.6; see gdi_capture.h for the contract). Hard-won GDI facts this
// honors (dda.c-era knowledge, spec reference only - no external code):
//   - GetDC(NULL) + GetSystemMetrics(SM_CX/CYSCREEN) share one
//     DPI-virtualization context per process, so the DC and the metrics are
//     always a consistent pair (no per-monitor DPI work; plan ruling);
//   - BitBlt needs CAPTUREBLT to include layered windows (tooltips et al.);
//   - GetDIBits with biHeight NEGATIVE returns top-down rows (row 0 = top),
//     matching the FrameBlob convention CompactBgraRows establishes for
//     DXGI; 32bpp BI_RGB rows are DWORD aligned = tightly packed at w*4;
//   - the bitmap must not be selected into the DC handed to GetDIBits -
//     the SCREEN dc is passed for palette info instead, mem_dc stays owner;
//   - GDI cannot see the secure desktop: while UAC/winlogon is up, BitBlt
//     keeps returning the stale default-desktop image (CRC static ->
//     err_timeout). Degraded-but-stable, by design (可操作的低帧率 > 黑屏).
// GDI has no present notification, so change detection is the sampled CRC
// (GdiSampledCrc, 8 rows) - that is the err_timeout source.
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>  // GetDC/BitBlt/GetDIBits/GetSystemMetrics/Sleep

#include <cstdio>

#include "../common/log.h"
#include "dxgi_capture.h"  // NowMonoUs (shared mono-us clock)
#include "gdi_capture.h"

namespace xnc {

namespace {

uint64_t NowMs() { return GetTickCount64(); }

// Destroys every GDI object (SelectObject restore order matters for the
// owned bitmap: it cannot be deleted while selected into mem_dc).

}  // namespace

struct GdiCapture::Impl {
  HDC screen_dc = nullptr;  // GetDC(NULL), shared for BitBlt + GetDIBits
  HDC mem_dc = nullptr;     // compatible memory DC holding the bitmap
  HBITMAP bmp = nullptr;    // w_ x h_ compatible bitmap (capture target)
  HBITMAP old = nullptr;    // SelectObject return (restored before delete)

  void Teardown() {
    if (mem_dc != nullptr && old != nullptr) {
      SelectObject(mem_dc, old);  // un-select before deleting the bitmap
      old = nullptr;
    }
    if (bmp != nullptr) {
      DeleteObject(bmp);
      bmp = nullptr;
    }
    if (mem_dc != nullptr) {
      DeleteDC(mem_dc);
      mem_dc = nullptr;
    }
    if (screen_dc != nullptr) {
      ReleaseDC(nullptr, screen_dc);
      screen_dc = nullptr;
    }
  }
};

GdiCapture::GdiCapture() : impl_(new Impl) {}

GdiCapture::~GdiCapture() {
  impl_->Teardown();
  delete impl_;
}

bool GdiCapture::Init(std::string* err) {
  impl_->Teardown();
  w_ = static_cast<uint32_t>(GetSystemMetrics(SM_CXSCREEN));
  h_ = static_cast<uint32_t>(GetSystemMetrics(SM_CYSCREEN));
  if (w_ == 0 || h_ == 0 || BgraBytes(w_, h_) == 0) {
    w_ = h_ = 0;
    if (err) *err = "gdi: degenerate screen metrics";
    return false;
  }
  impl_->screen_dc = GetDC(nullptr);
  if (impl_->screen_dc == nullptr) {
    if (err) *err = "gdi: GetDC(NULL) failed";
    return false;
  }
  impl_->mem_dc = CreateCompatibleDC(impl_->screen_dc);
  if (impl_->mem_dc == nullptr) {
    impl_->Teardown();
    if (err) *err = "gdi: CreateCompatibleDC failed";
    return false;
  }
  impl_->bmp = CreateCompatibleBitmap(impl_->screen_dc, w_, h_);
  if (impl_->bmp == nullptr) {
    impl_->Teardown();
    if (err) *err = "gdi: CreateCompatibleBitmap failed";
    return false;
  }
  impl_->old = static_cast<HBITMAP>(SelectObject(impl_->mem_dc, impl_->bmp));
  have_frame_ = false;      // first frame after (re)create is always a frame
  last_crc_ = 0;
  last_capture_ms_ = 0;
  XNC_LOG_INFO("gdi init w=%u h=%u", w_, h_);
  return true;
}

bool GdiCapture::Rebuild(std::string* err) {
  ++rebuilds_;
  const bool ok = Init(err);
  if (ok) XNC_LOG_INFO("gdi rebuilt total=%u", rebuilds_);
  return ok;
}

bool GdiCapture::Acquire(FrameBlob& blob, std::string* err, uint32_t timeout_ms) {
  (void)timeout_ms;  // GDI paces itself to the 15 fps cap (spec §7.6)
  const auto set_err = [err](const char* e) {
    if (err) *err = e;
  };
  // Broken since a previous failed rebuild: one fresh attempt per call;
  // persistent failure surfaces as err_access_lost so the unified reset
  // owns retrying (never fatal - DXGI parity).
  if (impl_->mem_dc == nullptr || impl_->bmp == nullptr) {
    std::string rerr;
    if (Rebuild(&rerr)) {
      set_err("err_rebuilt");
      return false;
    }
    XNC_LOG_ERROR("gdi re-init failed err=\"%s\"", rerr.c_str());
    set_err("err_access_lost");
    return false;
  }
  // Metrics drift (resolution change): adopt the new mode like DXGI adopts
  // a new duplication - the pipeline sees the new dims on the next frame
  // and routes through the unified reset (encoder re-init + IDR).
  const uint32_t sw = static_cast<uint32_t>(GetSystemMetrics(SM_CXSCREEN));
  const uint32_t sh = static_cast<uint32_t>(GetSystemMetrics(SM_CYSCREEN));
  if (sw != w_ || sh != h_) {
    XNC_LOG_INFO("gdi metrics changed old=%ux%u new=%ux%u", w_, h_, sw, sh);
    std::string rerr;
    if (Rebuild(&rerr)) {
      set_err("err_rebuilt");
      return false;
    }
    set_err("err_access_lost");
    return false;
  }
  // 15 fps cap (spec §7.6): at most one BitBlt per kGdiFrameIntervalMs.
  const uint64_t now = NowMs();
  if (last_capture_ms_ != 0 && now - last_capture_ms_ < kGdiFrameIntervalMs) {
    const uint64_t wait = kGdiFrameIntervalMs - (now - last_capture_ms_);
    Sleep(static_cast<DWORD>(wait));
  }
  last_capture_ms_ = NowMs();
  if (!BitBlt(impl_->mem_dc, 0, 0, static_cast<int>(w_), static_cast<int>(h_),
              impl_->screen_dc, 0, 0, SRCCOPY | CAPTUREBLT)) {
    const DWORD e = GetLastError();
    XNC_LOG_ERROR("gdi bitblt failed err=%lu", static_cast<unsigned long>(e));
    std::string rerr;
    if (Rebuild(&rerr)) {
      set_err("err_rebuilt");  // transient: healed in place, retry
      return false;
    }
    set_err("err_access_lost");  // persistent: unified reset retries
    return false;
  }
  const size_t need = BgraBytes(w_, h_);
  if (need == 0) {
    set_err("err_access_lost");  // degenerate dims; rebuild path will refire
    return false;
  }
  BITMAPINFO bmi{};
  bmi.bmiHeader.biSize = sizeof(BITMAPINFOHEADER);
  bmi.bmiHeader.biWidth = static_cast<LONG>(w_);
  bmi.bmiHeader.biHeight = -static_cast<LONG>(h_);  // NEGATIVE = top-down
  bmi.bmiHeader.biPlanes = 1;
  bmi.bmiHeader.biBitCount = 32;                    // tightly packed BGRA
  bmi.bmiHeader.biCompression = BI_RGB;
  blob.bgra.resize(need);
  // The screen dc is passed (palette context); the bitmap stays selected
  // in mem_dc - GetDIBits must not get the selecting dc per the API rule.
  if (GetDIBits(impl_->screen_dc, impl_->bmp, 0, h_, blob.bgra.data(), &bmi,
                DIB_RGB_COLORS) != h_) {
    const DWORD e = GetLastError();
    XNC_LOG_ERROR("gdi getdibits failed err=%lu", static_cast<unsigned long>(e));
    blob.bgra.clear();
    std::string rerr;
    if (Rebuild(&rerr)) {
      set_err("err_rebuilt");
      return false;
    }
    set_err("err_access_lost");
    return false;
  }
  // Change detection (the GDI stand-in for DXGI's present notification):
  // identical sampled CRC = static screen = err_timeout, interface parity.
  const uint64_t crc = GdiSampledCrc(blob.bgra.data(), w_, h_);
  if (have_frame_ && crc == last_crc_) {
    set_err("err_timeout");
    return false;
  }
  const bool first = !have_frame_;
  have_frame_ = true;
  last_crc_ = crc;
  blob.w = w_;
  blob.h = h_;
  blob.mono_us = NowMonoUs();
  if (first) XNC_LOG_INFO("gdi base frame w=%u h=%u mono_us=%llu", w_, h_,
                          static_cast<unsigned long long>(blob.mono_us));
  return true;
}

std::unique_ptr<ICapture> TryCreateGdiCapture(std::string* err) {
  auto cap = std::make_unique<GdiCapture>();
  std::string ierr;
  if (!cap->Init(&ierr)) {
    if (err) *err = ierr;
    XNC_LOG_ERROR("gdi init failed err=\"%s\"", ierr.c_str());
    return nullptr;
  }
  return cap;
}

}  // namespace xnc
