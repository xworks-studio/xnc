// subscribers.h - subscriber model for the real-time pipe server (plan
// M1-Slice2 Task 2, spec §7.5/§7.9). Pure header-only bookkeeping on
// purpose: desktop_selftest covers every drop/merge rule without a pipe, a
// desktop or an encoder; rt_pipe_server owns the pipes/threads and drives
// these objects under its locks.
//
// Rules baked in (spec §7.9 + Slice1 carry-over):
//   - attach marks the subscriber needs_keyframe (reason "sub_join"): the
//     joiner joins the stream FROM its IDR, so deltas arriving before it
//     are dropped (undecodable), not queued;
//   - per-subscriber video send queue depth 3 (kSubQueueDepth): full +
//     delta -> drop the delta and mark needs_keyframe (the overflow will
//     be merged into one pipeline IDR with reason "queue_overflow");
//   - a key AU is never dropped: if the queue is full the queued deltas
//     are displaced (they are undecodable after the keyframe anyway and
//     by definition stale - the queue only stalls when the consumer is
//     slower than the encoder);
//   - control frames (HOST_HELLO, STATE, PONG, errors) use a separate
//     backlog and are never dropped; kCtrlBacklogMax overflow means the
//     connection is wedged and must be disconnected (spec: control queue
//     congestion -> drop the subscriber);
//   - needsKeyframe requests are MERGED: the table exposes one pending
//     reason while any subscriber still needs a keyframe; it clears
//     naturally when the key AU is enqueued to every needy subscriber.
//
// M1 Task 4 adds the Pipe v2 per-subscriber WAIT_IDR state machine (spec
// §9/§10.2/§12.3): the v2 wire carries epochs, so a subscriber can be told
// exactly which generation may revive it. PushAuV2 rules:
//   - join starts kWaitIdr (the v1 needs_keyframe joins-from-IDR rule);
//   - kWaitIdr delivers nothing but the IDR of the expected epoch pair
//     (an older epoch's IDR - a regression - is dropped; the pipeline's
//     FrameIdentityLedger already prevents it upstream);
//   - queue overflow (kLive + full + delta): clear the queue, kWaitIdr,
//     needs_keyframe (merged queue_overflow IDR request);
//   - epoch advance: discontinuity (caller must emit 0x020B with the NEW
//     epochs) + clear + kWaitIdr, then the new epoch's IDR resumes kLive;
//   - OnRebuildDiscontinuity (capture thread, at a rebuild): clear +
//     kWaitIdr and suppress EVERY pre-rebuild AU still in flight (encoder
//     lookahead tail) until an epoch pair NEWER than the rebuild floor
//     (the last pair the server fanned out pre-rebuild) publishes - which
//     then takes the 0x020B discontinuity path (spec 10.2: old epochs
//     never publish again after the rebuild). M1-deferred convergence
//     note: a subscriber ATTACHING between the OnState(capture_rebuilt)
//     broadcast and the first new-epoch AU has no floor yet and may
//     briefly adopt an old-epoch tail IDR; the 0x020B that rides the
//     epoch advance drains it - convergence is guaranteed either way;
//   - kPaused is reserved (spec 12.3 tab-hidden/explicit pause; no pipe
//     message drives it yet);
//   - PushAuV2 never blocks: the key path displaces stale deltas instead
//     of waiting, so OnAu can never stall for an IDR.
// The v1 path (PushAu) is untouched - M0 scenarios pin it byte-for-byte.
#ifndef XNC_NATIVE_DESKTOP_SUBSCRIBERS_H_
#define XNC_NATIVE_DESKTOP_SUBSCRIBERS_H_

#include <cstdint>
#include <cstring>
#include <deque>
#include <map>
#include <memory>
#include <utility>  // std::move

#include "../common/frame.h"  // xnc::Frame (outbound item type)

