// pipeline.h - capture -> FrameCache -> encode -> shaped Annex-B dump loop
// (plan M1-Slice1 Task 5; capture/encode decoupled by pipeline-decouple).
// Pipeline::Run drives one diagnostic capture session end to end:
//
//   CAPTURE thread (the Run caller's thread): Acquire (timeout = spf) ->
//   FrameCache state machine -> push the scaled frame into a bounded
//   handoff queue (depth 2, drop-oldest on full -> keep the LATEST frame,
//   ADR-014). ENCODE thread: pop -> MfSoftEncoder -> per-AU stream shaping
//   -> sink.OnAu, running at encode speed (under motion 30fps+; queue empty
//   on a static screen -> idle), then MfSoftEncoder::FlushTail (lookahead
//   window recovery) at end. The unified CaptureReset synchronizes with
//   both threads (drain + encode-mutex re-init); see pipeline.cpp.
//
// Stream contract per AU (spec §7.10, port of vclNALUs semantics from
// agent/screen-helper/capture_windows.go - same rules, C++ rewrite):
//   - IDR AU = cached SPS+PPS (from the encoder's first IDR) prefixed
//     before the VCL NALUs;
//   - parameter sets (7/8) and AUD (9) are dropped from encoder output
//     (duplicate parameter sets / AUD prefixes make some WebCodecs
//     decoders reject frames);
//   - every NALU is normalized to a 4-byte start code (00 00 00 01).
//
// Warm-up (spec §7.4 as amended 2026-08-22, commit 1a1dd41): CMSH264EncoderMFT
// has ~17 frames of startup lookahead, so the first submitted frame does not
// emerge as an AU until ~17 inputs later. On a static screen (queue empty,
// gated on a recent capture timeout) the ENCODE thread re-feeds the cached
// latest captured frame WITHOUT re-forcing the IDR until the first keyframe
// AU emerges
// (old timeout-path semantics, moved with the pacing). Bounds: at most
// min(2 x lookahead window frames, 2 s at target fps) re-feeds; warm-up feed
// counts are logged. ForceNextIdr is never called during warm-up (E2).
//
// stats.json: FormatStatsJson/WriteStatsJson produce the run sidecar next to
// the --out file (same fields as the per-second log + duration/w/h/bitrate).
#ifndef XNC_NATIVE_DESKTOP_PIPELINE_H_
#define XNC_NATIVE_DESKTOP_PIPELINE_H_

#include <atomic>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <deque>
#include <mutex>
#include <string>
#include <vector>

#include "capture.h"       // ICapture, FrameBlob
#include "capture_reset.h"  // CaptureReset (M2-S1 Task 2)
#include "frame_cache.h"   // FrameCache, FrameCacheCounters
#include "media_types.h"   // FrameIdentity, EncodedAU, AuFlags (M1 Task 1)
#include "mf_encoder.h"    // MfSoftEncoder

