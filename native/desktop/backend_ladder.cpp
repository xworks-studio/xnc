// backend_ladder.cpp - LadderCapture runtime (see backend_ladder.h): env
// hook parsers, DXGI probe thread, backend swaps, health scoring. Acquire/
// Rebuild run on the pipeline thread; the probe thread only touches
// atomics, its own probe capture and the (thread-safe) CaptureReset.
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>  // Sleep, GetTickCount64

#include <thread>

#include "../common/log.h"
#include "backend_ladder.h"
#include "dxgi_capture.h"  // TryCreateDxgiCapture, DxgiErrIsDesktopAccessDenied
#include "gdi_capture.h"   // TryCreateGdiCapture
#include "pipeline.h"      // AuSink (STATE backend_changed)

namespace xnc {

bool ParseForceHealthEnv(const char* v, int32_t* out) {
  if (v == nullptr || out == nullptr || *v == '\0') return false;
  int32_t n = 0;
  for (const char* p = v; *p != '\0'; ++p) {
    if (*p < '0' || *p > '9') return false;  // pure digits: no sign/space
    n = n * 10 + (*p - '0');
    if (n > 100) return false;  // reject early instead of overflowing
  }
  if (n > 100) return false;
  *out = n;  // 0..100 (0 is a valid forced score)
  return true;
}

bool ParseForceBackendEnv(const char* v) {
  if (v == nullptr) return false;
  // case-insensitive "gdi" only (lstrcmpiA semantics, hand-rolled: 3 chars)
  size_t i = 0;
  for (; v[i] != '\0' && i < 3; ++i) {
    char c = v[i];
    if (c >= 'A' && c <= 'Z') c = static_cast<char>(c - 'A' + 'a');
    const char want = "gdi"[i];
    if (c != want) return false;
  }
  return i == 3 && v[3] == '\0';
}

namespace {

uint64_t LadderDefaultClock() { return GetTickCount64(); }

// gpu-readback: the DXGI rung factory honors the ladder's gpu_max_w - the
// GPU downscale+NV12 pipeline when scaling is wanted, the legacy full-BGRA
// path otherwise.
std::unique_ptr<ICapture> LadderMakeDxgiReal(uint32_t max_w, std::string* err) {
  return max_w > 0 ? TryCreateDxgiCaptureGpu(max_w, err)
                   : TryCreateDxgiCapture(err);
}

std::unique_ptr<ICapture> LadderMakeGdiReal(uint32_t max_w, std::string* err) {
  (void)max_w;  // GDI is always full-BGRA; the ScaledCapture CPU path scales
  return TryCreateGdiCapture(err);
}

}  // namespace

struct LadderCapture::Impl {
  std::unique_ptr<ICapture> dxgi;
  std::unique_ptr<ICapture> gdi;
  AuSink* state_sink = nullptr;  // SetStateSink (pipeline-thread calls only)
  std::thread probe;
  std::atomic<bool> probe_stop{false};
};

LadderCapture::LadderCapture(const LadderOpts& o) : impl_(new Impl), o_(o) {
  if (o_.make_dxgi == nullptr) o_.make_dxgi = &LadderMakeDxgiReal;
  if (o_.make_gdi == nullptr) o_.make_gdi = &LadderMakeGdiReal;
  if (o_.dxgi_init_denied == nullptr) o_.dxgi_init_denied = &DxgiErrIsDesktopAccessDenied;
  if (o_.clock_ms == nullptr) o_.clock_ms = &LadderDefaultClock;
  if (o_.init_health > 100) o_.init_health = 100;
  if (o_.downgrade_below > 100) o_.downgrade_below = 100;
}

LadderCapture::~LadderCapture() {
  impl_->probe_stop.store(true);
  if (impl_->probe.joinable()) impl_->probe.join();
  impl_->dxgi.reset();
  impl_->gdi.reset();
  delete impl_;
}

bool LadderCapture::GateIsDefault() const {
  if (o_.reset == nullptr) return true;  // no gate wired: score everything
  return o_.reset->Desktop() == ResetDesktop::kDefault;
}

void LadderCapture::UpdateGateStability() {
  gate_default_since_ms_ =
      GateDefaultSinceStep(GateIsDefault(), gate_default_since_ms_, o_.clock_ms());
}

// GDI away notice (M2-Slice2 Task 1): GDI cannot see the secure desktop -
// viewers keep getting frames that do NOT include it. Emit the STATE at most
// every 5 s while that condition holds (pipeline thread only).
void LadderCapture::EmitGdiAwayNotice() {
  const uint64_t now = o_.clock_ms();
  if (gdi_notice_ms_ != 0 && now >= gdi_notice_ms_ &&
      now - gdi_notice_ms_ < 5000)
    return;
  gdi_notice_ms_ = now != 0 ? now : 1;
  gdi_notice_count_.fetch_add(1, std::memory_order_relaxed);
  XNC_LOG_INFO("gdi_stale_secure_desktop notices=%u",
               gdi_notice_count_.load(std::memory_order_relaxed));
  if (impl_->state_sink != nullptr)
    impl_->state_sink->OnState("gdi_stale_secure_desktop", true);
}

void LadderCapture::EmitBackendChanged(const char* backend, const char* reason) {
  XNC_LOG_INFO("backend_changed backend=%s reason=%s switches=%u health=%u probe_ok=%d",
               backend, reason, switches_.load(std::memory_order_relaxed),
               health_.load(std::memory_order_relaxed),
               probe_ok_.load(std::memory_order_relaxed) ? 1 : 0);
  if (impl_->state_sink != nullptr)
    impl_->state_sink->OnState("backend_changed", true);
  if (o_.on_switch != nullptr) o_.on_switch(o_.on_switch_ctx, backend, reason);
}

bool LadderCapture::SwapToDxgi(const char* reason) {
  std::string derr;
  auto c = o_.make_dxgi(o_.gpu_max_w, &derr);
  if (c == nullptr) {
    XNC_LOG_ERROR("backend_swap dxgi create failed err=\"%s\"", derr.c_str());
    return false;
  }
  impl_->gdi.reset();  // stop the rung we are leaving (destroy before adopt)
  impl_->dxgi = std::move(c);
  active_.store(BackendKind::kDxgi, std::memory_order_relaxed);
  health_.store(o_.init_health, std::memory_order_relaxed);
  probe_ok_.store(false, std::memory_order_relaxed);
  switches_.fetch_add(1, std::memory_order_relaxed);
  EmitBackendChanged("dxgi", reason);
  return true;
}

bool LadderCapture::SwapToGdi(const char* reason) {
  std::string gerr;
  auto c = o_.make_gdi(o_.gpu_max_w, &gerr);
  if (c == nullptr) {
    XNC_LOG_ERROR("backend_swap gdi create failed err=\"%s\"", gerr.c_str());
    return false;
  }
  impl_->dxgi.reset();
  impl_->gdi = std::move(c);
  active_.store(BackendKind::kGdi, std::memory_order_relaxed);
  switches_.fetch_add(1, std::memory_order_relaxed);
  EmitBackendChanged("gdi", reason);
  return true;
}

bool LadderCapture::Init(std::string* err) {
  health_.store(o_.init_health, std::memory_order_relaxed);
  if (o_.force_health >= 0 && o_.force_health <= 100)
    health_.store(static_cast<uint32_t>(o_.force_health), std::memory_order_relaxed);
  if (o_.probe_interval_ms > 0) {
    impl_->probe = std::thread(&LadderCapture::ProbeLoop, this);
  }
  if (o_.force_gdi) {
    std::string gerr;
    impl_->gdi = o_.make_gdi(o_.gpu_max_w, &gerr);
    if (impl_->gdi == nullptr) {
      if (err) *err = gerr.empty() ? "gdi init failed (forced)" : gerr;
      XNC_LOG_ERROR("backend_ladder_init failed backend=gdi(forced) err=\"%s\"",
                    gerr.c_str());
      return false;
    }
    active_.store(BackendKind::kGdi, std::memory_order_relaxed);
    XNC_LOG_INFO("backend_ladder_init backend=gdi forced=1 health=%u probe_ms=%u",
                 health_.load(std::memory_order_relaxed), o_.probe_interval_ms);
    return true;
  }
  std::string derr;
  impl_->dxgi = o_.make_dxgi(o_.gpu_max_w, &derr);
  if (impl_->dxgi != nullptr) {
    active_.store(BackendKind::kDxgi, std::memory_order_relaxed);
    XNC_LOG_INFO("backend_ladder_init backend=dxgi health=%u force_health=%d probe_ms=%u",
                 health_.load(std::memory_order_relaxed), o_.force_health,
                 o_.probe_interval_ms);
    return true;
  }
  // DXGI init failed. A desktop-access denial (session 0 without the Task 6
  // bridge) must NOT silently fall back to GDI: GetDC(NULL) would happily
  // capture the WRONG desktop from session 0. Keep the historical marker.
  if (o_.dxgi_init_denied != nullptr && o_.dxgi_init_denied(derr)) {
    if (err) *err = derr;
    XNC_LOG_INFO("backend_ladder dxgi init denied (no gdi fallback) err=\"%s\"",
                 derr.c_str());
    return false;
  }
  // Genuine DXGI failure (driver/GPU): GDI is the designed rung below.
  std::string gerr;
  impl_->gdi = o_.make_gdi(o_.gpu_max_w, &gerr);
  if (impl_->gdi != nullptr) {
    active_.store(BackendKind::kGdi, std::memory_order_relaxed);
    switches_.store(0, std::memory_order_relaxed);  // start choice, not a swap
    EmitBackendChanged("gdi", "dxgi_init_failed");
    XNC_LOG_INFO("backend_ladder_init backend=gdi(dxgi init failed) err=\"%s\"",
                 derr.c_str());
    return true;
  }
  if (err) *err = derr + " | gdi: " + gerr;
  XNC_LOG_ERROR("backend_ladder_init failed dxgi err=\"%s\" gdi err=\"%s\"",
                derr.c_str(), gerr.c_str());
  return false;
}

bool LadderCapture::Acquire(FrameBlob& blob, std::string* err, uint32_t timeout_ms) {
  // Inline upgrade (no coordinator to ride): swap now and surface the swap
  // as the retryable "err_rebuilt" (the pipeline rewinds + arms the IDR).
  if (active_.load(std::memory_order_relaxed) == BackendKind::kGdi &&
      probe_ok_.load(std::memory_order_relaxed) && o_.reset == nullptr) {
    if (SwapToDxgi("probe")) {
      if (err) *err = "err_rebuilt";
      return false;
    }
    probe_ok_.store(false, std::memory_order_relaxed);  // broken again
  }
  UpdateGateStability();
  // Away notice: GDI serving while the watch reports a secure desktop.
  if (active_.load(std::memory_order_relaxed) == BackendKind::kGdi &&
      o_.reset != nullptr &&
      o_.reset->Desktop() == ResetDesktop::kNonDefault)
    EmitGdiAwayNotice();
  ICapture* cap =
      active_.load(std::memory_order_relaxed) == BackendKind::kDxgi
          ? impl_->dxgi.get()
          : impl_->gdi.get();
  std::string e;
  bool ok = false;
  if (cap != nullptr) {
    ok = cap->Acquire(blob, &e, timeout_ms);
  } else {
    e = "err_access_lost";  // no backend (init edge): unified reset territory
  }
  health_.store(
      ApplyDxgiHealthEvent(health_.load(std::memory_order_relaxed),
                           ClassifyAcquireOutcome(ok, e.c_str())),
      std::memory_order_relaxed);
  // Downgrade check (DXGI active): below threshold -> ask the unified reset
  // for a backend swap (debounced/merged with anything in flight), or swap
  // in place when no coordinator is wired.
  if (active_.load(std::memory_order_relaxed) == BackendKind::kDxgi &&
      !switch_pending_ &&
      health_.load(std::memory_order_relaxed) < o_.downgrade_below) {
    switch_pending_ = true;
    if (o_.reset != nullptr) {
      o_.reset->RequestReset(kResetReasonChangeBackend);
      // Containment: with a coordinator, a fatal-family error converts to
      // the access_lost family so the pipeline routes it into the reset
      // instead of dying (see header). The swap itself happens in Rebuild.
      if (!ok && e != "err_timeout" && e != "err_rebuilt" &&
          e != "err_access_lost")
        e = "err_access_lost";
    } else if (SwapToGdi("health")) {
      ok = false;
      e = "err_rebuilt";
    }
  } else if (o_.reset != nullptr && !ok &&
             e != "err_timeout" && e != "err_rebuilt" && e != "err_access_lost") {
    // Fatal-family error while still healthy: contained the same way - the
    // unified reset (access_lost reason) retries; persistent hard failures
    // keep draining the score until the downgrade fires.
    e = "err_access_lost";
  }
  if (err) *err = e;
  return ok;
}

uint32_t LadderCapture::Width() const {
  const ICapture* cap = active_.load(std::memory_order_relaxed) == BackendKind::kDxgi
                            ? impl_->dxgi.get()
                            : impl_->gdi.get();
  return cap != nullptr ? cap->Width() : 0;
}

uint32_t LadderCapture::Height() const {
  const ICapture* cap = active_.load(std::memory_order_relaxed) == BackendKind::kDxgi
                            ? impl_->dxgi.get()
                            : impl_->gdi.get();
  return cap != nullptr ? cap->Height() : 0;
}

uint32_t LadderCapture::RebuildCount() const {
  const ICapture* cap = active_.load(std::memory_order_relaxed) == BackendKind::kDxgi
                            ? impl_->dxgi.get()
                            : impl_->gdi.get();
  return cap != nullptr ? cap->RebuildCount() : 0;
}

bool LadderCapture::Rebuild(std::string* err) {
  if (err) err->clear();
  // Upgrade first: GDI + a probe-blessed DXGI.
  if (active_.load(std::memory_order_relaxed) == BackendKind::kGdi) {
    if (probe_ok_.load(std::memory_order_relaxed)) {
      if (SwapToDxgi("probe")) {
        switch_pending_ = false;
        return true;
      }
      // DXGI broke again between the probe and the swap: disarm and keep
      // serving from GDI so the reset sequence completes and the stream
      // resumes (the next probe re-arms the upgrade).
      probe_ok_.store(false, std::memory_order_relaxed);
    }
    ICapture* gdi = impl_->gdi.get();
    if (gdi != nullptr) return gdi->Rebuild(err);
    if (err) *err = "no gdi backend";
    return false;
  }
  // Downgrade: DXGI health below the threshold.
  if (health_.load(std::memory_order_relaxed) < o_.downgrade_below) {
    if (SwapToGdi("health")) {
      switch_pending_ = false;
      return true;
    }
    if (err) *err = "gdi create failed during downgrade";
    return false;
  }
  // Normal rebuild: delegate; a failure with the desktop gate DEFAULT and
  // STABLE >= 2 s scores kCreateFail (secure-desktop refusals and their
  // immediate tail are expected, T1 evidence - M2-Slice2 Task 1 gate).
  ICapture* dxgi = impl_->dxgi.get();
  if (dxgi == nullptr) {
    // No DXGI backend (should not happen on this path): try a fresh create.
    if (SwapToDxgi("recreate")) return true;
    if (err) *err = "dxgi backend missing and re-create failed";
    return false;
  }
  std::string rerr;
  const bool ok = dxgi->Rebuild(&rerr);
  UpdateGateStability();
  if (!ok && GateStableFor(gate_default_since_ms_, o_.clock_ms(), kGateStableMs)) {
    health_.store(ApplyDxgiHealthEvent(health_.load(std::memory_order_relaxed),
                                       DxgiHealthEvent::kCreateFail),
                  std::memory_order_relaxed);
    XNC_LOG_INFO("backend_ladder rebuild failed scored health=%u err=\"%s\"",
                 health_.load(std::memory_order_relaxed), rerr.c_str());
  } else if (!ok) {
    XNC_LOG_INFO("backend_ladder rebuild failed unscored (gate not stable >=%ums) err=\"%s\"",
                 kGateStableMs, rerr.c_str());
  }
  if (ok) {
    // CaptureReset success (base frame follows this rebuild) -> health back
    // to 100 (M2-Slice2 Task 1).
    health_.store(100, std::memory_order_relaxed);
  }
  if (err) *err = rerr;
  return ok;
}

void LadderCapture::SetStateSink(AuSink* sink) { impl_->state_sink = sink; }

// Probe loop: every probe_interval_ms while GDI is active, create a THROWAWAY
// DXGI capture (make_dxgi), run ONE Acquire (frame/timeout/rebuilt all prove
// the duplication is functional - timeout means a healthy static screen),
// then release it. Success: health back to 100, probe_ok armed, and a
// "change_backend" reset request so the pipeline thread performs the swap
// (with its base-frame rewind + IDR). Failure: keep probing (DXGI still
// broken); probe failures do NOT touch the health score - GDI is serving.
void LadderCapture::ProbeLoop() {
  const uint32_t slice_ms = 100;
  uint64_t next_probe_ms = o_.clock_ms() + o_.probe_interval_ms;
  for (;;) {
    const uint64_t now = o_.clock_ms();
    if (now < next_probe_ms) {
      const uint64_t wait = next_probe_ms - now;
      const DWORD sleep = static_cast<DWORD>(
          wait > slice_ms ? slice_ms : wait);
      if (sleep > 0) Sleep(sleep);
      if (impl_->probe_stop.load(std::memory_order_relaxed)) return;
      continue;
    }
    next_probe_ms = next_probe_ms + o_.probe_interval_ms;
    if (active_.load(std::memory_order_relaxed) != BackendKind::kGdi) continue;
    if (probe_ok_.load(std::memory_order_relaxed)) continue;  // already armed
    std::string perr;
    std::unique_ptr<ICapture> probe = o_.make_dxgi(o_.gpu_max_w, &perr);
    bool ok = false;
    if (probe != nullptr) {
      FrameBlob blob;
      std::string aerr;
      ok = probe->Acquire(blob, &aerr) || aerr == "err_timeout" ||
           aerr == "err_rebuilt";
    }
    if (ok) {
      probe_ok_count_.fetch_add(1, std::memory_order_relaxed);
      probe_ok_.store(true, std::memory_order_relaxed);
      health_.store(o_.init_health, std::memory_order_relaxed);
      XNC_LOG_INFO("dxgi_probe ok=1 attempt=%u",
                   probe_ok_count_.load(std::memory_order_relaxed));
      if (o_.reset != nullptr) o_.reset->RequestReset(kResetReasonChangeBackend);
    } else {
      probe_fail_count_.fetch_add(1, std::memory_order_relaxed);
      XNC_LOG_INFO("dxgi_probe ok=0 fails=%u err=\"%s\"",
                   probe_fail_count_.load(std::memory_order_relaxed),
                   perr.c_str());
    }
  }
}

}  // namespace xnc
