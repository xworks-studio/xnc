// backend_ladder.h - DXGI health score + GDI fallback ladder + 30s DXGI
// probe recovery (M2-Slice1 Task 3, spec §7.6). LadderCapture wraps TWO
// ICapture backends behind ONE ICapture, so the pipeline (Pipeline::Run)
// needs NO changes: the ladder IS the capture the pipeline drives. Mid-run
// swaps ride the T2 unified CaptureReset (reason "change_backend"):
// suspend -> swap -> new base frame -> ForceIDR, exactly like every other
// rebuild trigger, and subscribers see STATE backend_changed.
//
// DXGI health score - spec §7.6 mapping (see DxgiHealthDelta for the table):
//   init 100; WAIT_TIMEOUT 0 (static screen is health); ACCESS_LOST -0
//   (spec says -10 可自愈不扣: this build routes ACCESS_LOST into the T2
//   unified reset and scores the OUTCOME instead - a denied rebuild while
//   the desktop gate is non-DEFAULT is the expected secure-desktop refusal
//   (T1 evidence), not unhealth); Create failure -40 (observed: active
//   backend Rebuild() failure while the desktop gate IS default); GPU
//   removed -50 (pure-table parity: at runtime DxgiCapture re-creates the
//   device - a failed re-creation folds into Create -40, a successful one
//   is a self-heal 0); no-useful-frame -10 per acquire in the fatal family.
//   M2-Slice2 amendments: a SUCCESSFUL CaptureReset (Rebuild ok - the base
//   frame follows) restores the score to 100, and Create failure -40 is only
//   recorded while the desktop gate has been DEFAULT continuously >= 2 s.
//   While GDI serves and the watch reports a non-DEFAULT desktop, the ladder
//   emits STATE "gdi_stale_secure_desktop" every 5 s (viewer-facing honesty:
//   GDI cannot see the secure desktop). <60
//   (LadderOpts::downgrade_below, configurable per spec) triggers the
//   DXGI->GDI downgrade.
//
// Containment: backend acquire errors in the FATAL family are scored and
// then converted to "err_access_lost" before they reach the pipeline (when
// a reset coordinator is wired) - with a ladder in place a hard backend
// failure degrades the backend instead of killing the stream. Without a
// coordinator (selftest-only configuration) fatal errors pass through when
// the health is still >= threshold, and the downgrade swaps in place
// returning "err_rebuilt".
//
// Probe: while GDI is active a background thread re-tries
// make_dxgi(TryCreateDxgiCapture) + ONE Acquire + release every
// probe_interval_ms (default 30s). Success restores health 100 and requests
// a "change_backend" reset; LadderCapture::Rebuild swaps back to DXGI and
// the reset's phase 4 forces the IDR (spec: 恢复 DXGI -> CaptureReset ->
// Force IDR). If DXGI broke again between probe and swap, Rebuild keeps GDI
// alive instead of stranding the reset retry loop.
//
// Diagnostic env hooks (diagnostic-only, documented in --help):
//   XNC_FORCE_DXGI_HEALTH=N (0..100) - initial score override; N<60 forces
//     the downgrade on the first acquire observation
//   XNC_FORCE_BACKEND=gdi - start on GDI (the CLI --backend gdi wins)
//
// Pure decision logic is header-only (selftest table-drives it without
// Win32); the threaded class implementation is in backend_ladder.cpp.
#ifndef XNC_NATIVE_DESKTOP_BACKEND_LADDER_H_
#define XNC_NATIVE_DESKTOP_BACKEND_LADDER_H_

#include <atomic>
#include <cstdint>
#include <cstring>
#include <memory>
#include <string>

#include "capture.h"        // ICapture, FrameBlob
#include "capture_reset.h"  // CaptureReset, kResetReasonChangeBackend

