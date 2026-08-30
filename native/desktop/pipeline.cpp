// pipeline.cpp - Pipeline::Run + stats.json writers (see pipeline.h).
// Loop shape (plan Task 5, spec §7.4/§7.5; pipeline-decouple rework):
//
// TWO threads joined by a bounded handoff queue (frame_queue.h):
//
//   CAPTURE thread (RunCore's thread): Acquire (timeout = spf) -> scale
//   (ScaledCapture inside capture; the DXGI GPU path scales+converts to NV12
//   in the VideoProcessor instead) -> push the scaled frame (BGRA or NV12
//   per blob.pixfmt - the encode thread routes the encoder entry on it) into
//   the encode queue (depth 2, drop-oldest on full -> keep the LATEST frame,
//   ADR-014), preserving the capture mono_us timestamp. err_timeout (static)
//   produces no frame (the encode thread idles); err_rebuilt / unified
//   CaptureReset rewind the state machine and synchronize with BOTH threads
//   (the reset drains the queue, then re-inits the encoder under the encode
//   mutex). The capture thread paces pushes to spf - the old SubmitFrame
//   pacing moved here so the encode thread can run at encode speed.
//
//   ENCODE thread: waits on the queue -> takes the latest frame -> encode
//   (MFT) -> shape every AU (SPS/PPS prefix on IDR, 4B start codes, AUD
//   dropped) -> sink->OnAu; no pacing beyond the queue (drop-oldest keeps
//   the latest; under motion encode runs at encode speed, under static the
//   queue is empty -> idle). While the queue is empty the warm-up / on-
//   demand IDR re-feed of the latest captured frame runs here (the old timeout-
//   path semantics, gated on a recent capture timeout so re-feeds still only
//   happen on a static screen), and the merged IDR request (PendingIdrReason)
//   is polled here (it owns the encoder). At run end the encode thread
//   drains the queue (nothing captured is dropped), then RunCore recovers
//   the lookahead window's AUs via FlushTail through the same shaping path.
//
// The FrameCache state machine is shared: capture thread counts captured/
// timeouts and snapshots the latest frame; encode thread counts encoded/warmup
// feeds and observes keyframes - all under one shared mutex (quick ops
// only; the 17-20ms Encode call itself runs outside it).
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>  // Sleep, GetTickCount64
#include <mmsystem.h>  // timeBeginPeriod/timeEndPeriod (1 ms timer resolution)

#include <algorithm>  // std::sort (StageHistogram percentiles)
#include <atomic>
#include <cerrno>
#include <cmath>      // std::ceil (nearest-rank percentiles)
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <mutex>
#include <string>
#include <thread>
#include <vector>

#include "../common/log.h"
#include "dxgi_capture.h"  // Fnv1a64, SamplePointsNotUniform (first-frame diag), NowMonoUs
#include "frame_queue.h"   // bounded capture->encode handoff (pipeline-decouple)
#include "pipeline.h"

namespace xnc {

bool SubmissionLedger::Submit(uint64_t mono_us) {
  if (pending_.size() >= kMaxPending) return false;
  pending_.push_back(mono_us);
  return true;
}

bool SubmissionLedger::Take(uint64_t* mono_us) {
  if (mono_us == nullptr || pending_.empty()) return false;
  *mono_us = pending_.front();
  pending_.pop_front();
  return true;
}

size_t SubmissionLedger::Pending() const { return pending_.size(); }

void SubmissionLedger::Clear() { pending_.clear(); }

bool SubmissionLedger::RollbackNewest(uint64_t mono_us) {
  if (pending_.empty() || pending_.back() != mono_us) return false;
  pending_.pop_back();
  return true;
}

void LatestFrameStore::Update(const FrameBlob& f) {
  std::lock_guard<std::mutex> lk(mu_);
  frame_ = f;
  ++generation_;
  valid_ = true;
}

bool LatestFrameStore::Snapshot(FrameBlob* out) const {
  return Snapshot(out, nullptr);
}

bool LatestFrameStore::Snapshot(FrameBlob* out, uint64_t* generation) const {
  if (out == nullptr) return false;
  std::lock_guard<std::mutex> lk(mu_);
  if (!valid_) return false;
  *out = frame_;
  if (generation != nullptr) *generation = generation_;
  return true;
}

bool LatestFrameStore::IsCurrent(uint64_t generation) const {
  std::lock_guard<std::mutex> lk(mu_);
  return valid_ && generation == generation_;
}

void LatestFrameStore::Invalidate() {
  std::lock_guard<std::mutex> lk(mu_);
  frame_ = FrameBlob{};
  ++generation_;
  valid_ = false;
}

namespace {

// Pacing clock (2026-08-30): QPC-derived, not GetTickCount64. The comment
// below (and years of measurement) assumed timeBeginPeriod(1) lifted the
// tick to ~1 ms; on this box's Windows 11 it does NOT - the tick stays
// ~15.6 ms, quantizing the spf pacing (33 ms @ 30 fps) up to 46.9 ms
// (~21 fps served). QPC keeps the pacing exact. All call sites compare
// values from this same clock (identity stamps use NowMonoUs directly).
uint64_t NowMs() { return NowMonoUs() / 1000; }

// Idle nap between do-nothing timeouts (static screen after warm-up): real
// backends block inside Acquire (spf or 100 ms), fakes return instantly -
// this keeps the capture loop off the CPU in both cases.
constexpr DWORD kIdleSleepMs = 15;

// 1 ms timer resolution for the run's pacing Sleeps. Windows Sleep/
// GetTickCount64 default to ~15.6 ms granularity: with the GPU readback the
// capture-side spf pacing (33 ms @ 30 fps) becomes the cadence limiter and a
// tick-quantized Sleep(30) actually sleeps 30-46 ms (measured ~22-24 fps at
// spf=33). timeBeginPeriod(1) makes the pacing exact (~30 fps) - the standard
// media-loop practice for a dedicated capture/encode process. RAII: restored
// on every RunCore exit path.
class TimePeriodGuard {
 public:
  TimePeriodGuard() { timeBeginPeriod(1); }
  ~TimePeriodGuard() { timeEndPeriod(1); }
};

// Warm-up wall-clock bound (spec §7.4): 2 s. Also bounds the on-demand
// IDR re-feed window (same rule, spec §7.5 carry-over).
constexpr uint64_t kWarmupWallBoundMs = 2000ull;

// ---- unified capture reset (M2-Slice1 Task 2, spec §7.5) ----
// Wait-desktop poll cadence; rebuild retry backoffs (task ruling: retryable
// failure -> 500 ms backoff; a hard streak emits STATE capture_failed once
// and slows the cadence to 1 s - the run keeps retrying, never fatal).
constexpr uint32_t kResetPollMs = 100;
constexpr uint32_t kResetBackoffMs = 500;
constexpr uint32_t kResetHardBackoffMs = 1000;
constexpr uint32_t kResetHardFailStreak = 3;

// On-demand (subscriber-initiated) IDR state. "Armed" spans from the
// ForceNextIdr call to the IDR AU actually emerging: while armed no new
// request is honored (one in flight) and the timeout path re-feeds the
// latest captured frame, bounded like the initial warm-up and never re-forcing
// (the force is one-shot and rides the FIRST submission after arming).
struct OnDemandIdr {
  bool armed = false;
  bool exhaust_logged = false;
  uint64_t armed_ms = 0;
  uint32_t feeds = 0;
  char reason[32] = {0};

  void Arm(const char* why, uint64_t now_ms) {
    armed = true;
    exhaust_logged = false;
    armed_ms = now_ms;
    feeds = 0;
    std::snprintf(reason, sizeof(reason), "%s", why != nullptr ? why : "?");
  }
  // True while a re-feed is allowed (static screen): the IDR has not
  // emerged yet and neither bound is hit.
  bool FeedAllowed(uint64_t now_ms, uint32_t feed_bound) const {
    return armed && feeds < feed_bound && now_ms - armed_ms < kWarmupWallBoundMs;
  }
};

// Diag FILE* sink: the exact pre-slice dump behavior (binary fwrite of
// every shaped AU; fatal error string "fwrite out failed"). OnAu is called
// from the ENCODE thread only.
class FileAuSink final : public AuSink {
 public:
  explicit FileAuSink(FILE* f) : f_(f) {}
  const char* OnAu(const EncodedAU& au) override {
    if (au.annexb == nullptr || au.annexb->empty()) return nullptr;
    const uint8_t* p = au.annexb->data();
    const size_t len = au.annexb->size();
    if (std::fwrite(p, 1, len, f_) != len) {
      XNC_LOG_ERROR("write_out_failed");
      return "fwrite out failed";
    }
    return nullptr;
  }

