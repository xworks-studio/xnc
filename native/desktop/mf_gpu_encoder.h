// mf_gpu_encoder.h - M2 Task 3: D3D11-aware encoder SESSIONS over the M2
// Task 1 NV12 pool. Two implementations of one interface:
//
//   MfGpuEncoder - the D3D11-aware hardware-MFT session (the 0xC00D6D77
//     fix). Hardware encoder MFTs (Intel QSV etc.) are ASYNC MFTs locked
//     until MF_TRANSFORM_ASYNC_UNLOCK is set on the MFT's own attribute
//     store (measured: SetInputType/SET_D3D_MANAGER/ProcessInput all
//     return MF_E_TRANSFORM_ASYNC_LOCKED == 0xC00D6D77 until unlocked -
//     that error, previously attributed to "CPU input not supported", is
//     the async lock). The session then drives the documented event
//     protocol (METransformNeedInput gates ProcessInput,
//     METransformHaveOutput gates ProcessOutput; MFStartup is required
//     for those events to flow at all - the rest of the binary runs sync
//     MFTs without it) and submits DXGI-surface-backed IMFMediaBuffer
//     samples built directly on the pool's NV12 textures. NO GPU->CPU
//     readback anywhere in this session: the texture stays GPU-side and
//     the encoded AUs come back as CPU bytes inside the MFT's output
//     samples (that direction is the MFT's own doing).
//
//   MfCpuEncoder - the software/fallback rung: an adapter implementing
//     the same session interface over the existing MfSoftEncoder
//     (M0-pinned CPU-NV12 MFT wrapper). Its Submit must first copy the
//     leased NV12 texture back to CPU bytes (one persistent staging
//     texture) because MfSoftEncoder takes memory buffers; that readback
//     is the CPU rung's inherent cost, NOT the healthy path's (the
//     global no-readback constraint covers the DXGI/hardware path).
//
// Identity contract (spec §5.3 / §8.3.3, carried M0-final-review item):
// every session assigns each submission a strictly increasing 100ns
// sample time keyed to the caller's encode_seq, registers it in an
// OutputIdentityTracker, and consumes outputs BY THAT EXACT TIME. An
// output whose time is missing, unknown, or duplicated fails with
// encoder_identity_mismatch - the 1:1 output<->input mapping assertion.
// (Measured on the software MFT: output times echo input times verbatim
// but AUs surface REORDERED, so FIFO pairing would cross-wire; only the
// time-keyed lookup is correct.)
//
// Lease discipline: the SurfaceLease moved into Submit is released
// EXACTLY ONCE per submission - MfGpuEncoder completes it when the MFT
// emits that input's output (GPU in-flight is bounded by the 3-slot
// pool: spec §9 "MFT in-flight 3 surfaces"), MfCpuEncoder completes it
// as soon as the NV12 bytes are secured (the CPU MFT's ~17-frame
// lookahead lives in CPU memory, not GPU surfaces). Both complete every
// outstanding lease on Shutdown/failure paths. The submit id is the
// encode_seq (unique among SUBMITTED slots by construction - the pool's
// Complete-by-id safety net from Task 1).
//
// Header organization mirrors mf_encoder.h/gpu_surface.h: the pure
// section (types, tracker, color rule) is header-only with no D3D/MF
// includes so the selftest covers it without any device; the sessions
// are pimpl'd below it. IEncoderSession lives HERE (mf_gpu_encoder.h):
// the interface is born with the GPU session, and mf_encoder.h stays the
// M0-pinned MfSoftEncoder surface.
#ifndef XNC_NATIVE_DESKTOP_MF_GPU_ENCODER_H_
#define XNC_NATIVE_DESKTOP_MF_GPU_ENCODER_H_

#include <cstdint>
#include <deque>
#include <string>
#include <vector>

#include "gpu_surface.h"   // SurfaceLease, Nv12SurfacePool (Submit's currency)
#include "media_types.h"   // FrameIdentity
#include "mf_encoder.h"    // MfSoftEncoder (MfCpuEncoder's engine)

