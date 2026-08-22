// nv12.cpp - see nv12.h. Kept in a TU (not header-inline) because the loop
// bodies are shared by every encoded frame; the declaration stays free of
// windows.h so selftests and headers stay clean.
#include "nv12.h"

#include <cstring>

namespace xnc {

bool BgraToNv12(const uint8_t* bgra, size_t bgra_len, uint8_t* nv12, size_t nv12_len,
                uint32_t w, uint32_t h) {
  if (!bgra || !nv12 || w == 0 || h == 0) return false;
  if ((w % 2) != 0 || (h % 2) != 0) return false;  // NV12 needs even dims
  const size_t src_bytes = static_cast<size_t>(w) * h * 4;
  if (bgra_len < src_bytes) return false;
  const size_t dst_bytes = Nv12Bytes(w, h);
  if (nv12_len < dst_bytes) return false;

  uint8_t* y_plane = nv12;
  uint8_t* uv_plane = nv12 + static_cast<size_t>(w) * h;
  const size_t stride = static_cast<size_t>(w) * 4;

  // Luma: BT.601 limited-range Y from full-range RGB.
  for (uint32_t row = 0; row < h; ++row) {
    const uint8_t* src = bgra + static_cast<size_t>(row) * stride;
    uint8_t* y_row = y_plane + static_cast<size_t>(row) * w;
    for (uint32_t x = 0; x < w; ++x) {
      const int b = src[x * 4 + 0];
      const int g = src[x * 4 + 1];
      const int r = src[x * 4 + 2];
      y_row[x] = static_cast<uint8_t>(((66 * r + 129 * g + 25 * b + 128) >> 8) + 16);
    }
  }

  // Chroma: average each 2x2 block in RGB first, then convert (matches the
  // reference implementation's averaging order).
  for (uint32_t row = 0; row < h / 2; ++row) {
    const uint8_t* src0 = bgra + static_cast<size_t>(row * 2) * stride;
    const uint8_t* src1 = bgra + static_cast<size_t>(row * 2 + 1) * stride;
    uint8_t* uv_row = uv_plane + static_cast<size_t>(row) * w;
    for (uint32_t cx = 0; cx < w / 2; ++cx) {
      const size_t p0 = cx * 8;        // left pixel of the 2x2 block
      const size_t p1 = cx * 8 + 4;    // right pixel
      const int b = (src0[p0] + src0[p1] + src1[p0] + src1[p1]) / 4;
      const int g = (src0[p0 + 1] + src0[p1 + 1] + src1[p0 + 1] + src1[p1 + 1]) / 4;
      const int r = (src0[p0 + 2] + src0[p1 + 2] + src1[p0 + 2] + src1[p1 + 2]) / 4;
      uv_row[cx * 2 + 0] = static_cast<uint8_t>(((-38 * r - 74 * g + 112 * b + 128) >> 8) + 128);
      uv_row[cx * 2 + 1] = static_cast<uint8_t>(((112 * r - 94 * g - 18 * b + 128) >> 8) + 128);
    }
  }
  return true;
}

}  // namespace xnc
