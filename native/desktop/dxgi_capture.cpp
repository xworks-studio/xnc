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
//
// gpu-readback task (this branch): the full-resolution CPU readback
// (~99 ms at 2880x1800 on XIAOXIN) was the residual fps/latency limiter.
// The GPU path (InitGpuScale with max_w > 0) replaces the CopyResource-full-
// BGRA + CPU downscale + CPU BGRA->NV12 chain with ONE VideoProcessorBlt
// (scale to max_w + NV12 conversion + rotation in a single pass) into a
// persistent NV12 output texture, then reads back only the small NV12 frame
// (~4x less data). The FrameBlob is Pixfmt::kNv12 at the scaled dims; the
// pipeline feeds the encoder's NV12 entry directly. Drivers without the NV12
// video-processor conversion degrade IN PLACE to the legacy BGRA path
// (logged) - the CPU ScaledCapture wrapper then works as before.
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>

#include <d3d11.h>
#include <d3d11_1.h>   // ID3D11VideoContext1 (VideoProcessorSetStreamRotation)
#include <dxgi1_2.h>  // IDXGIOutput1, DXGI_OUTDUPL_*
#include <dxgi1_5.h>  // IDXGIOutput5::DuplicateOutput1
#include <wrl/client.h>

#include <cstdio>
#include <cstdlib>  // strtoul
#include <memory>
#include <mutex>
#include <string>
#include <vector>

#include "../common/log.h"
#include "capture.h"
#include "dxgi_capture.h"
#include "nv12.h"  // Nv12Bytes (GPU path NV12 blob sizing)

namespace xnc {

namespace {

using Microsoft::WRL::ComPtr;

constexpr UINT kAcquireTimeoutMs = 100;  // plan: ~100ms per Acquire

// Error strings are "<step>: hr=0x%08lX" so DxgiErrIsDesktopAccessDenied can
// parse the HRESULT back out.
void SetHrErr(std::string* err, const char* step, HRESULT hr) {
  char buf[160];
  _snprintf_s(buf, sizeof(buf), _TRUNCATE, "%s: hr=0x%08lX", step,
              static_cast<unsigned long>(hr));
  if (err) *err = buf;
}

}  // namespace

// Declared in dxgi_capture.h (shared with the pipeline's latency clock);
// defined out of the anonymous namespace so the declaration links.
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
struct DxgiCapture::Impl {
  ComPtr<ID3D11Device> dev;
  ComPtr<ID3D11DeviceContext> ctx;
  ComPtr<IDXGIOutput1> out1;   // fallback re-duplication handle
  ComPtr<IDXGIOutput5> out5;   // preferred DuplicateOutput1 handle
  ComPtr<IDXGIOutputDuplication> dupl;
  // Persistent CPU-readable full-frame copies (pipeline-decouple perf pass:
  // DOUBLE-BUFFERED so the CPU readback of frame N overlaps the GPU copy of
  // frame N+1 - a single staging texture makes Map(D3D11_MAP_READ) block on
  // the just-issued CopyResource, which was the live-pipeline capture limiter
  // on XIAOXIN (~90ms/frame vs ~17ms standalone readback).
  ComPtr<ID3D11Texture2D> staging_[2];
  uint32_t staging_cur_ = 0;  // buffer the NEXT copy lands in
  bool have_staged_ = false;  // false until the first frame is read back

