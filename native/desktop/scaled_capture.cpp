// scaled_capture.cpp - see scaled_capture.h. Downscale via the existing
// xnc::DownscaleBgra box filter (jpeg_wic.h) - the --jpeg-single snapshot
// path already uses it, so the pixel path is selftest-covered. This wrapper
// adds the pipeline integration: the blob dims become the scaled dims, the
// destination buffer is persistent across frames, and odd target dims are
// snapped to even (encoder contract) by dropping one edge row/column.
//
// gpu-readback task: when the inner capture already outputs scaled NV12 (the
// DXGI VideoProcessor GPU path - blob.pixfmt == kNv12), Acquire passes the
// frame through untouched - no CPU downscale, no copy (the GPU did both the
// scaling and the BGRA->NV12 conversion). The CPU downscale stays for the
// BGRA backends (GDI produces BGRA; a degraded DXGI instance falls back
// here too).
#include "scaled_capture.h"

#include "../common/log.h"
#include "jpeg_wic.h"

namespace xnc {

bool ScaledDims(uint32_t w, uint32_t h, uint32_t max_w, uint32_t* ow,
                uint32_t* oh) {
  if (ow == nullptr || oh == nullptr) return false;
  if (w == 0 || h == 0) return false;
  uint32_t nw = w, nh = h;
  if (max_w > 0 && w > max_w) {
    nw = max_w;
    nh = static_cast<uint32_t>(((static_cast<uint64_t>(h) * nw + w - 1) / w));
  }
  if (nw == 0 || nh == 0) return false;
  // Even snap (encoder requires even w/h; at most 1 px off the true aspect).
  if ((nw % 2) != 0) nw -= 1;
  if ((nh % 2) != 0) nh -= 1;
  if (nw == 0 || nh == 0) return false;
  *ow = nw;
  *oh = nh;
  return true;
}

ScaledCapture::ScaledCapture(std::unique_ptr<ICapture> inner, uint32_t max_w)
    : inner_(std::move(inner)), max_w_(max_w) {}

bool ScaledCapture::Acquire(FrameBlob& blob, std::string* err, uint32_t timeout_ms) {
  if (!inner_) {
    if (err) *err = "no inner capture";
    return false;
  }
  FrameBlob raw;
  if (!inner_->Acquire(raw, err, timeout_ms)) return false;  // err_timeout/rebuilt/... passthrough

  // gpu-readback: inner (DXGI GPU path) already scaled to max_w and
  // converted to NV12 in the VideoProcessor - pass through untouched (no
  // CPU downscale, no copy; the blob's dims are the GPU-scaled ones).
  if (raw.pixfmt == Pixfmt::kNv12) {
    scaled_w_ = raw.w;
    scaled_h_ = raw.h;
    blob = std::move(raw);
    return true;
  }

  uint32_t nw = 0, nh = 0;
  if (!ScaledDims(raw.w, raw.h, max_w_, &nw, &nh)) {
    if (err) *err = "scaled dims rejected";
    XNC_LOG_ERROR("scaled_capture dims rejected w=%u h=%u max_w=%u", raw.w, raw.h, max_w_);
    return false;
  }
  scaled_w_ = nw;
  scaled_h_ = nh;

  if (nw == raw.w && nh == raw.h) {
    // Identity: hand the inner buffer through (no copy, no downscale).
    blob = std::move(raw);
    return true;
  }

  // Scaled: downscale into the persistent dst buffer (blob.bgra aliases it
  // until the next Acquire - the pipeline consumes it synchronously).
  uint32_t dw = 0, dh = 0;
  if (!DownscaleBgra(raw.bgra.data(), raw.w, raw.h, nw, &blob.bgra, &dw, &dh)) {
    if (err) *err = "downscale failed";
    XNC_LOG_ERROR("scaled_capture downscale failed src=%ux%u nw=%u", raw.w, raw.h, nw);
    return false;
  }
  // Even snap: DownscaleBgra's height is ceil(h*nw/w), which can overshoot
  // our even target by 1 - truncate the trailing row (tightly packed rows,
  // so resize drops exactly the last row). An odd width (identity source
  // with odd w) strips the last column row-wise.
  if (dh > nh) {
    blob.bgra.resize(static_cast<size_t>(nw) * nh * 4);
    dh = nh;
  } else if (dh < nh) {
    if (err) *err = "downscale height mismatch";
    return false;
  }
  if (dw > nw) {
    // Strip one trailing column per row.
    const size_t row_bytes = static_cast<size_t>(nw) * 4;
    std::vector<uint8_t> stripped(static_cast<size_t>(nw) * nh * 4);
    for (uint32_t y = 0; y < nh; ++y)
      std::memcpy(stripped.data() + static_cast<size_t>(y) * row_bytes,
                  blob.bgra.data() + static_cast<size_t>(y) * (nw + 1) * 4, row_bytes);
    blob.bgra = std::move(stripped);
    dw = nw;
  } else if (dw < nw) {
    if (err) *err = "downscale width mismatch";
    return false;
  }
  blob.w = dw;
  blob.h = dh;
  blob.pixfmt = Pixfmt::kBgra;  // explicit: blobs are reused across acquires
  blob.gpu_scale_us = 0;
  blob.mono_us = raw.mono_us;
  return true;
}

uint32_t ScaledCapture::Width() const {
  const uint32_t w = inner_ != nullptr ? inner_->Width() : 0;
  const uint32_t h = inner_ != nullptr ? inner_->Height() : 0;
  if (w == 0 || h == 0) return scaled_w_;
  uint32_t ow = 0, oh = 0;
  if (ScaledDims(w, h, max_w_, &ow, &oh)) return ow;
  return scaled_w_;
}

uint32_t ScaledCapture::Height() const {
  const uint32_t w = inner_ != nullptr ? inner_->Width() : 0;
  const uint32_t h = inner_ != nullptr ? inner_->Height() : 0;
  if (w == 0 || h == 0) return scaled_h_;
  uint32_t ow = 0, oh = 0;
  if (ScaledDims(w, h, max_w_, &ow, &oh)) return oh;
  return scaled_h_;
}

uint32_t ScaledCapture::RebuildCount() const {
  return inner_ != nullptr ? inner_->RebuildCount() : 0;
}

bool ScaledCapture::Rebuild(std::string* err) {
  if (!inner_) {
    if (err) *err = "no inner capture";
    return false;
  }
  return inner_->Rebuild(err);
}

}  // namespace xnc
