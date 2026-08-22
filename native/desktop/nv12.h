// nv12.h - BGRA→NV12 (BT.601 limited range) conversion for the CPU encode
// route (plan M1-Slice1 Task 4). Input is a FrameBlob's compact top-down
// BGRA; output is NV12 for CMSH264EncoderMFT (Y plane then interleaved
// half-resolution UV plane, tight stride = width).
//
// Integer approximation re-derived from the reference implementation
// (agent/screen-helper/pixel_windows.go, clean-room rewrite) so both sides
// produce identical bytes:
//   Y = ((66*r + 129*g + 25*b + 128) >> 8) + 16
//   U = ((-38*r -  74*g + 112*b + 128) >> 8) + 128   (2x2 box-averaged RGB)
//   V = ((112*r -  94*g -  18*b + 128) >> 8) + 128
// MSVC's arithmetic >> on the negative U/V numerands is relied upon (the
// build is MSVC-only; see native/desktop/build.bat).
#ifndef XNC_NATIVE_DESKTOP_NV12_H_
#define XNC_NATIVE_DESKTOP_NV12_H_

#include <cstddef>
#include <cstdint>

namespace xnc {

// Byte size of an NV12 frame (w*h + w*h/2). Returns 0 for odd dimensions
// (NV12/H.264 require even) or absurd sizes, mirroring BgraBytes' guard.
inline size_t Nv12Bytes(uint32_t w, uint32_t h) {
  if (w == 0 || h == 0 || (w % 2) != 0 || (h % 2) != 0) return 0;
  const uint64_t bytes = static_cast<uint64_t>(w) * static_cast<uint64_t>(h) * 3ull / 2ull;
  if (bytes > (1ull << 33)) return 0;  // same 8 GiB sanity cap as BgraBytes
  return static_cast<size_t>(bytes);
}

// Converts one compact top-down BGRA frame (bgra_len >= w*h*4) into nv12
// (nv12_len >= Nv12Bytes(w,h)). w and h must be even. Returns false without
// touching dst when any argument is invalid. The UV plane is built from the
// unweighted average of each 2x2 RGB block, matching the reference formulas.
bool BgraToNv12(const uint8_t* bgra, size_t bgra_len, uint8_t* nv12, size_t nv12_len,
                uint32_t w, uint32_t h);

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_NV12_H_
