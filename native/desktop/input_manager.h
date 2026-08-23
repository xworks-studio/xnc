// input_manager.h - SendInput injection engine for xnc-desktop (plan
// M1-Slice3 Task 1, spec 11.4/11.5). Receives decoded 0x0108 InputMsg
// frames (rt_pipe_server.h) from the rt pipe reader threads and turns
// them into SendInput batches:
//
//   MOVE    absolute virtual-desktop pointer (MOUSEEVENTF_ABSOLUTE|
//           VIRTUALDESK) with the HOST_HELLO w/h stream space normalized
//           onto the real virtual desktop metrics (SM_X/CXVIRTUALSCREEN);
//           the u16 buttons bitmask is diffed against the recorded button
//           state and becomes UP-before-move / DOWN-after-move events;
//   BUTTON  explicit button down/up (mask bits 1L 2R 4M 8X1 16X2);
//   WHEEL   vertical MOUSEEVENTF_WHEEL / horizontal MOUSEEVENTF_HWHEEL;
//           non-trackpad values are notches scaled by WHEEL_DELTA,
//           trackpad values pass through as fine-grained wheel units;
//   KEY     physical keys via KEYEVENTF_SCANCODE (+EXTENDEDKEY for E0
//           codes) - modifiers are ALWAYS explicit down/up sequences, the
//           manager never synthesizes temporary modifier presses;
//   TEXT    KEYEVENTF_UNICODE down+up per UTF-16 code unit (surrogate
//           pairs ride as two units = four events);
//   LOCK    LockState sync: desired caps/num diffed against GetKeyState
//           and only mismatches inject a CapsLock(0x3A)/NumLock(E0 0x45)
//           press+release (RustDesk LockModesHandler semantics).
//
// Every injected INPUT carries the fixed dwExtraInfo marker (self-injection
// identification / loop prevention). Stuck-key janitor: a background sweep
// every kJanitorScanMs force-releases keys/buttons held longer than
// kStuckReleaseMs; ReleaseAll force-releases everything (drain / last
// subscriber detach / process exit). SendInput failure triggers ONE
// desktop rebind (OpenInputDesktop + SetThreadDesktop, spec 7.3) and one
// retry; a second failure counts as INPUT_DESKTOP_MISMATCH.
//
// Seq policy (decided in Task 1, documented for T3/T6): per-sub_id
// STRICTLY INCREASING seq is enforced HERE - next to the key/button state
// it guards - not in rt_pipe_server. Equal-or-lower seq is dropped as a
// replay; a sub's baseline is cleared by ForgetSub when that subscriber
// detaches, so a reconnecting viewer starts fresh. A message whose
// injection failed is still "consumed" (its seq is recorded) - replaying
// it would not fix a desktop mismatch.
//
// MOVE is absolute-only; MOVE_RELATIVE with its +-10000 delta clamp
// (spec 11.3) is deferred to M2. Wheel deltas are defensively clamped to
// +-10000 (same RustDesk large-jump lesson). Rate limiting (move <=500Hz,
// overall <=1000eps, spec 11.7) lives in the agent, not here.
//
// The Win32 entry points are injectable Opts function pointers so the
// headless selftest drives the full logic (state tables, janitor with a
// fake clock, lock diffs against a fake GetKeyState, coordinate math
// against fake virtual-desktop metrics) and records the exact INPUT
// structs; the REAL SendInput path is compiled here but only exercised by
// the T6 session-1 probe.
#ifndef XNC_NATIVE_DESKTOP_INPUT_MANAGER_H_
#define XNC_NATIVE_DESKTOP_INPUT_MANAGER_H_

#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>

#include <atomic>
#include <cmath>
#include <cstdint>
#include <map>
#include <mutex>
#include <thread>
#include <vector>

#include "rt_pipe_server.h"  // InputMsg + wire types

