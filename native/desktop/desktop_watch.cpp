// desktop_watch.cpp - see desktop_watch.h. The state machine is a pure
// function over (observed name, timestamp); the Win32 polling thread and
// the seam-injectable watch live here.
#include "desktop_watch.h"

#include <cstdio>
#include <cwchar>

#include "../common/log.h"

namespace xnc {

const char* DesktopStateName(DesktopState s) {
  switch (s) {
    case DesktopState::kDefault: return "Default";
    case DesktopState::kTransition: return "Transition";
    case DesktopState::kWinlogon: return "Winlogon";
  }
  return "?";
}

bool IsDefaultDesktopName(const char* name) {
  if (name == nullptr) return false;
  // Case-insensitive compare against "Default" (the OS spells it exactly
  // so; defensive only).
  static const char kWant[] = "Default";
  for (size_t i = 0;; ++i) {
    char a = name[i], b = kWant[i];
    if (a >= 'A' && a <= 'Z') a = static_cast<char>(a - 'A' + 'a');
    if (b >= 'A' && b <= 'Z') b = static_cast<char>(b - 'A' + 'a');
    if (a != b) return false;
    if (b == '\0') return true;
  }
}

bool DesktopStep(DesktopMachineState* st, const char* name, uint64_t now_ms,
                 uint64_t transition_timeout_ms, DesktopTransition* ev) {
  if (st == nullptr || name == nullptr || *name == '\0') return false;

  const DesktopState from = st->state;
  const bool to_default = IsDefaultDesktopName(name);
  bool changed = false;
  const char* reason = nullptr;

  if (to_default) {
    if (st->state != DesktopState::kDefault) {
      st->state = DesktopState::kDefault;
      st->state_since_ms = now_ms;
      st->nondefault_since_ms = 0;
      changed = true;
      reason = "back_to_default";
    }
    std::snprintf(st->name, sizeof(st->name), "%s", kDefaultDesktopName);
  } else {
    const bool name_changed = std::strcmp(st->name, name) != 0;
    if (st->state == DesktopState::kDefault || name_changed) {
      // Fresh non-default observation: (re)enter TRANSITION and restart the
      // debounce timer - a "Winlogon" -> "Other" switch must wait its own
      // full window before WINLOGON again.
      st->state = DesktopState::kTransition;
      st->state_since_ms = now_ms;
      st->nondefault_since_ms = now_ms;
      changed = true;
      reason = "name_change";
    } else if (st->state == DesktopState::kTransition &&
               now_ms - st->nondefault_since_ms >= transition_timeout_ms) {
      st->state = DesktopState::kWinlogon;
      st->state_since_ms = now_ms;
      changed = true;
      reason = "transition_timeout";
    }
    // WINLOGON + same name: steady, no event.
    std::snprintf(st->name, sizeof(st->name), "%s", name);
  }

  if (!changed || ev == nullptr) return false;
  ev->from = from;
  ev->to = st->state;
  ev->at_ms = now_ms;
  std::snprintf(ev->name, sizeof(ev->name), "%s", name);
  std::snprintf(ev->reason, sizeof(ev->reason), "%s", reason != nullptr ? reason : "?");
  return true;
}

// ---- DesktopWatch ----

DesktopWatch::~DesktopWatch() { Stop(); }

bool DesktopWatch::Start() {
  if (thread_.joinable()) return false;
  stop_.store(false);
  thread_ = std::thread([this] { ThreadMain(); });
  XNC_LOG_INFO("desktop_watch_start poll_ms=%u timeout_ms=%llu",
               static_cast<unsigned>(o_.poll_ms),
               static_cast<unsigned long long>(o_.transition_timeout_ms));
  return true;
}

void DesktopWatch::Stop() {
  if (!thread_.joinable()) return;
  stop_.store(true);
  thread_.join();
  thread_ = std::thread();
  DesktopMachineState st;
  {
    std::lock_guard<std::mutex> lk(mu_);
    st = machine_;
  }
  XNC_LOG_INFO("desktop_watch_stop polls=%llu failures=%llu state=%s name=%s",
               static_cast<unsigned long long>(polls_),
               static_cast<unsigned long long>(poll_failures_),
               DesktopStateName(st.state), st.name);
}

DesktopMachineState DesktopWatch::Snapshot() const {
  std::lock_guard<std::mutex> lk(mu_);
  return machine_;
}

void DesktopWatch::CurrentName(char* out, size_t cap) const {
  if (out == nullptr || cap == 0) return;
  std::lock_guard<std::mutex> lk(mu_);
  std::snprintf(out, cap, "%s", observed_ ? machine_.name : "");
}

uint64_t DesktopWatch::polls() const {
  std::lock_guard<std::mutex> lk(mu_);
  return polls_;
}

uint64_t DesktopWatch::poll_failures() const {
  std::lock_guard<std::mutex> lk(mu_);
  return poll_failures_;
}

void DesktopWatch::ThreadMain() {
  for (;;) {
    if (stop_.load()) break;
    PollOnce();
    // Sliced sleep keeps Stop responsive (<= 25 ms granularity).
    for (uint32_t slept = 0; slept < o_.poll_ms && !stop_.load();) {
      const uint32_t slice = o_.poll_ms - slept > 25 ? 25 : o_.poll_ms - slept;
      Sleep(slice);
      slept += slice;
    }
  }
}

void DesktopWatch::PollOnce() {
  char name[kDesktopNameMax];
  bool failed = false;
  DWORD err = 0;

  // OpenInputDesktop reflects the desktop actually receiving input (NOT
  // GetThreadDesktop, which would just echo our own binding). DESKTOP_
  // READOBJECTS is the access needed to query the object's name.
  HDESK desk = o_.open_input_desktop(0, FALSE, DESKTOP_READOBJECTS);
  if (desk == nullptr) {
    err = GetLastError();
    std::snprintf(name, sizeof(name), "%s", kDesktopOpenFailedName);
    failed = true;
  } else {
    wchar_t wname[kDesktopNameMax];
    DWORD needed = 0;
    if (!o_.get_user_object_info(desk, UOI_NAME, wname, sizeof(wname),
                                 &needed)) {
      err = GetLastError();
      std::snprintf(name, sizeof(name), "%s", kDesktopNameFailedName);
      failed = true;
    } else {
      // Desktop names are ASCII ("Default"/"Winlogon"); narrow defensively.
      size_t k = 0;
      for (; k + 1 < sizeof(name) && wname[k] != L'\0'; ++k)
        name[k] = wname[k] < 128 ? static_cast<char>(wname[k]) : '?';
      name[k] = '\0';
      if (k == 0) {  // degenerate empty name: treat as a failed query
        std::snprintf(name, sizeof(name), "%s", kDesktopNameFailedName);
        failed = true;
      }
    }
    o_.close_desktop(desk);
  }

  DesktopMachineState st;
  {
    std::lock_guard<std::mutex> lk(mu_);
    st = machine_;
    ++polls_;
    if (failed) {
      ++poll_failures_;
      if (!in_failure_run_) {
        in_failure_run_ = true;
        XNC_LOG_ERROR("desktop_poll_failed err=%lu name_substituted=%s "
                      "(entering state machine as synthetic input)",
                      err, name);
      }
    } else if (in_failure_run_) {
      in_failure_run_ = false;
      XNC_LOG_INFO("desktop_poll_recovered");
    }
  }

  DesktopTransition ev;
  const bool fired =
      DesktopStep(&st, name, o_.clock_ms(), o_.transition_timeout_ms, &ev);
  {
    std::lock_guard<std::mutex> lk(mu_);
    machine_ = st;
    observed_ = true;
  }
  if (fired) {
    XNC_LOG_INFO("desktop_transition from=%s to=%s name=%s state=%s reason=%s at_ms=%llu",
                 DesktopStateName(ev.from), DesktopStateName(ev.to), ev.name,
                 DesktopStateName(ev.to), ev.reason,
                 static_cast<unsigned long long>(ev.at_ms));
    if (o_.on_transition) o_.on_transition(ev);
  }
}

}  // namespace xnc
