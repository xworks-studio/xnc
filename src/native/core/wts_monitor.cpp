// wts_monitor.cpp - see wts_monitor.h. The notification leg owns a
// message-only window class registered once per process; the poll leg is a
// SetTimer on that window (or a sliced-sleep loop in pure-poll mode). Both
// legs call Sample(), which owns the cache + change detection.
#include "wts_monitor.h"

#include <cstdio>

#include "../common/log.h"

namespace xnc {

// Our private messages (WM_APP range, never conflicts with WM_WTSSESSION_
// CHANGE / WM_TIMER). Namespace scope (not anonymous): WndProc - a class
// member definition - references them from outside the anonymous namespace
// below.
constexpr UINT kMsgStop = WM_APP + 1;
constexpr UINT kPollTimerId = 1;
const wchar_t kWcName[] = L"XncWtsMonitorWnd";

// The window extra bytes carry the WtsMonitor* (set by the creating thread
// immediately after CreateWindowEx - before any message is dispatched, so
// the GWLP_USERDATA read in WndProc never sees a stale value).
LRESULT CALLBACK WtsMonitor::WndProc(HWND h, UINT msg, WPARAM wp, LPARAM lp) {
  switch (msg) {
    case WM_WTSSESSION_CHANGE: {
      // wp = notification code, lp = session id. The session VALUE comes
      // from the authority (session_fn) inside Sample; the code only names
      // the reason.
      if (WtsMonitor* m = reinterpret_cast<WtsMonitor*>(
              GetWindowLongPtrW(h, GWLP_USERDATA)))
        m->Sample(WtsChangeReasonForCode(static_cast<DWORD>(wp)));
      return 0;
    }
    case WM_TIMER:
      if (wp == kPollTimerId) {
        if (WtsMonitor* m = reinterpret_cast<WtsMonitor*>(
                GetWindowLongPtrW(h, GWLP_USERDATA)))
          m->Sample("poll");
      }
      return 0;
    case kMsgStop:
      DestroyWindow(h);
      return 0;
    case WM_DESTROY:
      PostQuitMessage(0);
      return 0;
    default:
      return DefWindowProcW(h, msg, wp, lp);
  }
}

const char* WtsChangeReasonForCode(DWORD code) {
  switch (code) {
    case WTS_CONSOLE_CONNECT: return "console_connect";
    case WTS_CONSOLE_DISCONNECT: return "console_disconnect";
    case WTS_REMOTE_CONNECT: return "remote_connect";
    case WTS_REMOTE_DISCONNECT: return "remote_disconnect";
    case WTS_SESSION_LOGON: return "session_logon";
    case WTS_SESSION_LOGOFF: return "session_logoff";
    case WTS_SESSION_LOCK: return "session_lock";
    case WTS_SESSION_UNLOCK: return "session_unlock";
    case WTS_SESSION_REMOTE_CONTROL: return "remote_control";
    default: return "unknown";
  }
}

WtsMonitor::~WtsMonitor() { Stop(); }

bool WtsMonitor::Start() {
  if (thread_.joinable()) return false;
  stop_.store(false);
  hwnd_.store(nullptr);
  notified_ = false;
  // Prime the cache SYNCHRONOUSLY so console_session() is correct in the
  // window between Start() returning and the thread's first Sample (the
  // live T4 run caught the unprimed 0xFFFFFFFF in the startup log; first
  // Sample on the thread is then a no-op, primed_ already set).
  Sample("baseline");
  thread_ = std::thread([this] { ThreadMain(); });
  XNC_LOG_INFO("wts_monitor_start poll_ms=%u notifications=%d console=%lu",
               static_cast<unsigned>(o_.poll_ms),
               o_.use_notifications ? 1 : 0,
               static_cast<unsigned long>(console_session()));
  return true;
}

void WtsMonitor::Stop() {
  if (!thread_.joinable()) return;
  stop_.store(true);
  if (HWND h = hwnd_.load()) PostMessageW(h, kMsgStop, 0, 0);
  thread_.join();
  thread_ = std::thread();
  XNC_LOG_INFO("wts_monitor_stop changes=%llu notified=%d",
               static_cast<unsigned long long>(changes()),
               notified_ ? 1 : 0);
}

uint32_t WtsMonitor::console_session() {
  if (thread_.joinable()) {
    std::lock_guard<std::mutex> lk(mu_);
    return cached_;
  }
  const uint32_t live = static_cast<uint32_t>(o_.session_fn());
  std::lock_guard<std::mutex> lk(mu_);
  cached_ = live;  // keep the observability cache coherent outside Start
  return live;
}

uint64_t WtsMonitor::changes() const {
  std::lock_guard<std::mutex> lk(mu_);
  return changes_;
}

void WtsMonitor::SetSessionFnForTest(SessionFn fn) {
  if (thread_.joinable()) return;  // ignored while running, by contract
  o_.session_fn = fn != nullptr ? fn : &::WTSGetActiveConsoleSessionId;
}

void WtsMonitor::ThreadMain() {
  // Baseline (a no-op when Start() already primed synchronously - the
  // pre-Sample() race guard): the first Sample never fires a synthetic
  // 0xFFFFFFFF->X "change".
  Sample("baseline");

  if (!o_.use_notifications || !CreateNotificationWindow()) {
    if (o_.use_notifications)
      XNC_LOG_INFO("wts_monitor: notification window unavailable, poll-only");
    RunPollLoop();
    return;
  }
  RunMessageLoop();
  ShutdownWindow();
}

bool WtsMonitor::CreateNotificationWindow() {
  WNDCLASSW wc{};
  wc.lpfnWndProc = &WtsMonitor::WndProc;
  wc.hInstance = GetModuleHandleW(nullptr);
  wc.lpszClassName = kWcName;
  // RegisterClassW fails harmlessly when already registered (multiple
  // monitor instances / restarts) - that is the success case too.
  RegisterClassW(&wc);

  // HWND_MESSAGE parent = message-only window: no z-order, no visibility,
  // receives posted/sent messages and WTS notifications only.
  HWND h = CreateWindowExW(0, kWcName, kWcName, 0, 0, 0, 0, 0, HWND_MESSAGE,
                           nullptr, wc.hInstance, nullptr);
  if (h == nullptr) {
    XNC_LOG_ERROR("wts_monitor: CreateWindow(message-only) failed err=%lu",
                  GetLastError());
    return false;
  }
  SetWindowLongPtrW(h, GWLP_USERDATA, reinterpret_cast<LONG_PTR>(this));
  hwnd_.store(h);

  // NOTIFY_FOR_ALL_SESSIONS is what a SYSTEM core wants (changes on ANY
  // session re-attach the console); it needs SYSTEM, so a user-token run
  // falls back to NOTIFY_FOR_THIS_SESSION and leans on the poll leg.
  if (!WTSRegisterSessionNotificationEx(WTS_CURRENT_SERVER_HANDLE, h,
                                        NOTIFY_FOR_ALL_SESSIONS)) {
    const DWORD err = GetLastError();
    if (!WTSRegisterSessionNotificationEx(WTS_CURRENT_SERVER_HANDLE, h,
                                          NOTIFY_FOR_THIS_SESSION)) {
      XNC_LOG_ERROR(
          "wts_monitor: session notification registration failed err=%lu "
          "(all+this), poll leg only",
          err);
    } else {
      notified_ = true;
      XNC_LOG_INFO("wts_monitor: registered NOTIFY_FOR_THIS_SESSION only "
                   "(all-sessions denied err=%lu)",
                   err);
    }
  } else {
    notified_ = true;
    XNC_LOG_INFO("wts_monitor: registered NOTIFY_FOR_ALL_SESSIONS");
  }
  SetTimer(h, kPollTimerId, o_.poll_ms, nullptr);
  return true;
}

void WtsMonitor::RunMessageLoop() {
  MSG msg;
  for (;;) {
    // Wake at least every 25ms even with no messages so stop_ is honored
    // even when Stop() ran BEFORE hwnd_ was published (PostMessage skipped)
    // or the post was lost - a plain blocking GetMessage could then hang
    // shutdown forever (self-review catch).
    MsgWaitForMultipleObjects(0, nullptr, FALSE, 25, QS_ALLINPUT);
    if (stop_.load()) break;
    while (PeekMessageW(&msg, nullptr, 0, 0, PM_REMOVE)) {
      if (msg.message == WM_QUIT) return;  // PostQuitMessage from WM_DESTROY
      TranslateMessage(&msg);
      DispatchMessageW(&msg);
    }
  }
}

void WtsMonitor::RunPollLoop() {
  for (;;) {
    if (stop_.load()) break;
    Sample("poll");
    // Sliced sleep keeps Stop responsive (<= 25ms granularity).
    for (uint32_t slept = 0; slept < o_.poll_ms && !stop_.load();) {
      const uint32_t slice = o_.poll_ms - slept > 25 ? 25 : o_.poll_ms - slept;
      Sleep(slice);
      slept += slice;
    }
  }
}

void WtsMonitor::ShutdownWindow() {
  if (HWND h = hwnd_.load()) {
    KillTimer(h, kPollTimerId);  // window is dying; belt and suspenders
    WTSUnRegisterSessionNotificationEx(WTS_CURRENT_SERVER_HANDLE, h);
    // DestroyWindow already ran when the loop ended via kMsgStop/WM_DESTROY;
    // the stop_ break path (no WM_DESTROY) still needs the teardown, and a
    // GetMessage error path leaves the window alive too - IsWindow guards.
    if (IsWindow(h)) DestroyWindow(h);
  }
  hwnd_.store(nullptr);
}

void WtsMonitor::Sample(const char* reason) {
  const uint32_t now = static_cast<uint32_t>(o_.session_fn());
  WtsSessionChange ev;
  bool fire = false;
  {
    std::lock_guard<std::mutex> lk(mu_);
    if (!primed_) {
      primed_ = true;
      cached_ = now;
      return;  // baseline, not a change
    }
    if (now != cached_) {
      ev.from = cached_;
      ev.to = now;
      snprintf(ev.reason, sizeof(ev.reason), "%s",
               reason != nullptr ? reason : "?");
      cached_ = now;
      ++changes_;
      fire = true;
    }
  }
  if (!fire) return;
  XNC_LOG_INFO("session_changed from=%lu to=%lu reason=%s",
               static_cast<unsigned long>(ev.from),
               static_cast<unsigned long>(ev.to), ev.reason);
  if (o_.on_change) o_.on_change(ev);
}

}  // namespace xnc
