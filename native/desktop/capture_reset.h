// capture_reset.h - unified capture reset coordinator (M2-Slice1 Task 2,
// spec §7.5). ONE entry point for every capture rebuild trigger:
//
//   desktop switch (DesktopWatch transition, T1 evidence: any entry into
//     the secure desktop revokes the duplication and re-duplication is
//     DENIED 0x80070005 until it returns)
//   resolution / mode change (frame size != encoder size)
//   ACCESS_LOST with a denied internal rebuild
//   backend ladder switches (change_backend; T3 GDI wiring joins here)
//
// Contract (pipeline side): producers call RequestReset(reason) from ANY
// thread (watch poll thread, pipeline thread). Requests MERGE inside a
// <=100 ms debounce window: the first request arms `ready_at = now + debounce`;
// later requests inside the window bump the merged counter and may raise
// the reason's priority (desktop_switch > resolution > access_lost /
// change_backend). The pipeline thread consumes via TakeReset(), which fires
// only once the window has elapsed - so two requests 50 ms apart execute ONE
// reset carrying the highest-priority reason.
//
// The reset EXECUTION (suspend -> wait for the desktop to return -> rebuild
// -> new base frame -> ForceIDR -> DISPLAY_CHANGED on dimension change)
// lives in pipeline.cpp; this header only owns request merging, debounce,
// the desktop gate and the counters. Header-only + injectable clock on
// purpose: desktop_selftest drives the merge table without any Win32.
#ifndef XNC_NATIVE_DESKTOP_CAPTURE_RESET_H_
#define XNC_NATIVE_DESKTOP_CAPTURE_RESET_H_

#include <cstdint>
#include <cstring>
#include <mutex>

namespace xnc {

// Stable reset reason vocabulary (STATE-side strings; also the
// DISPLAY_CHANGED 0x010A reason field).
inline constexpr char kResetReasonDesktopSwitch[] = "desktop_switch";
inline constexpr char kResetReasonResolution[] = "resolution";
inline constexpr char kResetReasonAccessLost[] = "access_lost";
inline constexpr char kResetReasonChangeBackend[] = "change_backend";
// M2-Slice3 Task 5: MSG_SWITCH_DISPLAY 0x0128 - the rebuild rebinds the
// selected output (DxgiSelectDisplay) and DISPLAY_CHANGED carries this reason.
inline constexpr char kResetReasonSwitch[] = "switch";

// Width of the DISPLAY_CHANGED reason field (0x010A [char reason[24]]).
inline constexpr size_t kResetReasonMax = 24;

// Pipeline-side view of the DesktopWatch gate (DEFAULT vs anything else).
// Declared here so pipeline.h needs no desktop_watch.h/windows.h.
enum class ResetDesktop { kDefault = 0, kNonDefault = 1 };

// Merge priority: a pending request can be superseded only by a
// higher-priority reason arriving inside the same debounce window
// (access_lost -> desktop_switch when the watch flips mid-debounce).
inline int ResetReasonPriority(const char* reason) {
  if (reason == nullptr) return 0;
  if (std::strcmp(reason, kResetReasonDesktopSwitch) == 0) return 3;
  if (std::strcmp(reason, kResetReasonResolution) == 0) return 2;
  if (std::strcmp(reason, kResetReasonSwitch) == 0) return 2;  // explicit request
  return 1;  // access_lost / change_backend / unknown
}

// Bounded NUL-padded copy into dst[cap] (strncpy-free: /W3 clean).
inline void CopyReason(char* dst, size_t cap, const char* src) {
  if (dst == nullptr || cap == 0) return;
  size_t i = 0;
  if (src != nullptr)
    for (; i < cap - 1 && src[i] != '\0'; ++i) dst[i] = src[i];
  for (; i < cap; ++i) dst[i] = '\0';
}

class CaptureReset {
 public:
  struct Opts {
    // Merge window (plan Task 2: <= 100 ms).
    uint32_t debounce_ms = 100;
    // REQUIRED-ish monotonic millisecond clock (injectable: selftest fakes
    // it deterministically; xnc-desktop passes GetTickCount64). Null = take
    // immediately (no debounce; legacy behavior family).
    uint64_t (*clock_ms)() = nullptr;
    // DesktopWatch gate provider (null = always kDefault: reset waits for
    // nothing and retries on rebuild failure alone).
    ResetDesktop (*desktop_fn)(void*) = nullptr;
    void* desktop_ctx = nullptr;
  };

  explicit CaptureReset(const Opts& o = Opts{}) : o_(o) {}

  // Thread-safe. Merges per the debounce/priority rules; every call counts
  // in requests(). Null/empty reason is recorded as "?".
  void RequestReset(const char* reason) {
    std::lock_guard<std::mutex> lk(mu_);
    ++requests_;
    char r[kResetReasonMax];
    CopyReason(r, sizeof(r), reason != nullptr && reason[0] != '\0' ? reason : "?");
    if (!pending_) {
      pending_ = true;
      ready_ms_ = o_.clock_ms != nullptr ? o_.clock_ms() + o_.debounce_ms : 0;
      CopyReason(reason_, sizeof(reason_), r);
    } else {
      ++merged_;
      if (ResetReasonPriority(r) > ResetReasonPriority(reason_))
        CopyReason(reason_, sizeof(reason_), r);
    }
  }

  // Pipeline thread: consumes a request whose debounce window has elapsed,
  // copying the winning reason (bounded, NUL-padded). False when nothing is
  // pending or the window has not elapsed yet.
  bool TakeReset(char* reason_out, size_t cap) {
    std::lock_guard<std::mutex> lk(mu_);
    if (!pending_) return false;
    if (o_.clock_ms != nullptr && o_.clock_ms() < ready_ms_) return false;
    pending_ = false;
    ++executed_;
    CopyReason(last_reason_, sizeof(last_reason_), reason_);
    if (reason_out != nullptr && cap > 0) CopyReason(reason_out, cap, reason_);
    return true;
  }

  // Current desktop gate (kDefault when no provider is wired).
  ResetDesktop Desktop() const {
    return o_.desktop_fn != nullptr ? o_.desktop_fn(o_.desktop_ctx)
                                    : ResetDesktop::kDefault;
  }

  uint32_t requests() const {
    std::lock_guard<std::mutex> lk(mu_);
    return requests_;
  }
  uint32_t merged() const {
    std::lock_guard<std::mutex> lk(mu_);
    return merged_;
  }
  uint32_t executed() const {
    std::lock_guard<std::mutex> lk(mu_);
    return executed_;
  }
  void LastReason(char* out, size_t cap) const {
    std::lock_guard<std::mutex> lk(mu_);
    CopyReason(out, cap, last_reason_);
  }

 private:
  Opts o_;
  mutable std::mutex mu_;
  bool pending_ = false;
  uint64_t ready_ms_ = 0;
  char reason_[kResetReasonMax] = {0};
  uint32_t requests_ = 0, merged_ = 0, executed_ = 0;
  char last_reason_[kResetReasonMax] = {0};
};

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_CAPTURE_RESET_H_