namespace xnc {

// Fixed dwExtraInfo marker on every injected input (spec 11.5).
inline constexpr uintptr_t kInputExtraInfoMarker = 0x584E4301u;
// Stuck-key janitor cadence (spec 11.5): scan every 10s, force-release
// keys/buttons recorded as down for more than 30s.
inline constexpr uint32_t kJanitorScanMs = 10000;
inline constexpr uint32_t kStuckReleaseMs = 30000;
// Lock-sync scan codes (set 1): CapsLock and NumLock both plain — T6 live
// evidence: E0-prefixed NumLock does not toggle VK_NUMLOCK (gate-1 probe).
inline constexpr uint16_t kScanCapsLock = 0x3A, kScanNumLock = 0x45;
// Key-table id packs the extended flag above the 16-bit scan code.
inline constexpr uint32_t kKeyExtendedBit = 0x10000u;

// Maps a coordinate in the HOST_HELLO stream space (logical px,
// 0..hello_dim) onto the MOUSEEVENTF_ABSOLUTE|MOUSEEVENTF_VIRTUALDESK axis
// over the virtual desktop (vd_origin..vd_origin+vd_extent): normalize by
// hello_dim, place onto the virtual desktop, then scale by 65536/extent
// (the OS inverse mapping), clamped to [0,65535]. With a stream covering
// the whole virtual desktop this degenerates to nx*65536.
inline int32_t MapMoveToAbs(int32_t coord, uint32_t hello_dim, int vd_origin,
                            int vd_extent) {
  if (hello_dim == 0 || vd_extent <= 0) return 0;
  double nx = static_cast<double>(coord);
  if (nx < 0) nx = 0;
  const double dim = static_cast<double>(hello_dim);
  if (nx > dim) nx = dim;
  nx /= dim;
  const double virt = vd_origin + nx * vd_extent;  // px on the virtual desktop
  const double a = (virt - vd_origin) * 65536.0 / static_cast<double>(vd_extent);
  long v = std::lround(a);
  if (v < 0) v = 0;
  if (v > 65535) v = 65535;
  return static_cast<int32_t>(v);
}

class InputManager {
 public:
  enum class Result {
    kInjected,   // consumed (including intentional no-ops)
    kInvalid,    // semantically invalid (post-decode defensive layer)
    kStaleSeq,   // seq not strictly greater than the sub's last seq
    kSendFailed  // SendInput failed after the one desktop-rebind retry
  };

  struct Opts {
    // Coordinate space of MOVE payloads = HOST_HELLO w/h (stream space).
    uint32_t hello_w = 0, hello_h = 0;
    // Injectable Win32 seams (defaults = the real functions).
    UINT(WINAPI *send_input)(UINT, LPINPUT, int) = &::SendInput;
    SHORT(WINAPI *get_key_state)(int) = &::GetKeyState;
    int(WINAPI *get_system_metrics)(int) = &::GetSystemMetrics;
    HDESK(WINAPI *open_input_desktop)(DWORD, BOOL, ACCESS_MASK) = &::OpenInputDesktop;
    BOOL(WINAPI *set_thread_desktop)(HDESK) = &::SetThreadDesktop;
    ULONGLONG(WINAPI *clock_ms)() = &::GetTickCount64;
    uint32_t janitor_scan_ms = kJanitorScanMs;
    uint32_t stuck_release_ms = kStuckReleaseMs;
  };

  struct Stats {
    uint64_t injected = 0;        // messages consumed
    uint64_t invalid = 0;         // semantic rejects
    uint64_t stale_seq = 0;       // replay/backward seq drops
    uint64_t send_failures = 0;   // SendInput batches failing even after rebind
    uint64_t desktop_rebinds = 0; // successful OpenInputDesktop+SetThreadDesktop
    uint64_t desktop_mismatch = 0;  // INPUT_DESKTOP_MISMATCH occurrences
    uint64_t janitor_released = 0;  // stuck keys/buttons force-released
    uint64_t release_all = 0;       // ReleaseAll batches sent (drain/last-detach)
  };

  explicit InputManager(const Opts& o = Opts{}) : o_(o) {}
  ~InputManager();  // StopJanitor + ReleaseAll (process-exit hard rule)
  InputManager(const InputManager&) = delete;
  InputManager& operator=(const InputManager&) = delete;

  // One decoded 0x0108 message (wire validation happened in DecodeInputMsg).
  // Thread-safe: serialized on the internal lock (called from every reader
  // thread). A failed injection has still consumed its seq (see header).
  Result Inject(const InputMsg& m);

  // Force KeyUp/UP for every recorded key and button, clear the tables.
  // Idempotent; called on last-subscriber-detach, DRAIN and destruction.
  void ReleaseAll();

  // Drops a subscriber's seq baseline (its identity ended - a reconnecting
  // sub_id starts a fresh monotonic sequence).
  void ForgetSub(uint32_t sub_id);

  // Background janitor (sweep every opts.janitor_scan_ms).
  void StartJanitor();
  void StopJanitor();

  // One janitor pass at now_ms: releases keys/buttons pressed for longer
  // than opts.stuck_release_ms. Public so the selftest drives it with an
  // injected clock (the thread body calls it with clock_ms()).
  void JanitorSweep(uint64_t now_ms);

  Stats stats();
  // Held-state sizes (janitor/ReleaseAll observability; also T6 debugging).
  size_t HeldKeys();
  size_t HeldButtons();

 private:
  // Sends one batch with the rebind-retry-once policy. Returns false when
  // even the post-rebind retry failed (counts INPUT_DESKTOP_MISMATCH).
  // Caller must hold mu_.
  bool SendBatchLocked(const INPUT* in, UINT n);
  bool RebindInputDesktopLocked();

  Opts o_;
  std::mutex mu_;                            // guards everything below
  std::map<uint32_t, uint64_t> seq_;         // sub_id -> last consumed seq
  std::map<uint32_t, uint64_t> keys_down_;   // scan|ext flag -> press ms
  std::map<uint16_t, uint64_t> buttons_down_;  // mask bit -> press ms
  Stats stats_;
  std::thread janitor_;
  std::atomic<bool> janitor_stop_{false};
};

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_INPUT_MANAGER_H_
