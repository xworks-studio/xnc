// gpu_surface.h - M2 Task 1: owned GPU surfaces and their lease state
// machine. Two deliberately separated layers (plan ruling 1):
//   1. SurfaceLeaseModel - the PURE lease state machine over a fixed slot
//      count: FREE -> CONVERTING -> SUBMITTED -> RETIRED -> FREE. No D3D
//      types, no locking, header-testable standalone (media_types.h
//      header-only pattern).
//   2. LatestSurface / Nv12SurfacePool / SurfaceLease - the D3D11 wrappers.
//      Pimpl + forward-declared interface names keep d3d11.h out of this
//      header entirely (dxgi_capture.h pattern), so including this file
//      never requires a D3D SDK header; the model section stays usable
//      without any D3D device at all.
//
// Single-thread contract (plan ruling 2): EVERY method below is called from
// ONE thread in production - the media GPU thread. Under MediaPipelineV2
// (M2 Task 4) that thread is the single media loop which ALSO drives
// ICaptureSurface::AcquireSurface (capture + copy + convert + submit all
// run there - see capture.h's threading note); "capture thread" and "media
// GPU thread" are one and the same. The M0 pipeline's two-thread use of
// ICapture/FrameBlob (a capture thread plus a separate encode thread) is
// the legacy shape and never touches these objects. Members are plain and
// unsynchronized; concurrent use from more than one thread is undefined.
//
// No GPU->CPU readback anywhere in these wrappers (global constraint): the
// surfaces stay on the GPU; later tasks hand them to the VideoProcessor and
// the encoder MFT without mapping.
#ifndef XNC_NATIVE_DESKTOP_GPU_SURFACE_H_
#define XNC_NATIVE_DESKTOP_GPU_SURFACE_H_

#include <cstddef>
#include <cstdint>
#include <string>
#include <vector>

#include "media_types.h"  // FrameIdentity (the CopyFrom stamp)

// Forward declarations ONLY, at GLOBAL scope where the real D3D interfaces
// live (d3d11.h declares them there) - keeps this header free of any D3D
// SDK include while the .cpp's <d3d11.h> definitions bind to these names.
struct ID3D11Device;
struct ID3D11DeviceContext;
struct ID3D11Texture2D;

namespace xnc {

// ---- 1. Pure lease state machine ----
//
// One slot's lifecycle:
//   FREE       - acquirable.
//   CONVERTING - a lease holder (the GPU thread) owns the slot for an
//                in-flight BGRA->NV12 conversion.
//   SUBMITTED  - the slot's texture was handed to the encoder MFT under a
//                submission id; only Complete(id) or RetireAll move it on.
//   RETIRED    - the encoder output for the id was observed (Complete) or a
//                bulk retire happened (RetireAll); awaiting the sweep that
//                returns it to FREE.
//
// Complete() parks slots in RETIRED; the NEXT Acquire() sweeps retired slots
// back to FREE before allocating (so "Complete(id) && Acquire()" holds - the
// same-thread immediate-context ordering makes that retire fence trivially
// satisfied by then). FreeRetired() is the explicit sweep.
enum class SurfaceLeaseState : uint8_t {
  kFree = 0,
  kConverting,
  kSubmitted,
  kRetired,
};

class SurfaceLeaseModel {
 public:
  // Handle to one acquired slot; valid only while the model lives and the
  // holder has not given the slot up (ReleaseFree/Complete/RetireAll). A
  // default-constructed Lease is the null handle (operator bool == false).
  class Lease {
   public:
    // CONVERTING -> SUBMITTED under `submit_id` (the submission token later
    // Complete() looks up; unique among SUBMITTED slots). False in any other
    // state or for a duplicate live id.
    bool Submit(uint64_t submit_id) {
      return model_ != nullptr && model_->SubmitSlot(index_, submit_id);
    }
    size_t index() const { return index_; }
    SurfaceLeaseState state() const {
      return model_ != nullptr ? model_->State(index_)
                               : SurfaceLeaseState::kFree;
    }
    explicit operator bool() const { return model_ != nullptr; }

   private:
    friend class SurfaceLeaseModel;
    Lease() = default;
    Lease(SurfaceLeaseModel* model, size_t index) : model_(model), index_(index) {}
    SurfaceLeaseModel* model_ = nullptr;
    size_t index_ = 0;
  };

  // A pool of `slot_count` slots, all FREE. Must be >= 1 to be useful
  // (0 is a valid degenerate pool that never yields a lease).
  explicit SurfaceLeaseModel(size_t slot_count);

