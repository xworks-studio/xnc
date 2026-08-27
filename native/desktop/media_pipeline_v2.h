// media_pipeline_v2.h - M2 Task 4: the depth-one GPU media pipeline.
//
// Pipeline::Run (pipeline.h) is the M0 two-thread shape: CAPTURE thread ->
// depth-2 FrameBlob queue -> ENCODE thread. MediaPipelineV2 is the M2
// single-loop redesign around the Task 1-3 GPU object set: ONE thread (the
// media GPU thread) owns capture (ICaptureSurface::AcquireSurface into the
// LatestSurface), the FPS gate, the BGRA->NV12 VideoProcessorBlt into a
// leased NV12 pool slot, the IEncoderSession Submit, output collection and
// AU publication to the AuSink (RtServer / file sink). No GPU->CPU readback
// anywhere on the healthy path (the software encoder rung's staging
// readback inside MfCpuEncoder is the designed fallback cost, not this
// pipeline's).
//
// The ONLY cross-thread structure is the command mailbox (MediaMailbox,
// plan ruling 2): a depth-one coalescing cell for the latest un-submitted
// content identity plus three sticky command flags (IDR, reconfigure,
// reset). There is NO frame FIFO anywhere: when all three NV12 slots are
// busy, new contentIds replace the pending cell, and when a slot frees ONLY
// the latest one is converted and submitted (the task's binding test).
//
// Identity (ruling 3 / capture.h contract): the loop speculatively assigns
// capture_epoch/codec_epoch/content_id into AcquireSurface and commits on
// kFrame; it stamps encode_seq/present_mono_us at submission; every
// identity that can reach the wire runs through the M1 FrameIdentityLedger
// at the submission boundary - the M0 pipeline's exact policy site (the
// software rung's AUs surface REORDERED, measured in M1 Task 3, so
// emission order cannot be the gate) - and publication additionally
// rejects unknown or duplicated encode_seqs. Any regression is fatal.
// DELIVERY-order monotonicity (the v2 wire contract: strictly increasing
// encode_seq per epoch pair as delivered - the Go client's frameLedger
// tears down on the first regression) is restored by a bounded
// reorder-before-publish window in CollectOutputs: in-order outputs
// publish immediately with their true (time-keyed) identities, an output
// whose seq skips parks until the gap fills, and a gap that exceeds 2x
// the lookahead window or outlives a hold timeout is skipped + recovered
// by a sticky "reorder_gap" IDR.
//
// AU shaping is the SAME stream contract as the M0 pipeline (pipeline.h:
// SPS/PPS prefixed on IDR, 4-byte start codes, parameter sets/AUD dropped).
// The sessions do not expose MfSoftEncoder::SpsPps(), so MediaPipelineV2
// harvests SPS/PPS from the encoder's own key AUs (NalExtractTypes) and
// re-prefixes from that cache.
//
// Threading contract: every method of LatestSurface / Nv12SurfacePool /
// the encoder session / the VideoProcessor is called from the ONE media
// loop thread (see the gpu_surface.h single-thread note - under
// MediaPipelineV2 the loop thread IS the capture thread, which is how
// capture.h's "capture thread drives AcquireSurface" and gpu_surface.h's
// "media GPU thread" are the same thread; ruling 4). RequestIdr /
// Reconfigure / Reset are safe from ANY thread (they only touch the
// mailbox); Start/Stop are called from the owner thread and Stop joins the
// loop. The AuSink contract matches pipeline.h: OnAu / OnState /
// OnDisplayChanged / PendingIdrReason all fire on the media loop thread
// (RtServer is thread-safe across those).
#ifndef XNC_NATIVE_DESKTOP_MEDIA_PIPELINE_V2_H_
#define XNC_NATIVE_DESKTOP_MEDIA_PIPELINE_V2_H_

#include <atomic>
#include <cstdint>
#include <cstring>
#include <mutex>
#include <string>
#include <thread>