namespace xnc {

// ---- Session vocabulary (pure) ----

// One Submit's outcome. kOk means the input was accepted (outputs may lag
// by the MFT's pipeline depth - collect via TakeOutput).
enum class SubmitResult : uint8_t {
  kOk = 0,
  kRejected = 1,       // the MFT/session refused the input (lease released)
  kIdentityFault = 2,  // non-monotonic encode_seq or the session is already
                       // poisoned by an identity mismatch (lease released)
  kNotReady = 3,       // not initialized / no submit permission in budget
};

// Shutdown flavor. kDrain flushes the MFT's buffered outputs through the
// same identity mapping (validating the 1:1 pairing, completing leases)
// before teardown; kImmediate tears down without draining. The session
// does not outlive Shutdown either way - collect outputs BEFORE it (the
// drained tail is validated and dropped, with its count logged).
enum class ShutdownMode : uint8_t { kDrain = 0, kImmediate = 1 };

// TakeOutput failure reason (sessions expose last_error(); the bool return
// stays the brief's exact signature).
enum class EncoderSessionError : uint8_t {
  kNone = 0,
  kTimeout = 1,                   // no output before timeout_ms
  kNotReady = 2,                  // session not initialized / shut down
  kEncoderIdentityMismatch = 3,   // output time missing/unknown/duplicated
  kCollectFailed = 4,             // MFT ProcessOutput failed
};

// One collected output AU paired back to its input.
struct EncoderOutput {
  FrameIdentity id{};        // the identity Submit received for this AU's input
  uint64_t submit_id = 0;    // the lease submit id (== encode_seq)
  size_t slot = 0;           // the lease slot index that carried the input
  int64_t sample_time = 0;   // the session-assigned 100ns time of that input
  bool key = false;          // IDR AU (NAL type 5 present)
  std::vector<uint8_t> au;   // raw Annex-B AU as the MFT emitted it
};

// The registration a session keeps per in-flight submission.
struct OutputIdentityRecord {
  FrameIdentity id{};
  uint64_t submit_id = 0;
  size_t slot = 0;
};

// Consume() verdicts - the three encoder_identity_mismatch failure modes
// (missing/unknown/duplicated output sample time) plus success.
enum class OutputConsume : uint8_t {
  kOk = 0,
  kTimeMissing = 1,    // the output sample carries no timestamp at all
  kTimeUnknown = 2,    // no live registration carries that time
  kTimeDuplicated = 3  // that time was already consumed by an earlier output
};

// Pure 1:1 output<-input mapping machinery (header-only; the sessions and
// the selftest share it). Register() enforces strictly increasing times
// (the "100ns sample times keyed to encodeSeq" invariant); Consume() looks
// an output up BY EXACT TIME among the live registrations.
//
// Bounded by construction (review fix): consumed entries are pruned from
// the FRONT as soon as the prefix is fully consumed (outputs arrive in
// roughly time order with a small reorder depth), and RETAINED entries
// (pending + a consumed tail behind the oldest pending record) are hard-
// capped at kMaxTracked. Hitting the cap means the encoder stopped
// emitting outputs for >= kMaxTracked submissions - Register() then fails
// and the sessions turn that into the identity fault (hard failure, lease
// released), never into unbounded memory growth or O(n^2) scans.
class OutputIdentityTracker {
 public:
  // Retained-entry cap: far above every legitimate pipeline depth (the
  // software MFT's ~17-frame lookahead, the 3-slot GPU pool, drain tails)
  // and small enough to bound memory/work per session.
  static constexpr size_t kMaxTracked = 512;

  // Records `sample_time` -> `rec`. False when the time is not strictly
  // greater than the last registered time (collisions/regressions would
  // break the 1:1 mapping) OR the retained-entry cap is exhausted (the
  // encoder owes >= kMaxTracked unconsumed outputs - hard failure).
  bool Register(int64_t sample_time, const OutputIdentityRecord& rec) {
    if (sample_time <= last_time_ && has_any_) return false;
    if (entries_.size() >= kMaxTracked) return false;
    Entry e{};
    e.time = sample_time;
    e.rec = rec;
    entries_.push_back(e);
    last_time_ = sample_time;
    has_any_ = true;
    return true;
  }

  // Removes the newest registration (Submit rollback after a rejected
  // input - the time was never handed to the MFT successfully... on the
  // CPU rung the MFT may still emit it, so rollback is only used when the
  // submission failed BEFORE ProcessInput consumed the sample).
  void RollbackNewest() {
    if (!entries_.empty()) entries_.pop_back();
    if (entries_.empty()) {
      has_any_ = false;
      last_time_ = 0;
    } else {
      last_time_ = entries_.back().time;
    }
  }