namespace xnc {

struct PipelineOpts {
  uint32_t duration_s = 10;            // run length in seconds (> 0)
  uint32_t fps = 30;                   // target fps (paces submissions)
  uint32_t target_bitrate_bps = 2300000;
  // Optional early-stop flag (real-time mode: Ctrl+C / RtServer::Shutdown).
  // Checked once per loop iteration; nullptr = run the full duration.
  const std::atomic<bool>* stop = nullptr;
  // Optional live desktop-name provider (M2-Slice1 DesktopWatch): when set,
  // the per-second diag_pipeline line appends ` desktop=<name>`. Pure
  // observation (Task 1 evidence probe) - no capture behavior change; null
  // keeps the historical log format byte-identical.
  const char* (*desktop_name_fn)(void*) = nullptr;
  void* desktop_name_ctx = nullptr;
  // Unified capture reset coordinator (M2-Slice1 Task 2): when set, the
  // pipeline routes ACCESS_LOST-class acquire errors and frame-size changes
  // here instead of dying (suspend -> wait-desktop -> rebuild -> new base
  // -> ForceIDR; STATE recovering/capture_rebuilt; DISPLAY_CHANGED on
  // dimension change). Null = legacy behavior (those errors stay fatal).
  CaptureReset* reset = nullptr;
};

struct PipelineResult {
  bool ok = true;
  std::string err;                 // fatal message when !ok
  FrameCacheCounters counters;     // final totals (stats.json source)
  uint32_t width = 0, height = 0;
  uint64_t aus_written = 0;        // shaped AUs fwrite'd to out
  uint64_t bytes_written = 0;      // total bytes fwrite'd
  uint32_t resets = 0;             // unified CaptureReset executions (M2-S1 T2)
  uint32_t storm_resets = 0;       // resets delayed by storm backoff (M2-S2 T1)
  char last_reset_reason[kResetReasonMax] = {0};  // reason of the last reset
  // M2 Task 6: pre-rendered stage-histogram block for the stats.json
  // sidecar (the "stages" members + the V2 cpu_readbacks line, produced by
  // FormatStagesJson). EMPTY for M0 runs keeps the M0 sidecar byte-identical
  // (FormatStatsJson splices it in only when non-empty).
  std::string stages_json;
};

// Measured lookahead window of CMSH264EncoderMFT on the dev/target machines
// (Task 4: latency exactly 17 frames, then 1 AU per submit in input order).
inline constexpr uint32_t kEncoderLookaheadFrames = 17;

// Warm-up re-feed bound (spec §7.4): min(2 x window frames, 2 s worth of
// frames at the target fps).
inline uint32_t WarmupFeedBound(uint32_t fps) {
  const uint32_t by_window = 2u * kEncoderLookaheadFrames;
  const uint32_t by_time = 2u * (fps > 0 ? fps : 1u);
  return by_window < by_time ? by_window : by_time;
}

// Minimum interval between pipeline-INITIATED IDRs (spec §7.5: 主动 IDR
// 请求最小间隔 500ms). The natural first IDR and rebuild forces are not
// pipeline-initiated and are not throttled by this.
inline constexpr uint64_t kIdrMinIntervalMs = 500;

// FIFO identity of successful MFT input submissions. CMSH264EncoderMFT may
// emit an input's AU many calls later, so output timestamps cannot come from
// the current Encode call. The cap exceeds both the measured 17-frame
// lookahead and the 34-feed warm-up bound; overflow is a fatal encoder error.
// M0-pinned interface: M1 Task 1 keeps this FIFO pairing unchanged and adds
// the SEPARATE FrameIdentityLedger (media_types.h) as the monotonicity
// accept/reject gate; the pipeline runs a 1:1 identity FIFO alongside it.
class SubmissionLedger {
 public:
  bool Submit(uint64_t mono_us);
  bool Take(uint64_t* mono_us);
  size_t Pending() const;
  void Clear();

  // Encode/EncodeNV12 returned before accepting the just-registered input.
  // Removes only that newest registration; older delayed inputs stay paired.
  bool RollbackNewest(uint64_t mono_us);

 private:
  static constexpr size_t kMaxPending = 64;
  std::deque<uint64_t> pending_;
};

// Thread-safe, single-snapshot capture store used by idle re-encoding.
// Update replaces the previous owned FrameBlob; Invalidate drops it across
// capture rebuilds so no pre-reset pixels can be submitted afterward.
class LatestFrameStore {
 public:
  void Update(const FrameBlob& f);
  bool Snapshot(FrameBlob* out) const;
  bool Snapshot(FrameBlob* out, uint64_t* generation) const;
  bool IsCurrent(uint64_t generation) const;
  void Invalidate();

 private:
  mutable std::mutex mu_;
  FrameBlob frame_;
  uint64_t generation_ = 0;
  bool valid_ = false;
};

// ---- rebuild-storm backoff (M2-Slice2 Task 1) ----
// Same-reason CaptureReset executions >= kStormThreshold within
// kStormWindowMs are a rebuild storm: every FURTHER reset of that reason
// waits an exponential backoff first (1 s, 2 s, 4 s ... capped at
// kStormBackoffCapMs) and the pipeline logs a `reset_storm` line. A reset
// carrying a DIFFERENT reason restarts the tracking (storms are per-reason).
// Pure/injectable clock on purpose: the selftest drives the whole backoff
// sequence with a fake clock.
class ResetStormTracker {
 public:
  static constexpr uint32_t kWindowMs = 10000;
  static constexpr uint32_t kThreshold = 3;
  static constexpr uint32_t kBaseMs = 1000;
  static constexpr uint32_t kBackoffCapMs = 10000;
  static constexpr size_t kRing = 16;  // recent same-reason executions kept