#include "capture.h"        // ICapture, ICaptureSurface
#include "capture_reset.h"  // CaptureReset, kResetReasonMax (opt. coordinator)
#include "gpu_surface.h"    // LatestSurface, Nv12SurfacePool (pimpl, no d3d11.h)
#include "media_types.h"    // FrameIdentity, FrameIdentityLedger, EncodedAU
#include "mf_gpu_encoder.h"  // IEncoderSession (+ both rungs, in the .cpp)
#include "pipeline.h"       // AuSink (publication seam, same as M0)

#include <memory>

namespace xnc {

// ---- M2 Task 5: unified reset + backend fallback vocabulary (pure) ----
//
// The reset sequence every executed reset runs, in spec order. The phases
// are recorded per executed reset (Result::reset_phases) so the wire
// contract is assertable end to end:
//   discontinuity (0x020B reaches subscribers with the new epoch's first
//     AU; the old generation's pending outputs are REJECTED from here)
//   -> stop submissions -> retire leases -> rebuild -> base -> config
//   -> IDR -> running.
enum class ResetPhase : uint8_t {
  kDiscontinuity = 1,
  kStopSubmissions,
  kRetireLeases,
  kRebuild,
  kBase,
  kConfig,
  kIdr,
  kRunning,
};
inline const char* ResetPhaseName(ResetPhase p) {
  switch (p) {
    case ResetPhase::kDiscontinuity: return "discontinuity";
    case ResetPhase::kStopSubmissions: return "stop_submissions";
    case ResetPhase::kRetireLeases: return "retire_leases";
    case ResetPhase::kRebuild: return "rebuild";
    case ResetPhase::kBase: return "base";
    case ResetPhase::kConfig: return "config";
    case ResetPhase::kIdr: return "idr";
    case ResetPhase::kRunning: return "running";
  }
  return "?";
}

// Severity order for reason coalescing (ruling 2):
//   device-removed > desktop/display change > access-lost > encoder
// > unknown ("manual" etc.). A pending reset reason is superseded only by
// a strictly higher severity (MediaMailbox::RequestReset).
inline int ResetSeverity(const char* reason) {
  if (reason == nullptr) return 0;
  if (std::strcmp(reason, kResetReasonDeviceRemoved) == 0) return 5;
  if (std::strcmp(reason, kResetReasonDesktopSwitch) == 0) return 4;
  if (std::strcmp(reason, kResetReasonResolution) == 0) return 3;
  if (std::strcmp(reason, kResetReasonSwitch) == 0) return 3;
  if (std::strcmp(reason, kResetReasonAccessLost) == 0) return 2;
  if (std::strcmp(reason, kResetReasonChangeBackend) == 0) return 2;
  if (std::strcmp(reason, kResetReasonEncoder) == 0) return 1;
  return 0;
}

// Exponential backoff for repeated IDENTICAL reset reasons (ruling 2):
// `streak` is the consecutive-execution count of the same reason (1 = the
// first). The first repeat is free, then base doubles per repeat, capped.
// base=500/cap=4000 (production): 0, 0, 500, 1000, 2000, 4000, 4000...
inline uint32_t ResetStormBackoffMs(uint32_t base_ms, uint32_t cap_ms,
                                    uint32_t streak) {
  if (streak <= 1 || base_ms == 0) return 0;
  uint64_t v = base_ms;
  for (uint32_t i = 2; i < streak; ++i) {
    v <<= 1;
    if (v >= cap_ms) return cap_ms;
  }
  return static_cast<uint32_t>(v < cap_ms ? v : cap_ms);
}

// The two desktop backends MediaPipelineV2 can fall between (ruling 3;
// BackendKind in backend_ladder.h is the M0 ladder's own - this one keeps
// the V2 path decoupled from the ladder).
enum class MediaBackend : uint8_t { kDxgi = 0, kGdi = 1 };
inline const char* MediaBackendName(MediaBackend b) {
  return b == MediaBackend::kDxgi ? "dxgi" : "gdi";
}

// Process-lifetime hardware-encoder fallback lock (ruling 3): after
// kMaxFailures hardware-encoder CONTRACT failures (the rung's Init probe
// fails, or a live hardware session breaks the session contract - submit
// rejected / 1:1 output mapping lost) the pipeline locks to the software
// rung until process restart. Injectable so the selftest isolates
// scenarios deterministically; production shares ONE instance per process
// (ProcessEncoderLock, defined in media_pipeline_v2.cpp).
struct EncoderFallbackLock {
  static constexpr uint32_t kMaxFailures = 3;
  std::atomic<uint32_t> hw_failures{0};
  std::atomic<bool> software_locked{false};
  // Records one contract failure. True when THIS failure tripped the lock.
  bool NoteHwFailure() {
    const uint32_t n = hw_failures.fetch_add(1, std::memory_order_relaxed) + 1;
    if (n >= kMaxFailures) {
      software_locked.store(true, std::memory_order_relaxed);
      return true;
    }
    return false;
  }
  bool SoftwareLocked() const {
    return software_locked.load(std::memory_order_relaxed);
  }
  uint32_t failures() const { return hw_failures.load(std::memory_order_relaxed); }
};

// The process-wide lock instance (function-local static: process lifetime,
// thread-safe initialization). MediaPipelineV2 uses it when Config::
// encoder_lock is null.
EncoderFallbackLock* ProcessEncoderLock();

// ---- the depth-one command mailbox (plan ruling 2) ----
//
// Four cells, each depth one, each replacing (never queueing):
//   latest content - the FrameIdentity of the newest captured-but-not-yet-
//                    submitted content. A publish while a previous identity
//                    is still pending REPLACES it (coalescing: only the
//                    newest content can ever be submitted).
//   sticky IDR     - set by ArmIdr (any thread), consumed by the loop's
//                    NEXT submission exactly once (TakeIdr). While set it
//                    survives any number of loop iterations - even across
//                    the busy-slot wait - and is never re-armed by itself.
//   reconfigure    - bitrate/fps params, latest wins.
//   reset          - capture-reset request flag + reason, latest wins.
//
// Thread-safe (one small mutex); the media loop is the only consumer, any
// thread may produce. Header-only pure logic on purpose - the selftest
// pins the coalescing/sticky semantics without a device or encoder.
class MediaMailbox {
 public:
  // ---- latest content (depth one, coalescing) ----
  void PublishContent(const FrameIdentity& id) {
    std::lock_guard<std::mutex> lk(mu_);
    content_ = id;
    has_content_ = true;
  }
  // Consumes the pending identity (the only consumer: the media loop's
  // submission step, after a pool slot was acquired).
  bool TakeContent(FrameIdentity* out) {
    std::lock_guard<std::mutex> lk(mu_);
    if (!has_content_) return false;
    if (out != nullptr) *out = content_;
    has_content_ = false;
    return true;
  }
  bool HasContent() const {
    std::lock_guard<std::mutex> lk(mu_);
    return has_content_;
  }

