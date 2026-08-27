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

#include <d3d11.h>  // M2 Task 2: the surface-path upload device
#include <wrl/client.h>

#include <cstdio>
#include <string>

#include "../common/log.h"
#include "dxgi_capture.h"  // NowMonoUs (shared mono-us clock)
#include "gdi_capture.h"
#include "gpu_surface.h"  // LatestSurface (AcquireSurface destination)

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

  // M2 Task 2 (ICaptureSurface): the upload device for the surface path.
  // upload_ is a w_ x h_ DEFAULT-usage BGRA texture: UpdateSubresource
  // fills it from the compact GetDIBits buffer, then the desc-identical
  // LatestSurface::CopyFrom stamps it into the caller-owned surface. The
  // device is created lazily (hardware -> WARP) and deliberately NOT
  // touched by Teardown: Init/Rebuild only recycle the GDI objects. It IS
  // dropped when the caller's LatestSurface needs a full (re)Init (a
  // Reset()-dropped surface reports width 0 - see AcquireSurface), so the
  // surface and the upload texture always share one LIVE device: a
  // TDR-killed device passes every dims check, and re-creating it there is
  // the only signal that reaches it. The ComPtrs release when Impl is
  // deleted (~GdiCapture).
  Microsoft::WRL::ComPtr<ID3D11Device> dev;
  Microsoft::WRL::ComPtr<ID3D11DeviceContext> ctx;
  Microsoft::WRL::ComPtr<ID3D11Texture2D> upload;

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
  have_surface_base_ = false;  // M2 Task 2: next surface frame is the base
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

// M2 Task 2: lazily creates the D3D upload device (hardware -> WARP, the
// selftest device pattern; the GDI rung may be a VM/RDP box where WARP is
// the only certainty) and keeps the w_ x h_ DEFAULT-usage BGRA upload
// texture in step with the current screen metrics.
bool GdiCapture::EnsureGpuUpload(std::string* err) {
  if (!impl_->dev) {
    D3D_FEATURE_LEVEL fl{};
    HRESULT hr = D3D11CreateDevice(
        nullptr, D3D_DRIVER_TYPE_HARDWARE, nullptr,
        D3D11_CREATE_DEVICE_BGRA_SUPPORT, nullptr, 0, D3D11_SDK_VERSION,
        &impl_->dev, &fl, &impl_->ctx);
    if (FAILED(hr)) {
      hr = D3D11CreateDevice(nullptr, D3D_DRIVER_TYPE_WARP, nullptr,
                             D3D11_CREATE_DEVICE_BGRA_SUPPORT, nullptr, 0,
                             D3D11_SDK_VERSION, &impl_->dev, &fl, &impl_->ctx);
    }
    if (FAILED(hr)) {
      char buf[96];
      _snprintf_s(buf, sizeof(buf), _TRUNCATE,
                  "D3D11CreateDevice(upload): hr=0x%08lX",
                  static_cast<unsigned long>(hr));
      if (err) *err = buf;
      return false;
    }
    XNC_LOG_INFO("gdi surface upload device ready");
  }
  if (impl_->upload) {
    D3D11_TEXTURE2D_DESC ud{};
    impl_->upload->GetDesc(&ud);
    if (ud.Width == w_ && ud.Height == h_) return true;
    impl_->upload.Reset();
  }
  // CopyFrom demands a desc-identical source: 1 mip / 1 array slice / 1
  // sample / BGRA / DEFAULT usage, exactly like LatestSurface's texture.
  D3D11_TEXTURE2D_DESC td{};
  td.Width = w_;
  td.Height = h_;
  td.MipLevels = 1;
  td.ArraySize = 1;
  td.SampleDesc.Count = 1;
  td.Format = DXGI_FORMAT_B8G8R8A8_UNORM;
  td.Usage = D3D11_USAGE_DEFAULT;
  const HRESULT hr = impl_->dev->CreateTexture2D(&td, nullptr, &impl_->upload);
  if (FAILED(hr)) {
    char buf[112];
    _snprintf_s(buf, sizeof(buf), _TRUNCATE,
                "CreateTexture2D(gdi upload): hr=0x%08lX",
                static_cast<unsigned long>(hr));
    if (err) *err = buf;
    return false;
  }
  return true;
}