namespace xnc {

class AuSink;  // pipeline.h (pointer-only use here)

// Which rung the ladder is on (also the on_state/backend_changed name).
enum class BackendKind : uint8_t { kDxgi = 0, kGdi = 1 };
inline const char* BackendKindName(BackendKind b) {
  return b == BackendKind::kDxgi ? "dxgi" : "gdi";
}

// ---- DXGI health score (spec §7.6 table; mapping documented above) ----

enum class DxgiHealthEvent : uint8_t {
  kFrame = 0,        // useful frame (or a self-healed err_rebuilt)
  kTimeout,          // WAIT_TIMEOUT - static screen, health neutral
  kAccessLost,       // ACCESS_LOST - T2 unified-reset domain, neutral here
  kCreateFail,       // Create/Rebuild failure with the desktop gate default
  kGpuRemoved,       // GPU removed (pure-table parity; see header mapping)
  kNoUsefulFrame,    // acquire error in the fatal family
};

// Score delta per event (spec §7.6: 100 初始; WAIT_TIMEOUT 0; ACCESS_LOST
// -10 可自愈不扣; Create 失败 -40; GPU removed -50; 连续无有效帧 -10/次).
inline int DxgiHealthDelta(DxgiHealthEvent e) {
  switch (e) {
    case DxgiHealthEvent::kFrame: return 0;
    case DxgiHealthEvent::kTimeout: return 0;
    case DxgiHealthEvent::kAccessLost: return 0;
    case DxgiHealthEvent::kCreateFail: return -40;
    case DxgiHealthEvent::kGpuRemoved: return -50;
    case DxgiHealthEvent::kNoUsefulFrame: return -10;
  }
  return 0;
}

// Applies one event, clamped to [0, 100] (score never rises via events).
inline uint32_t ApplyDxgiHealthEvent(uint32_t score, DxgiHealthEvent e) {
  int64_t s = static_cast<int64_t>(score) + DxgiHealthDelta(e);
  if (s < 0) s = 0;
  if (s > 100) s = 100;
  return static_cast<uint32_t>(s);
}

// Maps one ICapture acquire outcome to a health event. "err_rebuilt" counts
// as kFrame: an in-place self-heal is progress, not unhealth.
inline DxgiHealthEvent ClassifyAcquireOutcome(bool ok, const char* err) {
  if (ok) return DxgiHealthEvent::kFrame;
  if (err == nullptr || err[0] == '\0') return DxgiHealthEvent::kNoUsefulFrame;
  if (std::strcmp(err, "err_timeout") == 0) return DxgiHealthEvent::kTimeout;
  if (std::strcmp(err, "err_rebuilt") == 0) return DxgiHealthEvent::kFrame;
  if (std::strcmp(err, "err_access_lost") == 0) return DxgiHealthEvent::kAccessLost;
  return DxgiHealthEvent::kNoUsefulFrame;
}

// Create-fail gate stability (M2-Slice2 Task 1): the -40 (Create failure)
// is only real unhealth when the desktop gate has been DEFAULT for >= 2 s
// CONTINUOUSLY - a create failure right after the secure desktop dropped
// is the tail of the expected T1 refusal, not DXGI decay.
inline constexpr uint32_t kGateStableMs = 2000;

// Pure gate-stability step: returns the new "default since" stamp (0 = not
// default). Call once per observation; the stamp anchors the 2 s window.
inline uint64_t GateDefaultSinceStep(bool gate_default, uint64_t since_ms,
                                     uint64_t now_ms) {
  if (!gate_default) return 0;
  return since_ms != 0 ? since_ms : now_ms;
}

// True when the gate has been default for >= min_stable_ms.
inline bool GateStableFor(uint64_t since_ms, uint64_t now_ms,
                          uint32_t min_stable_ms) {
  return since_ms != 0 && now_ms >= since_ms &&
         now_ms - since_ms >= min_stable_ms;
}

// Ladder decision (pure): DXGI downgrades when health < threshold; GDI
// upgrades only after a successful DXGI probe.
enum class LadderAction : uint8_t { kStay = 0, kDowngrade, kUpgrade };
inline LadderAction LadderDecision(BackendKind active, uint32_t dxgi_health,
                                   uint32_t downgrade_below, bool dxgi_probe_ok) {
  if (active == BackendKind::kDxgi)
    return dxgi_health < downgrade_below ? LadderAction::kDowngrade
                                         : LadderAction::kStay;
  return dxgi_probe_ok ? LadderAction::kUpgrade : LadderAction::kStay;
}

// XNC_FORCE_DXGI_HEALTH value parser (pure): "N" with 0 <= N <= 100 (pure
// digits, no sign/space; leading zeros fine). Returns false on anything
// else; *out untouched on failure.
bool ParseForceHealthEnv(const char* v, int32_t* out);

// XNC_FORCE_BACKEND parser (pure): "gdi" (case-insensitive) forces GDI;
// every other value (including "dxgi", garbage, null) leaves the default.
// Returns true only when GDI is forced.
bool ParseForceBackendEnv(const char* v);

// LadderCapture options. Defaults = the production wiring; the selftest
// injects fake factories, a tiny probe interval and fake env parsers.
struct LadderOpts {
  uint32_t init_health = 100;      // spec §7.6 initial score
  uint32_t downgrade_below = 60;   // spec §7.6 <60 触发 GDI (configurable)
  uint32_t probe_interval_ms = 30000;  // 30s DXGI probe while on GDI (0=off)
  int32_t force_health = -1;       // XNC_FORCE_DXGI_HEALTH (>=0 = override)
  bool force_gdi = false;          // XNC_FORCE_BACKEND=gdi / --backend gdi
  // Backend factories (null = the real TryCreateDxgiCapture /
  // TryCreateGdiCapture, wired in backend_ladder.cpp).
  std::unique_ptr<ICapture> (*make_dxgi)(std::string* err) = nullptr;
  std::unique_ptr<ICapture> (*make_gdi)(std::string* err) = nullptr;
  // "DXGI init error means no desktop access" predicate (null = the real
  // DxgiErrIsDesktopAccessDenied). When true the ladder does NOT silently
  // fall back to GDI - GetDC(NULL) would happily capture the WRONG desktop
  // from session 0; that case must stay the dxgi_access_denied_session0
  // exit the Task 6 spawn contract expects.
  bool (*dxgi_init_denied)(const std::string& err) = nullptr;
  // Unified reset coordinator (strongly recommended; both xnc-desktop
  // console modes wire it): backend swaps ride its reset machinery.
  CaptureReset* reset = nullptr;
  // Diagnostic switch observer (called on the pipeline thread at every
  // backend swap with the new backend name + "health"/"probe"/"dxgi_init_failed"
  // reason; also logged unconditionally). Null = log only.
  void (*on_switch)(void* ctx, const char* backend, const char* reason) = nullptr;
  void* on_switch_ctx = nullptr;
  // Probe clock (null = GetTickCount64, wired in backend_ladder.cpp).
  uint64_t (*clock_ms)() = nullptr;
};

// ICapture wrapper owning the DXGI/GDI ladder. Acquire/Rebuild run on the
// pipeline CAPTURE thread (pipeline-decouple); the DXGI probe runs on its
// own thread and touches only atomics + its own probe capture (never the
// active backend).
class LadderCapture final : public ICapture {
 public:
  explicit LadderCapture(const LadderOpts& o = LadderOpts{});
  ~LadderCapture() override;
  LadderCapture(const LadderCapture&) = delete;
  LadderCapture& operator=(const LadderCapture&) = delete;

