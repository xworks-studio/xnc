// crc32c.h - CRC32C (Castagnoli) checksum, header-only, no external deps.
// Used by the Pipe v2 media frame codec (rt_pipe_server.h) to validate the
// header+payload before the payload is materialized.
//
// Standard iSCSI/ext4 CRC32C semantics (spec: RFC 3720 B.4): reflected
// polynomial 0x82F63B78 (reversed Castagnoli), init 0xFFFFFFFF,
// xorout 0xFFFFFFFF. Check value: Crc32c("123456789") == 0xE3069283.
//
// Streaming: pass crc == 0 for a fresh run; chain segments by passing the
// previous return value, e.g. Crc32c(tail, tail_len, Crc32c(head, head_len)).
#ifndef XNC_NATIVE_COMMON_CRC32C_H_
#define XNC_NATIVE_COMMON_CRC32C_H_

#include <array>
#include <cstddef>
#include <cstdint>

namespace xnc {

namespace crc_detail {

// Reflected Castagnoli polynomial (0x82F63B78).
constexpr uint32_t kCrc32cPoly = 0x82F63B78u;

// 256-entry lookup table built once (static local, thread-safe in C++11).
inline const uint32_t* Crc32cTable() {
  static const std::array<uint32_t, 256> table = []() {
    std::array<uint32_t, 256> t{};
    for (size_t i = 0; i < t.size(); ++i) {
      uint32_t r = static_cast<uint32_t>(i);
      for (int b = 0; b < 8; ++b)
        r = (r & 1u) != 0 ? (r >> 1) ^ kCrc32cPoly : r >> 1;
      t[i] = r;
    }
    return t;
  }();
  return table.data();
}

}  // namespace crc_detail

// CRC32C over [data, data+len). A null data pointer is treated as len 0.
// `crc` chains a previous segment's output (0 starts a fresh checksum).
inline uint32_t Crc32c(const uint8_t* data, size_t len, uint32_t crc = 0) {
  if (data == nullptr) len = 0;
  const uint32_t* t = crc_detail::Crc32cTable();
  uint32_t c = ~crc;
  for (size_t i = 0; i < len; ++i) c = (c >> 8) ^ t[(c ^ data[i]) & 0xFFu];
  return ~c;
}

}  // namespace xnc

#endif  // XNC_NATIVE_COMMON_CRC32C_H_
