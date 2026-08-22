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
// plus the capture timestamp in monotonic-clock microseconds.
struct FrameBlob {
  std::vector<uint8_t> bgra;
  uint32_t w, h;
  uint64_t mono_us;
};

// Capture backend. Acquire semantics (pinned for Task 3/5): true = FrameBlob
// filled in; false with silent retry = timeout (screen static);
// ACCESS_LOST/DEVICE_REMOVED are rebuilt internally by the backend.
class ICapture {
 public:
  virtual ~ICapture() = default;
  virtual bool Acquire(FrameBlob&) = 0;
  virtual uint32_t Width() const = 0;
  virtual uint32_t Height() const = 0;
};

// Task 3 implements this (DXGI CPU readback). Returns null and sets *err
// when no output can be duplicated; declared here since Task 2 pins the
// capture contract.
std::unique_ptr<ICapture> TryCreateDxgiCapture(std::string* err);

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_CAPTURE_H_