  // ---- sticky IDR (set once, consumed once by a submission) ----
  void ArmIdr(const char* reason) {
    std::lock_guard<std::mutex> lk(mu_);
    idr_armed_ = true;
    CopyPad(idr_reason_, reason);
  }
  // True + copies the reason when armed; consumes the flag (exactly-once).
  bool TakeIdr(char* out, size_t cap) {
    std::lock_guard<std::mutex> lk(mu_);
    if (!idr_armed_) return false;
    if (out != nullptr && cap > 0) {
      std::memcpy(out, idr_reason_, cap < sizeof(idr_reason_) ? cap
                                                             : sizeof(idr_reason_));
      out[cap - 1] = '\0';
    }
    idr_armed_ = false;
    return true;
  }
  bool idr_armed() const {
    std::lock_guard<std::mutex> lk(mu_);
    return idr_armed_;
  }

  // ---- reconfigure (depth one, latest wins) ----
  void RequestReconfigure(uint32_t bitrate_bps, uint32_t fps) {
    std::lock_guard<std::mutex> lk(mu_);
    reconf_bitrate_ = bitrate_bps;
    reconf_fps_ = fps;
    reconf_pending_ = true;
  }
  bool TakeReconfigure(uint32_t* bitrate_bps, uint32_t* fps) {
    std::lock_guard<std::mutex> lk(mu_);
    if (!reconf_pending_) return false;
    if (bitrate_bps != nullptr) *bitrate_bps = reconf_bitrate_;
    if (fps != nullptr) *fps = reconf_fps_;
    reconf_pending_ = false;
    return true;
  }