  // Matches an output by exact time. kOk fills *out and consumes the
  // registration; every other verdict poisons the caller (identity fault).
  OutputConsume Consume(bool has_time, int64_t sample_time,
                        OutputIdentityRecord* out) {
    if (!has_time) return OutputConsume::kTimeMissing;
    size_t found_live = kNpos;
    size_t found_used = kNpos;
    for (size_t i = 0; i < entries_.size(); ++i) {
      if (entries_[i].time != sample_time) continue;
      if (entries_[i].consumed) {
        found_used = i;
      } else {
        found_live = i;
        break;
      }
    }
    if (found_live == kNpos)
      return found_used == kNpos ? OutputConsume::kTimeUnknown
                                 : OutputConsume::kTimeDuplicated;
    entries_[found_live].consumed = true;
    if (out != nullptr) *out = entries_[found_live].rec;
    ++consumed_;
    PruneConsumedFront();
    return OutputConsume::kOk;
  }

  // Still-unconsumed registrations (Shutdown completes exactly these).
  std::vector<OutputIdentityRecord> PendingRecords() const {
    std::vector<OutputIdentityRecord> out;
    for (const auto& e : entries_)
      if (!e.consumed) out.push_back(e.rec);
    return out;
  }

  size_t PendingCount() const {
    size_t n = 0;
    for (const auto& e : entries_)
      if (!e.consumed) ++n;
    return n;
  }

  void Clear() {
    entries_.clear();
    last_time_ = 0;
    has_any_ = false;
    consumed_ = 0;
  }

  size_t consumed_count() const { return consumed_; }
  int64_t last_registered_time() const { return last_time_; }
  // Entries currently retained (pending + consumed tail behind the oldest
  // pending record); <= kMaxTracked always.
  size_t retained() const { return entries_.size(); }

 private:
  static constexpr size_t kNpos = static_cast<size_t>(-1);
  struct Entry {
    int64_t time = 0;
    OutputIdentityRecord rec;
    bool consumed = false;
  };
  // Deque: O(1) front pops keep the healthy path (register -> consume)
  // constant-size; a vector's erase(begin) would re-shift per consume.
  void PruneConsumedFront() {
    while (!entries_.empty() && entries_.front().consumed)
      entries_.pop_front();
  }
  std::deque<Entry> entries_;
  int64_t last_time_ = 0;
  bool has_any_ = false;
  size_t consumed_ = 0;
};

// ---- Color rule ----
// Nv12ColorConfig + Nv12ColorForSize live in mf_encoder.h (the encoder's
// media-type concern; shared by both session rungs and the VideoProcessor
// config without an include cycle - this header includes that one).

// ---- The session interface (brief Task 3, exact) ----
//
// Single-threaded (the media GPU thread) like everything under gpu_surface.
class IEncoderSession {
 public:
  virtual SubmitResult Submit(const FrameIdentity&, SurfaceLease&&,
                              bool force_idr) = 0;
  virtual bool TakeOutput(EncoderOutput*, uint32_t timeout_ms) = 0;
  virtual bool Reconfigure(uint32_t bitrate, uint32_t fps) = 0;
  virtual void Shutdown(ShutdownMode) = 0;
  virtual ~IEncoderSession() = default;
};

// ---- Sessions (pimpl; COM/D3D live in mf_gpu_encoder.cpp) ----

// The D3D11-aware hardware session. Init walks every MFT_ENUM_FLAG_HARDWARE
// H.264 encoder; a candidate must fully negotiate under the device manager
// AND pass the eight-frame startup probe (spec §8.3: >=8 synthetic inputs
// with distinct content ids, first output within two inputs or 100ms,
// 1:1 sample-time mapping, IDR NAL structure valid), after which a
// PRISTINE instance of the winner is re-activated for the live stream.
// Init fails when no candidate passes (the caller falls back to
// MfCpuEncoder); on this RDP box the QSV MFT negotiates but never
// completes encode work - the documented machine shape, skip-clean.
class MfGpuEncoder final : public IEncoderSession {
 public:
  MfGpuEncoder();
  ~MfGpuEncoder();
  MfGpuEncoder(const MfGpuEncoder&) = delete;
  MfGpuEncoder& operator=(const MfGpuEncoder&) = delete;

