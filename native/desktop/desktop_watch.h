// desktop_watch.h - input-desktop observer + state machine for xnc-desktop
// (M2-Slice1 Task 1, spec §7.3 DesktopSupervisor groundwork). EVIDENCE
// FIRST: this is pure observation, no capture behavior change - the live
// XIAOXIN UAC/lock probe (scripts/uac-probe.ps1 + report) decides what Task
// 2 does on desktop switches.
//
// What it watches: the INPUT desktop of the process's session, polled every
// 500 ms via OpenInputDesktop(0, FALSE, DESKTOP_READOBJECTS) +
// GetUserObjectInformationW(UOI_NAME) + CloseDesktop. OpenInputDesktop (NOT
// GetThreadDesktop) reflects the desktop actually receiving input, so a UAC
// consent prompt / lock screen switch (winsta0\Default -> winsta0\Winlogon)
// is visible even though this process's threads stay bound to Default.
//
// State machine (pure, table-testable - the transition logic is a function
// over (observed name, timestamp), no Win32 inside):
//
//   DEFAULT      input desktop is "Default" (the user desktop).
//   TRANSITION   a non-Default name is seen; held for up to
//                transition_timeout_ms (2 s) to debounce blips.
//   WINLOGON     the non-Default name persisted >= 2 s - the secure desktop
//                (UAC consent, lock screen, logon UI; stock Windows shows it
//                as "Winlogon"). Name is recorded verbatim so the probe log
//                carries the real evidence, whatever it turns out to be.
//
//   DEFAULT   --name_change-->      TRANSITION (name recorded)
//   TRANSITION --same name, >=2s--> WINLOGON   (transition_timeout)
//   TRANSITION --different name-->  TRANSITION (timer restarts, name_change)
//   WINLOGON   --different name-->  TRANSITION (timer restarts, name_change)
//   any non-DEFAULT --"Default"-->  DEFAULT    (back_to_default)
//
// Poll failures (OpenInputDesktop/GetUserObjectInformation returning error)
// are themselves evidence - e.g. a secure desktop refusing access to the
// caller - so they enter the machine as the synthetic names
// "(open_failed)"/"(name_failed)" instead of being skipped.
//
// Every state change fires the Opts::on_transition callback and logs
// `desktop_transition from= to= name= state= reason= at_ms=`; the owner
// (xnc-desktop) exposes the live name to the per-second pipeline beat via
// CurrentName (diag_pipeline ... desktop=<name>). Win32 entry points are
// injectable Opts function pointers (InputManager pattern) so the headless
// selftest drives the threaded watch with scripted desktop names + a fake
// clock and proves CloseDesktop pairing.
#ifndef XNC_NATIVE_DESKTOP_DESKTOP_WATCH_H_
#define XNC_NATIVE_DESKTOP_DESKTOP_WATCH_H_

#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>  // HDESK, OpenInputDesktop, GetUserObjectInformationW

#include <atomic>
#include <cstdint>
#include <cstring>
#include <functional>
#include <mutex>
#include <thread>

