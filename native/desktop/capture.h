// capture.h - frame capture seam for xnc-desktop (plan M1-Slice1 Task 2).
// FrameBlob is the unit handed to the encoder pipeline (Task 4/5); ICapture
// is the backend interface - Task 3 implements DXGI DuplicateOutput with CPU
// readback (dxgi_capture.h/.cpp), selftests use fakes. M2 Task 2 adds the
// GPU twin ICaptureSurface: backends publish the newest desktop into a
// caller-owned LatestSurface with a full-resource GPU copy (no readback).
#ifndef XNC_NATIVE_DESKTOP_CAPTURE_H_
#define XNC_NATIVE_DESKTOP_CAPTURE_H_

#include <cstdint>
#include <memory>
#include <string>
#include <vector>

#include "gpu_surface.h"  // LatestSurface (ICaptureSurface's destination);
                          // brings media_types.h (FrameIdentity) along

namespace xnc {

// Pixel layout of a FrameBlob buffer (gpu-readback task): the DXGI GPU path
// produces NV12 directly (VideoProcessor scale+convert, no CPU BGRA), the
// GDI/CPU paths stay BGRA. The pipeline routes on this field: NV12 -> the
// encoder's NV12 entry (no BGRA->NV12 conversion), BGRA -> ScaledCapture CPU
// downscale + encoder BGRA entry.
enum class Pixfmt : uint8_t { kBgra = 0, kNv12 = 1 };

// One captured frame: tightly packed 8-bit pixels (bgra.size() == w * h * 4
// for BGRA, w * h * 3 / 2 for NV12) plus the capture timestamp in
// monotonic-clock microseconds. pixfmt says which layout bgra holds. Fields
// are default-initialized so a default-constructed blob is {empty, 0, 0, 0,
// BGRA, 0} (value semantics matter: diag/pipeline code reuses FrameBlob
// locals); the M1 Task 1 identity fields below default to 0 likewise.
struct FrameBlob {
  std::vector<uint8_t> bgra;
  uint32_t w = 0, h = 0;
  Pixfmt pixfmt = Pixfmt::kBgra;
  uint64_t mono_us = 0;
  // GPU-path diag: microseconds the DXGI backend spent in the GPU
  // scale/NV12 + readback portion of this frame (0 = not measured / not the
  // GPU path). Pipeline windows it into the gpu_scale_ms log line.
  uint64_t gpu_scale_us = 0;
  // M1 Task 1: content identity assigned at capture (pipeline.cpp) and
  // carried through the handoff queue to the encode thread. capture_epoch/
  // codec_epoch are the generation values at capture time; content_id
  // identifies this pixel content; source_mono_us is the desktop-capture
  // time of these pixels (spec §5.1/§5.2). encode_seq and present_mono_us
  // are assigned later, at successful encoder submission.
  uint64_t capture_epoch = 0;
  uint64_t codec_epoch = 0;
  uint64_t content_id = 0;
  uint64_t source_mono_us = 0;
};

// Capture backend. Acquire semantics (pinned for Task 3/5, M2-S1 T2 adds
// the err_access_lost family): true = FrameBlob filled in; false +
// *err == "err_timeout" = no change (screen static, retry silently, no
// blob); false + *err == "err_rebuilt" = access lost and the backend
// rebuilt its duplication in place (no frame this call, retry); false +
// *err == "err_access_lost" = access lost and the internal rebuild was
// REFUSED (secure desktop up: re-duplication is denied 0x80070005 even as
// SYSTEM - T1 evidence) or no duplication exists - the pipeline routes
// this into the unified CaptureReset instead of treating it fatal; any
// other *err = fatal (repeated hard failures etc.).
//
// timeout_ms bounds the block for one new frame (pipeline-decouple: the
// capture thread passes spf so a static screen wakes at the target frame
// cadence instead of the backend's default wait). 0 = backend default
// (DxgiCapture: 100 ms; GDI/fakes: their own pacing). Fakes may ignore it.
class ICapture {
 public:
  virtual ~ICapture() = default;
  virtual bool Acquire(FrameBlob&, std::string* err = nullptr,
                       uint32_t timeout_ms = 0) = 0;
  virtual uint32_t Width() const = 0;
  virtual uint32_t Height() const = 0;
  // Total in-place rebuilds so far (ACCESS_LOST/DEVICE_REMOVED) - diag
  // observability only; default 0 for backends that never rebuild.
  virtual uint32_t RebuildCount() const { return 0; }
  // Full out-of-band rebuild (device + duplication re-creation) driven by
  // the unified CaptureReset path (M2-S1 Task 2). Default: unsupported
  // (fakes/backends with nothing to rebuild); DxgiCapture maps this to a
  // fresh Init. Called on the pipeline thread only.
  virtual bool Rebuild(std::string* err) {
    if (err) *err = "rebuild unsupported";
    return false;
  }
};

// ---- M2 Task 2: GPU surface acquisition (ICaptureSurface) ----
//
// The surface twin of ICapture::Acquire: instead of a CPU FrameBlob, the
// backend publishes the newest desktop INTO a caller-owned LatestSurface
// (gpu_surface.h) with a full-resource GPU CopyResource - no readback
// anywhere on this path. The conceptual return is the plan's
// CapturedSurface (below); concretely the texture + identity arrive via
// LatestSurface::Snapshot and `changed` rides the CaptureStatus.
//
// Identity authority (controller ruling 1): the CALLER assigns
// capture_epoch/codec_epoch/content_id and passes them in *id; the backend
// COMPLETES that identity (source_mono_us = the acquire timestamp;
// encode_seq/present_mono_us are zeroed - the encode thread fills them at
// successful submission) and stamps it into the surface with the copy. The
// backend never invents content ids: cursor-only frames (DXGI
// LastPresentTime == 0) and static screens return kNoChange WITHOUT
// touching the surface, so a cursor move never counts as changed content.
// On kNoChange *id echoes the identity currently stamped in the surface.
//
// Ownership (DXGI, ruling 1c): ReleaseFrame fires immediately after the
// CopyResource, before the method returns - the duplication is never held
// into encoder work. The LatestSurface OBJECT is caller-owned; the backend
// (re)Inits it (backend device, backend dims) whenever its device is
// recreated or the mode changed, so the caller passes the SAME LatestSurface
// to every call of one backend. After kRetry/kAccessLost a previously
// Snapshot()-leased texture may belong to a dead device - the unified
// CaptureReset (M2-S1) owns the retry that re-Inits it.
//
// Threading (M2 Task 4 ruling 4): under MediaPipelineV2 the MEDIA GPU
// thread drives AcquireSurface - its single loop owns capture, the
// LatestSurface copy, the VideoProcessor conversion and the encoder
// Submit, so "the capture thread" and "the media GPU thread" are the SAME
// thread (this is how this note and gpu_surface.h's single-thread
// contract align). One call at a time per backend either way. The M0
// pipeline's two-thread shape (a capture thread producing FrameBlobs over
// ICapture::Acquire, a separate encode thread) is the LEGACY configuration
// - it never touches this interface. timeout_ms mirrors ICapture::Acquire
// (0 = the backend default; bounds the block for one new frame).
enum class CaptureStatus : uint8_t {
  kFrame = 0,       // changed: new content copied + the given identity stamped
  kNoChange = 1,    // static screen / cursor-only: surface + identity untouched
  kRetry = 2,       // "err_rebuilt": backend healed in place, call again
  kAccessLost = 3,  // "err_access_lost": the unified CaptureReset owns retrying
  kFatal = 4,       // anything else; *err carries the detail
};

// The plan's conceptual AcquireSurface return shape: `id` + `texture` are
// exactly what LatestSurface::Snapshot() leases after a kFrame status, and
// `changed` is the kFrame-vs-kNoChange half of CaptureStatus. Kept concrete
// so later tasks (and the selftest) can phrase things in its terms.
struct CapturedSurface {
  FrameIdentity id{};
  ID3D11Texture2D* texture = nullptr;  // AddRef'd by Snapshot()
  bool changed = false;
};

class ICaptureSurface {
 public:
  virtual ~ICaptureSurface() = default;
  // *id is IN/OUT (see the identity-authority note above). *err mirrors the
  // ICapture vocabulary on the non-frame statuses ("err_timeout" /
  // "err_rebuilt" / "err_access_lost"; detail text for kFatal) and is
  // untouched on kFrame.
  virtual CaptureStatus AcquireSurface(LatestSurface& latest,
                                       uint32_t timeout_ms, FrameIdentity* id,
                                       std::string* err) = 0;
};

// The err-string -> status mapping the backends (and the selftest fakes)
// share: the surface statuses are the ICapture::Acquire err vocabulary.
inline CaptureStatus CaptureStatusFromErr(const std::string& err) {
  if (err == "err_timeout") return CaptureStatus::kNoChange;
  if (err == "err_rebuilt") return CaptureStatus::kRetry;
  if (err == "err_access_lost") return CaptureStatus::kAccessLost;
  return CaptureStatus::kFatal;
}

// Task 3 implements this (DXGI CPU readback). Returns null and sets *err
// when no output can be duplicated; declared here since Task 2 pins the
// capture contract.
std::unique_ptr<ICapture> TryCreateDxgiCapture(std::string* err);

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_CAPTURE_H_