  // Delay to apply BEFORE executing a reset with this reason (0 = no storm).
  // count_out (optional) receives the same-reason executions inside the
  // window (for the reset_storm log line).
  uint32_t BackoffMs(const char* reason, uint64_t now_ms,
                     uint32_t* count_out = nullptr) const {
    if (reason == nullptr || reason[0] == '\0' ||
        std::strcmp(reason_, reason) != 0) {
      if (count_out != nullptr) *count_out = 0;
      return 0;
    }
    uint32_t n = 0;
    for (uint32_t i = 0; i < n_ && i < kRing; ++i)
      if (now_ms >= times_[i] && now_ms - times_[i] < kWindowMs) ++n;
    if (count_out != nullptr) *count_out = n;
    if (n < kThreshold) return 0;
    // n == 3 -> 1 s, 4 -> 2 s, 5 -> 4 s, ... capped at 10 s.
    uint64_t ms = kBaseMs;
    for (uint32_t i = kThreshold; i < n; ++i) {
      ms <<= 1;
      if (ms >= kBackoffCapMs) { ms = kBackoffCapMs; break; }
    }
    return static_cast<uint32_t>(ms < kBackoffCapMs ? ms : kBackoffCapMs);
  }

  // Records one reset execution (call when the sequence starts).
  void RecordExecuted(const char* reason, uint64_t now_ms) {
    if (reason == nullptr || reason[0] == '\0') reason = "?";
    if (std::strcmp(reason_, reason) != 0) {
      CopyReason(reason_, sizeof(reason_), reason);
      n_ = 0;
    }
    if (n_ < kRing) {
      times_[n_++] = now_ms;
      return;
    }
    for (uint32_t i = 1; i < kRing; ++i) times_[i - 1] = times_[i];
    times_[kRing - 1] = now_ms;
  }

  uint32_t storm_resets() const { return storm_count_; }
  void NoteStorm() { ++storm_count_; }

 private:
  uint64_t times_[kRing] = {0};
  char reason_[kResetReasonMax] = {0};
  uint32_t n_ = 0;
  uint32_t storm_count_ = 0;
};

// ---- bounded stage histogram (M2 Task 6: GPU media latency diag) ----
//
// One stage's latency/occupancy samples (uint64: microseconds or counts) in
// a fixed-capacity ring buffer - drop-oldest keeps the NEWEST samples, so a
// long run summarizes its most recent window with ZERO per-sample
// allocation. Percentiles are NEAREST-RANK (the standard definition: rank =
// ceil(p/100 * n), value = the rank-th smallest sample), computed on a
// reused sort scratch - one sort per emission, never per sample.
//
// Zero-sample stages are never fake-zero: Percentile returns false and the
// rendering helpers OMIT the stage entirely (stats.json sidecar + log
// lines). Single-threaded by contract (the V2 media loop thread or the
// selftest); hot-path cost is one store + index bump.
class StageHistogram {
 public:
  struct Percentiles {
    uint64_t n = 0;
    uint64_t p50 = 0, p95 = 0, p99 = 0;
    bool has_samples = false;  // false = zero samples (never fake-zero)
  };

  // `name` must have static storage duration (string literals in the
  // caller); `capacity` is the ring size (0 = degenerate, Add is a no-op).
  StageHistogram(const char* name, size_t capacity);
  StageHistogram() = default;

  void Add(uint64_t sample);
  bool Empty() const { return count_ == 0; }
  uint64_t Count() const { return static_cast<uint64_t>(count_); }
  const char* name() const { return name_; }

  // NEAREST-RANK percentile over the retained samples; p must be in
  // (0, 100]. False (and *out untouched) when empty or p is invalid.
  bool Percentile(double p, uint64_t* out) const;
  // p50/p95/p99 in one sorted pass; has_samples=false when empty.
  Percentiles Summary() const;
  // Drops every sample (the per-10s window twin's reset).
  void Reset();

