// jpeg_wic.h - M2-Slice3 Task 3: one-shot JPEG snapshot support for
// xnc-desktop --jpeg-single. Two units:
//   DownscaleBgra  pure box-filter downscale (BGRA, max-width clamp; only
//                  ever shrinks - a frame already <= max_w passes through)
//   WicEncodeJpeg  BGRA memory -> WIC JPEG bytes (quality 0..1, we use
//                  0.85; GUID_WICPixelFormat32bppBGRA source bitmap ->
//                  Jpeg encoder -> HGLOBAL stream)
// The encode path is Windows SDK only (windowscodecs.lib) - no GDI+, no
// new dependencies. Pure DownscaleBgra is selftest-covered; WicEncodeJpeg
// needs COM (CoInitializeEx on the caller's thread) and is exercised by
// the selftest's synthetic-frame round trip (JFIF magic + SOF dims).
#ifndef XNC_NATIVE_DESKTOP_JPEG_WIC_H_
#define XNC_NATIVE_DESKTOP_JPEG_WIC_H_

#include <cstdint>
#include <vector>

namespace xnc {

// Box-filter downscale of a tightly packed BGRA buffer to fit max_w.
// Only shrinks: w <= max_w returns a plain copy (oh == w). Aspect is
// preserved; a max_w of 0 is treated as "no clamp" (identity copy). Pure.
bool DownscaleBgra(const uint8_t* src, uint32_t w, uint32_t h, uint32_t max_w,
                   std::vector<uint8_t>* out, uint32_t* ow, uint32_t* oh);

// BGRA buffer (tightly packed, w*h*4) -> JPEG file bytes via WIC.
// quality is the WIC encoder quality option (0..1; 0.85 is the snapshot
// default). Requires CoInitializeEx on the calling thread. Returns false
// and sets *err on any WIC failure (err is a stable ASCII tag).
bool WicEncodeJpeg(const uint8_t* bgra, uint32_t w, uint32_t h, float quality,
                   std::vector<uint8_t>* jpeg, std::string* err);

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_JPEG_WIC_H_