  // dev: the shared media-GPU-thread D3D11 device (plan ruling 4; not
  // owned - AddRef'd for the session's lifetime). pool: the NV12 pool
  // whose leases Submit receives (the Complete-by-id target). w/h even,
  // fps > 0; bitrate_bps == 0 -> 2 Mbps (MfSoftEncoder convention).
  bool Init(ID3D11Device* dev, Nv12SurfacePool* pool, uint32_t w, uint32_t h,
            uint32_t fps, uint32_t bitrate_bps, std::string* err);

  SubmitResult Submit(const FrameIdentity&, SurfaceLease&&,
                      bool force_idr) override;
  bool TakeOutput(EncoderOutput*, uint32_t timeout_ms) override;
  bool Reconfigure(uint32_t bitrate, uint32_t fps) override;
  void Shutdown(ShutdownMode) override;

  EncoderSessionError last_error() const { return last_error_; }
  bool initialized() const { return impl_ != nullptr; }
  const std::string& FriendlyName() const { return friendly_name_; }
  // The MF_MT_YUV_MATRIX the negotiated input type carries (1 = BT.709,
  // 2 = BT.601, 0 = not negotiated / not initialized) - the color
  // agreement surface on the hardware rung (mirrors
  // MfSoftEncoder::negotiated_input_matrix). Defined in the .cpp (Impl
  // is pimpl'd).
  uint32_t negotiated_input_matrix() const;
  // "hardware" after a successful Init, "(none)" before/after failure.
  const char* BackendName() const {
    return impl_ != nullptr ? "hardware" : "(none)";
  }

 private:
  struct Impl;
  Impl* impl_ = nullptr;
  Nv12SurfacePool* pool_ = nullptr;  // non-owning; set by Init
  EncoderSessionError last_error_ = EncoderSessionError::kNone;
  std::string friendly_name_;
};

// The CPU/fallback session over MfSoftEncoder (M0-pinned engine; this
// class only adapts it to the session interface). Submit copies the
// leased NV12 texture into a persistent staging buffer (the CPU rung's
// readback) and feeds MfSoftEncoder::EncodeNV12 with session-assigned
// sample times so outputs map back by the same time-keyed rule as the
// GPU session.
class MfCpuEncoder final : public IEncoderSession {
 public:
  MfCpuEncoder();
  ~MfCpuEncoder();
  MfCpuEncoder(const MfCpuEncoder&) = delete;
  MfCpuEncoder& operator=(const MfCpuEncoder&) = delete;

  // dev/pool as MfGpuEncoder::Init (dev only backs the staging readback;
  // WARP is fine). The MfSoftEncoder ladder (hardware-CPU-input attempt,
  // software fallback, self-check) runs unchanged inside.
  bool Init(ID3D11Device* dev, Nv12SurfacePool* pool, uint32_t w, uint32_t h,
            uint32_t fps, uint32_t bitrate_bps, std::string* err);

  SubmitResult Submit(const FrameIdentity&, SurfaceLease&&,
                      bool force_idr) override;
  bool TakeOutput(EncoderOutput*, uint32_t timeout_ms) override;
  bool Reconfigure(uint32_t bitrate, uint32_t fps) override;
  void Shutdown(ShutdownMode) override;

  EncoderSessionError last_error() const { return last_error_; }
  bool initialized() const { return inited_; }
  // MfSoftEncoder's backend ("hardware"/"software" - its own ladder).
  const char* BackendName() const;
  xnc::EncoderBackend backend() const;
  const std::string& FriendlyName() const;

 private:
  struct Impl;
  Impl* impl_ = nullptr;      // staging texture + device/context refs
  MfSoftEncoder enc_;         // the engine (M0 surface, untouched behavior)
  Nv12SurfacePool* pool_ = nullptr;
  OutputIdentityTracker tracker_;
  std::vector<EncoderOutput> queued_;
  uint64_t last_seq_ = 0;
  int64_t last_time_ = 0;
  uint32_t fps_ = 0;
  bool inited_ = false;
  bool identity_fault_ = false;
  EncoderSessionError last_error_ = EncoderSessionError::kNone;
};

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_MF_GPU_ENCODER_H_
