// scaled_capture.h - capture-side encode downscale (hw-encode task 2 Part B:
// rt fluency fix). ScaledCapture wraps any ICapture and downscales every
// acquired frame to fit --max-w BEFORE encode, exposing the SCALED
// dimensions through Width()/Height() so the encoder Init, the HOST_HELLO
// geometry and the input/cursor coordinate mapping all work in the stream's
// (scaled) space. The downscale target buffer lives in the wrapper and is
// reused across frames (no per-frame allocation); the blob handed to the
// pipeline aliases it (valid until the next Acquire, per the ICapture
// contract). Dimensions are snapped to even (the encoder requires even
// w/h); the aspect is preserved, only shrinking ever happens.
//
// gpu-readback task: when the wrapped capture already outputs scaled NV12
// (DXGI GPU VideoProcessor path - blob.pixfmt == kNv12), Acquire is a pure
// pass-through: no CPU downscale, no copy. The CPU downscale remains for
// BGRA backends (GDI; degraded DXGI instances).
#ifndef XNC_NATIVE_DESKTOP_SCALED_CAPTURE_H_
#define XNC_NATIVE_DESKTOP_SCALED_CAPTURE_H_

#include <atomic>
#include <cstdint>
#include <memory>
#include <string>

#include "capture.h"

namespace xnc {

// Pure target-dims helper: even-snapped scaled dims for a source w*h and a
// max width. Identity when w <= max_w (or max_w == 0). nh keeps the aspect
// (rounded up, then snapped down to even - the encoder rejects odd sizes).
// Returns false on degenerate input (zero w/h, or max_w would upscale).
bool ScaledDims(uint32_t w, uint32_t h, uint32_t max_w, uint32_t* ow,
                uint32_t* oh);

// ICapture decorator performing the downscale. Thread contract: used on the
// pipeline CAPTURE thread only (pipeline-decouple), like the backends it
// wraps. SetMaxW is the exception: callable from any thread (M3 Task 3's
// SET_VIDEO_CONFIG applier thread; the field is atomic).
class ScaledCapture final : public ICapture {
 public:
  // Takes ownership of inner; max_w == 0 disables scaling (pure passthrough).
  ScaledCapture(std::unique_ptr<ICapture> inner, uint32_t max_w);
  bool Acquire(FrameBlob& blob, std::string* err = nullptr,
               uint32_t timeout_ms = 0) override;
  uint32_t Width() const override;
  uint32_t Height() const override;
  uint32_t RebuildCount() const override;
  bool Rebuild(std::string* err) override;
  // M3 Task 3 (SET_VIDEO_CONFIG): live max_w change. Applies at the NEXT
  // Acquire (CPU downscale path); the resulting blob dims then differ from
  // the encoder's, which the pipeline routes through the unified reset
  // (reason=resolution) -> encoder re-Init -> codec epoch. NOTE: while the
  // GPU rung serves (inner emits NV12) Acquire is a passthrough and a max_w
  // change is a no-op there - the V2 pipeline owns scaling in that topology.
  void SetMaxW(uint32_t max_w);

 private:
  std::unique_ptr<ICapture> inner_;
  std::atomic<uint32_t> max_w_{0};
  uint32_t scaled_w_ = 0, scaled_h_ = 0;  // last scaled dims seen (Width
                                          // fallback when inner dims are 0)
};

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_SCALED_CAPTURE_H_