 private:
  FILE* f_;
};

// ---- shared state between the capture and encode threads ----
// Everything the two threads must agree on lives here under ONE mutex:
// the FrameCache state machine (capture thread: OnCapturedFrame/OnTimeout/
// OnRebuild; encode thread: OnEncoded/OnWarmupFeed/OnKeyframeAu), the cached
// latest captured frame (capture replaces it, encode snapshots it for
// re-feeds), the
// warm-up / on-demand counters, the encoder dims and the PipelineResult
// fields. All lock hold times are microseconds - the encoder's Encode/Init
// calls NEVER run under this mutex.
struct PipelineShared {
  std::mutex mu;
  FrameCache cache;
  PipelineResult* res = nullptr;  // every write happens under mu
  LatestFrameStore latest;
  uint64_t warmup_started_ms = 0; // base submission time (2 s wall bound)
  uint32_t warmup_gen_feeds = 0;  // re-feeds this generation (rebuild resets)
  bool warmup_phase_logged = false;  // one warmup_done/exhausted log per gen
  OnDemandIdr ondemand;
  uint64_t last_initiated_idr_ms = 0;  // kIdrMinIntervalMs throttle anchor
  SubmissionLedger submissions;    // successful MFT inputs awaiting output
  uint32_t enc_w = 0, enc_h = 0;   // encoder's dims (reset re-init updates)
  uint64_t last_timeout_ms = 0;    // last static-screen observation (capture
                                   // thread) - gates re-feeds on the encode
                                   // thread so they only happen on a static
                                   // screen (old timeout-path semantics)
  // M1 Task 1: frame-identity generation counters + the submission-identity
  // FIFO paired 1:1 with `submissions` (same push/pop/rollback/clear points,
  // so the M0 SubmissionLedger timestamp pairing stays exact - ruling 2).
  // capture_epoch++ on every capture rebuild/reset, codec_epoch++ on every
  // encoder re-init, next_content_id++ per accepted captured content,
  // next_encode_seq++ per successful encoder submission (spec §5.1).
  // identity_ledger is the separate monotonicity accept/reject gate
  // (media_types.h), never merged into SubmissionLedger (ruling 1).
  uint64_t capture_epoch = 1;
  uint64_t codec_epoch = 1;
  uint64_t next_content_id = 0;
  uint64_t next_encode_seq = 0;
  FrameIdentityLedger identity_ledger;
  std::deque<FrameIdentity> pending_ids;  // pairs 1:1 with submissions
};

// Per-frame pipeline latency window (pipeline-decouple): capture mono_us ->
// OnAu completion, windowed avg logged every kLatencyWindowFrames. Measures
// the end-to-end delay the decoupling removes (was ~acquire-wait + encode
// serialized; now encode + handoff). Samples whose stamp is older than
// kLatencyMaxUs are dropped: a stamp that old means the capture clock is not
// the QPC wall clock (unit/fake captures), so it is not pipeline latency.
constexpr uint32_t kLatencyWindowFrames = 60;
constexpr uint64_t kLatencyMaxUs = 60ull * 1000000ull;  // 60 s sanity bound
struct LatencyWindow {
  const char* label = "pipe_latency_ms";
  uint64_t sum_us = 0;
  uint32_t n = 0;
  void Add(uint64_t mono_us) {
    const uint64_t now = NowMonoUs();
    if (now <= mono_us || now - mono_us > kLatencyMaxUs) return;  // bad stamp
    sum_us += now - mono_us;
    ++n;
    if (n >= kLatencyWindowFrames) Flush();
  }
  void Flush() {
    if (n == 0) return;
    XNC_LOG_INFO("%s avg=%.2f n=%u", label,
                 static_cast<double>(sum_us) / 1000.0 / static_cast<double>(n), n);
    sum_us = 0;
    n = 0;
  }
};

// Duration window (gpu-readback diag): averages a measured duration in
// microseconds (e.g. the DXGI backend's GPU scale+NV12+readback time, from
// FrameBlob::gpu_scale_us), flushed like LatencyWindow.
struct DurationWindow {
  const char* label;
  uint64_t sum_us = 0;
  uint32_t n = 0;
  void Add(uint64_t us) {
    sum_us += us;
    ++n;
    if (n >= kLatencyWindowFrames) Flush();
  }
  void Flush() {
    if (n == 0) return;
    XNC_LOG_INFO("%s avg=%.2f n=%u", label,
                 static_cast<double>(sum_us) / 1000.0 / static_cast<double>(n), n);
    sum_us = 0;
    n = 0;
  }
};

// ---- encode-thread context ----
struct EncodeCtx {
  MfSoftEncoder& enc;
  AuSink& sink;
  PipelineShared& sh;
  FrameQueue& queue;
  const PipelineOpts& opt;
  std::mutex enc_mu;  // serializes Encode/FlushTail (encode thread) vs
                      // Init (unified reset on the capture thread)
  std::atomic<bool> fatal{false};  // encode-side fatal: run must end
  std::vector<std::vector<uint8_t>> aus;  // per-submission encoder outputs
  std::vector<uint8_t> shaped;            // shaped-AU scratch
  LatencyWindow lat;
  // M3 Task 3 (SET_VIDEO_CONFIG fps_hint): spf_ms/warmup_feed_bound are
  // atomics - the capture thread re-keys them when the live fps hint moves
  // while the encode thread paces idle re-feeds off them.
  std::atomic<uint32_t> spf_ms{0};
  std::atomic<uint32_t> warmup_feed_bound{0};
  uint32_t static_grace_ms = 0;  // re-feed gate: a timeout within this
                                 // window means "static screen"
  uint64_t last_feed_ms = 0;     // re-feed pacing anchor (old SubmitFrame
                                 // pacing, applied to synthetic re-feeds)

