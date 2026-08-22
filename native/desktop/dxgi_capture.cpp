// dxgi_capture.cpp - DXGI Desktop Duplication capture backend (Task 3).
// Rewritten in C++ for xnc-desktop against the plan's contract; the legacy
// C reference (agent/screen-helper/dda/dda.c) established these hard-won
// facts, all honored here:
//   - device chain D3D11CreateDevice(NULL, HARDWARE, VIDEO_SUPPORT|BGRA) →
//     QI(IDXGIDevice) → GetAdapter → EnumOutputs → AttachedToDesktop filter;
//     no CoInitializeEx needed;
//   - AcquireNextFrame yields IDXGIResource - MUST QueryInterface to
//     ID3D11Texture2D before CopyResource (raw casts silently no-op and were
//     the root cause of the historical all-black-frames incident);
//   - ReleaseFrame immediately after the copy, never held across iterations;
//   - dimensions come from the duplication's GetDesc (native resolution,
//     DPI-virtualization free); rotated outputs are rejected for now;
//   - ACCESS_LOST rebuild is refused while a secure desktop (UAC) is active
//     even for SYSTEM - hence rebuild failures stay retryable once before
//     turning fatal.
// Task 3 additions per spec §7.4: DuplicateOutput1(B8G8R8A8) preferred with
// DuplicateOutput fallback; LastPresentTime==0 / WAIT_TIMEOUT = no change;
// first frame after create/rebuild is always a full readback.
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>

#include <d3d11.h>
#include <dxgi1_2.h>  // IDXGIOutput1, DXGI_OUTDUPL_*
#include <dxgi1_5.h>  // IDXGIOutput5::DuplicateOutput1
#include <wrl/client.h>

#include <cstdio>
#include <cstdlib>  // strtoul
#include <memory>
#include <string>

#include "../common/log.h"
#include "capture.h"
#include "dxgi_capture.h"