namespace xnc {

// Per-subscriber video queue depth (spec §7.9: 3 compressed AUs).
inline constexpr uint32_t kSubQueueDepth = 3;
// Control backlog bound; exceeding it disconnects the subscriber.
inline constexpr uint32_t kCtrlBacklogMax = 64;

// One subscriber's outbound state: two queues (control first, then video)
// plus the needs-keyframe flag and (M1 Task 4) the v2 WAIT_IDR state
// machine. Not thread-safe by itself - rt_pipe_server serializes access.
class SubSendQueue {
 public:
  enum class AuAction {
    kEnqueued,               // queued for send
    kDroppedNeedKey,         // delta dropped: subscriber awaits its IDR
    kDroppedQueueFull,       // delta dropped: queue full -> needs_keyframe
    kEnqueuedDisplacingDeltas  // key AU queued, stale deltas displaced
  };

  // M1 Task 4: v2 per-subscriber video state (spec §12.3 ViewerSender).
  enum class VideoState { kWaitIdr, kLive, kPaused };

  // PushAuV2 result: the shared drop bookkeeping plus the epoch pair the
  // caller must broadcast as 0x020B STREAM_DISCONTINUITY when a
  // discontinuity was detected (the epochs are the NEW generation's).
  struct V2Push {
    AuAction action = AuAction::kEnqueued;
    bool discontinuity = false;
    uint64_t capture_epoch = 0, codec_epoch = 0;
  };

  explicit SubSendQueue(uint32_t depth = kSubQueueDepth)
      : depth_(depth == 0 ? 1 : depth) {}

  // Control frames are never dropped while the connection is healthy.
  // Returns false when the control backlog is exhausted - the caller must
  // disconnect this subscriber (a wedged control path never recovers).
  bool PushControl(const Frame& f) {
    if (ctrl_.size() >= kCtrlBacklogMax) return false;
    ctrl_.push_back(f);
    return true;
  }

  // Video AU with the spec §7.9 drop policy (see header comment). `au` must
  // be a fully built kMsgFrame event; it is copied on enqueue. The v1 wire
  // path - unchanged since M1-Slice2 (M0 scenarios pin it).
  AuAction PushAu(bool is_idr, const Frame& au) {
    if (!is_idr) {
      if (needs_keyframe_) return AuAction::kDroppedNeedKey;
      if (video_.size() >= depth_) {
        needs_keyframe_ = true;
        return AuAction::kDroppedQueueFull;
      }
      video_.push_back(au);
      return AuAction::kEnqueued;
    }
    if (video_.size() >= depth_) {
      video_.clear();  // stale deltas before the keyframe
      video_.push_back(au);
      needs_keyframe_ = false;
      return AuAction::kEnqueuedDisplacingDeltas;
    }
    video_.push_back(au);
    needs_keyframe_ = false;
    return AuAction::kEnqueued;
  }

