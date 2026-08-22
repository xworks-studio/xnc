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
// plus the needs-keyframe flag. Not thread-safe by itself - rt_pipe_server
// serializes access.
class SubSendQueue {
 public:
  enum class AuAction {
    kEnqueued,               // queued for send
    kDroppedNeedKey,         // delta dropped: subscriber awaits its IDR
    kDroppedQueueFull,       // delta dropped: queue full -> needs_keyframe
    kEnqueuedDisplacingDeltas  // key AU queued, stale deltas displaced
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
  // be a fully built kMsgFrame event; it is copied on enqueue.
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
  std::deque<Frame> ctrl_, video_;
  uint32_t depth_;
  bool needs_keyframe_ = true;  // attach == needs (joiner joins from its IDR)
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
