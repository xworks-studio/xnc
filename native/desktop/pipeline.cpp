// pipeline.cpp - Pipeline::Run + stats.json writers (see pipeline.h).
// Loop shape (plan Task 5, spec §7.4/§7.5):
//   Acquire -> frame (base/incremental via FrameCache) | err_timeout (no
//   encode; warm-up re-feed while no keyframe AU yet) | err_rebuilt
//   (FrameCache.OnRebuild + one-shot "rebuild" force) | fatal (stop)
//   -> Encode -> shape every AU (SPS/PPS prefix on IDR, 4B start codes,
//   AUD dropped) -> sink->OnAu; per-second counter beat; at end FlushTail
//   recovers the lookahead window's AUs through the same shaping path.
//
// M1-Slice2 Task 2: the loop emits through an AuSink. The diag FILE* dump
// is FileAuSink below (identical behavior to the pre-slice code, including
// error strings); RtServer is the fan-out sink. On-demand IDR (spec §7.5 +
// Slice1 carry-over): while the screen is static a subscriber join
// (PendingIdrReason, e.g. "sub_join") arms ONE ForceNextIdr - at most once
// per kIdrMinIntervalMs, only after the stream's first keyframe, never
// while a previous request is in flight - and the cached base frame is
// re-fed on the timeout path (same bounds as warm-up: 2 x lookahead window
// or 2 s; never re-forcing) until the IDR AU emerges (on-demand warm-up).
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>  // Sleep, GetTickCount64

#include <cerrno>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <string>
#include <vector>

#include "../common/log.h"
#include "dxgi_capture.h"  // Fnv1a64, SamplePointsNotUniform (first-frame diag)
#include "pipeline.h"

namespace xnc {
namespace {

uint64_t NowMs() { return GetTickCount64(); }

// Idle nap between do-nothing timeouts (static screen after warm-up): real
// backends block inside Acquire (~100 ms), fakes return instantly - this
// keeps the loop off the CPU in both cases.
constexpr DWORD kIdleSleepMs = 15;

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
// cached base frame, bounded like the initial warm-up and never re-forcing
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
// every shaped AU; fatal error string "fwrite out failed").
class FileAuSink final : public AuSink {
 public:
  explicit FileAuSink(FILE* f) : f_(f) {}
  const char* OnAu(bool, uint64_t, const uint8_t* au, size_t len) override {
    if (std::fwrite(au, 1, len, f_) != len) {
      XNC_LOG_ERROR("write_out_failed");
      return "fwrite out failed";
    }
    return nullptr;
  }

