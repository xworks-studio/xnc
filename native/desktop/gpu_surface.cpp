// gpu_surface.cpp - M2 Task 1: implementation of the D3D11 wrappers declared
// in gpu_surface.h (the pure SurfaceLeaseModel is header-only by design -
// plan ruling 1 - and compiled here only because this TU includes the
// header). Everything runs on the single media GPU thread (ruling 2): no
// internal locking, and the retire -> free sweep in Acquire relies on
// same-thread immediate-context ordering. No Map()/readback anywhere: GPU
// surfaces stay GPU-side (global constraint).
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>

#include <d3d11.h>
#include <wrl/client.h>

#include <cstdio>

#include "gpu_surface.h"

namespace xnc {

using Microsoft::WRL::ComPtr;

namespace {

// "<step>: hr=0x%08lX" - the dxgi_capture.cpp error-string convention.
void SetHrErr(std::string* err, const char* step, HRESULT hr) {
  char buf[160];
  _snprintf_s(buf, sizeof(buf), _TRUNCATE, "%s: hr=0x%08lX", step,
              static_cast<unsigned long>(hr));
  if (err) *err = buf;
}

void SetMsgErr(std::string* err, const char* msg) {
  if (err) *err = msg;
}

}  // namespace

// ---- 1. Pure lease state machine ----

SurfaceLeaseModel::SurfaceLeaseModel(size_t slot_count) : slots_(slot_count) {
  for (size_t i = 0; i < slots_.size(); ++i)
    slots_[i].lease = Lease(this, i);
}

SurfaceLeaseModel::Lease* SurfaceLeaseModel::Acquire() {
  FreeRetired();  // the retire sweep (see header): Complete()+Acquire() holds
  for (auto& s : slots_) {
    if (s.state == SurfaceLeaseState::kFree) {
      s.state = SurfaceLeaseState::kConverting;
      s.submit_id = 0;
      return &s.lease;
    }
  }
  return nullptr;
}

bool SurfaceLeaseModel::ReleaseFree(size_t index) {
  if (index >= slots_.size()) return false;
  if (slots_[index].state != SurfaceLeaseState::kConverting) return false;
  FreeSlot(index);
  return true;
}

bool SurfaceLeaseModel::Complete(uint64_t submit_id) {
  for (auto& s : slots_) {
    if (s.state == SurfaceLeaseState::kSubmitted && s.submit_id == submit_id) {
      s.state = SurfaceLeaseState::kRetired;
      return true;
    }
  }
  return false;
}

size_t SurfaceLeaseModel::RetireAll() {
  size_t retired = 0;
  for (auto& s : slots_) {
    if (s.state == SurfaceLeaseState::kConverting ||
        s.state == SurfaceLeaseState::kSubmitted) {
      s.state = SurfaceLeaseState::kRetired;
      ++retired;
    }
  }
  return retired;
}

size_t SurfaceLeaseModel::FreeRetired() {
  size_t freed = 0;
  for (size_t i = 0; i < slots_.size(); ++i) {
    if (slots_[i].state == SurfaceLeaseState::kRetired) {
      FreeSlot(i);
      ++freed;
    }
  }
  return freed;
}

size_t SurfaceLeaseModel::LiveLeases() const {
  size_t live = 0;
  for (const auto& s : slots_)
    if (s.state != SurfaceLeaseState::kFree) ++live;
  return live;
}

size_t SurfaceLeaseModel::FreeCount() const {
  size_t free = 0;
  for (const auto& s : slots_)
    if (s.state == SurfaceLeaseState::kFree) ++free;
  return free;
}

size_t SurfaceLeaseModel::RetiredCount() const {
  size_t retired = 0;
  for (const auto& s : slots_)
    if (s.state == SurfaceLeaseState::kRetired) ++retired;
  return retired;
}

SurfaceLeaseState SurfaceLeaseModel::State(size_t index) const {
  if (index >= slots_.size()) return SurfaceLeaseState::kFree;
  return slots_[index].state;
}

bool SurfaceLeaseModel::SubmitSlot(size_t index, uint64_t submit_id) {
  if (index >= slots_.size()) return false;
  if (slots_[index].state != SurfaceLeaseState::kConverting) return false;
  // Submission ids are the encoder-pairing tokens Complete() looks up; a
  // duplicate among live SUBMITTED slots would make Complete ambiguous.
  for (const auto& s : slots_)
    if (s.state == SurfaceLeaseState::kSubmitted && s.submit_id == submit_id)
      return false;
  slots_[index].state = SurfaceLeaseState::kSubmitted;
  slots_[index].submit_id = submit_id;
  return true;
}

void SurfaceLeaseModel::FreeSlot(size_t index) {
  slots_[index].state = SurfaceLeaseState::kFree;
  slots_[index].submit_id = 0;
}

// ---- 2. D3D11 wrappers ----

struct LatestSurface::Impl {
  ComPtr<ID3D11Texture2D> tex;
};

struct Nv12SurfacePool::Impl {
  ComPtr<ID3D11Texture2D> tex[Nv12SurfacePool::kSlotCount];
};

LatestSurface::LatestSurface() : impl_(new Impl) {}
LatestSurface::~LatestSurface() { delete impl_; }

bool LatestSurface::Init(ID3D11Device* dev, uint32_t w, uint32_t h,
                         std::string* err) {
  impl_->tex.Reset();
  valid_ = false;
  last_id_ = FrameIdentity{};
  w_ = 0;
  h_ = 0;
  if (dev == nullptr || w == 0 || h == 0) {
    SetMsgErr(err, "latest init: bad args");
    return false;
  }
  // Owned persistent desktop copy: DEFAULT usage (GPU-only, no CPU access),
  // no bind flags needed - it is a CopyResource destination and, in the
  // later GPU-convert task, a VideoProcessor input (input views need no
  // bind flag on DEFAULT textures; dxgi_capture.cpp does the same on the
  // duplication textures themselves).
  D3D11_TEXTURE2D_DESC td{};
  td.Width = w;
  td.Height = h;
  td.MipLevels = 1;
  td.ArraySize = 1;
  td.SampleDesc.Count = 1;
  td.Format = DXGI_FORMAT_B8G8R8A8_UNORM;
  td.Usage = D3D11_USAGE_DEFAULT;
  const HRESULT hr = dev->CreateTexture2D(&td, nullptr, &impl_->tex);
  if (FAILED(hr)) {
    SetHrErr(err, "CreateTexture2D(latest bgra)", hr);
    return false;
  }
  w_ = w;
  h_ = h;
  return true;
}

bool LatestSurface::CopyFrom(ID3D11DeviceContext* ctx, ID3D11Texture2D* src,
                             const FrameIdentity& id, std::string* err) {
  if (!impl_->tex || ctx == nullptr || src == nullptr) {
    SetMsgErr(err, "latest copy: not initialized");
    return false;
  }
  // CopyResource demands identical descs; reject mismatches explicitly so a
  // bad caller fails here instead of tripping the D3D runtime (and the
  // debug layer in the selftest).
  D3D11_TEXTURE2D_DESC sd{};
  src->GetDesc(&sd);
  if (sd.Width != w_ || sd.Height != h_ ||
      sd.Format != DXGI_FORMAT_B8G8R8A8_UNORM || sd.MipLevels != 1 ||
      sd.ArraySize != 1 || sd.SampleDesc.Count != 1) {
    char buf[128];
    _snprintf_s(buf, sizeof(buf), _TRUNCATE,
                "latest copy: desc mismatch src=%ux%u fmt=%u mips=%u arr=%u "
                "samples=%u",
                sd.Width, sd.Height, static_cast<unsigned>(sd.Format),
                sd.MipLevels, sd.ArraySize, sd.SampleDesc.Count);
    if (err) *err = buf;
    return false;
  }
  ctx->CopyResource(impl_->tex.Get(), src);
  last_id_ = id;
  valid_ = true;
  return true;
}

bool LatestSurface::Snapshot(FrameIdentity* id_out, ID3D11Texture2D** tex_out) {
  if (tex_out == nullptr || !valid_ || !impl_->tex) return false;
  impl_->tex->AddRef();  // caller-owned reference; the surface keeps its own
  *tex_out = impl_->tex.Get();
  if (id_out != nullptr) *id_out = last_id_;
  return true;
}

void LatestSurface::Invalidate() {
  valid_ = false;
  last_id_ = FrameIdentity{};
}

// Final-review fix 2026-08: the full-drop counterpart of Invalidate (see
// gpu_surface.h) - Init's teardown half, so the next backend Init re-binds
// the surface to ITS device even at identical dims.
void LatestSurface::Reset() {
  impl_->tex.Reset();
  valid_ = false;
  last_id_ = FrameIdentity{};
  w_ = 0;
  h_ = 0;
}

// ---- Nv12SurfacePool / SurfaceLease ----

Nv12SurfacePool::Nv12SurfacePool()
    : impl_(new Impl), model_(kSlotCount), leases_{} {}

Nv12SurfacePool::~Nv12SurfacePool() { delete impl_; }

bool Nv12SurfacePool::Init(ID3D11Device* dev, uint32_t w, uint32_t h,
                           std::string* err) {
  if (dev == nullptr || w == 0 || h == 0) {
    SetMsgErr(err, "pool init: bad args");
    return false;
  }
  // NV12 planes subsample chroma 2x2: odd dims are not a valid NV12 texture.
  if ((w % 2) != 0 || (h % 2) != 0) {
    SetMsgErr(err, "pool init: nv12 requires even dims");
    return false;
  }
  // Same shape as dxgi_capture.cpp's VideoProcessorBlt destinations: NV12 +
  // DEFAULT + RENDER_TARGET (the VP output-view contract the later
  // BGRA->NV12 convert task feeds). The encoder MFT input side needs no
  // extra flags.
  D3D11_TEXTURE2D_DESC td{};
  td.Width = w;
  td.Height = h;
  td.MipLevels = 1;
  td.ArraySize = 1;
  td.SampleDesc.Count = 1;
  td.Format = DXGI_FORMAT_NV12;
  td.Usage = D3D11_USAGE_DEFAULT;
  td.BindFlags = D3D11_BIND_RENDER_TARGET;
  for (size_t i = 0; i < kSlotCount; ++i) {
    impl_->tex[i].Reset();
    const HRESULT hr = dev->CreateTexture2D(&td, nullptr, &impl_->tex[i]);
    if (FAILED(hr)) {
      SetHrErr(err, "CreateTexture2D(pool nv12)", hr);
      return false;
    }
  }
  // Any leases outstanding from a previous Init generation go through the
  // retire path; their (freed) handles must never be used again.
  RetireAll();
  FreeRetired();
  w_ = w;
  h_ = h;
  return true;
}

SurfaceLease* Nv12SurfacePool::Acquire() {
  SurfaceLeaseModel::Lease* l = model_.Acquire();
  if (l == nullptr) return nullptr;
  SurfaceLease& s = leases_[l->index()];
  s.pool_ = this;
  s.lease_ = l;
  return &s;
}

bool Nv12SurfacePool::Complete(uint64_t submit_id) {
  return model_.Complete(submit_id);
}

size_t Nv12SurfacePool::RetireAll() { return model_.RetireAll(); }

size_t Nv12SurfacePool::FreeRetired() { return model_.FreeRetired(); }

bool SurfaceLease::Submit(uint64_t submit_id) {
  if (pool_ == nullptr || lease_ == nullptr) return false;
  return pool_->model_.SubmitSlot(lease_->index(), submit_id);
}

bool SurfaceLease::Release() {
  if (pool_ == nullptr || lease_ == nullptr) return false;
  return pool_->model_.ReleaseFree(lease_->index());
}

ID3D11Texture2D* SurfaceLease::texture() const {
  if (pool_ == nullptr || lease_ == nullptr || pool_->impl_ == nullptr)
    return nullptr;
  return pool_->impl_->tex[lease_->index()].Get();
}

size_t SurfaceLease::index() const { return lease_ != nullptr ? lease_->index() : 0; }

SurfaceLeaseState SurfaceLease::state() const {
  if (pool_ == nullptr || lease_ == nullptr) return SurfaceLeaseState::kFree;
  return pool_->model_.State(lease_->index());
}

}  // namespace xnc