namespace xnc {

// ---- state machine (pure; usable without any Win32) ----

enum class DesktopState { kDefault = 0, kTransition = 1, kWinlogon = 2 };

// "Default" / "Transition" / "Winlogon" (log + selftest friendly).
const char* DesktopStateName(DesktopState s);

// Poll cadence (plan Task 1: 500 ms) and TRANSITION->WINLOGON debounce
// window (plan: <= 2 s).
inline constexpr uint32_t kDesktopWatchPollMs = 500;
inline constexpr uint64_t kDesktopTransitionTimeoutMs = 2000;

// Longest desktop name stored (names come from the OS; "Winlogon" fits).
inline constexpr size_t kDesktopNameMax = 64;

// The user desktop's name on winsta0 in a normal console session.
inline constexpr char kDefaultDesktopName[] = "Default";
// Synthetic observation names for poll failures (see header comment).
inline constexpr char kDesktopOpenFailedName[] = "(open_failed)";
inline constexpr char kDesktopNameFailedName[] = "(name_failed)";

// Case-insensitive "Default" check (the OS spells it "Default"; defensive
// against case variants in the evidence log).
bool IsDefaultDesktopName(const char* name);

// The machine's full state. state_since_ms = when the current state was
// entered; nondefault_since_ms = when the CURRENT non-default run began
// (restarted by every name change while non-default - a "Winlogon" ->
// "Other" switch must wait its own 2 s before WINLOGON again).
struct DesktopMachineState {
  DesktopState state = DesktopState::kDefault;
  uint64_t state_since_ms = 0;
  uint64_t nondefault_since_ms = 0;
  char name[kDesktopNameMax] = "Default";
};

// One transition event (from -> to) with the triggering observation.
struct DesktopTransition {
  DesktopState from = DesktopState::kDefault;
  DesktopState to = DesktopState::kDefault;
  uint64_t at_ms = 0;                    // observation clock
  char name[kDesktopNameMax] = {0};      // name that triggered it
  // "name_change" | "back_to_default" | "transition_timeout".
  char reason[48] = {0};
};

// Pure step: observes desktop `name` at `now_ms`. Updates *st in place;
// when a transition fires fills *ev (from/to/name/reason/at_ms) and returns
// true. Same-name no-op observations update nothing observable (state stays,
// name stays) and return false. Null/empty names are ignored (return false).
// Pure on purpose: desktop_selftest.cpp table-drives every rule above.
bool DesktopStep(DesktopMachineState* st, const char* name, uint64_t now_ms,
                 uint64_t transition_timeout_ms, DesktopTransition* ev);

// ---- the watch (threaded wrapper around DesktopStep) ----

class DesktopWatch {
 public:
  struct Opts {
    uint32_t poll_ms = kDesktopWatchPollMs;
    uint64_t transition_timeout_ms = kDesktopTransitionTimeoutMs;
    // Fired (on the poll thread) for every transition, after the internal
    // desktop_transition log line. Optional; Task 1 wiring passes none
    // (observation only) - Task 2 subscribes CaptureReset here.
    std::function<void(const DesktopTransition&)> on_transition;
    // Injectable Win32 seams (defaults = the real functions).
    HDESK(WINAPI* open_input_desktop)(DWORD, BOOL, ACCESS_MASK) =
        &::OpenInputDesktop;
    BOOL(WINAPI* get_user_object_info)(HANDLE, int, void*, DWORD,
                                       LPDWORD) = &::GetUserObjectInformationW;
    BOOL(WINAPI* close_desktop)(HDESK) = &::CloseDesktop;
    ULONGLONG(WINAPI* clock_ms)() = &::GetTickCount64;
  };

  explicit DesktopWatch(const Opts& o = Opts{}) : o_(o) {}
  ~DesktopWatch();  // Stop (process-exit hard rule: never leak the thread)
  DesktopWatch(const DesktopWatch&) = delete;
  DesktopWatch& operator=(const DesktopWatch&) = delete;

  // Spawns the poll thread; the first observation happens immediately.
  // Returns false when already running. Never blocks on Win32 beyond one
  // OpenInputDesktop round.
  bool Start();

  // Stops + joins the poll thread (sliced sleeps make this <= poll_ms).
  // Idempotent; safe when never started. The poll thread never runs after
  // Stop returns, so post-Stop Snapshot/CurrentName are stable.
  void Stop();

  bool running() const { return thread_.joinable(); }

  // Latest machine state (mutex-guarded copy; DEFAULT/"Default" before the
  // first observation lands).
  DesktopMachineState Snapshot() const;

  // Copies the current desktop name (or "" before the first observation).
  void CurrentName(char* out, size_t cap) const;

  // Counters (observability): total polls, failed polls (open or name).
  uint64_t polls() const;
  uint64_t poll_failures() const;

 private:
  void ThreadMain();
  void PollOnce();

  Opts o_;
  std::thread thread_;
  std::atomic<bool> stop_{false};
  mutable std::mutex mu_;              // guards machine_, observed_, counters
  DesktopMachineState machine_;
  bool observed_ = false;
  uint64_t polls_ = 0;
  uint64_t poll_failures_ = 0;
  bool in_failure_run_ = false;        // logs one desktop_poll_failed per run
};

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_DESKTOP_WATCH_H_
