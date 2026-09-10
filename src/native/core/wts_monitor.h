// wts_monitor.h - active-console-session monitor for xnc-core (M2-Slice1
// Task 4, spec §6.5). Watches exactly ONE thing: the session id that
// WTSGetActiveConsoleSessionId reports (the session attached to the physical
// console - the only legal capture target per SessionTargetAllowed), and
// fires a callback when it CHANGES. A change (fast user switch, logoff to a
// new logon) invalidates a running capture child: pipe_server terminates it
// (scoped by its stored handle, NEVER by image name) and the next
// StartCapture re-spawns into the new session.
//
// Hybrid transport (plan preference - notification + poll):
//   * notification leg: a message-only window on the monitor thread receives
//     WM_WTSSESSION_CHANGE via WTSRegisterSessionNotificationEx. Tried as
//     NOTIFY_FOR_ALL_SESSIONS (needs the caller to run as SYSTEM, which the
//     production core does), falling back to NOTIFY_FOR_THIS_SESSION. The
//     notification names WHY the console changed (console_connect /
//     session_logon / ...) and arrives immediately.
//   * poll leg (the floor, always on): a SetTimer(poll_ms=500) on the same
//     window re-reads WTSGetActiveConsoleSessionId. Notifications can be
//     unavailable (non-SYSTEM console run, registration failure); polling
//     always works and is the only leg the headless selftest drives (via the
//     injectable session_fn seam). use_notifications=false forces pure-poll
//     mode with a sliced-sleep loop (no window at all).
// The two legs converge on one sampler: refresh the cached session id, and
// when it differs from the cached value fire on_change with the triggering
// reason ("poll" for the poll leg). WTSGetActiveConsoleSessionId stays the
// AUTHORITY for the value even on the notification leg - the notification
// only contributes the reason string. A lock/unlock does NOT change the
// console session id and correctly fires nothing.
//
// The callback runs on the monitor thread. Production wiring
// (OnActiveConsoleSessionChanged in pipe_server.cpp) briefly blocks on the
// capture mutex, which a StartCapture in flight may hold for ~2s (pipe-ready
// wait) - bounded, monitor timing is not latency-critical.
//
// Win32 entry points are injectable function pointers (DesktopWatch
// pattern) so the selftest scripts session ids with zero real WTS state.
#ifndef XNC_NATIVE_CORE_WTS_MONITOR_H_
#define XNC_NATIVE_CORE_WTS_MONITOR_H_

#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>
#include <wtsapi32.h>  // WTSRegisterSessionNotificationEx (wtsapi32.lib)

#include <atomic>
#include <cstdint>
#include <functional>
#include <mutex>
#include <thread>

namespace xnc {

// Poll cadence (plan Task 4: 500ms).
inline constexpr uint32_t kWtsPollMs = 500;

// One console-session change: from/to session ids plus the leg that
// observed it. reason = a WTS notification class name (WtsChangeReasonForCode)
// when the notification leg saw it, "poll" for the poll leg.
struct WtsSessionChange {
  uint32_t from = 0xFFFFFFFF;
  uint32_t to = 0xFFFFFFFF;
  char reason[24] = {0};  // console_connect / ... / session_logoff / poll
};

// WTS notification code (WM_WTSSESSION_CHANGE wParam) -> stable reason name.
// Pure; unknown codes map to "unknown" (future Windows codes stay visible).
const char* WtsChangeReasonForCode(DWORD code);

// The watch itself. Start() spawns the thread (notification window + timer
// by default); Stop() joins it. console_session() is safe before Start
// (calls the session_fn live - the pre-monitor behavior of
// WTSGetActiveConsoleSessionId at the call site) and while running (returns
// the thread-maintained cache).
class WtsMonitor {
 public:
  using SessionFn = DWORD (WINAPI*)();  // matches WTSGetActiveConsoleSessionId
  using ChangeFn = std::function<void(const WtsSessionChange&)>;

  struct Opts {
    uint32_t poll_ms = kWtsPollMs;
    bool use_notifications = true;  // false = pure poll loop, no window
    SessionFn session_fn = &::WTSGetActiveConsoleSessionId;
    ChangeFn on_change;             // optional; runs on the monitor thread
  };

  explicit WtsMonitor(const Opts& o = Opts{}) : o_(o) {}
  ~WtsMonitor();  // Stop (process-exit hard rule: never leak the thread)
  WtsMonitor(const WtsMonitor&) = delete;
  WtsMonitor& operator=(const WtsMonitor&) = delete;

  // Spawns the monitor thread. False when already running.
  bool Start();

  // Stops + joins (<= poll_ms with notifications, <=25ms slices in poll
  // mode). Idempotent; safe when never started. After Stop the cached
  // console_session() reverts to live session_fn() reads.
  void Stop();

  bool running() const { return thread_.joinable(); }

  // Replace the options BEFORE Start (RunPipeServer wires on_change here);
  // ignored while running so the thread never sees options mutate.
  void Configure(const Opts& o) {
    if (thread_.joinable()) return;
    o_ = o;
  }

  // Current active console session id. While running: the monitored cache
  // (notification-fast, poll-fresh <=500ms). Not running: session_fn() live.
  uint32_t console_session();

  // True when WTSRegisterSessionNotificationEx succeeded (diag: false means
  // the poll leg is the only one).
  bool notifications() const { return notified_; }

  // Session-change callbacks fired so far (observability + selftest).
  uint64_t changes() const;

  // Test seam: replace the session source BEFORE Start (nullptr restores
  // WTSGetActiveConsoleSessionId). Ignored while running.
  void SetSessionFnForTest(SessionFn fn);

 private:
  void ThreadMain();
  bool CreateNotificationWindow();  // window + WTS registration + timer
  void RunMessageLoop();
  void RunPollLoop();
  void Sample(const char* reason);  // the converging sampler (see header)
  void ShutdownWindow();
  // Message-only window procedure (dispatches WM_WTSSESSION_CHANGE /
  // WM_TIMER to Sample); static so WNDCLASSW can take its address.
  static LRESULT CALLBACK WndProc(HWND h, UINT msg, WPARAM wp, LPARAM lp);

  Opts o_;
  std::thread thread_;
  std::atomic<bool> stop_{false};
  std::atomic<HWND> hwnd_{nullptr};
  bool notified_ = false;          // monitor-thread written, read after join
  mutable std::mutex mu_;          // guards cached_, primed_, changes_
  uint32_t cached_ = 0xFFFFFFFF;
  bool primed_ = false;            // first Sample sets the baseline silently
  uint64_t changes_ = 0;
};

}  // namespace xnc

#endif  // XNC_NATIVE_CORE_WTS_MONITOR_H_