 private:
  FILE* f_;
};

// One submission (real captured frame, warm-up re-feed or on-demand IDR
// re-feed): paces to the target fps, applies any pending one-shot IDR
// request (armed by a capture rebuild - E2 contract: it rides the next
// submission exactly once and is never re-armed), encodes, shapes and
// delivers every output AU to the sink. mono_us is the capture timestamp
// of the submitted frame (the base frame's for re-feeds) and stamps every
// AU of this submission. Returns nullptr on success; a non-null fatal
// message (res already flagged) aborts the run.
const char* SubmitFrame(MfSoftEncoder& enc, AuSink& sink, const uint8_t* bgra,
                        size_t len, uint64_t mono_us, FrameCache& cache,
                        PipelineResult* res, bool warmup, uint32_t spf_ms,
                        uint64_t* last_submit_ms, uint64_t* last_mono_us,
                        std::vector<std::vector<uint8_t>>& aus,
                        std::vector<uint8_t>& shaped, OnDemandIdr* ondemand) {
  // Pace submissions to the target fps (the encoder timestamps with the
  // wall clock, so submit cadence == frame cadence).
  if (*last_submit_ms != 0) {
    const uint64_t since = NowMs() - *last_submit_ms;
    if (since < spf_ms) Sleep(static_cast<DWORD>(spf_ms - since));
  }
  *last_submit_ms = NowMs();
  *last_mono_us = mono_us;

  const char* idr_reason = cache.TakePendingIdrReason();
  if (idr_reason != nullptr) enc.ForceNextIdr(idr_reason);

  std::string eerr;
  if (!enc.Encode(bgra, len, aus, &eerr)) {
    res->ok = false;
    res->err = eerr.empty() ? "encode failed" : eerr;
    XNC_LOG_ERROR("encode_failed err=\"%s\"", res->err.c_str());
    return "encode";
  }
  if (warmup) {
    cache.OnWarmupFeed();
  } else {
    cache.OnEncoded();
  }
  for (const auto& au : aus) {
    const bool is_idr = NalHasType(au.data(), au.size(), 5);
    ShapeAu(au.data(), au.size(), is_idr, enc.SpsPps(), &shaped);
    if (!shaped.empty()) {
      res->aus_written++;
      res->bytes_written += shaped.size();
      if (const char* err = sink.OnAu(is_idr, mono_us, shaped.data(), shaped.size())) {
        res->ok = false;
        res->err = err;
        return err;
      }
    }
    if (is_idr) {
      cache.OnKeyframeAu();  // ends warm-up (§7.4)
      if (ondemand->armed) {
        XNC_LOG_INFO("idr_delivered reason=%s feeds=%u", ondemand->reason,
                     ondemand->feeds);
        ondemand->armed = false;
      }
    }
  }
  return nullptr;
}

// ---- unified capture reset execution (M2-Slice1 Task 2, spec §7.5) ----
//
// One reset sequence, run entirely on the pipeline thread (the acquire loop
// IS the suspension - nothing calls Acquire while this runs):
//   1. STATE "recovering" (recoverable): the pipe stays alive and the
//      cursor/input threads (RtServer-owned) keep serving;
//   2. while the desktop gate is non-DEFAULT (secure desktop up - T1
//      evidence: re-duplication/init are DENIED 0x80070005 even as SYSTEM)
//      poll every kResetPollMs; aborts on stop/duration;
//   3. single rebuild path: ICapture::Rebuild, then encoder re-Init when
//      the dimensions changed. Retryable failures back off 500 ms; a
//      kResetHardFailStreak streak emits STATE "capture_failed" once and
//      slows the retry cadence to 1 s. If the desktop leaves again
//      mid-retry, re-enter the wait;
//   4. on success the stream rewinds exactly like an err_rebuilt
//      (FrameCache::OnRebuild -> next frame is the new base frame + the
//      one-shot "rebuild" ForceIDR), PipelineResult gains the new dims and
//      reset accounting, STATE "capture_rebuilt" fires (RtServer:
//      generation++), and a dimension change surfaces via OnDisplayChanged
//      (0x010A broadcast).
struct ResetSequence {
  ICapture* cap;
  MfSoftEncoder* enc;
  AuSink* sink;
  FrameCache* cache;
  PipelineResult* res;
  const PipelineOpts* opt;
  uint32_t* enc_w;
  uint32_t* enc_h;
  std::vector<uint8_t>* base;
  uint64_t* warmup_started_ms;
  uint32_t* warmup_gen_feeds;
  bool* warmup_phase_logged;
  uint64_t run_t0;       // RunCore start (duration deadline anchor)
  uint64_t duration_ms;
  const char* reason;
};

// Returns true when the RUN must end (stop flag / duration), false when the
// reset completed and the acquire loop should resume.
bool RunResetSequence(ResetSequence& s) {
  const auto abort = [&s] {
    return (s.opt->stop != nullptr && s.opt->stop->load()) ||
           NowMs() - s.run_t0 >= s.duration_ms;
  };
  const auto gate_away = [&s] {
    return s.opt->reset != nullptr &&
           s.opt->reset->Desktop() == ResetDesktop::kNonDefault;
  };

  const uint64_t t_start = NowMs();
  XNC_LOG_INFO("capture_reset_start reason=%s desktop_away=%d", s.reason,
               gate_away() ? 1 : 0);
  s.sink->OnState("recovering", true);

  // Phase 2: wait for the desktop to come back (immediate rebuilds are
  // provably futile while the secure desktop holds the output).
  while (gate_away()) {
    if (abort()) return true;
    Sleep(kResetPollMs);
  }

  // Phase 3: single rebuild path with retry/backoff.
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
      if (new_w != *s.enc_w || new_h != *s.enc_h) {
        ok = s.enc->Init(new_w, new_h, s.opt->fps, s.opt->target_bitrate_bps, &rerr);
        if (ok)
          XNC_LOG_INFO("capture_reset encoder re-init w=%u h=%u", new_w, new_h);
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
  const bool dims_changed = new_w != *s.enc_w || new_h != *s.enc_h;
  *s.enc_w = new_w;
  *s.enc_h = new_h;
  s.res->width = new_w;
  s.res->height = new_h;
  s.res->resets++;
  CopyReason(s.res->last_reset_reason, sizeof(s.res->last_reset_reason), s.reason);
  s.cache->OnRebuild();  // WAIT_BASE_FRAME + one-shot "rebuild" IDR
  s.base->clear();
  *s.warmup_started_ms = 0;
  *s.warmup_gen_feeds = 0;
  *s.warmup_phase_logged = false;
  s.sink->OnState("capture_rebuilt", true);  // RtServer: generation++
  if (dims_changed) s.sink->OnDisplayChanged(new_w, new_h, s.reason);
  XNC_LOG_INFO("capture_reset_done reason=%s w=%u h=%u dims_changed=%d elapsed_ms=%llu",
               s.reason, new_w, new_h, dims_changed ? 1 : 0,
               static_cast<unsigned long long>(NowMs() - t_start));
  return false;
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

  FrameCache cache;
  cache.Start();
  const uint32_t spf_ms = 1000u / opt.fps;
  const uint32_t warmup_feed_bound = WarmupFeedBound(opt.fps);

  std::vector<uint8_t> base;              // cached base frame (re-feed source)
  std::vector<std::vector<uint8_t>> aus;  // per-submission encoder outputs
  std::vector<uint8_t> shaped;            // shaped-AU scratch
  FrameBlob blob;
  std::string acq_err;

  const uint64_t t0 = NowMs();
  const uint64_t duration_ms = static_cast<uint64_t>(opt.duration_s) * 1000ull;
  uint32_t next_beat_s = 1;
  uint64_t last_submit_ms = 0;
  uint64_t last_mono_us = 0;       // stamps FlushTail AUs
  uint64_t base_mono_us = 0;       // cached base frame timestamp (re-feeds)
  uint64_t warmup_started_ms = 0;  // base submission time (2 s wall bound)
  uint64_t last_initiated_idr_ms = 0;  // kIdrMinIntervalMs throttle anchor
  uint32_t warmup_gen_feeds = 0;   // re-feeds this generation (rebuild resets)
  bool warmup_phase_logged = false;  // one warmup_done/exhausted log per gen
  bool first_frame_logged = false;
  uint32_t enc_w = cap.Width(), enc_h = cap.Height();  // encoder's dims
  OnDemandIdr ondemand;
  ResetStormTracker storm;  // same-reason rebuild-storm backoff (M2-S2 T1)

  for (;;) {
    if (NowMs() - t0 >= duration_ms) break;
    if (opt.stop != nullptr && opt.stop->load()) break;

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
                NowMs() - t0 >= duration_ms) {
              storm_abort = true;
              break;
            }
            Sleep(kResetPollMs);
          }
          if (storm_abort) break;
        }
        storm.RecordExecuted(reset_reason, NowMs());
        ResetSequence seq{&cap,           &enc,
                          &sink,          &cache,
                          &res,           &opt,
                          &enc_w,         &enc_h,
                          &base,          &warmup_started_ms,
                          &warmup_gen_feeds, &warmup_phase_logged,
                          t0,             duration_ms,
                          reset_reason};
        if (RunResetSequence(seq)) break;
        continue;  // re-poll stop/duration/pending resets before acquiring
      }
    }

    // Merged IDR request (spec §7.5): arm at most once per 500 ms, only
    // after the stream's first keyframe (an in-progress initial warm-up
    // will deliver that IDR anyway) and never while one is in flight.
    const char* pending_reason = sink.PendingIdrReason();
    if (pending_reason != nullptr && cache.HaveKeyframe() && !ondemand.armed &&
        NowMs() - last_initiated_idr_ms >= kIdrMinIntervalMs) {
      enc.ForceNextIdr(pending_reason);
      sink.ConsumePendingIdr(pending_reason);
      last_initiated_idr_ms = NowMs();
      ondemand.Arm(pending_reason, NowMs());
      XNC_LOG_INFO("idr_request reason=%s min_interval_ms=%llu", pending_reason,
                   static_cast<unsigned long long>(kIdrMinIntervalMs));
    }

    acq_err.clear();
    if (cap.Acquire(blob, &acq_err)) {
      // Frame-size change (M2-S1 Task 2): the backend adopted a new mode
      // but the encoder is still at the old size - route to the unified
      // reset (resolution) instead of feeding a wrong-sized frame in.
      if (opt.reset != nullptr && (blob.w != enc_w || blob.h != enc_h)) {
        opt.reset->RequestReset(kResetReasonResolution);
        Sleep(kIdleSleepMs);  // ride the debounce window
        continue;
      }
      const bool is_base = cache.OnCapturedFrame();  // captured++ inside
      if (is_base) {
        base = blob.bgra;  // full frame: warm-up re-feed source
        base_mono_us = blob.mono_us;
        res.width = blob.w;
        res.height = blob.h;
        warmup_started_ms = 0;  // re-anchored at this generation's first submit
        warmup_gen_feeds = 0;
        warmup_phase_logged = false;
        XNC_LOG_INFO("base_frame w=%u h=%u state=%s", blob.w, blob.h, cache.StateName());
      }
      if (!first_frame_logged) {
        first_frame_logged = true;
        const size_t head = blob.bgra.size() < 64 ? blob.bgra.size() : 64;
        const unsigned long long hash =
            static_cast<unsigned long long>(Fnv1a64(blob.bgra.data(), head));
        const bool non_black = SamplePointsNotUniform(blob.bgra.data(), blob.w, blob.h);
        XNC_LOG_INFO("first_frame hash_head64=%016llx non_black=%d w=%u h=%u mono_us=%llu",
                     hash, non_black ? 1 : 0, blob.w, blob.h,
                     static_cast<unsigned long long>(blob.mono_us));
        if (!non_black)
          XNC_LOG_ERROR("first_frame_uniform (all 256 sampled pixels equal - suspect black/garbage frame)");
      }
      if (warmup_started_ms == 0) warmup_started_ms = NowMs();
      if (SubmitFrame(enc, sink, blob.bgra.data(), blob.bgra.size(), blob.mono_us, cache,
                      &res, false, spf_ms, &last_submit_ms, &last_mono_us, aus, shaped,
                      &ondemand) != nullptr)
        break;
    } else if (acq_err == "err_timeout") {
      cache.OnTimeout();  // static screen: no encode, no packet (§7.4)
      const bool have_base = !cache.NeedsBaseFrame() && !base.empty();
      const uint64_t warmup_elapsed = warmup_started_ms != 0 ? NowMs() - warmup_started_ms : 0;
      const bool warmup_feed_ok = have_base && !cache.HaveKeyframe() &&
                                  warmup_gen_feeds < warmup_feed_bound &&
                                  warmup_elapsed < kWarmupWallBoundMs;
      // On-demand IDR re-feed (static screen + armed subscriber request):
      // same source frame, same bounds, never a second force (§7.5).
      const bool ondemand_feed_ok =
          have_base && cache.HaveKeyframe() && ondemand.FeedAllowed(NowMs(), warmup_feed_bound);
      if (warmup_feed_ok || ondemand_feed_ok) {
        if (warmup_feed_ok) {
          ++warmup_gen_feeds;
        } else {
          ++ondemand.feeds;
        }
        if (SubmitFrame(enc, sink, base.data(), base.size(), base_mono_us, cache, &res,
                        true, spf_ms, &last_submit_ms, &last_mono_us, aus, shaped,
                        &ondemand) != nullptr)
          break;
      } else {
        // One warm-up outcome log per generation: done (first keyframe AU
        // emerged) or exhausted (bound hit without one - spec §7.4 amended:
        // then we simply wait for real frames; never re-force).
        if (!warmup_phase_logged && have_base) {
          if (cache.HaveKeyframe()) {
            warmup_phase_logged = true;
            XNC_LOG_INFO("warmup_done feeds=%u keyframes=%llu", warmup_gen_feeds,
                         static_cast<unsigned long long>(cache.counters().keyframes));
          } else if (warmup_gen_feeds >= warmup_feed_bound ||
                     warmup_elapsed >= kWarmupWallBoundMs) {
            warmup_phase_logged = true;
            XNC_LOG_INFO("warmup_exhausted feeds=%u bound=%u keyframes=%llu",
                         warmup_gen_feeds, warmup_feed_bound,
                         static_cast<unsigned long long>(cache.counters().keyframes));
          }
        }
        // On-demand window exhausted without an IDR: log once; the armed
        // force stays consumed and the IDR surfaces with the next real
        // frame batch (still never re-forced).
        if (ondemand.armed && !ondemand.exhaust_logged && have_base &&
            !ondemand.FeedAllowed(NowMs(), warmup_feed_bound)) {
          ondemand.exhaust_logged = true;
          XNC_LOG_INFO("idr_feed_exhausted reason=%s feeds=%u bound=%u",
                       ondemand.reason, ondemand.feeds, warmup_feed_bound);
        }
        Sleep(kIdleSleepMs);
      }
    } else if (acq_err == "err_rebuilt") {
      // Backend rebuilt its duplication in place: rewind the state machine
      // (next frame is the new base, full readback) and arm the one-shot
      // "rebuild" IDR; the old cached base frame is invalid across rebuild.
      cache.OnRebuild();
      base.clear();
      warmup_started_ms = 0;
      warmup_gen_feeds = 0;
      warmup_phase_logged = false;
      sink.OnState("capture_rebuilt", true);
      XNC_LOG_INFO("capture_rebuild handled rebuilds=%u state=%s",
                   cache.counters().rebuilds, cache.StateName());
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
      const FrameCacheCounters& c = cache.counters();
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
                     static_cast<unsigned long long>(c.warmup_feeds), c.rebuilds, res.width,
                     res.height, static_cast<unsigned long long>(res.aus_written),
                     static_cast<unsigned long long>(res.bytes_written), desktop);
      } else {
        XNC_LOG_INFO("diag_pipeline elapsed=%us captured=%llu encoded=%llu keyframes=%llu timeouts=%llu warmup_feeds=%llu rebuilds=%u w=%u h=%u aus=%llu bytes=%llu",
                     elapsed_s, static_cast<unsigned long long>(c.captured),
                     static_cast<unsigned long long>(c.encoded),
                     static_cast<unsigned long long>(c.keyframes),
                     static_cast<unsigned long long>(c.timeouts),
                     static_cast<unsigned long long>(c.warmup_feeds), c.rebuilds, res.width,
                     res.height, static_cast<unsigned long long>(res.aus_written),
                     static_cast<unsigned long long>(res.bytes_written));
      }
      next_beat_s = elapsed_s + 1;
    }
  }

  // End of run: flush the encoder tail (NOTIFY_DRAIN) so the lookahead
  // window's AUs are not dropped, through the same shaping path.
  if (res.ok) {
    aus.clear();
    enc.FlushTail(aus);
    if (!aus.empty())
      XNC_LOG_INFO("encoder_flush_tail aus=%zu", aus.size());
    for (const auto& au : aus) {
      const bool is_idr = NalHasType(au.data(), au.size(), 5);
      ShapeAu(au.data(), au.size(), is_idr, enc.SpsPps(), &shaped);
      if (!shaped.empty()) {
        res.aus_written++;
        res.bytes_written += shaped.size();
        const char* err = sink.OnAu(is_idr, last_mono_us, shaped.data(), shaped.size());
        if (err != nullptr) {
          res.ok = false;
          res.err = std::strcmp(err, "fwrite out failed") == 0
                        ? "fwrite out failed (flush)"
                        : err;
          break;
        }
      }
      if (is_idr) cache.OnKeyframeAu();
    }
  }
  sink.OnState("stream_end", res.ok);

  res.counters = cache.counters();
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
                "  \"bytes_written\": %llu,\n"
                "  \"ok\": %d\n"
                "}\n",
                o.duration_s, r.width, r.height, o.fps, o.target_bitrate_bps,
                static_cast<unsigned long long>(r.counters.captured),
                static_cast<unsigned long long>(r.counters.encoded),
                static_cast<unsigned long long>(r.counters.keyframes),
                static_cast<unsigned long long>(r.counters.timeouts),
                static_cast<unsigned long long>(r.counters.warmup_feeds),
                r.counters.rebuilds, r.resets, static_cast<unsigned long long>(r.aus_written),
                static_cast<unsigned long long>(r.bytes_written), r.ok ? 1 : 0);
  return std::string(buf);
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