  // Non-copyable/non-movable on purpose: Lease handles point back into the
  // model's slot storage, so moving/replacing the storage would dangle them.
  SurfaceLeaseModel(const SurfaceLeaseModel&) = delete;
  SurfaceLeaseModel& operator=(const SurfaceLeaseModel&) = delete;

  // Sweeps retired slots to FREE, then FREE -> CONVERTING on the first free
  // slot. Returns null when no slot is acquirable (pool exhausted).
  Lease* Acquire();

  // CONVERTING -> FREE (abandon a conversion that will not be submitted).
  // False in any other state - a SUBMITTED slot is not reusable directly
  // (the encoder owns it until Complete/RetireAll).
  bool ReleaseFree(size_t index);

  // SUBMITTED with `submit_id` -> RETIRED. False when no slot is SUBMITTED
  // under that id (unknown/double-complete).
  bool Complete(uint64_t submit_id);

  // Bulk flush/teardown: every CONVERTING/SUBMITTED slot -> RETIRED.
  // Returns how many slots were retired (0 = nothing outstanding).
  size_t RetireAll();

  // RETIRED -> FREE sweep (the explicit half of the retire path; Acquire
  // performs the same sweep first). Returns how many slots were freed.
  size_t FreeRetired();

  // Live leases: every slot not FREE (CONVERTING + SUBMITTED + RETIRED).
  // Teardown asserts 0 after RetireAll()+FreeRetired().
  size_t LiveLeases() const;

  size_t FreeCount() const;
  size_t RetiredCount() const;
  size_t slot_count() const { return slots_.size(); }

  // Out-of-range indexes read as FREE (never crash a diag).
  SurfaceLeaseState State(size_t index) const;

  // Lease::Submit implementation (public so the D3D SurfaceLease wrapper can
  // reach it by index).
  bool SubmitSlot(size_t index, uint64_t submit_id);

 private:
  friend class Lease;
  struct Slot {
    SurfaceLeaseState state = SurfaceLeaseState::kFree;
    uint64_t submit_id = 0;  // meaningful only while SUBMITTED/RETIRED
    Lease lease;             // points back here via (model, index)
  };
  void FreeSlot(size_t index);
  std::vector<Slot> slots_;
};

// ---- 2. D3D11 wrappers (pimpl; single media-GPU thread) ----
//
// The owned persistent BGRA texture that always holds the latest COMPLETE
// desktop image. CopyFrom stamps the FrameIdentity of the copy; Snapshot
// hands out an AddRef'd view of that exact image + identity so a snapshot is
// never torn by a later capture (the copy is atomic on the immediate
// context). No readback: consumers use it as a GPU-side source only.
class LatestSurface {
 public:
  LatestSurface();
  ~LatestSurface();
  LatestSurface(const LatestSurface&) = delete;
  LatestSurface& operator=(const LatestSurface&) = delete;

  // (Re)creates the owned w*h BGRA texture and drops any prior content:
  // Snapshot fails until the next CopyFrom completes. Re-Init on a resize
  // keeps the object identity (slots/wrap-up in later tasks key on it).
  bool Init(ID3D11Device* dev, uint32_t w, uint32_t h, std::string* err);

  // CopyResource src -> the owned texture + stamps `id` as the current
  // identity. `src` must match Init's dimensions, format B8G8R8A8, 1 mip,
  // 1 array slice, 1 sample (CopyResource requires identical descs).
  bool CopyFrom(ID3D11DeviceContext* ctx, ID3D11Texture2D* src,
                const FrameIdentity& id, std::string* err);

  // AddRef'd pointer to the owned texture + the FrameIdentity stamped by the
  // most recent CopyFrom. False after Invalidate()/Reset()/Init() until a
  // CopyFrom completes (or for a null out pointer). Caller owns one
  // reference and must Release() it; the surface keeps its own.
  bool Snapshot(FrameIdentity* id_out, ID3D11Texture2D** tex_out);

  // Marks the content unusable (capture access lost / unified reset): the
  // texture stays allocated (persistent) but Snapshot fails until the next
  // CopyFrom. Also clears the stale identity.
  void Invalidate();

  // FULL teardown of the surface's device pairing (backend swap / unified
  // reset - final-review fix 2026-08): drops the owned texture AND the
  // recorded dims, not just the validity; width()/height() report 0 until
  // the next Init, so the FIRST AcquireSurface of whatever backend now
  // serves re-Inits the surface on ITS device. Invalidate cannot express
  // that: with EQUAL dims (the DXGI->GDI fall on a duplicated primary) the
  // next backend would keep the old device's texture and its CopyFrom
  // would pair two D3D devices - API-invalid, silently undefined in retail
  // (the ever-"healthy" frozen-desktop failure class). Object identity is
  // preserved: the pipeline's single LatestSurface instance survives every
  // reset; only its texture/dims/identity die.
  void Reset();