  // gpu-readback task: VideoProcessor pipeline (scale+NV12+rotate in one
  // blt). nv12_out_ is the DEFAULT-usage NV12 blt destination (double
  // buffered like the BGRA staging: the blt of frame N+1 never races the
  // copy of frame N); nv12_stage_ the CPU-readable copies. staging_cur_ /
  // have_staged_ track the NV12 buffers in GPU mode (mutually exclusive
  // with the BGRA path above - one mode per instance).
  ComPtr<ID3D11VideoDevice> vid_dev;
  ComPtr<ID3D11VideoContext> vid_ctx;
  ComPtr<ID3D11VideoProcessorEnumerator> vpe;
  ComPtr<ID3D11VideoProcessor> vp;
  ComPtr<ID3D11Texture2D> nv12_out_[2];
  ComPtr<ID3D11VideoProcessorOutputView> nv12_out_view_[2];  // one per out texture
  ComPtr<ID3D11Texture2D> nv12_stage_[2];
};

// ---- M2-Slice3 Task 5: process-global displays table + selection ----
// The table refreshes on every DxgiCapture::Init (and on the first snapshot
// request); g_desired records the requested output index (kDisplaySelectAuto
// = primary-or-0) and is consumed by the NEXT Init - that is how a switch
// rides the unified CaptureReset (set + RequestReset("switch") -> rebuild).
namespace {
std::mutex g_disp_mu;
std::vector<DisplayInfo> g_displays;
uint32_t g_desired = kDisplaySelectAuto;

// One enumerated attached output with its owning adapter kept alive.
struct EnumeratedOutput {
  ComPtr<IDXGIAdapter> adapter;
  ComPtr<IDXGIOutput> output;
  RawDisplayOutput raw;
};

// Walks EVERY adapter x output of the DXGI factory (M2-S3 Task 5): all
// desktop-attached outputs, with desktop coordinates + primary flag +
// HMONITOR as the dedupe/order key. The legacy Init bound output #0 of the
// device's own adapter; this walk is what makes the full table (and cross-
// adapter switching) possible.
bool EnumerateAllOutputs(ComPtr<IDXGIFactory1>* factory,
                         std::vector<EnumeratedOutput>* outs, std::string* err) {
  HRESULT hr = CreateDXGIFactory1(__uuidof(IDXGIFactory1),
                                  reinterpret_cast<void**>(factory->GetAddressOf()));
  if (FAILED(hr)) {
    SetHrErr(err, "CreateDXGIFactory1", hr);
    return false;
  }
  for (UINT a = 0;; ++a) {
    ComPtr<IDXGIAdapter> adapter;
    hr = (*factory)->EnumAdapters(a, &adapter);
    if (hr == DXGI_ERROR_NOT_FOUND) break;
    if (FAILED(hr)) {
      SetHrErr(err, "EnumAdapters", hr);
      return false;
    }
    for (UINT o = 0;; ++o) {
      ComPtr<IDXGIOutput> output;
      hr = adapter->EnumOutputs(o, &output);
      if (hr == DXGI_ERROR_NOT_FOUND) break;
      if (FAILED(hr)) break;  // per-adapter walk: skip the rest of this one
      DXGI_OUTPUT_DESC od{};
      if (FAILED(output->GetDesc(&od)) || !od.AttachedToDesktop) continue;
      EnumeratedOutput e;
      e.adapter = adapter;
      e.output = output;
      e.raw.monitor_id = static_cast<uint64_t>(
          reinterpret_cast<uintptr_t>(od.Monitor));
      e.raw.x = od.DesktopCoordinates.left;
      e.raw.y = od.DesktopCoordinates.top;
      e.raw.w = static_cast<uint32_t>(od.DesktopCoordinates.right -
                                      od.DesktopCoordinates.left);
      e.raw.h = static_cast<uint32_t>(od.DesktopCoordinates.bottom -
                                      od.DesktopCoordinates.top);
      if (od.Monitor != nullptr) {
        MONITORINFO mi{};
        mi.cbSize = sizeof(mi);
        if (GetMonitorInfoW(od.Monitor, &mi))
          e.raw.primary = (mi.dwFlags & MONITORINFOF_PRIMARY) != 0;
      }
      outs->push_back(std::move(e));
    }
  }
  return true;
}

// GDI monitor order (EnumDisplayMonitors callback order - the same order
// EnumDisplayDevices reports displays in, M0 repo knowledge). These ids
// become the STABLE display indices; outputs GDI does not list append.
BOOL CALLBACK CollectMonitorOrder(HMONITOR mon, HDC, LPRECT, LPARAM lp) {
  auto* order = reinterpret_cast<std::vector<uint64_t>*>(lp);
  order->push_back(static_cast<uint64_t>(reinterpret_cast<uintptr_t>(mon)));
  return TRUE;
}

// Refreshes g_displays from a fresh adapter walk. Empty on failure (the
// caller's Init surfaces the error; snapshots just see an empty table).
void RefreshGlobalTable() {
  ComPtr<IDXGIFactory1> factory;
  std::vector<EnumeratedOutput> outs;
  std::string err;
  std::vector<RawDisplayOutput> raws;
  if (EnumerateAllOutputs(&factory, &outs, &err)) {
    raws.reserve(outs.size());
    for (const auto& e : outs) raws.push_back(e.raw);
  }
  std::vector<uint64_t> gdi_order;
  EnumDisplayMonitors(nullptr, nullptr, CollectMonitorOrder,
                      reinterpret_cast<LPARAM>(&gdi_order));
  std::vector<DisplayInfo> table = BuildDisplayTable(raws, gdi_order);
  std::lock_guard<std::mutex> lk(g_disp_mu);
  g_displays = std::move(table);
}

// Orders the enumerated outputs to match the (already GDI-ordered) table:
// table[i] <- first unused output whose monitor_id matches (or geometry
// matches when the id is 0). Null entries = no live output (dropped).
std::vector<EnumeratedOutput*> OrderOutputsByTable(
    std::vector<EnumeratedOutput>* outs, const std::vector<DisplayInfo>& table) {
  std::vector<EnumeratedOutput*> ordered(table.size(), nullptr);
  std::vector<bool> used(outs->size(), false);
  for (size_t t = 0; t < table.size(); ++t) {
    for (size_t i = 0; i < outs->size(); ++i) {
      if (used[i]) continue;
      const RawDisplayOutput& r = (*outs)[i].raw;
      const DisplayInfo& d = table[t];
      const bool match = d.monitor_id != 0 ? r.monitor_id == d.monitor_id
                                           : (r.x == d.origin_x && r.y == d.origin_y &&
                                              r.w == d.w && r.h == d.h);
      if (match) {
        used[i] = true;
        ordered[t] = &(*outs)[i];
        break;
      }
    }
  }
  return ordered;
}
}  // namespace

std::vector<DisplayInfo> DxgiDisplaysSnapshot() {
  {
    std::lock_guard<std::mutex> lk(g_disp_mu);
    if (!g_displays.empty()) return g_displays;
  }
  RefreshGlobalTable();  // never enumerated (e.g. GDI-forced run)
  std::lock_guard<std::mutex> lk(g_disp_mu);
  return g_displays;
}

bool DxgiSelectDisplay(uint32_t idx) {
  {
    std::lock_guard<std::mutex> lk(g_disp_mu);
    if (!g_displays.empty() && idx < g_displays.size()) {
      g_desired = idx;
      return true;
    }
  }
  RefreshGlobalTable();
  std::lock_guard<std::mutex> lk(g_disp_mu);
  if (idx >= g_displays.size()) return false;
  g_desired = idx;
  return true;
}

DxgiCapture::DxgiCapture() : impl_(new Impl) {}
DxgiCapture::~DxgiCapture() { delete impl_; }

// Creates the staging texture for the current w_/h_ (CPU read, BGRA).
bool DxgiCapture::MakeStaging(std::string* err) {
  impl_->staging_[0].Reset();
  impl_->staging_[1].Reset();
  impl_->staging_cur_ = 0;
  impl_->have_staged_ = false;
  D3D11_TEXTURE2D_DESC td{};
  td.Width = w_;
  td.Height = h_;
  td.MipLevels = 1;
  td.ArraySize = 1;
  td.SampleDesc.Count = 1;
  td.Format = DXGI_FORMAT_B8G8R8A8_UNORM;
  td.Usage = D3D11_USAGE_STAGING;
  td.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
  for (int i = 0; i < 2; i++) {
    const HRESULT hr = impl_->dev->CreateTexture2D(&td, nullptr, &impl_->staging_[i]);
    if (FAILED(hr)) {
      SetHrErr(err, "CreateTexture2D(staging)", hr);
      return false;
    }
  }
  return true;
}

// gpu-readback: (re)creates the VideoProcessor pipeline for the current
// source dims (w_/h_ = un-rotated duplication dims, format B8G8R8A8) and the
// NV12 output + double-buffered NV12 staging for out_w_ x out_h_ (scaled +
// rotated). Rotation rides the same blt via
// ID3D11VideoContext1::VideoProcessorSetStreamRotation (checked against the
// enumerator's feature caps). Called with w_/h_/rot_/out_w_/out_h_ set.
// Returns false WITHOUT touching gpu_max_w_ on failure - the caller decides
// whether to degrade to the BGRA path.
bool DxgiCapture::MakeGpuPipeline(std::string* err) {
  impl_->vpe.Reset();
  impl_->vp.Reset();
  impl_->nv12_out_[0].Reset();
  impl_->nv12_out_[1].Reset();
  impl_->nv12_out_view_[0].Reset();
  impl_->nv12_out_view_[1].Reset();
  impl_->nv12_stage_[0].Reset();
  impl_->nv12_stage_[1].Reset();
  impl_->staging_cur_ = 0;
  impl_->have_staged_ = false;

  if (!impl_->vid_dev) {
    const HRESULT hq = impl_->dev.As(&impl_->vid_dev);
    if (FAILED(hq)) {
      SetHrErr(err, "QI(ID3D11VideoDevice)", hq);
      return false;
    }
  }
  if (!impl_->vid_ctx) {
    const HRESULT hq = impl_->ctx.As(&impl_->vid_ctx);
    if (FAILED(hq)) {
      SetHrErr(err, "QI(ID3D11VideoContext)", hq);
      return false;
    }
  }

  // Enumerator for the CURRENT duplication geometry (frame rate unknown:
  // 0/0 - the docs allow unknown rates). The input format is the desktop's
  // (BGRA per DuplicateOutput1); the output is NV12.
  D3D11_VIDEO_PROCESSOR_CONTENT_DESC vd{};
  vd.InputFrameFormat = D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE;
  vd.InputFrameRate = {0, 0};
  vd.InputWidth = w_;
  vd.InputHeight = h_;
  vd.OutputFrameRate = {0, 0};
  vd.OutputWidth = out_w_;
  vd.OutputHeight = out_h_;
  HRESULT hr = impl_->vid_dev->CreateVideoProcessorEnumerator(&vd, &impl_->vpe);
  if (FAILED(hr)) {
    SetHrErr(err, "CreateVideoProcessorEnumerator", hr);
    return false;
  }
  UINT in_sup = 0, out_sup = 0;
  impl_->vpe->CheckVideoProcessorFormat(DXGI_FORMAT_B8G8R8A8_UNORM, &in_sup);
  impl_->vpe->CheckVideoProcessorFormat(DXGI_FORMAT_NV12, &out_sup);
  if (!in_sup || !out_sup) {
    if (err)
      *err = "video processor does not support bgra->nv12 conversion";
    return false;
  }
  D3D11_VIDEO_PROCESSOR_CAPS caps{};
  impl_->vpe->GetVideoProcessorCaps(&caps);
  if (rot_ != Rotate::kNone &&
      (caps.FeatureCaps & D3D11_VIDEO_PROCESSOR_FEATURE_CAPS_ROTATION) == 0) {
    if (err) *err = "rotated output not supported by video processor";
    return false;
  }
  hr = impl_->vid_dev->CreateVideoProcessor(impl_->vpe.Get(), 0, &impl_->vp);
  if (FAILED(hr)) {
    SetHrErr(err, "CreateVideoProcessor", hr);
    return false;
  }
  if (rot_ != Rotate::kNone) {
    ComPtr<ID3D11VideoContext1> vc1;
    if (SUCCEEDED(impl_->vid_ctx.As(&vc1))) {
      // StreamIndex 0 = the single stream the blt feeds (scale+rotate in
      // the same pass: the enumerator's output dims already carry the swap).
      vc1->VideoProcessorSetStreamRotation(impl_->vp.Get(), 0, TRUE,
                                           static_cast<D3D11_VIDEO_PROCESSOR_ROTATION>(rot_));
    }
  }

  // NV12 blt destination (D3D11_USAGE_DEFAULT + RENDER_TARGET bind: the
  // VideoProcessorBlt contract), double-buffered - see Impl comment. Each
  // texture gets ONE persistent output view (VideoProcessorBlt takes a view,
  // not the texture).
  D3D11_TEXTURE2D_DESC od{};
  od.Width = out_w_;
  od.Height = out_h_;
  od.MipLevels = 1;
  od.ArraySize = 1;
  od.SampleDesc.Count = 1;
  od.Format = DXGI_FORMAT_NV12;
  od.Usage = D3D11_USAGE_DEFAULT;
  od.BindFlags = D3D11_BIND_RENDER_TARGET;
  D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC ovd{};
  ovd.ViewDimension = D3D11_VPOV_DIMENSION_TEXTURE2D;
  ovd.Texture2D.MipSlice = 0;
  for (int i = 0; i < 2; i++) {
    hr = impl_->dev->CreateTexture2D(&od, nullptr, &impl_->nv12_out_[i]);
    if (FAILED(hr)) {
      SetHrErr(err, "CreateTexture2D(nv12 out)", hr);
      return false;
    }
    hr = impl_->vid_dev->CreateVideoProcessorOutputView(
        impl_->nv12_out_[i].Get(), impl_->vpe.Get(), &ovd,
        &impl_->nv12_out_view_[i]);
    if (FAILED(hr)) {
      SetHrErr(err, "CreateVideoProcessorOutputView", hr);
      return false;
    }
  }

  // Color spaces: the input is the desktop's full-range RGB (BGRA); the
  // NV12 output is limited-range BT.601 like the CPU BgraToNv12 reference
  // (same YUV the software encoder path produced) - set explicitly so the
  // GPU and CPU paths stay pixel-comparable and blacks are not washed out.
  // NOTE: the d3d11.h D3D11_VIDEO_PROCESSOR_COLOR_SPACE field values are
  // documented but the named constants are NOT defined in the SDK headers
  // (docs-only); the numeric values below are the documented ones.
  D3D11_VIDEO_PROCESSOR_COLOR_SPACE in_cs{};
  in_cs.Usage = 1;  // D3D11_VIDEO_PROCESSOR_INPUT_COLOR_SPACE_RGB_FULL_RANGE
  impl_->vid_ctx->VideoProcessorSetStreamColorSpace(impl_->vp.Get(), 0, &in_cs);
  D3D11_VIDEO_PROCESSOR_COLOR_SPACE out_cs{};
  out_cs.Usage = 3;      // D3D11_VIDEO_PROCESSOR_OUTPUT_COLOR_SPACE_YCBCR_STUDIO_RANGE
  out_cs.RGB_Range = 1;  // D3D11_VIDEO_PROCESSOR_RGB_RANGE_FULL
  out_cs.YCbCr_Matrix = 0;  // D3D11_VIDEO_PROCESSOR_YCBCR_MATRIX_BT601
  out_cs.Nominal_Range = D3D11_VIDEO_PROCESSOR_NOMINAL_RANGE_16_235;
  impl_->vid_ctx->VideoProcessorSetOutputColorSpace(impl_->vp.Get(), &out_cs);
  // CPU-readable NV12 copies (two subresources per texture: Y + interleaved
  // UV, each with its own RowPitch on Map).
  D3D11_TEXTURE2D_DESC sd{};
  sd.Width = out_w_;
  sd.Height = out_h_;
  sd.MipLevels = 1;
  sd.ArraySize = 1;
  sd.SampleDesc.Count = 1;
  sd.Format = DXGI_FORMAT_NV12;
  sd.Usage = D3D11_USAGE_STAGING;
  sd.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
  for (int i = 0; i < 2; i++) {
    hr = impl_->dev->CreateTexture2D(&sd, nullptr, &impl_->nv12_stage_[i]);
    if (FAILED(hr)) {
      SetHrErr(err, "CreateTexture2D(nv12 staging)", hr);
      return false;
    }
  }
  XNC_LOG_INFO("dxgi gpu pipeline src=%ux%u rot=%u out=%ux%u", w_, h_,
               static_cast<unsigned>(rot_), out_w_, out_h_);
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
  const bool mode_changed = (d.ModeDesc.Width != w_ || d.ModeDesc.Height != h_);

  // gpu-readback: GPU path. Rotation is NOT rejected anymore - the
  // VideoProcessor applies it in the same blt as the scale+NV12 conversion
  // (ID3D11VideoContext1::VideoProcessorSetStreamRotation). out dims =
  // scaled+rotated (Width()/Height()/blob dims live in that space).
  if (gpu_max_w_ > 0) {
    Rotate rot = RotateFromDxgi(static_cast<int>(d.Rotation));
    uint32_t ow = 0, oh = 0;
    if (!GpuScaledDims(d.ModeDesc.Width, d.ModeDesc.Height, rot, gpu_max_w_,
                       &ow, &oh)) {
      if (err) *err = "gpu scaled dims rejected";
      return false;
    }
    if (mode_changed || rot != rot_ || ow != out_w_ || oh != out_h_) {
      w_ = d.ModeDesc.Width;
      h_ = d.ModeDesc.Height;
      rot_ = rot;
      out_w_ = ow;
      out_h_ = oh;
      std::string perr;
      if (!MakeGpuPipeline(&perr)) {
        // Graceful degradation (driver without the NV12 video-processor
        // conversion): fall back to the legacy full-BGRA path IN PLACE -
        // the pipeline's ScaledCapture wrapper then does the CPU downscale
        // and the encoder its BGRA->NV12, exactly like the pre-GPU build.
        XNC_LOG_ERROR("dxgi gpu pipeline failed, degrading to cpu bgra err=\"%s\"",
                      perr.c_str());
        gpu_max_w_ = 0;
        rot_ = Rotate::kNone;
        out_w_ = out_h_ = 0;
        if (d.Rotation != DXGI_MODE_ROTATION_IDENTITY) {
          if (err) *err = "rotated output not supported";
          return false;
        }
        if (!MakeStaging(err)) return false;
        XNC_LOG_INFO("dxgi mode changed (cpu path) w=%u h=%u", w_, h_);
      } else {
        XNC_LOG_INFO("dxgi gpu mode src=%ux%u rot=%u -> out=%ux%u",
                     w_, h_, static_cast<unsigned>(rot_), out_w_, out_h_);
      }
    }
    return true;
  }

  // Legacy CPU path: rotation is unsupported here (pre-gpu-readback
  // behavior).
  if (d.Rotation != DXGI_MODE_ROTATION_IDENTITY) {
    if (err) *err = "rotated output not supported";
    return false;
  }
  if (mode_changed) {
    w_ = d.ModeDesc.Width;
    h_ = d.ModeDesc.Height;
    if (!MakeStaging(err)) return false;
    XNC_LOG_INFO("dxgi mode changed w=%u h=%u", w_, h_);
  }
  return true;
}

bool DxgiCapture::Init(std::string* err) {
  impl_->dupl.Reset();
  impl_->staging_[0].Reset();
  impl_->staging_[1].Reset();
  impl_->staging_cur_ = 0;
  impl_->have_staged_ = false;
  impl_->vid_dev.Reset();
  impl_->vid_ctx.Reset();
  impl_->vpe.Reset();
  impl_->vp.Reset();
  impl_->nv12_out_[0].Reset();
  impl_->nv12_out_[1].Reset();
  impl_->nv12_out_view_[0].Reset();
  impl_->nv12_out_view_[1].Reset();
  impl_->nv12_stage_[0].Reset();
  impl_->nv12_stage_[1].Reset();
  impl_->out1.Reset();
  impl_->out5.Reset();
  impl_->ctx.Reset();
  impl_->dev.Reset();
  w_ = 0;
  h_ = 0;  // forces Reduplicate to (re)create staging for the new mode
  rot_ = Rotate::kNone;
  out_w_ = 0;
  out_h_ = 0;

  // 1. Enumerate ALL attached outputs across every adapter (M2-S3 Task 5),
  //    build the GDI-ordered stable table and refresh the process-global
  //    snapshot (HOST_HELLO displays[] reads it).
  ComPtr<IDXGIFactory1> factory;
  std::vector<EnumeratedOutput> outs;
  if (!EnumerateAllOutputs(&factory, &outs, err)) return false;
  std::vector<uint64_t> gdi_order;
  EnumDisplayMonitors(nullptr, nullptr, CollectMonitorOrder,
                      reinterpret_cast<LPARAM>(&gdi_order));
  std::vector<RawDisplayOutput> raws;
  raws.reserve(outs.size());
  for (const auto& e : outs) raws.push_back(e.raw);
  std::vector<DisplayInfo> table = BuildDisplayTable(raws, gdi_order);
  {
    std::lock_guard<std::mutex> lk(g_disp_mu);
    g_displays = table;
  }
  if (table.empty()) {
    if (err) *err = "no desktop-attached output";
    return false;
  }

  // 2. Resolve the desired index: kDisplaySelectAuto = primary, else 0 (the
  //    pre-Task-5 "first duplicable output" family); an explicit selection
  //    (MSG_SWITCH_DISPLAY -> DxgiSelectDisplay) binds at the NEXT rebuild.
  uint32_t want = 0;
  bool stale = false;
  {
    std::lock_guard<std::mutex> lk(g_disp_mu);
    want = ResolveDisplayIndex(g_desired, table, &stale);
    if (stale) g_desired = kDisplaySelectAuto;  // one-time fallback, then auto
  }
  if (stale) {
    XNC_LOG_INFO("dxgi stale display selection (table=%zu) -> primary fallback idx=%u",
                 table.size(), want);
  }
  std::vector<EnumeratedOutput*> ordered = OrderOutputsByTable(&outs, table);

  // 3. Hardware device on the TARGET output's adapter (VIDEO_SUPPORT for the
  //    M4 GPU encoder path, BGRA for the CPU path). WARP fallback covers
  //    drivers without D3D11 hardware support (default adapter).
  const EnumeratedOutput* target = ordered[want];
  if (target == nullptr) {
    if (err) *err = "selected display has no live output";
    return false;
  }
  D3D_FEATURE_LEVEL fl{};
  HRESULT hr = D3D11CreateDevice(
      target->adapter.Get(), D3D_DRIVER_TYPE_UNKNOWN, nullptr,
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

  // 4. Duplicate the selected output; on failure walk the remaining table
  //    entries whose output lives on the SAME adapter (a device cannot
  //    duplicate another adapter's output - the fallback stays within it).
  std::string last_err = "no desktop-attached output";
  for (uint32_t t = want; t < table.size(); ++t) {
    const EnumeratedOutput* e = ordered[t];
    if (e == nullptr || e->adapter.Get() != target->adapter.Get()) continue;
    ComPtr<IDXGIOutput1> o1;
    ComPtr<IDXGIOutput5> o5;
    e->output.As(&o1);  // always available on DXGI 1.2+
    e->output.As(&o5);  // Windows 10+; absence is fine (fallback below)
    impl_->out1 = o1;
    impl_->out5 = o5;

    if (Reduplicate(err)) {
      have_base_frame_ = false;
      XNC_LOG_INFO("dxgi duplication ready display=%u of %zu w=%u h=%u via=%s",
                   t, table.size(), w_, h_,
                   impl_->out5 ? "DuplicateOutput1" : "DuplicateOutput");
      return true;
    }
    last_err = err ? *err : "duplicate failed";
    XNC_LOG_INFO("dxgi duplicate failed on display %u err=\"%s\"", t, last_err.c_str());
    impl_->dupl.Reset();
    impl_->out1.Reset();
    impl_->out5.Reset();
  }
  if (err) *err = last_err;
  return false;
}

// gpu-readback: Init + the GPU-scale/NV12 pipeline (see dxgi_capture.h).
// max_w == 0 behaves exactly like Init (legacy full-BGRA path).
bool DxgiCapture::InitGpuScale(uint32_t max_w, std::string* err) {
  gpu_max_w_ = max_w;
  if (!Init(err)) {
    gpu_max_w_ = 0;
    return false;
  }
  if (max_w > 0 && gpu_max_w_ == 0) {
    XNC_LOG_ERROR("dxgi gpu scale degraded to cpu bgra (see prior error line)");
  }
  return true;
}

// Returns false always (retryable or fatal - see capture.h err contract);
// shapes *err accordingly.
// M2-Slice1 Task 2: a refused rebuild is NO LONGER fatal. T1 evidence
// (XIAOXIN): while the secure desktop is up, Reduplicate AND Init are
// denied 0x80070005 even as SYSTEM - the error becomes "err_access_lost"
// and the pipeline's unified CaptureReset owns waiting + retrying.
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
  XNC_LOG_ERROR("dxgi rebuild refused streak=%u err=\"%s\" (routing to unified reset)",
                consecutive_rebuild_failures_, rerr.c_str());
  if (err) *err = "err_access_lost";  // unified CaptureReset takes over
  return false;
}

bool DxgiCapture::Acquire(FrameBlob& blob, std::string* err, uint32_t timeout_ms) {
  if (!impl_->dupl) {
    // M2-S1 T2: no duplication = access lost. The old "err_not_initialized"
    // fatal artifact (the T1 probe's child-killer) is gone; re-creation is
    // the unified reset's job.
    if (err) *err = "err_access_lost";
    return false;
  }
  DXGI_OUTDUPL_FRAME_INFO info{};
  ComPtr<IDXGIResource> res;
  const UINT wait_ms = timeout_ms != 0 ? timeout_ms : kAcquireTimeoutMs;
  HRESULT hr = impl_->dupl->AcquireNextFrame(wait_ms, &info, &res);
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

  // gpu-readback GPU path: scale + NV12 convert (+ rotate) in ONE
  // VideoProcessorBlt into the persistent NV12 output, copy to the NV12
  // staging buffer, then read back only the small NV12 frame. Double
  // buffering: the blt/copy for frame N+1 overlaps the CPU readback of
  // frame N (same pattern as the BGRA staging below - Map on the PREVIOUS
  // buffer, which the GPU finished filling during this iteration).
  if (gpu_max_w_ > 0) {
    const uint64_t gpu_t0 = NowMonoUs();
    const uint32_t cur = impl_->staging_cur_;
    const uint32_t read_buf = impl_->have_staged_ ? (cur ^ 1) : cur;
    // Per-frame input view over the acquired desktop texture (the view is
    // tied to the texture, which changes every frame; the output view is
    // persistent, bound to the double-buffered NV12 output texture).
    D3D11_VIDEO_PROCESSOR_INPUT_VIEW_DESC ivd{};
    ivd.FourCC = 0;  // 0 = use the texture's format (B8G8R8A8, per DuplicateOutput1)
    ivd.ViewDimension = D3D11_VPIV_DIMENSION_TEXTURE2D;
    ivd.Texture2D.MipSlice = 0;
    ivd.Texture2D.ArraySlice = 0;
    ComPtr<ID3D11VideoProcessorInputView> in_view;
    hr = impl_->vid_dev->CreateVideoProcessorInputView(tex.Get(), impl_->vpe.Get(),
                                                       &ivd, in_view.GetAddressOf());
    tex.Reset();  // in_view keeps the texture alive through the blt
    if (FAILED(hr)) {
      impl_->dupl->ReleaseFrame();
      SetHrErr(err, "CreateVideoProcessorInputView", hr);
      XNC_LOG_ERROR("dxgi gpu input view failed hr=0x%08lX",
                    static_cast<unsigned long>(hr));
      return false;
    }
    D3D11_VIDEO_PROCESSOR_STREAM st{};
    st.Enable = TRUE;
    st.pInputSurface = in_view.Get();
    // No output rect: the blt targets the full output view (= the NV12
    // texture at out_w_ x out_h_, set by the enumerator's output dims).
    hr = impl_->vid_ctx->VideoProcessorBlt(impl_->vp.Get(),
                                           impl_->nv12_out_view_[cur].Get(),
                                           0, 1, &st);
    in_view.Reset();
    if (FAILED(hr)) {
      impl_->dupl->ReleaseFrame();
      SetHrErr(err, "VideoProcessorBlt", hr);
      XNC_LOG_ERROR("dxgi gpu blt failed hr=0x%08lX",
                    static_cast<unsigned long>(hr));
      return false;
    }
    impl_->ctx->CopyResource(impl_->nv12_stage_[cur].Get(),
                             impl_->nv12_out_[cur].Get());
    impl_->dupl->ReleaseFrame();

    const size_t need = Nv12Bytes(out_w_, out_h_);
    if (need == 0) {
      if (err) *err = "err_bad_dims";
      return false;
    }
    // D3D11 quirk: an NV12 staging texture is ONE subresource (Y rows then
    // UV rows, same RowPitch - see CompactNv12Rows); Map(subresource 1)
    // fails E_INVALIDARG on the Intel driver (verified 2026-08-25).
    D3D11_MAPPED_SUBRESOURCE ym{};
    hr = impl_->ctx->Map(impl_->nv12_stage_[read_buf].Get(), 0,
                         D3D11_MAP_READ, 0, &ym);
    if (FAILED(hr)) {
      SetHrErr(err, "Map(nv12 staging)", hr);
      XNC_LOG_ERROR("dxgi gpu map failed hr=0x%08lX",
                    static_cast<unsigned long>(hr));
      return false;
    }
    blob.bgra.resize(need);
    CompactNv12Rows(static_cast<const uint8_t*>(ym.pData), ym.RowPitch,
                    blob.bgra.data(), out_w_, out_h_);
    impl_->ctx->Unmap(impl_->nv12_stage_[read_buf].Get(), 0);
    impl_->staging_cur_ = cur ^ 1;
    impl_->have_staged_ = true;

    blob.w = out_w_;
    blob.h = out_h_;
    blob.pixfmt = Pixfmt::kNv12;
    blob.mono_us = NowMonoUs();
    blob.gpu_scale_us = NowMonoUs() - gpu_t0;
    if (!have_base_frame_) {
      have_base_frame_ = true;  // this full readback IS the base frame
      XNC_LOG_INFO("dxgi base frame (gpu nv12) w=%u h=%u mono_us=%llu",
                   out_w_, out_h_, static_cast<unsigned long long>(blob.mono_us));
    }
    return true;
  }

  // Full-frame copy into our persistent staging texture, then hand the
  // desktop image straight back - never held across iterations. The copy
  // lands in staging_[staging_cur_]; the CPU reads the OTHER buffer, which
  // the GPU finished copying during the previous iteration (double buffering
  // keeps Map from blocking on the just-issued CopyResource).
  const uint32_t read_buf =
      impl_->have_staged_ ? (impl_->staging_cur_ ^ 1) : impl_->staging_cur_;
  impl_->ctx->CopyResource(impl_->staging_[impl_->staging_cur_].Get(), tex.Get());
  tex.Reset();
  impl_->dupl->ReleaseFrame();

  D3D11_MAPPED_SUBRESOURCE mapped{};
  hr = impl_->ctx->Map(impl_->staging_[read_buf].Get(), 0, D3D11_MAP_READ, 0,
                       &mapped);
  if (FAILED(hr)) {
    SetHrErr(err, "Map(staging)", hr);
    return false;
  }
  const size_t need = BgraBytes(w_, h_);
  if (need == 0) {
    impl_->ctx->Unmap(impl_->staging_[read_buf].Get(), 0);
    if (err) *err = "err_bad_dims";
    return false;
  }
  blob.bgra.resize(need);
  CompactBgraRows(static_cast<const uint8_t*>(mapped.pData), mapped.RowPitch,
                  blob.bgra.data(), w_, h_);
  impl_->ctx->Unmap(impl_->staging_[read_buf].Get(), 0);
  impl_->staging_cur_ = impl_->staging_cur_ ^ 1;
  impl_->have_staged_ = true;

  blob.w = w_;
  blob.h = h_;
  blob.pixfmt = Pixfmt::kBgra;  // explicit: blobs are reused across acquires
  blob.gpu_scale_us = 0;
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

std::unique_ptr<ICapture> TryCreateDxgiCaptureGpu(uint32_t max_w, std::string* err) {
  auto cap = std::make_unique<DxgiCapture>();
  std::string ierr;
  if (!cap->InitGpuScale(max_w, &ierr)) {
    if (err) *err = ierr;
    XNC_LOG_ERROR("dxgi gpu init failed err=\"%s\"", ierr.c_str());
    return nullptr;
  }
  return cap;
}

}  // namespace xnc