 private:
  const char* name_ = "";
  std::vector<uint64_t> ring_;
  mutable std::vector<uint64_t> scratch_;  // reused sort buffer
  size_t head_ = 0;  // next write index (wraps; overwrites the oldest)
  size_t count_ = 0;
};

// One stage's rendered summary (key + percentiles) for the helpers below.
struct StageStat {
  const char* key;
  StageHistogram::Percentiles p;
};

// Log line: every SAMPLED stage as "key=p50/p95/p99(n=N)" joined by single
// spaces; zero-sample stages are omitted. Empty string when no stage has
// samples (the caller skips the log line entirely).
std::string FormatStageLog(const StageStat* stats, size_t n);

// stats.json sidecar block (one line per SAMPLED stage, 4-space member
// indent, zero-sample stages ABSENT - never fake-zero) plus the V2
// cpu_readbacks accounting line. Trailing-comma terminated for splicing
// before the "ok" member in FormatStatsJson. Empty string when no stage
// has samples (keeps the M0 sidecar byte-identical).
std::string FormatStagesJson(const StageStat* stats, size_t n,
                             uint64_t cpu_readbacks);

// Appends every NALU of `data` except parameter sets (7/8) and AUD (9),
// each re-emitted with a 4-byte start code, trailing zero bytes before the
// next start code dropped, original order preserved (port of vclNALUs -
// agent/screen-helper/capture_windows.go; the reference is read-only, this
// is the clean-room C++ rewrite of its semantics).
inline void VclNalus(const uint8_t* d, size_t n, std::vector<uint8_t>* out) {
  if (!d || !out || n == 0) return;
  static const uint8_t kSc[4] = {0, 0, 0, 1};
  const size_t kNpos = static_cast<size_t>(-1);
  size_t i = 0;
  for (;;) {
    // Next start code at/after i (3- or 4-byte); hdr = NAL header byte.
    size_t hdr = kNpos;
    for (size_t j = i; j + 3 < n; ++j) {
      if (d[j] == 0 && d[j + 1] == 0) {
        if (d[j + 2] == 1) {
          hdr = j + 3;
          break;
        }
        if (d[j + 2] == 0 && j + 4 < n && d[j + 3] == 1) {
          hdr = j + 4;
          break;
        }
      }
    }
    if (hdr == kNpos || hdr >= n) break;
    // NALU ends at the next start code's zero run (or stream end); the
    // leading zeros of that next code belong to the code, not the NALU.
    size_t end = n;
    for (size_t j = hdr + 1; j + 3 <= n; ++j) {
      if (d[j] == 0 && d[j + 1] == 0 &&
          (d[j + 2] == 1 || (j + 4 <= n && d[j + 2] == 0 && d[j + 3] == 1))) {
        end = j;
        break;
      }
    }
    while (end > hdr && d[end - 1] == 0) --end;
    const uint8_t type = static_cast<uint8_t>(d[hdr] & 0x1F);
    if (type != 7 && type != 8 && type != 9 && end > hdr) {
      out->insert(out->end(), kSc, kSc + 4);
      out->insert(out->end(), d + hdr, d + end);
    }
    i = end;
  }
}

// Shapes one raw encoder AU to the stream contract. IDR AUs get the cached
// SPS+PPS (4-byte start codes, from MfSoftEncoder::SpsPps()) prefixed
// before their VCL NALUs; every AU loses its own parameter sets/AUD and is
// normalized to 4-byte start codes. out is cleared first.
inline void ShapeAu(const uint8_t* au, size_t len, bool is_idr,
                    const std::vector<uint8_t>& spspps, std::vector<uint8_t>* out) {
  if (!out) return;
  out->clear();
  if (is_idr && !spspps.empty()) out->assign(spspps.begin(), spspps.end());
  VclNalus(au, len, out);
}

// Receiver of the pipeline's shaped Annex-B AUs (M1-Slice2 Task 2): the
// diag file dump and the real-time pipe fan-out are two implementations of
// ONE pipeline loop. Thread contract (pipeline-decouple): OnAu is called on
// the ENCODE thread; OnState/OnDisplayChanged/OnDisplayChanged from the
// CAPTURE thread (reset paths). Implementations must be thread-safe across
// those calls (RtServer is; FileAuSink only ever sees OnAu).
class AuSink {
 public:
  virtual ~AuSink() = default;

  // One immutable shaped AU (M1 Task 1): SPS/PPS-prefixed on IDR (the key
  // bit in au.flags), 4-byte start codes, no AUD; au.id.present_mono_us =
  // the mono_us stamp of the submission that produced it (push-time for
  // captured frames, feed-time for re-feeds) and au.id.source_mono_us =
  // the desktop-capture time of its pixels. The
  // payload is shared const and must not be mutated. Returns nullptr on
  // success; a non-null fatal message aborts the run
  // (PipelineResult::err = message).
  virtual const char* OnAu(const EncodedAU& au) = 0;

