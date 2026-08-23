// cursor_manager.h - host cursor position channel for xnc-desktop (plan
// M1-Slice3 Task 1, spec 7.8). While at least one rt subscriber exists,
// polls GetCursorInfo every 8ms and fires an event ONLY when the cursor
// position or visibility changed (first sample always fires, giving every
// fresh subscriber an initial position). Positions arrive in virtual
// desktop pixels and are mapped into the HOST_HELLO w/h stream space (the
// inverse of InputManager's MOVE mapping) so the viewer can overlay the
// dot on the video directly.
//
// The GetCursorInfo/GetSystemMetrics entry points are injectable Opts
// function pointers (headless selftest drives the full poll logic; the
// real path runs on any interactive desktop and is exercised end to end
// by the T6 probe).
#ifndef XNC_NATIVE_DESKTOP_CURSOR_MANAGER_H_
#define XNC_NATIVE_DESKTOP_CURSOR_MANAGER_H_

#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>

#include <atomic>
#include <cmath>
#include <cstdint>
#include <functional>
#include <mutex>
#include <thread>

namespace xnc {

// Inverse of the MOVE mapping: virtual-desktop px -> HOST_HELLO stream px
// (normalized over vd_origin..vd_origin+vd_extent, clamped to the stream).
inline int32_t MapCursorToStream(int32_t virt, uint32_t hello_dim, int vd_origin,
                                 int vd_extent) {
  if (hello_dim == 0 || vd_extent <= 0) return 0;
  double n =
      (static_cast<double>(virt) - static_cast<double>(vd_origin)) /
      static_cast<double>(vd_extent);
  if (n < 0) n = 0;
  if (n > 1) n = 1;
  long v = std::lround(n * static_cast<double>(hello_dim));
  if (v < 0) v = 0;
  if (v > static_cast<long>(hello_dim)) v = static_cast<long>(hello_dim);
  return static_cast<int32_t>(v);
}

class CursorManager {
 public:
  // Receives (x, y, visible) in HOST_HELLO stream space.
  using Sink = std::function<void(int32_t, int32_t, uint8_t)>;

  struct Opts {
    uint32_t hello_w = 0, hello_h = 0;  // HOST_HELLO stream space
    uint32_t poll_ms = 8;               // spec 7.8: 8ms poll
    BOOL(WINAPI *get_cursor_info)(PCURSORINFO) = &::GetCursorInfo;
    int(WINAPI *get_system_metrics)(int) = &::GetSystemMetrics;
  };

  explicit CursorManager(const Opts& o = Opts{}) : o_(o) {}
  ~CursorManager();
  CursorManager(const CursorManager&) = delete;
  CursorManager& operator=(const CursorManager&) = delete;

  // Installs the event sink and (re)starts the poll thread; idempotent.
  // A restart resets the change-detection baseline so the sink gets one
  // initial event for the fresh subscriber generation.
  void Start(Sink sink);
  // Stops the poll thread; idempotent; safe when never started.
  void Stop();
  bool running() const { return running_.load(); }

  // Stores the sink without spawning the thread (selftest drives PollOnce
  // synchronously; Start calls this internally).
  void SetSink(Sink sink);

  // One poll step: maps the current cursor into the stream space and
  // fires the sink iff position/visibility changed (or on first sample).
  // Returns true when an event fired. GetCursorInfo failures (e.g. no
  // desktop access) are silent no-ops. This is the thread body, exposed
  // for the headless selftest.
  bool PollOnce();

 private:
  Opts o_;
  std::mutex mu_;  // guards sink_ and the last-sample baseline
  Sink sink_;
  bool has_last_ = false;
  int32_t last_x_ = 0, last_y_ = 0;
  uint8_t last_visible_ = 0;
  std::thread th_;
  std::atomic<bool> stop_{false};
  std::atomic<bool> running_{false};
};

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_CURSOR_MANAGER_H_
