// capture.h - frame capture seam for xnc-desktop (plan M1-Slice1 Task 2).
// FrameBlob is the unit handed to the encoder pipeline (Task 4/5); ICapture
// is the backend interface - Task 3 implements DXGI DuplicateOutput with CPU
// readback (dxgi_capture.h/.cpp), selftests use fakes.
#ifndef XNC_NATIVE_DESKTOP_CAPTURE_H_
#define XNC_NATIVE_DESKTOP_CAPTURE_H_

#include <cstdint>
#include <memory>
#include <string>
#include <vector>

namespace xnc {

// One captured frame: tightly packed 8-bit BGRA (bgra.size() == w * h * 4)
// plus the capture timestamp in monotonic-clock microseconds. Fields are
// default-initialized so a default-constructed blob is {empty, 0, 0, 0}
// (value semantics matter: diag/pipeline code reuses FrameBlob locals).
struct FrameBlob {
  std::vector<uint8_t> bgra;
  uint32_t w = 0, h = 0;
  uint64_t mono_us = 0;
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
class ICapture {
 public:
  virtual ~ICapture() = default;
  virtual bool Acquire(FrameBlob&, std::string* err = nullptr) = 0;
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

// Task 3 implements this (DXGI CPU readback). Returns null and sets *err
// when no output can be duplicated; declared here since Task 2 pins the
// capture contract.
std::unique_ptr<ICapture> TryCreateDxgiCapture(std::string* err);

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_CAPTURE_H_
