// frame_queue.h - bounded capture->encode frame handoff (pipeline-decouple).
// The single-threaded capture->encode loop capped the effective frame rate
// at 1/(acquire-wait + encode + misc) because encode ran INSIDE the acquire
// loop. The decoupled pipeline runs capture and encode on separate threads
// joined by this queue:
//
//   capture thread  -> Push (one owned copy per frame; drop-oldest on full,
//                       keep the LATEST frame - ADR-014)
//   encode thread   -> TryPop / WaitPop (drain in FIFO order; drops never
//                       reorder - the queue only evicts the OLDEST item)
//
// Depth 2 = one frame in flight in the encoder + one pending; overflow
// evicts the oldest so the encoder always sees the freshest frame (latency
// bounded by encode time, not queue backlog). The capture thread paces its
// pushes to spf, so the queue fills only during encode stalls or present
// bursts - exactly the burst absorber ADR-014 wants.
//
// Shutdown: Shutdown() wakes every waiter; WaitPop keeps returning queued
// frames until the queue is drained, then returns false forever (the encode
// thread's exit condition). Header-only on purpose so desktop_selftest can
// unit-test drop-oldest/order/shutdown without a desktop or an encoder.
#ifndef XNC_NATIVE_DESKTOP_FRAME_QUEUE_H_
#define XNC_NATIVE_DESKTOP_FRAME_QUEUE_H_

#include <condition_variable>
#include <cstdint>
#include <deque>
#include <mutex>

#include "capture.h"  // FrameBlob

namespace xnc {

class FrameQueue {
 public:
  explicit FrameQueue(size_t depth) : depth_(depth > 0 ? depth : 1) {}

  // Capture thread: hands one scaled frame over. The blob becomes owned here
  // (a copy of the capture's persistent buffer - valid until the next
  // Acquire). When full, the OLDEST queued frame is dropped first so the
  // encoder always receives the latest frame (ADR-014).
  void Push(FrameBlob f) {
    std::lock_guard<std::mutex> lk(mu_);
    if (q_.size() >= depth_) q_.pop_front();
    q_.push_back(std::move(f));
    cv_.notify_one();
  }

  // Encode thread: non-blocking take; false when empty.
  bool TryPop(FrameBlob* out) {
    std::lock_guard<std::mutex> lk(mu_);
    return PopLocked(out);
  }

  // Encode thread: blocks up to timeout_ms for one frame (idle wakeups let
  // the encode thread run warm-up re-feed checks on a static screen). True
  // iff a frame was taken; false on timeout OR after Shutdown when the queue
  // is drained (the encode thread's exit condition).
  bool WaitPop(FrameBlob* out, uint32_t timeout_ms) {
    std::unique_lock<std::mutex> lk(mu_);
    cv_.wait_for(lk, std::chrono::milliseconds(timeout_ms),
                 [this] { return !q_.empty() || done_; });
    return PopLocked(out);
  }

  bool Empty() const {
    std::lock_guard<std::mutex> lk(mu_);
    return q_.empty();
  }

  size_t Size() const {
    std::lock_guard<std::mutex> lk(mu_);
    return q_.size();
  }

  // Shutdown: wakes every waiter; queued frames are still delivered (the
  // encode thread drains them before exiting - nothing captured is dropped
  // at run end). Idempotent; safe from either thread.
  void Shutdown() {
    std::lock_guard<std::mutex> lk(mu_);
    done_ = true;
    cv_.notify_all();
  }

  bool Done() const {
    std::lock_guard<std::mutex> lk(mu_);
    return done_;
  }

 private:
  // Caller holds mu_. Pops the oldest frame (FIFO - Push only ever evicts
  // from the FRONT, so the remaining order is always the arrival order).
  bool PopLocked(FrameBlob* out) {
    if (q_.empty()) return false;
    if (out != nullptr) *out = std::move(q_.front());
    q_.pop_front();
    return true;
  }

  size_t depth_;
  mutable std::mutex mu_;
  std::condition_variable cv_;
  std::deque<FrameBlob> q_;
  bool done_ = false;
};

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_FRAME_QUEUE_H_
