// frame_cache.h - per-display frame state machine + capture pipeline
// bookkeeping (plan M1-Slice1 Task 5, spec §7.5). States:
//
//   INIT -> WAIT_BASE_FRAME -> HAVE_BASE -> INCREMENTAL
//
// Rules baked in (spec §7.4/§7.5):
//   - before HAVE_BASE only full frames are accepted; the first frame after
//     (re)start/rebuild is THE base frame (full readback - dirty rects are
//     never applied without a base, M4 concern);
//   - WAIT_TIMEOUT never encodes and never sends (static screen = natural
//     0 fps), it only counts;
//   - a capture rebuild (ICapture "err_rebuilt") rewinds to WAIT_BASE_FRAME
//     and arms a ONE-SHOT "rebuild" IDR request the pipeline applies via
//     MfSoftEncoder::ForceNextIdr exactly once - re-arming "because no
//     keyframe was seen yet" is the E2 storm bug (spec §7.10/ADR-017);
//   - warm-up (spec §7.4, amended 2026-08-22): while no keyframe AU has
//     emerged for the current generation the pipeline re-feeds the cached
//     base frame (bounded, never re-forcing); those re-feeds are counted
//     here (counters.warmup_feeds) alongside normal encoded frames.
//
// Pure header-only logic on purpose: desktop_selftest covers every
// transition without an encoder, a desktop or a GPU. The pipeline
// (pipeline.h/.cpp) is the only intended consumer of the event methods.
#ifndef XNC_NATIVE_DESKTOP_FRAME_CACHE_H_
#define XNC_NATIVE_DESKTOP_FRAME_CACHE_H_

#include <cstdint>

namespace xnc {

// Cumulative totals over one Pipeline::Run (stats.json / per-second log
// fields). encoded includes warm-up re-feeds, so
// `encoded == captured + warmup_feeds` is a pipeline invariant.
struct FrameCacheCounters {
  uint64_t captured = 0;      // frames handed in by the capture backend
  uint64_t encoded = 0;       // frames submitted to the encoder (real + re-feeds)
  uint64_t keyframes = 0;     // IDR access units observed in encoder output
  uint64_t timeouts = 0;      // static-screen observations (never encoded)
  uint64_t warmup_feeds = 0;  // base-frame re-feeds during encoder warm-up (§7.4)
  uint32_t rebuilds = 0;      // capture rebuilds observed (ACCESS_LOST etc.)
};

class FrameCache {
 public:
  enum class State { kInit, kWaitBaseFrame, kHaveBase, kIncremental };

  // INIT -> WAIT_BASE_FRAME. Called once per run by the pipeline; a call
  // from any later state is a defensive no-op.
  void Start() {
    if (state_ == State::kInit) state_ = State::kWaitBaseFrame;
  }

  // One captured frame arrived. Returns true when this frame is the base
  // frame (first after Start/OnRebuild - full-frame semantics); transitions
  // WAIT_BASE_FRAME -> HAVE_BASE, then HAVE_BASE -> INCREMENTAL. From kInit
  // (Start not called yet) the first frame still counts as base - the
  // first-frame-is-full rule does not depend on call order.
  bool OnCapturedFrame() {
    ++c_.captured;
    const bool base = NeedsBaseFrame();
    state_ = base ? State::kHaveBase : State::kIncremental;
    return base;
  }

  // Acquire timeout (static screen): counts only - never encodes, never
  // sends (spec §7.4: WAIT_TIMEOUT 不编码不发包).
  void OnTimeout() { ++c_.timeouts; }

  // Capture rebuild: rewinds to WAIT_BASE_FRAME, rebuilds++, drops the
  // have-keyframe flag (warm-up restarts for the new generation) and arms
  // the one-shot "rebuild" IDR request (TakePendingIdrReason). Counters
  // stay cumulative - stats.json reports run totals, not per-generation.
  void OnRebuild() {
    state_ = State::kWaitBaseFrame;
    ++c_.rebuilds;
    have_keyframe_ = false;
    pending_rebuild_idr_ = true;
  }

  // One real frame submitted to the encoder. encoded++.
  void OnEncoded() { ++c_.encoded; }

  // One warm-up re-feed of the cached base frame (spec §7.4). Counts as an
  // encoded frame AND a warm-up feed (see FrameCacheCounters invariant).
  void OnWarmupFeed() {
    ++c_.encoded;
    ++c_.warmup_feeds;
  }

  // One IDR access unit observed in encoder output: keyframes++ and the
  // warm-up phase for this generation is over (HaveKeyframe turns true).
  void OnKeyframeAu() {
    ++c_.keyframes;
    have_keyframe_ = true;
  }

  State state() const { return state_; }
  const char* StateName() const {
    switch (state_) {
      case State::kInit: return "INIT";
      case State::kWaitBaseFrame: return "WAIT_BASE_FRAME";
      case State::kHaveBase: return "HAVE_BASE";
      case State::kIncremental: return "INCREMENTAL";
    }
    return "?";
  }
  // INIT or WAIT_BASE_FRAME: the next frame must be a full base frame.
  bool NeedsBaseFrame() const {
    return state_ == State::kInit || state_ == State::kWaitBaseFrame;
  }
  // A keyframe AU has emerged since the last Start/OnRebuild.
  bool HaveKeyframe() const { return have_keyframe_; }
  const FrameCacheCounters& counters() const { return c_; }

  // One-shot IDR request armed by OnRebuild. The pipeline applies it via
  // MfSoftEncoder::ForceNextIdr(reason) right before the next submission -
  // which, because rebuild rewinds to WAIT_BASE_FRAME, is the new base
  // frame. Returns "rebuild" exactly once per rebuild, then nullptr.
  const char* TakePendingIdrReason() {
    if (!pending_rebuild_idr_) return nullptr;
    pending_rebuild_idr_ = false;
    return "rebuild";
  }

 private:
  State state_ = State::kInit;
  FrameCacheCounters c_{};
  bool have_keyframe_ = false;
  bool pending_rebuild_idr_ = false;
};

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_FRAME_CACHE_H_