  // Merged IDR request (spec §7.5): non-null when the sink wants a
  // pipeline-initiated IDR (e.g. "sub_join"/"queue_overflow"/"explicit").
  // The pipeline polls this once per loop iteration and, when it arms
  // MfSoftEncoder::ForceNextIdr (throttled by kIdrMinIntervalMs, only after
  // the stream's first keyframe, never while a previous request is still in
  // flight), acknowledges it via ConsumePendingIdr with the same reason.
  virtual const char* PendingIdrReason() { return nullptr; }
  virtual void ConsumePendingIdr(const char* reason) { (void)reason; }

  // State transitions worth surfacing to subscribers (STATE events):
  // "capture_rebuilt" (recoverable), "capture_fatal"/"encoder_fatal"
  // (fatal), "recovering" (unified reset in progress - M2-S1 T2),
  // "capture_failed" (reset rebuild failing hard, still retrying).
  virtual void OnState(const char* code, bool recoverable) {
    (void)code;
    (void)recoverable;
  }

  // Display topology change observed by a completed reset (M2-S1 Task 2):
  // the stream now carries w x h; reason is the reset reason string
  // ("resolution" / "desktop_switch" / ...). Sinks broadcast DISPLAY_CHANGED
  // (0x010A) and update their HOST_HELLO geometry; called on the capture
  // thread right after the matching "capture_rebuilt" state event.
  virtual void OnDisplayChanged(uint32_t w, uint32_t h, const char* reason) {
    (void)w;
    (void)h;
    (void)reason;
  }
};

// Fan-in of two sinks: AUs go to both (first fatal error wins); IDR
// requests come from either and are acknowledged to both; states too. Used
// by --console-diag --pipe <name> --secret <hex> (file dump + rt server on
// one pipeline run).
class TeeAuSink final : public AuSink {
 public:
  TeeAuSink(AuSink* a, AuSink* b) : a_(a), b_(b) {}
  const char* OnAu(const EncodedAU& au) override {
    const char* e = a_ ? a_->OnAu(au) : nullptr;
    if (e != nullptr) return e;
    return b_ ? b_->OnAu(au) : nullptr;
  }
  const char* PendingIdrReason() override {
    if (a_ != nullptr) {
      if (const char* r = a_->PendingIdrReason()) return r;
    }
    return b_ ? b_->PendingIdrReason() : nullptr;
  }
  void ConsumePendingIdr(const char* reason) override {
    if (a_ != nullptr) a_->ConsumePendingIdr(reason);
    if (b_ != nullptr) b_->ConsumePendingIdr(reason);
  }
  void OnState(const char* code, bool recoverable) override {
    if (a_ != nullptr) a_->OnState(code, recoverable);
    if (b_ != nullptr) b_->OnState(code, recoverable);
  }
  void OnDisplayChanged(uint32_t w, uint32_t h, const char* reason) override {
    if (a_ != nullptr) a_->OnDisplayChanged(w, h, reason);
    if (b_ != nullptr) b_->OnDisplayChanged(w, h, reason);
  }

 private:
  AuSink *a_, *b_;
};

class Pipeline {
 public:
  // Runs the loop described in the header comment for opts.duration_s wall
  // seconds. `out` must be open in binary mode (caller closes it). The
  // encoder must already be Init'ed at the capture's dimensions unless the
  // first Acquire fails fatally (then it is never touched). Returns the
  // counters/totals; counters are also visible in the per-second log lines.
  static PipelineResult Run(ICapture& capture, MfSoftEncoder& encoder, FILE* out,
                            const PipelineOpts& opts);
  // Same loop, AUs delivered to `sink` instead of a file (real-time mode).
  static PipelineResult Run(ICapture& capture, MfSoftEncoder& encoder, AuSink& sink,
                            const PipelineOpts& opts);
  // File dump AND sink on one run (--console-diag with an optional rt
  // pipe): AUs are shaped once and delivered to both.
  static PipelineResult Run(ICapture& capture, MfSoftEncoder& encoder, FILE* out,
                            AuSink& sink, const PipelineOpts& opts);
};

// One-line-per-field JSON for the stats.json sidecar (pure, so the selftest
// can assert the field set without touching the filesystem).
std::string FormatStatsJson(const PipelineResult& r, const PipelineOpts& o);

// Writes "<directory of h264_out_path>\\stats.json". Returns false + *err on
// open/write failure. Used by --console-diag (also on capture/encoder init
// failure, with zeroed counters, so every diag run leaves a sidecar).
bool WriteStatsJson(const std::wstring& h264_out_path, const PipelineResult& r,
                    const PipelineOpts& o, std::wstring* err);

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_PIPELINE_H_