  EncodeCtx(MfSoftEncoder& e, AuSink& s, PipelineShared& sh_, FrameQueue& q,
            const PipelineOpts& o, uint32_t spf, uint32_t wf_bound)
      : enc(e),
        sink(s),
        sh(sh_),
        queue(q),
        opt(o),
        spf_ms(spf),
        warmup_feed_bound(wf_bound),
        static_grace_ms(spf * 2 + kIdleSleepMs + 50) {}
};

// ---- unified capture reset execution (M2-Slice1 Task 2, spec §7.5) ----
//
// One reset sequence, run entirely on the CAPTURE thread (the acquire loop
// IS the suspension - nothing calls Acquire while this runs); the encode
// thread keeps draining the queue and idling:
//   0. drain the handoff queue: frames already captured carry the PRE-reset
//      generation/dims and must go through the OLD encoder - the re-init
//      below invalidates them (the encode thread drains on its own; capture
//      is suspended here, so no new frames arrive);
//   1. STATE "recovering" (recoverable): the pipe stays alive and the
//      cursor/input threads (RtServer-owned) keep serving;
//   2. rewind the stream state (FrameCache::OnRebuild -> WAIT_BASE_FRAME +
//      one-shot "rebuild" IDR, latest snapshot cleared, warm-up reset) so the
//      encode thread stops re-feeding the old generation while the reset is in
//      flight;
//   3. while the desktop gate is non-DEFAULT (secure desktop up - T1
//      evidence: re-duplication/init are DENIED 0x80070005 even as SYSTEM)
//      poll every kResetPollMs; aborts on stop/duration/encode-fatal;
//   4. single rebuild path: ICapture::Rebuild, then encoder re-Init (under
//      the encode mutex - an in-flight Encode must finish first) when the
//      dimensions changed. Retryable failures back off 500 ms; a
//      kResetHardFailStreak streak emits STATE "capture_failed" once and
//      slows the retry cadence to 1 s. If the desktop leaves again
//      mid-retry, re-enter the wait;
//   5. on success the stream rewinds exactly like an err_rebuilt (the next
//      frame is the new base frame + the one-shot "rebuild" ForceIDR),
//      PipelineResult gains the new dims and reset accounting, STATE
//      "capture_rebuilt" fires (RtServer: generation++), and a dimension
//      change surfaces via OnDisplayChanged (0x010A broadcast).
struct ResetSequence {
  ICapture* cap;
  MfSoftEncoder* enc;
  AuSink* sink;
  PipelineShared* sh;
  const PipelineOpts* opt;
  FrameQueue* queue;
  std::mutex* enc_mu;
  std::atomic<bool>* fatal;
  uint64_t run_t0;       // RunCore start (duration deadline anchor)
  uint64_t duration_ms;
  const char* reason;
};

// Returns true when the RUN must end (stop flag / duration / encode fatal),
// false when the reset completed and the acquire loop should resume.
bool RunResetSequence(ResetSequence& s) {
  const auto abort = [&s] {
    return (s.opt->stop != nullptr && s.opt->stop->load()) ||
           NowMs() - s.run_t0 >= s.duration_ms ||
           s.fatal->load(std::memory_order_relaxed);
  };
  const auto gate_away = [&s] {
    return s.opt->reset != nullptr &&
           s.opt->reset->Desktop() == ResetDesktop::kNonDefault;
  };

  const uint64_t t_start = NowMs();
  XNC_LOG_INFO("capture_reset_start reason=%s desktop_away=%d", s.reason,
               gate_away() ? 1 : 0);
  s.sink->OnState("recovering", true);

  // Phase 0: drain the handoff queue (pre-reset frames through the old
  // encoder; the encode thread keeps popping on its own).
  while (!s.queue->Empty() && !abort()) Sleep(kResetPollMs);
  if (abort()) return true;

  // Phase 1: rewind the stream state NOW so the encode thread stops
  // re-feeding the old base while the reset is in flight.
  {
    // Submission barrier: an idle feed that already passed validation owns
    // enc_mu through ProcessFrameLocked and must finish before invalidation.
    // Conversely, once reset owns enc_mu no idle feed can validate/submit
    // until WAIT_BASE_FRAME and Invalidate are visible together.
    std::lock_guard<std::mutex> elk(*s.enc_mu);
    std::lock_guard<std::mutex> lk(s.sh->mu);
    s.sh->cache.OnRebuild();  // WAIT_BASE_FRAME + one-shot "rebuild" IDR
    s.sh->latest.Invalidate();
    s.sh->capture_epoch++;  // M1 Task 1: one real rebuild = one epoch (spec §11)
    s.sh->warmup_started_ms = 0;
    s.sh->warmup_gen_feeds = 0;
    s.sh->warmup_phase_logged = false;
  }

  // Phase 2: wait for the desktop to come back (immediate rebuilds are
  // provably futile while the secure desktop holds the output).
  while (gate_away()) {
    if (abort()) return true;
    Sleep(kResetPollMs);
  }

  // Phase 3: single rebuild path with retry/backoff.
  uint32_t old_w = 0, old_h = 0;
  {
    std::lock_guard<std::mutex> lk(s.sh->mu);
    old_w = s.sh->enc_w;
    old_h = s.sh->enc_h;
  }
  uint32_t streak = 0;
  bool failed_state_sent = false;
  uint32_t new_w = 0, new_h = 0;
  for (;;) {
    if (abort()) return true;
    std::string rerr;
    bool ok = s.cap->Rebuild(&rerr);
    if (ok) {
      new_w = s.cap->Width();
      new_h = s.cap->Height();
      if (new_w != old_w || new_h != old_h) {
        // Serialize with the encode thread: an in-flight Encode/FlushTail
        // must finish before the MFT is torn down and re-negotiated.
        // M3 Task 3: the reset re-init honors the LIVE 0x0129 hints (a hot
        // reconfigure must not be silently reverted by the next rebuild).
        const uint32_t fps_now =
            s.opt->fps_hint != nullptr && s.opt->fps_hint->load() != 0
                ? s.opt->fps_hint->load()
                : s.opt->fps;
        const uint32_t bitrate_now =
            s.opt->bitrate_hint != nullptr && s.opt->bitrate_hint->load() != 0
                ? s.opt->bitrate_hint->load()
                : s.opt->target_bitrate_bps;
        std::lock_guard<std::mutex> elk(*s.enc_mu);
        ok = s.enc->Init(new_w, new_h, fps_now, bitrate_now, &rerr);
        // Init starts a new MFT lifecycle even when negotiation fails: its
        // Shutdown first discards every delayed output from the old MFT.
        std::lock_guard<std::mutex> lk(s.sh->mu);
        s.sh->submissions.Clear();
        s.sh->pending_ids.clear();  // M1: the paired identity FIFO clears too
        if (ok) {
          s.sh->codec_epoch++;  // M1 Task 1: re-init = new codec generation
          XNC_LOG_INFO("capture_reset encoder re-init w=%u h=%u", new_w, new_h);
        }
      }
    }
    if (ok) break;
    ++streak;
    XNC_LOG_ERROR("capture_reset_rebuild_failed streak=%u err=\"%s\"", streak,
                  rerr.c_str());
    if (streak >= kResetHardFailStreak && !failed_state_sent) {
      failed_state_sent = true;
      s.sink->OnState("capture_failed", true);  // still retrying - not fatal
    }
    const uint32_t backoff = failed_state_sent ? kResetHardBackoffMs : kResetBackoffMs;
    for (uint32_t slept = 0; slept < backoff; slept += kResetPollMs) {
      if (abort()) return true;
      Sleep(kResetPollMs);
      while (gate_away()) {  // desktop left again mid-retry: wait it out
        if (abort()) return true;
        Sleep(kResetPollMs);
      }
    }
  }

  // Phase 4: rewind the stream state to the new generation.
  const bool dims_changed = new_w != old_w || new_h != old_h;
  {
    std::lock_guard<std::mutex> lk(s.sh->mu);
    s.sh->enc_w = new_w;
    s.sh->enc_h = new_h;
    s.sh->res->width = new_w;
    s.sh->res->height = new_h;
    s.sh->res->resets++;
  }
  CopyReason(s.sh->res->last_reset_reason,
             sizeof(s.sh->res->last_reset_reason), s.reason);
  s.sink->OnState("capture_rebuilt", true);  // RtServer: generation++
  // M2-S3 Task 5: a display SWITCH always notifies (0x010A reason="switch")
  // even when the two monitors share a resolution - viewers must refresh the
  // displays list and re-map input coordinates to the new output.
  if (dims_changed || std::strcmp(s.reason, kResetReasonSwitch) == 0)
    s.sink->OnDisplayChanged(new_w, new_h, s.reason);
  XNC_LOG_INFO("capture_reset_done reason=%s w=%u h=%u dims_changed=%d elapsed_ms=%llu",
               s.reason, new_w, new_h, dims_changed ? 1 : 0,
               static_cast<unsigned long long>(NowMs() - t_start));
  return false;
}

// One submission (real captured frame, warm-up re-feed or on-demand IDR
// re-feed): applies any pending one-shot IDR request (armed by a capture
// rebuild - E2 contract: it rides the next submission exactly once and is
// never re-armed), encodes, shapes and delivers every output AU to the
// sink. Each successful input is registered immediately before submission;
// output AUs consume the oldest registration because MFT lookahead means an
// AU generally belongs to an earlier Encode call. Runs on the ENCODE thread;
// enc_mu is held by the caller for the whole call (the encoder + its SpsPps
// cache must stay consistent with the AU shaping).
void ProcessFrameLocked(EncodeCtx& ctx, const FrameBlob& f, bool warmup) {
  // One-shot rebuild IDR request (E2 contract: ride the next submission).
  const char* idr_reason = nullptr;
  {
    std::lock_guard<std::mutex> lk(ctx.sh.mu);
    idr_reason = ctx.sh.cache.TakePendingIdrReason();
  }
  // enc_mu held: the one-shot flag is encoder stream state (Init clears it).
  if (idr_reason != nullptr) ctx.enc.ForceNextIdr(idr_reason);

  // Register at the input boundary, not at capture time: dropped handoff
  // frames never enter the MFT and therefore never enter this ledger.
  // M1 Task 1: assign encode_seq + present_mono_us at successful submission
  // (spec §5.1/§5.2); the identity FIFO pairs 1:1 with SubmissionLedger so
  // the M0 timestamp pairing stays exact (ruling 2).
  FrameIdentity id;
  id.capture_epoch = f.capture_epoch;
  id.codec_epoch = f.codec_epoch;
  id.content_id = f.content_id;
  id.source_mono_us = f.source_mono_us;
  {
    std::lock_guard<std::mutex> lk(ctx.sh.mu);
    id.encode_seq = ++ctx.sh.next_encode_seq;
    id.present_mono_us = f.mono_us;
    if (!ctx.sh.identity_ledger.Accept(id)) {
      ctx.sh.res->ok = false;
      ctx.sh.res->err = "encoder_identity_monotonicity";
      XNC_LOG_ERROR("encoder_identity_monotonicity cap_epoch=%llu codec_epoch=%llu content=%llu seq=%llu",
                    static_cast<unsigned long long>(id.capture_epoch),
                    static_cast<unsigned long long>(id.codec_epoch),
                    static_cast<unsigned long long>(id.content_id),
                    static_cast<unsigned long long>(id.encode_seq));
      ctx.fatal.store(true, std::memory_order_relaxed);
      return;
    }
    if (!ctx.sh.submissions.Submit(f.mono_us)) {
      ctx.sh.res->ok = false;
      ctx.sh.res->err = "encoder_submission_overflow";
      XNC_LOG_ERROR("encoder_submission_overflow pending=%zu",
                    ctx.sh.submissions.Pending());
      ctx.fatal.store(true, std::memory_order_relaxed);
      return;
    }
    ctx.sh.pending_ids.push_back(id);
  }
  std::string eerr;
  // gpu-readback: route on the blob's layout - NV12 (DXGI GPU path) skips
  // the encoder's BGRA->NV12 conversion, BGRA (GDI / degraded DXGI) uses
  // the classic entry.
  const EncoderSubmitResult submit =
      f.pixfmt == Pixfmt::kNv12
          ? ctx.enc.EncodeNV12(f.bgra.data(), f.bgra.size(), ctx.aus, &eerr)
          : ctx.enc.Encode(f.bgra.data(), f.bgra.size(), ctx.aus, &eerr);
  if (!submit.input_accepted) {
    const std::string msg = eerr.empty() ? "encode failed" : eerr;
    {
      std::lock_guard<std::mutex> lk(ctx.sh.mu);
      // Encode/EncodeNV12 rejected this input. Only its just-added tail
      // registration is removed; older delayed submissions remain intact.
      if (!ctx.sh.submissions.RollbackNewest(f.mono_us)) {
        ctx.sh.res->ok = false;
        ctx.sh.res->err = "encoder_identity_mismatch";
      } else {
        ctx.sh.pending_ids.pop_back();  // M1: the paired identity rolls back
        ctx.sh.res->ok = false;
        ctx.sh.res->err = msg;
      }
    }
    XNC_LOG_ERROR("encode_failed err=\"%s\"", msg.c_str());
    ctx.fatal.store(true, std::memory_order_relaxed);
    return;
  }
  // ProcessInput accepted this registration even when output collection
  // failed later. Count the input and retain its identity while delivering
  // every complete AU CollectOutputs appended before that failure.
  {
    std::lock_guard<std::mutex> lk(ctx.sh.mu);
    if (warmup)
      ctx.sh.cache.OnWarmupFeed();
    else
      ctx.sh.cache.OnEncoded();
  }
  for (const auto& au : ctx.aus) {
    uint64_t au_mono_us = 0;
    uint32_t au_w = 0, au_h = 0;
    FrameIdentity au_id;
    {
      std::lock_guard<std::mutex> lk(ctx.sh.mu);
      if (!ctx.sh.submissions.Take(&au_mono_us) || ctx.sh.pending_ids.empty()) {
        ctx.sh.res->ok = false;
        ctx.sh.res->err = "encoder_identity_mismatch";
        XNC_LOG_ERROR("encoder_identity_mismatch source=encode");
        ctx.fatal.store(true, std::memory_order_relaxed);
        return;
      }
      au_id = ctx.sh.pending_ids.front();
      ctx.sh.pending_ids.pop_front();
      if (au_id.present_mono_us != au_mono_us) {
        // M1 Task 1 ruling 2: the identity FIFO must pair exactly with the
        // SubmissionLedger timestamp. A drift is a pairing break, never a
        // silent success.
        ctx.sh.res->ok = false;
        ctx.sh.res->err = "encoder_identity_mismatch";
        XNC_LOG_ERROR("encoder_identity_mismatch source=encode_pairing");
        ctx.fatal.store(true, std::memory_order_relaxed);
        return;
      }
      au_w = ctx.sh.enc_w;
      au_h = ctx.sh.enc_h;
    }
    const bool is_idr = NalHasType(au.data(), au.size(), 5);
    ShapeAu(au.data(), au.size(), is_idr, ctx.enc.SpsPps(), &ctx.shaped);
    if (!ctx.shaped.empty()) {
      // M1 Task 1: the sink receives one IMMUTABLE AU - the payload is
      // copied out of the reused shaped scratch into a shared const vector
      // so the AU outlives the call and can never be mutated in place.
      EncodedAU eau;
      eau.id = au_id;
      eau.width = au_w;
      eau.height = au_h;
      eau.flags = is_idr ? AuFlags::kAuFlagKey : AuFlags::kAuFlagNone;
      eau.annexb = std::make_shared<const std::vector<uint8_t>>(ctx.shaped);
      {
        std::lock_guard<std::mutex> lk(ctx.sh.mu);
        ctx.sh.res->aus_written++;
        ctx.sh.res->bytes_written += ctx.shaped.size();
      }
      if (const char* err = ctx.sink.OnAu(eau)) {
        {
          std::lock_guard<std::mutex> lk(ctx.sh.mu);
          ctx.sh.res->ok = false;
          ctx.sh.res->err = err;
        }
        XNC_LOG_ERROR("sink_onau_failed err=\"%s\"", err);
        ctx.fatal.store(true, std::memory_order_relaxed);
        return;
      }
      ctx.lat.Add(au_mono_us);  // actual submitted input -> OnAu completion
    }
    if (is_idr) {
      std::lock_guard<std::mutex> lk(ctx.sh.mu);
      ctx.sh.cache.OnKeyframeAu();  // ends warm-up (§7.4)
      if (ctx.sh.ondemand.armed) {
        XNC_LOG_INFO("idr_delivered reason=%s feeds=%u", ctx.sh.ondemand.reason,
                     ctx.sh.ondemand.feeds);
        ctx.sh.ondemand.armed = false;
      }
    }
  }
  if (!submit.outputs_ok) {
    const std::string msg = eerr.empty()
                                ? "encoder_output_collection_failed"
                                : "encoder_output_collection_failed: " + eerr;
    {
      std::lock_guard<std::mutex> lk(ctx.sh.mu);
      ctx.sh.res->ok = false;
      ctx.sh.res->err = msg;
    }
    XNC_LOG_ERROR("encode_output_collection_failed err=\"%s\"", eerr.c_str());
    ctx.fatal.store(true, std::memory_order_relaxed);
  }
}

void ProcessFrame(EncodeCtx& ctx, const FrameBlob& f, bool warmup) {
  std::lock_guard<std::mutex> elk(ctx.enc_mu);
  ProcessFrameLocked(ctx, f, warmup);
}

// One-time warm-up outcome logs per generation (moved with the re-feed
// logic from the old timeout path). Caller holds ctx.sh.mu.
void PhaseOutcomeLogs(EncodeCtx& ctx) {
  const bool have_base = !ctx.sh.cache.NeedsBaseFrame();
  if (!ctx.sh.warmup_phase_logged && have_base) {
    if (ctx.sh.cache.HaveKeyframe()) {
      ctx.sh.warmup_phase_logged = true;
      XNC_LOG_INFO("warmup_done feeds=%u keyframes=%llu", ctx.sh.warmup_gen_feeds,
                   static_cast<unsigned long long>(ctx.sh.cache.counters().keyframes));
    } else if (ctx.sh.warmup_gen_feeds >= ctx.warmup_feed_bound.load() ||
               (ctx.sh.warmup_started_ms != 0 &&
                NowMs() - ctx.sh.warmup_started_ms >= kWarmupWallBoundMs)) {
      ctx.sh.warmup_phase_logged = true;
      XNC_LOG_INFO("warmup_exhausted feeds=%u bound=%u keyframes=%llu",
                   ctx.sh.warmup_gen_feeds, ctx.warmup_feed_bound.load(),
                   static_cast<unsigned long long>(ctx.sh.cache.counters().keyframes));
    }
  }
  // On-demand window exhausted without an IDR: log once; the armed force
  // stays consumed and the IDR surfaces with the next real frame batch
  // (still never re-forced).
  if (ctx.sh.ondemand.armed && !ctx.sh.ondemand.exhaust_logged && have_base &&
      !ctx.sh.ondemand.FeedAllowed(NowMs(), ctx.warmup_feed_bound.load())) {
    ctx.sh.ondemand.exhaust_logged = true;
    XNC_LOG_INFO("idr_feed_exhausted reason=%s feeds=%u bound=%u",
                 ctx.sh.ondemand.reason, ctx.sh.ondemand.feeds,
                 ctx.warmup_feed_bound.load());
  }
}

// Encoder-side merged IDR request (spec §7.5): arm at most once per 500 ms,
// only after the stream's first keyframe (an in-progress initial warm-up
// will deliver that IDR anyway) and never while one is in flight. Polled
// once per encode loop iteration (the encode thread owns the encoder).
void PollIdrRequest(EncodeCtx& ctx) {
  const char* pending_reason = ctx.sink.PendingIdrReason();
  if (pending_reason == nullptr) return;
  bool arm = false;
  {
    std::lock_guard<std::mutex> lk(ctx.sh.mu);
    arm = ctx.sh.cache.HaveKeyframe() && !ctx.sh.ondemand.armed &&
          NowMs() - ctx.sh.last_initiated_idr_ms >= kIdrMinIntervalMs;
    if (arm) {
      ctx.sh.last_initiated_idr_ms = NowMs();
      ctx.sh.ondemand.Arm(pending_reason, NowMs());
    }
  }
  if (!arm) return;
  // enc_mu held: the one-shot flag is encoder stream state (Init clears it).
  std::lock_guard<std::mutex> elk(ctx.enc_mu);
  ctx.enc.ForceNextIdr(pending_reason);
  ctx.sink.ConsumePendingIdr(pending_reason);
  XNC_LOG_INFO("idr_request reason=%s min_interval_ms=%llu", pending_reason,
               static_cast<unsigned long long>(kIdrMinIntervalMs));
}

// Encode thread's idle path (queue empty = static screen): the warm-up /
// on-demand IDR re-feed of the latest captured frame (old timeout-path
// semantics, spec §7.4/§7.5). Gated on a recent capture timeout so
// re-feeds only happen on a static screen - under motion real frames flow
// and the lookahead fills naturally. Re-feeds are paced to spf (old
// SubmitFrame pacing: the encoder timestamps with the wall clock, so
// submit cadence == stream cadence even for synthetic submissions). The
// pace slot comes FIRST: a frame arriving during the wait is handed to the
// caller's pop path and the feed counters are only bumped when the feed
// actually fires (encoded == captured + warmup_feeds stays exact).
// Returns true when a submission happened.
bool IdleFeed(EncodeCtx& ctx) {
  // Pace the re-feed slot to the target cadence (old SubmitFrame pacing).
  if (ctx.last_feed_ms != 0) {
    const uint64_t since = NowMs() - ctx.last_feed_ms;
    if (since < ctx.spf_ms) Sleep(static_cast<DWORD>(ctx.spf_ms - since));
  }
  ctx.last_feed_ms = NowMs();
  if (ctx.queue.Done()) return false;  // shutdown landed mid-pace: stop
  // A real frame arrived while we slept: it wins over the re-feed.
  FrameBlob f;
  if (ctx.queue.TryPop(&f)) {
    ProcessFrame(ctx, f, false);
    return true;
  }
  FrameBlob feed_frame;
  uint64_t feed_generation = 0;
  bool candidate = false;
  {
    std::lock_guard<std::mutex> lk(ctx.sh.mu);
    const bool have_base = !ctx.sh.cache.NeedsBaseFrame();
    // Old semantics: re-feeds only ever ran on the timeout path. Replicate
    // via the last-timeout gate instead of the (now capture-side) loop.
    const bool static_screen =
        NowMs() - ctx.sh.last_timeout_ms <= ctx.static_grace_ms;
    const uint64_t warmup_elapsed =
        ctx.sh.warmup_started_ms != 0 ? NowMs() - ctx.sh.warmup_started_ms : 0;
    const bool warmup_feed_ok =
        static_screen && have_base && !ctx.sh.cache.HaveKeyframe() &&
        ctx.sh.warmup_gen_feeds < ctx.warmup_feed_bound.load() &&
        warmup_elapsed < kWarmupWallBoundMs;
    // On-demand IDR re-feed (static screen + armed subscriber request):
    // same source frame, same bounds, never a second force (§7.5).
    const bool ondemand_feed_ok =
        static_screen && have_base && ctx.sh.cache.HaveKeyframe() &&
        ctx.sh.ondemand.FeedAllowed(NowMs(), ctx.warmup_feed_bound.load());
    if ((warmup_feed_ok || ondemand_feed_ok) &&
        ctx.sh.latest.Snapshot(&feed_frame, &feed_generation)) {
      candidate = true;
    } else {
      PhaseOutcomeLogs(ctx);
    }
  }
  if (!candidate) return false;

  // Submission barrier + deterministic generation validation. Reset takes
  // these locks in the same order before WAIT_BASE_FRAME/Invalidate. Thus a
  // pre-reset snapshot either completes while holding enc_mu or is rejected
  // here; it can never consume the rebuild IDR after invalidation.
  std::lock_guard<std::mutex> elk(ctx.enc_mu);
  {
    std::lock_guard<std::mutex> lk(ctx.sh.mu);
    if (ctx.sh.cache.NeedsBaseFrame() ||
        !ctx.sh.latest.IsCurrent(feed_generation)) {
      return false;
    }
    if (!ctx.sh.cache.HaveKeyframe()) {
      ++ctx.sh.warmup_gen_feeds;
    } else {
      ++ctx.sh.ondemand.feeds;
    }
  }
  feed_frame.mono_us = NowMonoUs();
  ProcessFrameLocked(ctx, feed_frame, true);
  return true;
}

// The encode thread: drains the frame queue at encode speed (no pacing -
// the capture thread paces pushes to spf). While the queue is empty (static
// screen) it idles with periodic warm-up / on-demand re-feed checks and the
// merged IDR poll; at run end it drains everything queued (nothing captured
// is dropped), then exits.
void EncodeLoop(EncodeCtx& ctx) {
  FrameBlob f;
  for (;;) {
    if (ctx.fatal.load(std::memory_order_relaxed)) break;
    PollIdrRequest(ctx);
    if (ctx.queue.TryPop(&f)) {
      ProcessFrame(ctx, f, false);
      continue;
    }
    if (ctx.queue.Done()) break;
    // Queue empty: warm-up / on-demand re-feed path (static screen).
    if (IdleFeed(ctx)) continue;
    if (ctx.queue.WaitPop(&f, kIdleSleepMs)) ProcessFrame(ctx, f, false);
  }
  ctx.lat.Flush();  // partial window at run end
}

PipelineResult RunCore(ICapture& cap, MfSoftEncoder& enc, AuSink& sink,
                       const PipelineOpts& opt) {
  PipelineResult res;
  res.width = cap.Width();
  res.height = cap.Height();
  if (opt.fps == 0) {
    res.ok = false;
    res.err = "fps must be > 0";
    return res;
  }
  TimePeriodGuard tpg;  // 1 ms timer granularity for the pacing Sleeps

  // M3 Task 3: spf is LIVE when fps_hint is wired (SET_VIDEO_CONFIG hot
  // fps); otherwise it stays the fixed opts.fps derivation (pre-M3 path).
  uint32_t spf_ms = 1000u / opt.fps;
  const uint32_t warmup_feed_bound = WarmupFeedBound(opt.fps);

  PipelineShared sh;
  sh.res = &res;
  sh.cache.Start();
  {
    std::lock_guard<std::mutex> lk(sh.mu);
    sh.enc_w = cap.Width();
    sh.enc_h = cap.Height();
  }

  // Bounded handoff: depth 2 = one frame in flight in the encoder + one
  // pending; overflow drops the OLDEST so the encoder always sees the
  // latest frame (ADR-014). Frames are owned copies - the capture's scaled
  // buffer is valid only until the next Acquire.
  FrameQueue queue(2);
  EncodeCtx ctx(enc, sink, sh, queue, opt, spf_ms, warmup_feed_bound);
  std::thread encode_th(EncodeLoop, std::ref(ctx));

  FrameBlob blob;
  std::string acq_err;

  const uint64_t t0 = NowMs();
  const uint64_t duration_ms = static_cast<uint64_t>(opt.duration_s) * 1000ull;
  uint32_t next_beat_s = 1;
  // Capture-side pacing: frame-index-aligned absolute deadline (push_t0 +
  // push_count*spf) instead of relative sleep - relative pacing drifts and
  // jitters with acquire latency variance, and the RTP timeline mirrors the
  // capture cadence: interval jitter shows up as playback speed wobble
  // (jelly effect). Absolute deadline keeps every interval exactly spf
  // (1 ms timer resolution), recovering naturally after slow acquires.
  uint64_t push_t0 = 0;
  uint64_t push_count = 0;
  bool first_frame_logged = false;
  LatencyWindow cap_win;  // capture thread cycle (acquire start -> push done)
  cap_win.label = "cap_cycle_ms";
  LatencyWindow acq_win;  // capture-side acquire+downscale duration
  acq_win.label = "cap_acq_ms";
  DurationWindow gpu_win;  // gpu-readback: GPU scale+NV12+readback duration
  gpu_win.label = "gpu_scale_ms";
  ResetStormTracker storm;  // same-reason rebuild-storm backoff (M2-S2 T1)

  for (;;) {
    if (NowMs() - t0 >= duration_ms) break;
    if (opt.stop != nullptr && opt.stop->load()) break;
    if (ctx.fatal.load(std::memory_order_relaxed)) break;  // encode died

    // M3 Task 3: hot fps hint (SET_VIDEO_CONFIG). Checked once per capture
    // iteration; a change re-keys spf for the acquire timeout and the pacing
    // deadline, rebases the absolute-deadline anchor (so the NEXT interval is
    // exactly the new spf instead of carrying the old cadence), and pushes
    // the fresh spf/warm-up bound into the encode context (atomics).
    if (opt.fps_hint != nullptr) {
      const uint32_t want_fps = opt.fps_hint->load(std::memory_order_relaxed);
      if (want_fps != 0) {
        const uint32_t want_spf = 1000u / want_fps;
        if (want_spf != spf_ms) {
          spf_ms = want_spf;
          if (push_t0 != 0)
            push_t0 = NowMs() - static_cast<uint64_t>(push_count) * spf_ms;
          ctx.spf_ms.store(spf_ms, std::memory_order_relaxed);
          ctx.warmup_feed_bound.store(WarmupFeedBound(want_fps),
                                      std::memory_order_relaxed);
          XNC_LOG_INFO("fps_hint applied fps=%u spf_ms=%u", want_fps, spf_ms);
        }
      }
    }

    // Unified capture reset (M2-Slice1 Task 2): consume a merged/debounced
    // request and run suspend -> wait-desktop -> rebuild -> resume. A
    // same-reason storm (>= kThreshold inside the window) backs off
    // exponentially first (M2-Slice2 Task 1).
    if (opt.reset != nullptr) {
      char reset_reason[kResetReasonMax];
      if (opt.reset->TakeReset(reset_reason, sizeof(reset_reason))) {
        if (uint32_t backoff = storm.BackoffMs(reset_reason, NowMs())) {
          storm.NoteStorm();
          XNC_LOG_INFO("reset_storm reason=%s backoff_ms=%u",
                       reset_reason, backoff);
          const uint64_t storm_until = NowMs() + backoff;
          bool storm_abort = false;
          while (NowMs() < storm_until) {
            if ((opt.stop != nullptr && opt.stop->load()) ||
                NowMs() - t0 >= duration_ms ||
                ctx.fatal.load(std::memory_order_relaxed)) {
              storm_abort = true;
              break;
            }
            Sleep(kResetPollMs);
          }
          if (storm_abort) break;
        }
        storm.RecordExecuted(reset_reason, NowMs());
        ResetSequence seq{&cap,     &enc,    &sink,  &sh,      &opt,
                          &queue,   &ctx.enc_mu, &ctx.fatal,
                          t0,       duration_ms,
                          reset_reason};
        if (RunResetSequence(seq)) break;
        continue;  // re-poll stop/duration/pending resets before acquiring
      }
    }

    acq_err.clear();
    // Acquire with timeout = spf: under motion the backend returns as soon
    // as a new frame presents; on a static screen it wakes every spf and
    // reports err_timeout (the old 100 ms backend default would stall the
    // paced capture loop).
    const uint64_t t_acq0 = NowMonoUs();
    if (cap.Acquire(blob, &acq_err, spf_ms)) {
      acq_win.Add(t_acq0);  // acquire (incl. downscale) duration window
      if (blob.gpu_scale_us != 0) gpu_win.Add(blob.gpu_scale_us);
      // Frame-size change (M2-S1 Task 2): the backend adopted a new mode
      // but the encoder is still at the old size - route to the unified
      // reset (resolution) instead of feeding a wrong-sized frame in.
      uint32_t ew = 0, eh = 0;
      {
        std::lock_guard<std::mutex> lk(sh.mu);
        ew = sh.enc_w;
        eh = sh.enc_h;
      }
      if (opt.reset != nullptr && (blob.w != ew || blob.h != eh)) {
        opt.reset->RequestReset(kResetReasonResolution);
        Sleep(kIdleSleepMs);  // ride the debounce window
        continue;
      }
      // Capture bookkeeping + latest-frame snapshot under the shared lock:
      // every accepted capture refreshes the idle re-feed source, while a
      // new base resets the generation's warm-up counters.
      {
        std::lock_guard<std::mutex> lk(sh.mu);
        const bool is_base = sh.cache.OnCapturedFrame();  // captured++ inside
        // M1 Task 1: content identity assigned at capture (spec §5.1/§5.2):
        // the current generations, a fresh content_id per accepted content
        // and this capture's timestamp as source time. encode_seq and
        // present_mono_us are added at successful submission (encode thread).
        blob.capture_epoch = sh.capture_epoch;
        blob.codec_epoch = sh.codec_epoch;
        blob.content_id = ++sh.next_content_id;
        blob.source_mono_us = blob.mono_us;
        sh.latest.Update(blob);
        if (is_base) {
          res.width = blob.w;
          res.height = blob.h;
          sh.warmup_started_ms = 0;  // re-anchored at this generation's first submit
          sh.warmup_gen_feeds = 0;
          sh.warmup_phase_logged = false;
          XNC_LOG_INFO("base_frame w=%u h=%u state=%s", blob.w, blob.h,
                       sh.cache.StateName());
        }
        if (sh.warmup_started_ms == 0) sh.warmup_started_ms = NowMs();
      }
      if (!first_frame_logged) {
        first_frame_logged = true;
        const size_t head = blob.bgra.size() < 64 ? blob.bgra.size() : 64;
        const unsigned long long hash =
            static_cast<unsigned long long>(Fnv1a64(blob.bgra.data(), head));
        // gpu-readback: the non-black probe reads Y values for NV12 blobs
        // (BGRA pixels otherwise - NV12 is not 4 bytes/pixel).
        const bool non_black =
            blob.pixfmt == Pixfmt::kNv12
                ? Nv12YNotUniform(blob.bgra.data(), blob.w, blob.h)
                : SamplePointsNotUniform(blob.bgra.data(), blob.w, blob.h);
        XNC_LOG_INFO("first_frame hash_head64=%016llx non_black=%d w=%u h=%u mono_us=%llu pixfmt=%s",
                     hash, non_black ? 1 : 0, blob.w, blob.h,
                     static_cast<unsigned long long>(blob.mono_us),
                     blob.pixfmt == Pixfmt::kNv12 ? "nv12" : "bgra");
        if (!non_black)
          XNC_LOG_ERROR("first_frame_uniform (all 256 sampled pixels equal - suspect black/garbage frame)");
      }
      // Hand the scaled frame over: owned copy of the capture's persistent
      // buffer (valid only until the next Acquire) + the capture timestamp.
      FrameBlob handoff;
      handoff.bgra = blob.bgra;
      handoff.w = blob.w;
      handoff.h = blob.h;
      handoff.pixfmt = blob.pixfmt;
      // M1 Task 1: the content identity rides with the handoff frame so the
      // encode thread pairs every AU back to its captured content.
      handoff.capture_epoch = blob.capture_epoch;
      handoff.codec_epoch = blob.codec_epoch;
      handoff.content_id = blob.content_id;
      handoff.source_mono_us = blob.source_mono_us;
      // Capture-side pacing: frame-index-aligned absolute deadline, sleep
      // BEFORE the push. The acquire waits for the compositor (0..16.7ms at
      // 60Hz) and this sleep tops the interval up to exactly spf - without
      // it the compositor phase leaks into the frame interval (measured
      // p50 38.9ms vs target 33.3ms), and since the RTP timeline mirrors
      // the capture cadence that wobble shows up as playback speed uneven
      // (jelly effect). A slow acquire pushes the deadline but the next
      // interval snaps back to spf.
      if (push_t0 == 0) push_t0 = NowMs();
      push_count++;
      const uint64_t deadline = push_t0 + push_count * spf_ms;
      const uint64_t now_ms = NowMs();
      if (deadline > now_ms) Sleep(static_cast<DWORD>(deadline - now_ms));
      // Timestamp at the PUSH moment (deadline-aligned), not at acquire
      // return: acquire return is quantized to the compositor's 60Hz grid
      // (0..16.7ms wait), so acquire-return timestamps beat against the
      // 33ms cadence and the RTP interval distribution gets a 33-50ms
      // wobble (measured p50 39ms). The RTP timeline mirrors this timestamp
      // - push-time stamps keep playback intervals exactly spf.
      handoff.mono_us = NowMonoUs();
      queue.Push(std::move(handoff));
      cap_win.Add(t_acq0);  // acquire start -> push done (XIAOXIN split diag)
    } else if (acq_err == "err_timeout") {
      // Static screen: no frame, no handoff - the encode thread idles (and
      // runs the warm-up / on-demand re-feed logic itself). Record the
      // observation for the re-feed gate + counters.
      {
        std::lock_guard<std::mutex> lk(sh.mu);
        sh.cache.OnTimeout();  // static screen: no encode, no packet (§7.4)
        sh.last_timeout_ms = NowMs();
      }
      Sleep(kIdleSleepMs);  // keep the capture loop off the CPU (fakes)
    } else if (acq_err == "err_rebuilt") {
      // Backend rebuilt its duplication in place: rewind the state machine
      // (next frame is the new base, full readback) and arm the one-shot
      // "rebuild" IDR; the old latest snapshot is invalid across rebuild.
      {
        std::lock_guard<std::mutex> elk(ctx.enc_mu);
        std::lock_guard<std::mutex> lk(sh.mu);
        sh.cache.OnRebuild();
        sh.latest.Invalidate();
        sh.capture_epoch++;  // M1 Task 1: in-place rebuild = new capture generation
        sh.warmup_started_ms = 0;
        sh.warmup_gen_feeds = 0;
        sh.warmup_phase_logged = false;
      }
      sink.OnState("capture_rebuilt", true);
      XNC_LOG_INFO("capture_rebuild handled rebuilds=%u state=%s",
                   sh.cache.counters().rebuilds, sh.cache.StateName());
    } else if (acq_err == "err_access_lost") {
      // M2-Slice1 Task 2: the internal rebuild was refused (secure desktop
      // holds the output) or no duplication exists. Route into the unified
      // reset; callers that never wired a coordinator keep the legacy
      // fatal behavior.
      if (opt.reset == nullptr) {
        res.ok = false;
        res.err = acq_err;
        sink.OnState("capture_fatal", false);
        XNC_LOG_ERROR("acquire_failed err=\"%s\"", res.err.c_str());
        break;
      }
      const bool away = opt.reset->Desktop() == ResetDesktop::kNonDefault;
      opt.reset->RequestReset(
          away ? kResetReasonDesktopSwitch : kResetReasonAccessLost);
      Sleep(kIdleSleepMs);  // debounce window: repeated errors merge
    } else {
      res.ok = false;
      res.err = acq_err.empty() ? "acquire failed" : acq_err;
      sink.OnState("capture_fatal", false);
      XNC_LOG_ERROR("acquire_failed err=\"%s\"", res.err.c_str());
      break;
    }

    const uint32_t elapsed_s = static_cast<uint32_t>((NowMs() - t0) / 1000);
    if (elapsed_s >= next_beat_s) {
      FrameCacheCounters c;
      uint32_t w = 0, h = 0;
      uint64_t aus = 0, bytes = 0;
      {
        std::lock_guard<std::mutex> lk(sh.mu);
        c = sh.cache.counters();
        w = res.width;
        h = res.height;
        aus = res.aus_written;
        bytes = res.bytes_written;
      }
      const char* desktop =
          opt.desktop_name_fn != nullptr
              ? opt.desktop_name_fn(opt.desktop_name_ctx)
              : nullptr;
      if (desktop != nullptr && *desktop != '\0') {
        XNC_LOG_INFO("diag_pipeline elapsed=%us captured=%llu encoded=%llu keyframes=%llu timeouts=%llu warmup_feeds=%llu rebuilds=%u w=%u h=%u aus=%llu bytes=%llu desktop=%s",
                     elapsed_s, static_cast<unsigned long long>(c.captured),
                     static_cast<unsigned long long>(c.encoded),
                     static_cast<unsigned long long>(c.keyframes),
                     static_cast<unsigned long long>(c.timeouts),
                     static_cast<unsigned long long>(c.warmup_feeds), c.rebuilds, w,
                     h, static_cast<unsigned long long>(aus),
                     static_cast<unsigned long long>(bytes), desktop);
      } else {
        XNC_LOG_INFO("diag_pipeline elapsed=%us captured=%llu encoded=%llu keyframes=%llu timeouts=%llu warmup_feeds=%llu rebuilds=%u w=%u h=%u aus=%llu bytes=%llu",
                     elapsed_s, static_cast<unsigned long long>(c.captured),
                     static_cast<unsigned long long>(c.encoded),
                     static_cast<unsigned long long>(c.keyframes),
                     static_cast<unsigned long long>(c.timeouts),
                     static_cast<unsigned long long>(c.warmup_feeds), c.rebuilds, w,
                     h, static_cast<unsigned long long>(aus),
                     static_cast<unsigned long long>(bytes));
      }
      next_beat_s = elapsed_s + 1;
    }
  }

  // Shutdown: wake the encode thread, let it drain everything queued (no
  // captured frame is dropped at run end), then join.
  queue.Shutdown();
  encode_th.join();
  cap_win.Flush();  // partial capture-cycle window at run end
  acq_win.Flush();  // partial acquire window at run end
  gpu_win.Flush();  // partial gpu-scale window at run end

  // End of run: collect any newly available output, then flush the encoder
  // tail (NOTIFY_DRAIN) so the lookahead window's AUs are not dropped. Both
  // paths consume one submission identity per raw AU. The encode thread is
  // gone; enc_mu also preserves the reset lock-order contract here.
  {
    std::vector<std::vector<uint8_t>> aus;
    std::vector<uint8_t> shaped;
    std::lock_guard<std::mutex> elk(ctx.enc_mu);
    const auto deliver = [&](const char* source, bool flushing) {
      for (const auto& au : aus) {
        uint64_t au_mono_us = 0;
        uint32_t au_w = 0, au_h = 0;
        FrameIdentity au_id;
        {
          std::lock_guard<std::mutex> lk(sh.mu);
          if (!sh.submissions.Take(&au_mono_us) || sh.pending_ids.empty()) {
            res.ok = false;
            res.err = "encoder_identity_mismatch";
            XNC_LOG_ERROR("encoder_identity_mismatch source=%s", source);
            return false;
          }
          au_id = sh.pending_ids.front();
          sh.pending_ids.pop_front();
          if (au_id.present_mono_us != au_mono_us) {
            // M1 Task 1 ruling 2: identity FIFO must pair with SubmissionLedger.
            res.ok = false;
            res.err = "encoder_identity_mismatch";
            XNC_LOG_ERROR("encoder_identity_mismatch source=%s_pairing", source);
            return false;
          }
          au_w = sh.enc_w;
          au_h = sh.enc_h;
        }
        const bool is_idr = NalHasType(au.data(), au.size(), 5);
        ShapeAu(au.data(), au.size(), is_idr, enc.SpsPps(), &shaped);
        if (!shaped.empty()) {
          EncodedAU eau;
          eau.id = au_id;
          eau.width = au_w;
          eau.height = au_h;
          eau.flags = is_idr ? AuFlags::kAuFlagKey : AuFlags::kAuFlagNone;
          eau.annexb = std::make_shared<const std::vector<uint8_t>>(shaped);
          res.aus_written++;
          res.bytes_written += shaped.size();
          const char* err = sink.OnAu(eau);
          if (err != nullptr) {
            res.ok = false;
            res.err = flushing && std::strcmp(err, "fwrite out failed") == 0
                          ? "fwrite out failed (flush)"
                          : err;
            return false;
          }
          ctx.lat.Add(au_mono_us);
        }
        if (is_idr) {
          std::lock_guard<std::mutex> lk(sh.mu);
          sh.cache.OnKeyframeAu();
        }
      }
      return true;
    };

    if (res.ok) {
      std::string drain_err;
      const bool drain_ok = enc.Drain(aus, &drain_err);
      if (!aus.empty())
        XNC_LOG_INFO("encoder_drain aus=%zu", aus.size());
      deliver("drain", false);
      if (res.ok && !drain_ok) {
        res.ok = false;
        res.err = drain_err.empty() ? "encoder_drain_failed"
                                    : "encoder_drain_failed: " + drain_err;
        XNC_LOG_ERROR("encoder_drain_failed err=\"%s\"", drain_err.c_str());
      }
    }
    if (res.ok) {
      aus.clear();
      std::string flush_err;
      const bool flush_ok = enc.FlushTail(aus, &flush_err);
      if (!aus.empty())
        XNC_LOG_INFO("encoder_flush_tail aus=%zu", aus.size());
      deliver("flush_tail", true);
      if (res.ok && !flush_ok) {
        res.ok = false;
        res.err = flush_err.empty() ? "encoder_flush_tail_failed"
                                    : "encoder_flush_tail_failed: " + flush_err;
        XNC_LOG_ERROR("encoder_flush_tail_failed err=\"%s\"", flush_err.c_str());
      }
    }

    // No delayed output can survive stream end or a fatal encoder/sink
    // path. A nominally successful drain must account for every accepted
    // input; unresolved identities are a fatal mismatch, never success.
    size_t discarded = 0;
    size_t id_discarded = 0;
    {
      std::lock_guard<std::mutex> lk(sh.mu);
      discarded = sh.submissions.Pending();
      id_discarded = sh.pending_ids.size();
      if (res.ok && discarded != 0) {
        res.ok = false;
        res.err = "encoder_identity_mismatch";
        XNC_LOG_ERROR("encoder_identity_mismatch source=stream_end pending=%zu",
                      discarded);
      }
      if (id_discarded != discarded) {
        // M1 Task 1 ruling 2: the paired FIFOs must agree at stream end; a
        // drift is a pairing break, never a silent success.
        if (res.ok) {
          res.ok = false;
          res.err = "encoder_identity_mismatch";
        }
        XNC_LOG_ERROR("encoder_identity_mismatch source=stream_end_pairing submissions=%zu ids=%zu",
                      discarded, id_discarded);
      }
      sh.submissions.Clear();
      sh.pending_ids.clear();
    }
    if (discarded != 0) {
      XNC_LOG_INFO("encoder_submission_clear pending=%zu reason=%s", discarded,
                   res.ok ? "stream_end" : "stream_abort");
    }
  }
  ctx.lat.Flush();  // drain/FlushTail may have completed the final window
  sink.OnState("stream_end", res.ok);

  {
    std::lock_guard<std::mutex> lk(sh.mu);
    res.counters = sh.cache.counters();
  }
  res.storm_resets = storm.storm_resets();
  const FrameCacheCounters& c = res.counters;
  XNC_LOG_INFO("pipeline_stop elapsed=%llums captured=%llu encoded=%llu keyframes=%llu timeouts=%llu warmup_feeds=%llu rebuilds=%u resets=%u w=%u h=%u aus=%llu bytes=%llu ok=%d",
               static_cast<unsigned long long>(NowMs() - t0),
               static_cast<unsigned long long>(c.captured),
               static_cast<unsigned long long>(c.encoded),
               static_cast<unsigned long long>(c.keyframes),
               static_cast<unsigned long long>(c.timeouts),
               static_cast<unsigned long long>(c.warmup_feeds), c.rebuilds,
               res.resets, res.width, res.height,
               static_cast<unsigned long long>(res.aus_written),
               static_cast<unsigned long long>(res.bytes_written), res.ok ? 1 : 0);
  return res;
}

}  // namespace

PipelineResult Pipeline::Run(ICapture& cap, MfSoftEncoder& enc, FILE* out,
                             const PipelineOpts& opt) {
  if (out == nullptr) {
    PipelineResult res;
    res.ok = false;
    res.err = "out file is null";
    return res;
  }
  FileAuSink sink(out);
  return RunCore(cap, enc, sink, opt);
}

PipelineResult Pipeline::Run(ICapture& cap, MfSoftEncoder& enc, AuSink& sink,
                             const PipelineOpts& opt) {
  return RunCore(cap, enc, sink, opt);
}

PipelineResult Pipeline::Run(ICapture& cap, MfSoftEncoder& enc, FILE* out,
                             AuSink& extra, const PipelineOpts& opt) {
  if (out == nullptr) {
    PipelineResult res;
    res.ok = false;
    res.err = "out file is null";
    return res;
  }
  FileAuSink file_sink(out);
  TeeAuSink tee(&file_sink, &extra);
  return RunCore(cap, enc, tee, opt);
}

// ---- M2 Task 6: stage histogram (pipeline.h) ----

StageHistogram::StageHistogram(const char* name, size_t capacity)
    : name_(name != nullptr ? name : "") {
  ring_.assign(capacity, 0);
}

void StageHistogram::Add(uint64_t sample) {
  if (ring_.empty()) return;
  ring_[head_] = sample;
  head_ = (head_ + 1) % ring_.size();
  if (count_ < ring_.size()) ++count_;
}

bool StageHistogram::Percentile(double p, uint64_t* out) const {
  if (out == nullptr || count_ == 0 || !(p > 0.0) || p > 100.0) return false;
  // Not full yet: the live samples are [0, count_). Full: every slot is a
  // live sample (order does not matter - the buffer is sorted next).
  if (count_ < ring_.size()) {
    scratch_.assign(ring_.begin(), ring_.begin() + count_);
  } else {
    scratch_.assign(ring_.begin(), ring_.end());
  }
  std::sort(scratch_.begin(), scratch_.end());
  // Nearest-rank: rank = ceil(p/100 * n), 1-based; value = rank-th smallest.
  double rank = std::ceil(p / 100.0 * static_cast<double>(count_));
  if (rank < 1.0) rank = 1.0;
  size_t idx = static_cast<size_t>(rank) - 1;
  if (idx >= count_) idx = count_ - 1;
  *out = scratch_[idx];
  return true;
}

StageHistogram::Percentiles StageHistogram::Summary() const {
  Percentiles out;
  uint64_t v = 0;
  if (!Percentile(50.0, &v)) return out;
  out.n = static_cast<uint64_t>(count_);
  out.p50 = v;
  if (!Percentile(95.0, &out.p95) || !Percentile(99.0, &out.p99)) {
    out = Percentiles{};
    return out;
  }
  out.has_samples = true;
  return out;
}

void StageHistogram::Reset() {
  count_ = 0;
  head_ = 0;
}

std::string FormatStageLog(const StageStat* stats, size_t n) {
  std::string out;
  for (size_t i = 0; i < n; ++i) {
    if (!stats[i].p.has_samples) continue;  // zero-sample: absent, not zero
    char buf[96];
    std::snprintf(buf, sizeof(buf), "%s%s=%llu/%llu/%llu(n=%llu)",
                  out.empty() ? "" : " ", stats[i].key,
                  static_cast<unsigned long long>(stats[i].p.p50),
                  static_cast<unsigned long long>(stats[i].p.p95),
                  static_cast<unsigned long long>(stats[i].p.p99),
                  static_cast<unsigned long long>(stats[i].p.n));
    out += buf;
  }
  return out;
}

const char* StageSemanticsNote() {
  // JSON-safe (no double quotes). Fix round 1: the semantics must live in
  // the EMITTED output, not only in source comments - every histogram line
  // and sidecar carries this note (or is preceded by the log preamble).
  return "loop-thread wall time in us; gpu_copy_us is wait-inclusive "
         "(AcquireNextFrame compositor wait up to spf_ms plus the async "
         "CopyResource enqueue), gpu_convert_us is the async "
         "VideoProcessorBlt enqueue cost, mft_submit_to_output_us and "
         "capture_to_au_us include the encoder rung lookahead, "
         "inflight_slots counts pool leases (0-3), queue_age_us is the "
         "depth-one mailbox wait; read cpu_readbacks together with "
         "encoder_backend - the software rung maps NV12 once per submit by "
         "design, the capture/convert stages never read back";
}

std::string FormatStagesJson(const StageStat* stats, size_t n,
                             uint64_t cpu_readbacks,
                             const char* encoder_backend) {
  std::string members;
  for (size_t i = 0; i < n; ++i) {
    if (!stats[i].p.has_samples) continue;  // zero-sample: absent, not zero
    char buf[128];
    std::snprintf(buf, sizeof(buf),
                  "    \"%s\": { \"n\": %llu, \"p50\": %llu, \"p95\": %llu, "
                  "\"p99\": %llu }",
                  stats[i].key, static_cast<unsigned long long>(stats[i].p.n),
                  static_cast<unsigned long long>(stats[i].p.p50),
                  static_cast<unsigned long long>(stats[i].p.p95),
                  static_cast<unsigned long long>(stats[i].p.p99));
    if (!members.empty()) members += ",\n";
    members += buf;
  }
  if (members.empty()) return std::string();
  // Separators go BETWEEN members only: a trailing comma before the closing
  // brace would make stats.json invalid JSON.
  std::string out = "  \"stages\": {\n";
  out += "    \"stage_semantics\": \"";
  out += StageSemanticsNote();
  out += "\",\n";
  out += members;
  out += "\n  },\n";
  char buf[96];
  std::snprintf(buf, sizeof(buf), "  \"cpu_readbacks\": %llu,\n",
                static_cast<unsigned long long>(cpu_readbacks));
  out += buf;
  std::snprintf(buf, sizeof(buf), "  \"encoder_backend\": \"%s\",\n",
                encoder_backend != nullptr ? encoder_backend : "");
  out += buf;
  return out;
}

std::string FormatStatsJson(const PipelineResult& r, const PipelineOpts& o) {
  char buf[640];
  std::snprintf(buf, sizeof(buf),
                "{\n"
                "  \"duration_s\": %u,\n"
                "  \"width\": %u,\n"
                "  \"height\": %u,\n"
                "  \"fps\": %u,\n"
                "  \"bitrate_bps\": %u,\n"
                "  \"captured\": %llu,\n"
                "  \"encoded\": %llu,\n"
                "  \"keyframes\": %llu,\n"
                "  \"timeouts\": %llu,\n"
                "  \"warmup_feeds\": %llu,\n"
                "  \"rebuilds\": %u,\n"
                "  \"resets\": %u,\n"
                "  \"aus_written\": %llu,\n"
                "  \"bytes_written\": %llu,\n",
                o.duration_s, r.width, r.height, o.fps, o.target_bitrate_bps,
                static_cast<unsigned long long>(r.counters.captured),
                static_cast<unsigned long long>(r.counters.encoded),
                static_cast<unsigned long long>(r.counters.keyframes),
                static_cast<unsigned long long>(r.counters.timeouts),
                static_cast<unsigned long long>(r.counters.warmup_feeds),
                r.counters.rebuilds, r.resets,
                static_cast<unsigned long long>(r.aus_written),
                static_cast<unsigned long long>(r.bytes_written));
  std::string out(buf);
  out += r.stages_json;  // M2 Task 6: empty for M0 = byte-identical sidecar
  char tail[32];
  std::snprintf(tail, sizeof(tail), "  \"ok\": %d\n}\n", r.ok ? 1 : 0);
  out += tail;
  return out;
}

bool WriteStatsJson(const std::wstring& h264_out_path, const PipelineResult& r,
                    const PipelineOpts& o, std::wstring* err) {
  const size_t slash = h264_out_path.find_last_of(L"\\/");
  const std::wstring dir = slash == std::wstring::npos
                               ? std::wstring(L".")
                               : h264_out_path.substr(0, slash + 1);
  const std::wstring path = dir + L"stats.json";
  FILE* f = nullptr;
  if (_wfopen_s(&f, path.c_str(), L"wb") != 0 || f == nullptr) {
    if (err) *err = L"open stats.json failed: " + path;
    return false;
  }
  const std::string json = FormatStatsJson(r, o);
  const bool ok = std::fwrite(json.data(), 1, json.size(), f) == json.size();
  std::fclose(f);
  if (!ok && err) *err = L"write stats.json failed: " + path;
  return ok;
}

}  // namespace xnc