namespace xnc {

namespace {

using Microsoft::WRL::ComPtr;

constexpr UINT kAcquireTimeoutMs = 100;  // plan: ~100ms per Acquire
constexpr uint32_t kMaxConsecutiveRebuildFailures = 2;  // "rebuild once, repeated failure = fatal"

// Error strings are "<step>: hr=0x%08lX" so DxgiErrIsDesktopAccessDenied can
// parse the HRESULT back out.
void SetHrErr(std::string* err, const char* step, HRESULT hr) {
  char buf[160];
  _snprintf_s(buf, sizeof(buf), _TRUNCATE, "%s: hr=0x%08lX", step,
              static_cast<unsigned long>(hr));
  if (err) *err = buf;
}

uint64_t NowMonoUs() {
  static LARGE_INTEGER freq = [] {
    LARGE_INTEGER f{};
    QueryPerformanceFrequency(&f);
    return f;
  }();
  LARGE_INTEGER c{};
  QueryPerformanceCounter(&c);
  return static_cast<uint64_t>(((static_cast<unsigned long long>(c.QuadPart)) * 1000000ull) /
                               static_cast<unsigned long long>(freq.QuadPart));
}

}  // namespace

struct DxgiCapture::Impl {
  ComPtr<ID3D11Device> dev;
  ComPtr<ID3D11DeviceContext> ctx;
  ComPtr<IDXGIOutput1> out1;   // fallback re-duplication handle
  ComPtr<IDXGIOutput5> out5;   // preferred DuplicateOutput1 handle
  ComPtr<IDXGIOutputDuplication> dupl;
  ComPtr<ID3D11Texture2D> staging;  // persistent CPU-readable full-frame copy
};

DxgiCapture::DxgiCapture() : impl_(new Impl) {}
DxgiCapture::~DxgiCapture() { delete impl_; }

// Creates the staging texture for the current w_/h_ (CPU read, BGRA).
bool DxgiCapture::MakeStaging(std::string* err) {
  impl_->staging.Reset();
  D3D11_TEXTURE2D_DESC td{};
  td.Width = w_;
  td.Height = h_;
  td.MipLevels = 1;
  td.ArraySize = 1;
  td.SampleDesc.Count = 1;
  td.Format = DXGI_FORMAT_B8G8R8A8_UNORM;
  td.Usage = D3D11_USAGE_STAGING;
  td.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
  const HRESULT hr = impl_->dev->CreateTexture2D(&td, nullptr, &impl_->staging);
  if (FAILED(hr)) {
    SetHrErr(err, "CreateTexture2D(staging)", hr);
    return false;
  }
  return true;
}

// Reads w/h/rotation off the duplication; rejects rotated outputs and
// (re)creates staging when the mode changed.
bool DxgiCapture::Reduplicate(std::string* err) {
  impl_->dupl.Reset();
  HRESULT hr = E_FAIL;
  if (impl_->out5 && impl_->dev) {
    const DXGI_FORMAT fmt = DXGI_FORMAT_B8G8R8A8_UNORM;
    hr = impl_->out5->DuplicateOutput1(impl_->dev.Get(), 0, 1, &fmt, &impl_->dupl);
  }
  if (!impl_->dupl && impl_->out1 && impl_->dev) {
    hr = impl_->out1->DuplicateOutput(impl_->dev.Get(), &impl_->dupl);
  }
  if (FAILED(hr) || !impl_->dupl) {
    SetHrErr(err, "Reduplicate", hr);
    return false;
  }
  DXGI_OUTDUPL_DESC d{};
  impl_->dupl->GetDesc(&d);
  if (d.Rotation != DXGI_MODE_ROTATION_IDENTITY) {
    if (err) *err = "rotated output not supported";
    return false;
  }
  if (d.ModeDesc.Width != w_ || d.ModeDesc.Height != h_) {
    w_ = d.ModeDesc.Width;
    h_ = d.ModeDesc.Height;
    if (!MakeStaging(err)) return false;
    XNC_LOG_INFO("dxgi mode changed w=%u h=%u", w_, h_);
  }
  return true;
}

bool DxgiCapture::Init(std::string* err) {
  impl_->dupl.Reset();
  impl_->staging.Reset();
  impl_->out1.Reset();
  impl_->out5.Reset();
  impl_->ctx.Reset();
  impl_->dev.Reset();
  w_ = 0;
  h_ = 0;  // forces Reduplicate to (re)create staging for the new mode

  // 1. Hardware device on the default adapter (VIDEO_SUPPORT for the M4 GPU
  //    encoder path, BGRA for the CPU path). WARP fallback covers drivers
  //    without D3D11 hardware support.
  D3D_FEATURE_LEVEL fl{};
  HRESULT hr = D3D11CreateDevice(nullptr, D3D_DRIVER_TYPE_HARDWARE, nullptr,
                                 D3D11_CREATE_DEVICE_VIDEO_SUPPORT | D3D11_CREATE_DEVICE_BGRA_SUPPORT,
                                 nullptr, 0, D3D11_SDK_VERSION, &impl_->dev, &fl, &impl_->ctx);
  if (FAILED(hr)) {
    XNC_LOG_INFO("dxgi hardware device failed hr=0x%08lX, trying WARP",
                 static_cast<unsigned long>(hr));
    hr = D3D11CreateDevice(nullptr, D3D_DRIVER_TYPE_WARP, nullptr,
                           D3D11_CREATE_DEVICE_VIDEO_SUPPORT | D3D11_CREATE_DEVICE_BGRA_SUPPORT,
                           nullptr, 0, D3D11_SDK_VERSION, &impl_->dev, &fl, &impl_->ctx);
    if (FAILED(hr)) {
      SetHrErr(err, "D3D11CreateDevice", hr);
      return false;
    }
  }

  // 2. Device → adapter → first desktop-attached output that duplicates.
  ComPtr<IDXGIDevice> dxgi_dev;
  hr = impl_->dev.As(&dxgi_dev);
  if (FAILED(hr)) {
    SetHrErr(err, "QI IDXGIDevice", hr);
    return false;
  }
  ComPtr<IDXGIAdapter> adapter;
  hr = dxgi_dev->GetAdapter(&adapter);
  if (FAILED(hr)) {
    SetHrErr(err, "GetAdapter", hr);
    return false;
  }

  std::string last_err = "no desktop-attached output";
  for (UINT i = 0;; ++i) {
    ComPtr<IDXGIOutput> output;
    hr = adapter->EnumOutputs(i, &output);
    if (hr == DXGI_ERROR_NOT_FOUND) break;
    if (FAILED(hr)) {
      SetHrErr(err, "EnumOutputs", hr);
      return false;
    }
    DXGI_OUTPUT_DESC od{};
    if (FAILED(output->GetDesc(&od)) || !od.AttachedToDesktop) continue;

    ComPtr<IDXGIOutput1> o1;
    ComPtr<IDXGIOutput5> o5;
    output.As(&o1);   // always available on DXGI 1.2+
    output.As(&o5);   // Windows 10+; absence is fine (fallback below)
    impl_->out1 = o1;
    impl_->out5 = o5;

    if (Reduplicate(err)) {
      have_base_frame_ = false;
      XNC_LOG_INFO("dxgi duplication ready output=%u w=%u h=%u via=%s", i, w_, h_,
                   impl_->out5 ? "DuplicateOutput1" : "DuplicateOutput");
      return true;
    }
    last_err = err ? *err : "duplicate failed";
    XNC_LOG_INFO("dxgi duplicate failed on output %u err=\"%s\"", i, last_err.c_str());
    impl_->dupl.Reset();
    impl_->out1.Reset();
    impl_->out5.Reset();
  }
  if (err) *err = last_err;
  return false;
}

// Returns false always (retryable or fatal - see capture.h err contract);
// shapes *err accordingly.
bool DxgiCapture::HandleAccessLost(std::string* err, long hr_long) {
  const HRESULT hr = static_cast<HRESULT>(hr_long);
  rebuilds_++;
  std::string rerr;
  bool ok = false;
  if (hr == DXGI_ERROR_DEVICE_REMOVED) {
    ok = Init(&rerr);  // device is dead - full re-creation required
  } else {
    ok = Reduplicate(&rerr);       // cheap path first (dda.c behavior)
    if (!ok) ok = Init(&rerr);     // escalate once to a full rebuild
  }
  if (ok) {
    consecutive_rebuild_failures_ = 0;
    have_base_frame_ = false;  // next content frame is the new base frame
    XNC_LOG_INFO("dxgi rebuilt total=%u w=%u h=%u", rebuilds_, w_, h_);
    if (err) *err = "err_rebuilt";  // retryable, no frame this call
    return false;
  }
  consecutive_rebuild_failures_++;
  XNC_LOG_ERROR("dxgi rebuild failed streak=%u err=\"%s\"",
                consecutive_rebuild_failures_, rerr.c_str());
  if (consecutive_rebuild_failures_ >= kMaxConsecutiveRebuildFailures) {
    if (err) *err = "rebuild failed repeatedly: " + rerr;  // fatal
  } else {
    if (err) *err = "err_rebuilt";  // one transient failure stays retryable
  }
  return false;
}

bool DxgiCapture::Acquire(FrameBlob& blob, std::string* err) {
  if (!impl_->dupl) {
    if (err) *err = "err_not_initialized";
    return false;
  }
  DXGI_OUTDUPL_FRAME_INFO info{};
  ComPtr<IDXGIResource> res;
  HRESULT hr = impl_->dupl->AcquireNextFrame(kAcquireTimeoutMs, &info, &res);
  if (hr == DXGI_ERROR_WAIT_TIMEOUT) {  // static screen: no change, no blob
    if (err) *err = "err_timeout";
    return false;
  }
  if (hr == DXGI_ERROR_ACCESS_LOST || hr == DXGI_ERROR_DEVICE_REMOVED)
    return HandleAccessLost(err, hr);
  if (FAILED(hr)) {
    SetHrErr(err, "AcquireNextFrame", hr);
    return false;
  }

  // No present since the last frame (cursor/metadata-only): treat as no
  // change per spec §7.4 - no encode, no blob.
  if (info.LastPresentTime.QuadPart == 0) {
    impl_->dupl->ReleaseFrame();
    if (err) *err = "err_timeout";
    return false;
  }

  // QI to the texture interface is mandatory (see file header); a raw cast
  // makes CopyResource silently no-op against the wrong vtable.
  ComPtr<ID3D11Texture2D> tex;
  if (!res || FAILED(res.As(&tex))) {
    impl_->dupl->ReleaseFrame();
    if (err) *err = "err_timeout";  // cursor-only frame, no new texture
    return false;
  }

  // Full-frame copy into our persistent staging texture, then hand the
  // desktop image straight back - never held across iterations.
  impl_->ctx->CopyResource(impl_->staging.Get(), tex.Get());
  tex.Reset();
  impl_->dupl->ReleaseFrame();

  D3D11_MAPPED_SUBRESOURCE mapped{};
  hr = impl_->ctx->Map(impl_->staging.Get(), 0, D3D11_MAP_READ, 0, &mapped);
  if (FAILED(hr)) {
    SetHrErr(err, "Map(staging)", hr);
    return false;
  }
  const size_t need = BgraBytes(w_, h_);
  if (need == 0) {
    impl_->ctx->Unmap(impl_->staging.Get(), 0);
    if (err) *err = "err_bad_dims";
    return false;
  }
  blob.bgra.resize(need);
  CompactBgraRows(static_cast<const uint8_t*>(mapped.pData), mapped.RowPitch,
                  blob.bgra.data(), w_, h_);
  impl_->ctx->Unmap(impl_->staging.Get(), 0);

  blob.w = w_;
  blob.h = h_;
  blob.mono_us = NowMonoUs();
  if (!have_base_frame_) {
    have_base_frame_ = true;  // this full readback IS the base frame
    XNC_LOG_INFO("dxgi base frame w=%u h=%u mono_us=%llu", w_, h_, blob.mono_us);
  }
  return true;
}

bool DxgiErrIsDesktopAccessDenied(const std::string& err) {
  const size_t pos = err.find("hr=0x");
  if (pos == std::string::npos) return false;
  const uint32_t hr = static_cast<uint32_t>(std::strtoul(err.c_str() + pos + 5, nullptr, 16));
  // Verified on LABS-XIAOXIN (2026-08-22): a SYSTEM process in session 0
  // gets NOT_CURRENTLY_AVAILABLE from EnumOutputs (outputs are invisible
  // without an interactive desktop); DuplicateOutput in that state returns
  // E_ACCESSDENIED. Both mean "no desktop access from this session" - the
  // Task 6 session bridge (CreateProcessAsUserW into the console session)
  // is the fix, not code here.
  return hr == 0x80070005u /* E_ACCESSDENIED */ ||
         hr == static_cast<uint32_t>(DXGI_ERROR_UNSUPPORTED) ||
         hr == static_cast<uint32_t>(DXGI_ERROR_NOT_CURRENTLY_AVAILABLE) ||
         hr == static_cast<uint32_t>(DXGI_ERROR_SESSION_DISCONNECTED);
}

std::unique_ptr<ICapture> TryCreateDxgiCapture(std::string* err) {
  auto cap = std::make_unique<DxgiCapture>();
  std::string ierr;
  if (!cap->Init(&ierr)) {
    if (err) *err = ierr;
    XNC_LOG_ERROR("dxgi init failed err=\"%s\"", ierr.c_str());
    return nullptr;
  }
  return cap;
}

}  // namespace xnc
