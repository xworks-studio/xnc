// watchdog.h - self-healing watchdog (spec 15): a monitor thread checks a
// progress timestamp every 5s; if the serve loop has reported no progress
// for more than 30s it logs an error and exits the process with code 1 so
// the supervisor restarts it. The serve loop's blocking waits are sliced
// into <=5s chunks that Heartbeat() between polls (see pipe_server.cpp), so
// a quiet-but-healthy loop never trips the watchdog - only a stuck one does.
#ifndef XNC_NATIVE_CORE_WATCHDOG_H_
#define XNC_NATIVE_CORE_WATCHDOG_H_

#include <atomic>
#include <chrono>
#include <cstdlib>
#include <thread>

#include "log.h"

namespace xnc {

class Watchdog {
 public:
  void Start() {
    last_ms_ = NowMs();
    if (!thread_.joinable()) thread_ = std::thread(&Watchdog::Run, this);
  }
  ~Watchdog() {
    stop_ = true;
    if (thread_.joinable()) thread_.join();
  }
  void Heartbeat() { last_ms_.store(NowMs(), std::memory_order_relaxed); }

 private:
  static int64_t NowMs() {
    return std::chrono::duration_cast<std::chrono::milliseconds>(
               std::chrono::steady_clock::now().time_since_epoch())
        .count();
  }
  void Run() {
    for (;;) {
      // 250ms slices keep Ctrl+C shutdown latency low between checks.
      for (int i = 0; i < kCheckMs / 250; i++) {
        std::this_thread::sleep_for(std::chrono::milliseconds(250));
        if (stop_.load()) return;
      }
      int64_t starved = NowMs() - last_ms_.load(std::memory_order_relaxed);
      if (starved > kStarveMs) {
        XNC_LOG_ERROR("watchdog: no progress for %lld ms, exiting", (long long)starved);
        std::exit(1);  // self-kill so the supervisor restarts us (spec 15)
      }
    }
  }

  static constexpr int64_t kCheckMs = 5000, kStarveMs = 30000;
  std::atomic<int64_t> last_ms_{0};
  std::atomic<bool> stop_{false};
  std::thread thread_;
};

}  // namespace xnc

#endif  // XNC_NATIVE_CORE_WATCHDOG_H_
