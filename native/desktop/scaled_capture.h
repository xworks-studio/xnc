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
#ifndef XNC_NATIVE_DESKTOP_SCALED_CAPTURE_H_
#define XNC_NATIVE_DESKTOP_SCALED_CAPTURE_H_

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
// wraps.
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

 private:
  std::unique_ptr<ICapture> inner_;
  uint32_t max_w_ = 0;
  uint32_t scaled_w_ = 0, scaled_h_ = 0;  // last scaled dims seen (Width
                                          // fallback when inner dims are 0)
};

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_SCALED_CAPTURE_H_