// M2 Task 2 (ICaptureSurface, ruling 3): BitBlt/DIB acquisition stays (the
// CPU Acquire IS the acquisition - pacing, self-heal and the sampled-CRC
// change detection included), then the compact BGRA rides
// UpdateSubresource into the upload texture and CopyFrom stamps it into
// the caller-owned LatestSurface. No duplication exists in GDI, so there
// is no ReleaseFrame-equivalent resource to release - nothing outlives the
// call. Statuses/identity semantics: capture.h (the GDI acquire timestamp
// blob.mono_us becomes source_mono_us).
CaptureStatus GdiCapture::AcquireSurface(LatestSurface& latest,
                                         uint32_t timeout_ms, FrameIdentity* id,
                                         std::string* err) {
  if (err) err->clear();
  FrameBlob blob;
  std::string aerr;
  if (!Acquire(blob, &aerr, timeout_ms)) {
    if (err) *err = aerr;
    if (id) *id = last_surface_id_;  // echo what the surface still holds
    return CaptureStatusFromErr(aerr);
  }
  std::string uerr;
  // Caller-owned surface on THIS backend's device at the current metrics:
  // a fresh surface reports width 0, a metrics change re-Inits, and so does
  // a surface dropped by LatestSurface::Reset (a backend swap or a unified
  // reset - final-review fix 2026-08; this is the DXGI->GDI fall that
  // otherwise keeps the dead device's texture at EQUAL dims). A forced
  // re-Init also drops the INTERNAL upload device + texture: the previous
  // device pairing is gone (or the device is TDR-dead, which no dims check
  // can see), so EnsureGpuUpload re-creates both below and the CopyFrom can
  // never pair two D3D devices.
  const bool reinit_latest = latest.width() != w_ || latest.height() != h_;
  if (reinit_latest) {
    impl_->dev.Reset();
    impl_->ctx.Reset();
    impl_->upload.Reset();
  }
  if (!EnsureGpuUpload(&uerr)) {
    if (err) *err = "gdi surface: " + uerr;
    return CaptureStatus::kFatal;
  }
  if (reinit_latest) {
    if (!latest.Init(impl_->dev.Get(), w_, h_, &uerr)) {
      if (err) *err = "gdi latest init: " + uerr;
      return CaptureStatus::kFatal;
    }
    last_surface_id_ = FrameIdentity{};
  }
  // CPU -> GPU upload of the tightly packed GetDIBits rows (row pitch w*4).
  impl_->ctx->UpdateSubresource(impl_->upload.Get(), 0, nullptr,
                                blob.bgra.data(), w_ * 4, 0);
  // Complete the GIVEN identity (ruling 1a): only the acquire timestamp is
  // backend-assigned; encode_seq/present_mono_us belong to the encode thread.
  FrameIdentity stamp = id != nullptr ? *id : FrameIdentity{};
  stamp.source_mono_us = blob.mono_us;
  stamp.encode_seq = 0;
  stamp.present_mono_us = 0;
  if (!latest.CopyFrom(impl_->ctx.Get(), impl_->upload.Get(), stamp, &uerr)) {
    if (err) *err = "gdi latest copy: " + uerr;
    return CaptureStatus::kFatal;
  }
  last_surface_id_ = stamp;
  if (!have_surface_base_) {
    have_surface_base_ = true;
    XNC_LOG_INFO("gdi surface base frame w=%u h=%u mono_us=%llu", w_, h_,
                 static_cast<unsigned long long>(stamp.source_mono_us));
  }
  if (id) *id = stamp;
  return CaptureStatus::kFrame;
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