  // ---- reset request (depth one; reasons COALESCE by severity, M2
  // Task 5): a pending reason is superseded only by a strictly higher
  // ResetSeverity (ties: the latest wins), so same-cycle reasons merge
  // into ONE reset carrying the most severe cause. ----
  void RequestReset(const char* reason) {
    std::lock_guard<std::mutex> lk(mu_);
    char next[32];
    CopyPad(next, reason);
    if (reset_pending_ && ResetSeverity(next) < ResetSeverity(reset_reason_))
      return;  // a lower-severity cause never displaces the pending one
    CopyPad(reset_reason_, next);
    reset_pending_ = true;
  }
  bool TakeReset(char* out, size_t cap) {
    std::lock_guard<std::mutex> lk(mu_);
    if (!reset_pending_) return false;
    if (out != nullptr && cap > 0) {
      std::memcpy(out, reset_reason_,
                  cap < sizeof(reset_reason_) ? cap : sizeof(reset_reason_));
      out[cap - 1] = '\0';
    }
    reset_pending_ = false;
    return true;
  }
  bool reset_pending() const {
    std::lock_guard<std::mutex> lk(mu_);
    return reset_pending_;
  }

 private:
  static void CopyPad(char* dst, const char* src) {
    size_t i = 0;
    if (src != nullptr)
      for (; i < 31 && src[i] != '\0'; ++i) dst[i] = src[i];
    for (; i < 32; ++i) dst[i] = '\0';
  }
  mutable std::mutex mu_;
  FrameIdentity content_{};
  bool has_content_ = false;
  bool idr_armed_ = false;
  char idr_reason_[32] = {0};
  bool reconf_pending_ = false;
  uint32_t reconf_bitrate_ = 0, reconf_fps_ = 0;
  bool reset_pending_ = false;
  char reset_reason_[32] = {0};
};

// ---- the pipeline ----
class MediaPipelineV2 {
 public:
  struct Impl;  // defined in media_pipeline_v2.cpp (D3D/COM live there)
  struct Config {
    // The capture backend: surf drives AcquireSurface (the LatestSurface
    // publish); cap is the SAME object in production (DxgiCapture /
    // GdiCapture implement both) and supplies Width/Height/Rebuild for the
    // reset path. Both must outlive Start..Stop.
    ICapture* cap = nullptr;
    ICaptureSurface* surf = nullptr;
    // Publication sink (RtServer, file sink, or a Tee of both). OnAu is
    // called on the media loop thread.
    AuSink* sink = nullptr;
    uint32_t fps = 30;                    // submission pacing (FPS gate)
    uint32_t bitrate_bps = 2300000;
    // 0 = run until the stop flag / Stop(). >0 = bounded run.
    uint32_t duration_s = 0;
    // Optional external stop flag (RtServer::RequestStop / Ctrl+C).
    const std::atomic<bool>* stop = nullptr;
    // Optional unified CaptureReset coordinator: the loop polls TakeReset
    // into the mailbox (DesktopWatch / 0x0128 switch requests) and uses its
    // desktop gate while a reset waits out a secure desktop. Null = the
    // loop's own RequestReset paths still work, ungated.
    CaptureReset* reset = nullptr;
    // Optional VideoProcessor downscale clamp (--max-w): the BGRA->NV12
    // conversion scales to <= max_w in the same Blt (GpuScaledDims shape).
    // 0 = native capture dims.
    uint32_t max_width = 0;
    // Skip the hardware session attempt entirely (--encoder software): the
    // degraded-restart contract (spec 15.2) must reach the CPU rung without
    // probing a possibly-broken GPU.
    bool force_software_encoder = false;
    // Optional DesktopWatch name provider for the per-second diag beat.
    const char* (*desktop_name_fn)(void*) = nullptr;
    void* desktop_name_ctx = nullptr;
    // Selftest/diag seam: when non-null, InitStream calls it instead of the
    // internal hardware-then-CPU session ladder (and again after every
    // reset - the returned session is owned by the pipeline and Shut down
    // before the next factory call). The pool is the pipeline's own
    // Nv12SurfacePool (already Init'ed at the stream dims). Null = the
    // production ladder (MfGpuEncoder, MfCpuEncoder fallback).
    IEncoderSession* (*session_factory)(void* ctx, Nv12SurfacePool* pool) = nullptr;
    void* session_ctx = nullptr;
    // M2 Task 5: the HARDWARE rung seam (injected contract failures - the
    // lock is testable via fakes, never by waiting on real hardware).
    // When non-null, CreateSession tries this FIRST and treats its session
    // as the hardware rung: a null return = hardware Init failure (one
    // strike on the process-lifetime fallback lock), and a live session's
    // submit/contract fault routes through the unified reset (also a
    // strike) instead of a fatal. Falls through to session_factory / the
    // internal CPU rung exactly like a failing MfGpuEncoder would. With
    // only session_factory set, behavior is exactly Task 4's (no lock
    // interplay).
    IEncoderSession* (*hw_session_factory)(void* ctx, Nv12SurfacePool* pool) = nullptr;
    void* hw_session_ctx = nullptr;
    // M2 Task 5 process-lifetime fallback lock. Null = the process-wide
    // instance (ProcessEncoderLock). Tests inject a fresh one per
    // scenario.
    EncoderFallbackLock* encoder_lock = nullptr;
    // M2 Task 5 backend fallback (ruling 3): when non-null, the pipeline
    // may swap the desktop backend DURING a reset - a DXGI rung whose
    // rebuild keeps failing falls to GDI, and a healthy DXGI probe (every
    // dxgi_reprobe_ms while serving on GDI) returns through the same reset
    // sequence. The factory returns an object implementing BOTH ICapture
    // and ICaptureSurface; the pipeline OWNS factory-created backends.
    // cfg.cap/cfg.surf remain the INITIAL backend (caller-owned, never
    // freed by the pipeline). Null = the Task 4 single-backend shape.
    std::unique_ptr<ICapture> (*make_backend)(void* ctx, MediaBackend kind,
                                              std::string* err) = nullptr;
    void* backend_ctx = nullptr;
    MediaBackend initial_backend = MediaBackend::kDxgi;  // cfg.cap's kind
    // DXGI re-probe cadence while on GDI (spec: 30 s; 0 = never return).
    uint32_t dxgi_reprobe_ms = 30000;
    // Exponential backoff base for repeated identical reset reasons AND
    // the rebuild-failure retry cadence (500 ms production; small values
    // keep the deterministic selftest scenarios fast). Hard backoff is
    // 2x this. 0 = no backoff.
    uint32_t reset_backoff_base_ms = 500;
  };

