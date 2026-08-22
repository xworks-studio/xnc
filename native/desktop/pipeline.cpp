// pipeline.cpp - Pipeline::Run + stats.json writers (see pipeline.h).
// Loop shape (plan Task 5, spec §7.4/§7.5):
//   Acquire -> frame (base/incremental via FrameCache) | err_timeout (no
//   encode; warm-up re-feed while no keyframe AU yet) | err_rebuilt
//   (FrameCache.OnRebuild + one-shot "rebuild" force) | fatal (stop)
//   -> Encode -> shape every AU (SPS/PPS prefix on IDR, 4B start codes,
//   AUD dropped) -> fwrite; per-second counter beat; at end FlushTail
//   recovers the lookahead window's AUs through the same shaping path.
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>  // Sleep, GetTickCount64

#include <cerrno>
#include <cstdint>
#include <cstdio>
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

// Warm-up wall-clock bound (spec §7.4): 2 s.
constexpr uint64_t kWarmupWallBoundMs = 2000ull;

// One submission (real captured frame or warm-up re-feed): paces to the
// target fps, applies any pending one-shot IDR request (armed by a capture
// rebuild - E2 contract: it rides the next submission exactly once and is
// never re-armed), encodes, shapes and writes every output AU. Returns
// false on fatal failure (res->ok already false, res->err set).
bool SubmitFrame(MfSoftEncoder& enc, FILE* out, const uint8_t* bgra, size_t len,
                 FrameCache& cache, PipelineResult* res, bool warmup, uint32_t spf_ms,
                 uint64_t* last_submit_ms, std::vector<std::vector<uint8_t>>& aus,
                 std::vector<uint8_t>& shaped) {
  // Pace submissions to the target fps (the encoder timestamps with the
  // wall clock, so submit cadence == frame cadence).
  if (*last_submit_ms != 0) {
    const uint64_t since = NowMs() - *last_submit_ms;
    if (since < spf_ms) Sleep(static_cast<DWORD>(spf_ms - since));
  }
  *last_submit_ms = NowMs();

  const char* idr_reason = cache.TakePendingIdrReason();
  if (idr_reason != nullptr) enc.ForceNextIdr(idr_reason);

  std::string eerr;
  if (!enc.Encode(bgra, len, aus, &eerr)) {
    res->ok = false;
    res->err = eerr.empty() ? "encode failed" : eerr;
    XNC_LOG_ERROR("encode_failed err=\"%s\"", res->err.c_str());
    return false;
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
      if (std::fwrite(shaped.data(), 1, shaped.size(), out) != shaped.size()) {
        res->ok = false;
        res->err = "fwrite out failed";
        XNC_LOG_ERROR("write_out_failed");
        return false;
      }
      res->aus_written++;
      res->bytes_written += shaped.size();
    }
    if (is_idr) cache.OnKeyframeAu();  // ends warm-up (§7.4)
  }
  return true;
}

}  // namespace

PipelineResult Pipeline::Run(ICapture& cap, MfSoftEncoder& enc, FILE* out,
                             const PipelineOpts& opt) {
  PipelineResult res;
  res.width = cap.Width();
  res.height = cap.Height();
  if (out == nullptr) {
    res.ok = false;
    res.err = "out file is null";
    return res;
  }
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
  uint64_t warmup_started_ms = 0;   // base submission time (2 s wall bound)
  uint32_t warmup_gen_feeds = 0;    // re-feeds this generation (rebuild resets)
  bool warmup_phase_logged = false; // one warmup_done/exhausted log per gen
  bool first_frame_logged = false;

  for (;;) {
    if (NowMs() - t0 >= duration_ms) break;
    acq_err.clear();
    if (cap.Acquire(blob, &acq_err)) {
      const bool is_base = cache.OnCapturedFrame();  // captured++ inside
      if (is_base) {
        base = blob.bgra;  // full frame: warm-up re-feed source
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
      if (!SubmitFrame(enc, out, blob.bgra.data(), blob.bgra.size(), cache, &res, false,
                       spf_ms, &last_submit_ms, aus, shaped))
        break;
    } else if (acq_err == "err_timeout") {
      cache.OnTimeout();  // static screen: no encode, no packet (§7.4)
      const bool have_base = !cache.NeedsBaseFrame() && !base.empty();
      const uint64_t warmup_elapsed = warmup_started_ms != 0 ? NowMs() - warmup_started_ms : 0;
      const bool feed_ok = have_base && !cache.HaveKeyframe() &&
                           warmup_gen_feeds < warmup_feed_bound &&
                           warmup_elapsed < kWarmupWallBoundMs;
      if (feed_ok) {
        ++warmup_gen_feeds;
        if (!SubmitFrame(enc, out, base.data(), base.size(), cache, &res, true, spf_ms,
                         &last_submit_ms, aus, shaped))
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
      XNC_LOG_INFO("capture_rebuild handled rebuilds=%u state=%s",
                   cache.counters().rebuilds, cache.StateName());
    } else {
      res.ok = false;
      res.err = acq_err.empty() ? "acquire failed" : acq_err;
      XNC_LOG_ERROR("acquire_failed err=\"%s\"", res.err.c_str());
      break;
    }

    const uint32_t elapsed_s = static_cast<uint32_t>((NowMs() - t0) / 1000);
    if (elapsed_s >= next_beat_s) {
      const FrameCacheCounters& c = cache.counters();
      XNC_LOG_INFO("diag_pipeline elapsed=%us captured=%llu encoded=%llu keyframes=%llu timeouts=%llu warmup_feeds=%llu rebuilds=%u w=%u h=%u aus=%llu bytes=%llu",
                   elapsed_s, static_cast<unsigned long long>(c.captured),
                   static_cast<unsigned long long>(c.encoded),
                   static_cast<unsigned long long>(c.keyframes),
                   static_cast<unsigned long long>(c.timeouts),
                   static_cast<unsigned long long>(c.warmup_feeds), c.rebuilds, res.width,
                   res.height, static_cast<unsigned long long>(res.aus_written),
                   static_cast<unsigned long long>(res.bytes_written));
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
        if (std::fwrite(shaped.data(), 1, shaped.size(), out) != shaped.size()) {
          res.ok = false;
          res.err = "fwrite out failed (flush)";
          break;
        }
        res.aus_written++;
        res.bytes_written += shaped.size();
      }
      if (is_idr) cache.OnKeyframeAu();
    }
  }
  if (std::fflush(out) != 0) {
    res.ok = false;
    res.err = "fflush out failed";
    XNC_LOG_ERROR("fflush_out_failed errno=%d", errno);
  }

  res.counters = cache.counters();
  const FrameCacheCounters& c = res.counters;
  XNC_LOG_INFO("pipeline_stop elapsed=%llums captured=%llu encoded=%llu keyframes=%llu timeouts=%llu warmup_feeds=%llu rebuilds=%u aus=%llu bytes=%llu ok=%d",
               static_cast<unsigned long long>(NowMs() - t0),
               static_cast<unsigned long long>(c.captured),
               static_cast<unsigned long long>(c.encoded),
               static_cast<unsigned long long>(c.keyframes),
               static_cast<unsigned long long>(c.timeouts),
               static_cast<unsigned long long>(c.warmup_feeds), c.rebuilds,
               static_cast<unsigned long long>(res.aus_written),
               static_cast<unsigned long long>(res.bytes_written), res.ok ? 1 : 0);
  return res;
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
                r.counters.rebuilds, static_cast<unsigned long long>(r.aus_written),
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