  // M1 Task 4: v2 per-subscriber WAIT_IDR push (see header comment). `au`
  // must be a fully built kMsgFrameV2 event; it is copied on enqueue.
  // Never blocks - OnAu must never stall for an IDR.
  V2Push PushAuV2(bool is_idr, uint64_t capture_epoch, uint64_t codec_epoch,
                  const Frame& au) {
    V2Push r;
    // A rebuild is pending: every AU of the old generation - including an
    // old IDR - is suppressed until an epoch pair strictly NEWER than the
    // rebuild floor publishes (the reset contract forces that first
    // publication to be the new IDR). The floor (the last epoch pair the
    // server fanned out before the rebuild) also protects a subscriber
    // with no baseline yet: its fresh join must not adopt the encoder's
    // pre-rebuild lookahead tail as the current generation.
    if (pending_rebuild_ &&
        !(capture_epoch > floor_cap_ ||
          (capture_epoch == floor_cap_ && codec_epoch > floor_codec_))) {
      r.action = AuAction::kDroppedNeedKey;
      return r;
    }
    if (has_epoch_ && IsOlder(capture_epoch, codec_epoch)) {
      // Epoch regression: defensive drop (the pipeline's identity ledger
      // rejects regressions before they reach a sink).
      r.action = AuAction::kDroppedNeedKey;
      return r;
    }
    if (!has_epoch_ && !pending_rebuild_) {
      // Fresh join: no discontinuity for the very first AU (the client
      // attaches with no frames yet); the first IDR establishes the epoch.
      has_epoch_ = true;
      cap_ = capture_epoch;
      codec_ = codec_epoch;
      if (!is_idr) {
        r.action = AuAction::kDroppedNeedKey;  // join from IDR (§7.5)
        return r;
      }
      state_ = VideoState::kLive;
      needs_keyframe_ = false;
      video_.push_back(au);
      r.action = AuAction::kEnqueued;
      return r;
    }
    if (IsNewer(capture_epoch, codec_epoch) || pending_rebuild_) {
      // Epoch advance = discontinuity: clear stale AUs, re-arm WAIT_IDR for
      // the new generation, and tell the caller to broadcast 0x020B. Also
      // the first post-rebuild publication for a baseline-less subscriber
      // (pending_rebuild_): the rebuild IS the discontinuity it missed.
      video_.clear();
      state_ = VideoState::kWaitIdr;
      pending_rebuild_ = false;
      has_epoch_ = true;
      cap_ = capture_epoch;
      codec_ = codec_epoch;
      r.discontinuity = true;
      r.capture_epoch = capture_epoch;
      r.codec_epoch = codec_epoch;
      if (is_idr) {  // the reset's forced IDR rides the first new AU
        state_ = VideoState::kLive;
        needs_keyframe_ = false;
        video_.push_back(au);
        r.action = AuAction::kEnqueued;
      } else {
        // The new epoch's first AU is NOT its IDR (encoder lookahead can
        // delay the reset's forced IDR): re-arm the merged keyframe
        // request (spec §13: codec-epoch change is a request source).
        needs_keyframe_ = true;
        r.action = AuAction::kDroppedNeedKey;
      }
      return r;
    }
    // Same epoch pair as last published.
    if (state_ == VideoState::kWaitIdr) {
      // WAIT_IDR delivers nothing but the expected epoch's IDR.
      if (!is_idr) {
        r.action = AuAction::kDroppedNeedKey;
        return r;
      }
      state_ = VideoState::kLive;
      needs_keyframe_ = false;
      video_.push_back(au);
      r.action = AuAction::kEnqueued;
      return r;
    }
    if (!is_idr) {
      if (video_.size() >= depth_) {
        // Overflow: clear the queue (spec §9 - the queued deltas are
        // undecodable after the dropped one) and wait for a merged IDR.
        video_.clear();
        state_ = VideoState::kWaitIdr;
        needs_keyframe_ = true;
        r.action = AuAction::kDroppedQueueFull;
        return r;
      }
      video_.push_back(au);
      r.action = AuAction::kEnqueued;
      return r;
    }
    // kLive IDR: never dropped (displaces stale deltas) and it satisfies
    // any merged keyframe request (v1 PushAu contract - the pending reason
    // clears when the key AU is enqueued).
    needs_keyframe_ = false;
    if (video_.size() >= depth_) {
      video_.clear();  // key never dropped: displace stale deltas
      video_.push_back(au);
      r.action = AuAction::kEnqueuedDisplacingDeltas;
      return r;
    }
    video_.push_back(au);
    r.action = AuAction::kEnqueued;
    return r;
  }

  // M1 Task 4 (v2 only): a capture rebuild invalidated every AU of the old
  // generation, queued or in flight. Clear + WAIT_IDR; only an epoch pair
  // strictly newer than the floor may publish from here (its first AU
  // triggers the 0x020B path). `floor_cap/floor_codec` = the last epoch
  // pair the SERVER fanned out before the rebuild (rt_pipe_server tracks
  // it) - a subscriber without a baseline needs it to tell the encoder's
  // pre-rebuild lookahead tail from the new generation.
  void OnRebuildDiscontinuity(uint64_t floor_cap, uint64_t floor_codec) {
    video_.clear();
    state_ = VideoState::kWaitIdr;
    pending_rebuild_ = true;
    floor_cap_ = floor_cap;
    floor_codec_ = floor_codec;
  }

  VideoState video_state() const { return state_; }

  // Control first, then video FIFO. False when both empty.
  bool Pop(Frame* out) {
    if (!ctrl_.empty()) {
      *out = std::move(ctrl_.front());
      ctrl_.pop_front();
      return true;
    }
    if (!video_.empty()) {
      *out = std::move(video_.front());
      video_.pop_front();
      return true;
    }
    return false;
  }