  struct Result {
    bool ok = true;
    std::string err;                // fatal message when !ok
    uint32_t width = 0, height = 0; // stream (converted) dims
    uint64_t captured = 0;          // kFrame acquisitions
    uint64_t encoded = 0;           // accepted encoder submissions
    uint64_t timeouts = 0;          // static-screen (kNoChange) observes
    uint64_t warmup_feeds = 0;      // idle re-feeds (spec 7.4/7.5)
    uint64_t keyframes = 0;         // IDR AUs published
    uint64_t aus_written = 0;       // shaped AUs pushed to the sink
    uint64_t bytes_written = 0;
    uint32_t resets = 0;            // executed capture resets
    uint32_t rebuilds = 0;          // backend RebuildCount() at stop
    // Reorder-window accounting (fix round 1): never-emitted encode_seqs
    // skipped to keep delivery monotonic (each skip arms a sticky IDR),
    // and outputs dropped for arriving below the already-published seq.
    uint64_t reorder_gap_skips = 0;
    uint64_t reorder_late_drops = 0;
    // M2 Task 5 unified-reset accounting: the ordered phases of EVERY
    // executed reset (8 per reset, ResetPhase order - assertable against
    // the spec sequence), the storm backoff applied per executed reset
    // (identical consecutive reasons), and the retired-generation outputs
    // dropped so no AU of a retired epoch can follow its successor on the
    // wire.
    std::vector<ResetPhase> reset_phases;
    std::vector<uint32_t> reset_storm_ms;
    uint64_t epoch_retired_drops = 0;
    // Hardware-encoder fallback lock state observed by this run (strikes
    // noted + whether the process lock was tripped at stop).
    uint32_t hw_contract_failures = 0;
    bool encoder_software_locked = false;
    // Backend fallback (Config::make_backend): swaps executed and the
    // rung serving at stop, plus the DXGI re-probe accounting while on
    // GDI (ruling 3).
    uint32_t backend_swaps = 0;
    uint32_t dxgi_probes_ok = 0;
    uint32_t dxgi_probes_failed = 0;
    MediaBackend backend_at_stop = MediaBackend::kDxgi;
    char last_reset_reason[kResetReasonMax] = {0};
    const char* encoder_backend = "(none)";  // which rung ran
    std::string encoder_friendly;
    // M2 Task 6: pre-rendered stage-histogram block for the stats.json
    // sidecar (FormatStagesJson: p50/p95/p99 per stage, zero-sample stages
    // ABSENT). Empty when the run recorded no samples in any stage.
    std::string stages_json;
    // M2 Task 6: CPU readbacks observed INSIDE the serving encoder rung
    // (the internal software rung's one staging Map per Submit - its
    // designed fallback cost). The V2 capture/conversion stages have no
    // readback site at all, so a GPU-rung run keeps this at 0; read it
    // together with encoder_backend (which rung served).
    uint64_t cpu_readbacks = 0;
  };