  // Builds the starting backend. DXGI by default; GDI when force_gdi; GDI
  // fallback only when DXGI failed WITHOUT a desktop-access denial (see
  // LadderOpts::dxgi_init_denied). *err is the DXGI error on failure.
  bool Init(std::string* err);

  // ICapture. Delegates to the active backend, scores the outcome, and
  // downgrades/converts per the header contract.
  bool Acquire(FrameBlob& blob, std::string* err = nullptr,
               uint32_t timeout_ms = 0) override;
  uint32_t Width() const override;
  uint32_t Height() const override;
  uint32_t RebuildCount() const override;

  // Unified-reset entry point. GDI active + probe ok -> swap back to DXGI
  // (a failed re-creation keeps GDI alive so the reset loop is never
  // stranded). DXGI active + health below threshold -> swap to GDI.
  // Otherwise delegate to the active backend; a delegated failure with the
  // desktop gate default scores kCreateFail.
  bool Rebuild(std::string* err) override;

  // Optional STATE sink (AuSink::OnState("backend_changed", true)), set
  // after construction where a sink exists (RtServer); null = log only.
  void SetStateSink(AuSink* sink);

  // Observability.
  uint32_t dxgi_health() const { return health_.load(std::memory_order_relaxed); }
  BackendKind active() const { return active_.load(std::memory_order_relaxed); }
  uint32_t switches() const { return switches_.load(std::memory_order_relaxed); }
  uint32_t probe_successes() const { return probe_ok_count_.load(std::memory_order_relaxed); }
  uint32_t probe_failures() const { return probe_fail_count_.load(std::memory_order_relaxed); }
  // "gdi_stale_secure_desktop" STATE notices emitted (M2-Slice2 Task 1).
  uint32_t gdi_away_notices() const { return gdi_notice_count_.load(std::memory_order_relaxed); }
  // True once the probe has blessed DXGI (cleared when the upgrade swap
  // consumes it or a re-creation fails).
  bool probe_ok() const { return probe_ok_.load(std::memory_order_relaxed); }

 private:
  struct Impl;  // probe thread + owned backends
  Impl* impl_;
  LadderOpts o_;
  std::atomic<uint32_t> health_{100};
  std::atomic<BackendKind> active_{BackendKind::kDxgi};
  std::atomic<uint32_t> switches_{0};
  std::atomic<bool> probe_ok_{false};
  std::atomic<uint32_t> probe_ok_count_{0};
  std::atomic<uint32_t> probe_fail_count_{0};
  std::atomic<uint32_t> gdi_notice_count_{0};
  bool switch_pending_ = false;  // pipeline thread only
  uint64_t gate_default_since_ms_ = 0;  // pipeline thread only (create-fail gate)
  uint64_t gdi_notice_ms_ = 0;          // pipeline thread only (5 s throttle)

  bool GateIsDefault() const;
  // Refreshes gate_default_since_ms_ from one observation (pipeline thread).
  void UpdateGateStability();
  void EmitBackendChanged(const char* backend, const char* reason);
  void EmitGdiAwayNotice();
  bool SwapToDxgi(const char* reason);
  bool SwapToGdi(const char* reason);
  void ProbeLoop();
};

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_BACKEND_LADDER_H_