  bool valid() const { return valid_; }
  uint32_t width() const { return w_; }
  uint32_t height() const { return h_; }

 private:
  struct Impl;  // ComPtr<ID3D11Texture2D> (d3d11.h stays in the .cpp)
  Impl* impl_;
  FrameIdentity last_id_{};
  uint32_t w_ = 0, h_ = 0;
  bool valid_ = false;
};

// One acquired pool slot: the NV12 texture plus its lease into the model.
// A SurfaceLease is valid only while held: after Release()/pool Complete/
// RetireAll the slot may be re-acquired by someone else - do not use a
// stale handle (production is single-threaded and hands the lease straight
// from Acquire to Submit/Release).
class SurfaceLease {
 public:
  SurfaceLease(const SurfaceLease&) = delete;
  SurfaceLease& operator=(const SurfaceLease&) = delete;

  // CONVERTING -> SUBMITTED under `submit_id` (the submission token the
  // later encoder-output task completes by). False in any other state or
  // for a duplicate live id.
  bool Submit(uint64_t submit_id);

  // CONVERTING -> FREE (abandon; the texture stays pool-owned). False for a
  // SUBMITTED slot - the encoder owns it until the pool-level Complete or
  // RetireAll (the "submitted is not reusable" rule).
  bool Release();

  // The slot's NV12 texture; pool-owned, NOT AddRef'd (the lease lifetime
  // covers its use). Null only for an unacquired handle.
  ID3D11Texture2D* texture() const;

  size_t index() const;
  SurfaceLeaseState state() const;
  explicit operator bool() const { return lease_ != nullptr; }

 private:
  friend class Nv12SurfacePool;
  SurfaceLease() = default;
  Nv12SurfacePool* pool_ = nullptr;
  SurfaceLeaseModel::Lease* lease_ = nullptr;
};

// The three-slot NV12 pool. Every slot is one DEFAULT-usage NV12 texture
// (VideoProcessorBlt destination contract, matching dxgi_capture.cpp); the
// state rules live entirely in the embedded SurfaceLeaseModel. Slots flow
// FREE -> CONVERTING (Acquire) -> SUBMITTED (SurfaceLease::Submit) ->
// RETIRED (Complete/RetireAll) -> FREE (FreeRetired / Acquire's sweep).
class Nv12SurfacePool {
 public:
  static constexpr size_t kSlotCount = 3;

  Nv12SurfacePool();
  ~Nv12SurfacePool();
  Nv12SurfacePool(const Nv12SurfacePool&) = delete;
  Nv12SurfacePool& operator=(const Nv12SurfacePool&) = delete;

  // (Re)creates the slot textures for w*h (NV12 requires even dims) and
  // retire-sweeps any outstanding leases back to FREE.
  bool Init(ID3D11Device* dev, uint32_t w, uint32_t h, std::string* err);

  // FREE -> CONVERTING; returns the slot's SurfaceLease or null when the
  // pool is exhausted (three outstanding leases). Sweeps retired slots
  // first (see SurfaceLeaseModel::Acquire).
  SurfaceLease* Acquire();

  // SUBMITTED with `submit_id` -> RETIRED (the encoder output for that
  // submission was observed; later task calls this from the output drain).
  bool Complete(uint64_t submit_id);

  // Bulk flush/teardown retire (encoder reconfigure/reset): every
  // CONVERTING/SUBMITTED slot -> RETIRED. Returns how many were retired.
  size_t RetireAll();

  // Explicit RETIRED -> FREE sweep. Returns how many were freed.
  size_t FreeRetired();

  // Teardown assertion helpers (ruling 3: zero live leases after teardown).
  size_t LiveLeases() const { return model_.LiveLeases(); }
  size_t FreeCount() const { return model_.FreeCount(); }
  SurfaceLeaseState State(size_t index) const { return model_.State(index); }

  uint32_t width() const { return w_; }
  uint32_t height() const { return h_; }

 private:
  friend class SurfaceLease;
  struct Impl;  // ComPtr<ID3D11Texture2D>[kSlotCount]
  Impl* impl_;
  SurfaceLeaseModel model_;
  SurfaceLease leases_[kSlotCount];
  uint32_t w_ = 0, h_ = 0;
};

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_GPU_SURFACE_H_