  MediaPipelineV2();
  ~MediaPipelineV2();
  MediaPipelineV2(const MediaPipelineV2&) = delete;
  MediaPipelineV2& operator=(const MediaPipelineV2&) = delete;

  // Validates cfg (cap/surf/sink non-null, fps > 0) and spawns the single
  // media loop thread. False + start_error() on a bad config (the loop
  // itself reports failures through Stop()'s Result - capture/encoder init
  // happens lazily at the first frame).
  bool Start(const Config& cfg);

  // ---- mailbox commands (any thread) ----
  // Arms the sticky IDR: the loop's NEXT submission is forced to an IDR,
  // exactly once (the brief's sticky-once contract).
  void RequestIdr(const char* reason);
  // Reconfigure request; applied by the loop before its next iteration.
  void Reconfigure(uint32_t bitrate_bps, uint32_t fps);
  // Requests a capture reset (rebuild + new generation + forced IDR).
  void Reset(const char* reason);

  // True while the media loop thread is running.
  bool running() const;
  // Joins the loop and returns the run result. Idempotent: a second call
  // returns the same Result. Also the destructor's cleanup path.
  Result Stop();
  const std::string& start_error() const;

 private:
  Impl* impl_;
};

// FILE* AuSink for the --console-diag path (the exact M0 FileAuSink
// contract: binary fwrite of every shaped AU; "fwrite out failed" fatal).
// Defined in media_pipeline_v2.cpp so this header needs no <cstdio>.
class MediaFileSink final : public AuSink {
 public:
  explicit MediaFileSink(FILE* f);
  ~MediaFileSink() override;
  MediaFileSink(const MediaFileSink&) = delete;
  MediaFileSink& operator=(const MediaFileSink&) = delete;
  const char* OnAu(const EncodedAU& au) override;

 private:
  FILE* f_;
};

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_MEDIA_PIPELINE_V2_H_