  bool HasWork() const { return !ctrl_.empty() || !video_.empty(); }
  bool needs_keyframe() const { return needs_keyframe_; }
  void MarkNeedsKeyframe() { needs_keyframe_ = true; }
  size_t video_depth() const { return video_.size(); }

 private:
  bool IsNewer(uint64_t ce, uint64_t ke) const {
    return !has_epoch_ || ce > cap_ || (ce == cap_ && ke > codec_);
  }
  bool IsOlder(uint64_t ce, uint64_t ke) const {
    return has_epoch_ && (ce < cap_ || (ce == cap_ && ke < codec_));
  }
  std::deque<Frame> ctrl_, video_;
  uint32_t depth_;
  bool needs_keyframe_ = true;  // attach == needs (joiner joins from its IDR)
  // ---- M1 Task 4: v2 WAIT_IDR state machine fields (v1 path ignores them) ----
  VideoState state_ = VideoState::kWaitIdr;
  bool has_epoch_ = false;       // any epoch pair published (baseline exists)
  bool pending_rebuild_ = false; // OnRebuildDiscontinuity: only post-floor epochs
  uint64_t cap_ = 0, codec_ = 0; // last published epoch pair
  uint64_t floor_cap_ = 0, floor_codec_ = 0;  // rebuild floor (pre-rebuild gen)
};

// sub_id -> SubSendQueue registry + the merged pending-IDR reason. The ids
// are client-chosen (non-zero, unique per server); the server checks
// max_subs capacity before calling Attach. Not thread-safe by itself.
class SubscriberTable {
 public:
  // False on duplicate sub_id. Marks the queue needs_keyframe with reason
  // "sub_join" (attach = needs, spec §7.5).
  bool Attach(uint32_t sub_id, std::shared_ptr<SubSendQueue> q) {
    if (subs_.count(sub_id) != 0) return false;
    q->MarkNeedsKeyframe();
    SetReason("sub_join");
    subs_[sub_id] = std::move(q);
    return true;
  }

  bool Detach(uint32_t sub_id) { return subs_.erase(sub_id) != 0; }

  SubSendQueue* Find(uint32_t sub_id) {
    const auto it = subs_.find(sub_id);
    return it == subs_.end() ? nullptr : it->second.get();
  }

  // Marks one subscriber; the reason feeds the merged pending reason
  // ("queue_overflow" / "explicit" / "sub_join"). Unknown id: no-op.
  void MarkNeedsKeyframe(uint32_t sub_id, const char* reason) {
    SubSendQueue* q = Find(sub_id);
    if (q == nullptr) return;
    q->MarkNeedsKeyframe();
    if (reason != nullptr && reason[0] != '\0') SetReason(reason);
  }

  bool AnyNeedsKeyframe() const {
    for (const auto& kv : subs_)
      if (kv.second->needs_keyframe()) return true;
    return false;
  }

  // Merged IDR request (spec §7.5): non-null while at least one subscriber
  // needs a keyframe; the string is the most recent reason. Points at
  // internal storage - consume before mutating the table.
  const char* pending_reason() const {
    if (!AnyNeedsKeyframe()) return nullptr;
    return reason_[0] != '\0' ? reason_ : "sub_join";
  }

  size_t size() const { return subs_.size(); }

  template <typename F>
  void ForEach(F fn) {
    for (auto& kv : subs_) fn(kv.first, kv.second.get());
  }

 private:
  // Bounded NUL-padded copy into a fixed char[32] (strncpy-free: /W3 clean).
  void SetReason(const char* r) {
    size_t i = 0;
    if (r != nullptr)
      for (; i < sizeof(reason_) - 1 && r[i] != '\0'; ++i) reason_[i] = r[i];
    for (; i < sizeof(reason_); ++i) reason_[i] = '\0';
  }
  std::map<uint32_t, std::shared_ptr<SubSendQueue>> subs_;
  char reason_[32] = {0};
};

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_SUBSCRIBERS_H_
