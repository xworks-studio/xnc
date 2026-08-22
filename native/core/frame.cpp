// frame.cpp - XNIP frame codec implementation, byte-identical to
// proto/ipc/frame.go. All multi-byte fields are assembled byte-by-byte in
// little-endian order (no host-endianness assumptions).
#include "frame.h"

#include <cstring>

namespace xnc {
namespace {

uint16_t GetU16(const uint8_t* p) {
  return static_cast<uint16_t>(static_cast<uint16_t>(p[0]) |
                               static_cast<uint16_t>(p[1]) << 8);
}

uint32_t GetU32(const uint8_t* p) {
  return static_cast<uint32_t>(p[0]) | static_cast<uint32_t>(p[1]) << 8 |
         static_cast<uint32_t>(p[2]) << 16 | static_cast<uint32_t>(p[3]) << 24;
}

void PutU16(uint8_t* p, uint16_t v) {
  p[0] = static_cast<uint8_t>(v);
  p[1] = static_cast<uint8_t>(v >> 8);
}

void PutU32(uint8_t* p, uint32_t v) {
  p[0] = static_cast<uint8_t>(v);
  p[1] = static_cast<uint8_t>(v >> 8);
  p[2] = static_cast<uint8_t>(v >> 16);
  p[3] = static_cast<uint8_t>(v >> 24);
}

// Validate and parse a 16-byte header (mirror of Go DecodeHeader, same check
// order: magic -> version -> size cap). Only called with len >= kHeaderSize.
DecodeResult DecodeHeader(const uint8_t* h, uint8_t& flags, uint16_t& message_type,
                          uint32_t& request_id, size_t& payload_len) {
  if (std::memcmp(h, kMagic, sizeof(kMagic)) != 0) return DecodeResult::BadMagic;
  if (h[4] != kProtocolVersion) return DecodeResult::BadVersion;
  uint32_t n = GetU32(h + 12);
  if (n > kMaxFrameBytes) return DecodeResult::TooLarge;
  flags = h[5];
  message_type = GetU16(h + 6);
  request_id = GetU32(h + 8);
  payload_len = n;
  return DecodeResult::Ok;
}

// Read exactly len bytes (pipes may return partial reads; loop until full,
// error, or EOF). Returns false on IO failure / premature EOF.
bool ReadFull(HANDLE file, uint8_t* buf, size_t len) {
  size_t done = 0;
  while (done < len) {
    DWORD got = 0;
    if (!ReadFile(file, buf + done, static_cast<DWORD>(len - done), &got, nullptr) ||
        got == 0) {
      return false;
    }
    done += got;
  }
  return true;
}

// Write all bytes.
bool WriteFull(HANDLE file, const uint8_t* buf, size_t len) {
  size_t done = 0;
  while (done < len) {
    DWORD put = 0;
    if (!WriteFile(file, buf + done, static_cast<DWORD>(len - done), &put, nullptr)) {
      return false;
    }
    done += put;
  }
  return true;
}

}  // namespace

bool EncodeFrame(const Frame& f, std::vector<uint8_t>& out) {
  if (f.payload.size() > kMaxFrameBytes) return false;
  out.assign(kHeaderSize + f.payload.size(), 0);
  std::memcpy(out.data(), kMagic, sizeof(kMagic));
  out[4] = kProtocolVersion;
  out[5] = f.flags;
  PutU16(out.data() + 6, f.message_type);
  PutU32(out.data() + 8, f.request_id);
  PutU32(out.data() + 12, static_cast<uint32_t>(f.payload.size()));
  if (!f.payload.empty()) {
    std::memcpy(out.data() + kHeaderSize, f.payload.data(), f.payload.size());
  }
  return true;
}

DecodeResult DecodeFrame(const uint8_t* data, size_t len, Frame& out) {
  if (len < kHeaderSize) return DecodeResult::Truncated;
  uint8_t flags;
  uint16_t message_type;
  uint32_t request_id;
  size_t payload_len;
  DecodeResult r = DecodeHeader(data, flags, message_type, request_id, payload_len);
  if (r != DecodeResult::Ok) return r;
  if (len < kHeaderSize + payload_len) return DecodeResult::Truncated;
  out.flags = flags;
  out.message_type = message_type;
  out.request_id = request_id;
  out.payload.assign(data + kHeaderSize, data + kHeaderSize + payload_len);
  return DecodeResult::Ok;
}

DecodeResult ReadFrame(HANDLE file, Frame& out) {
  uint8_t h[kHeaderSize];
  if (!ReadFull(file, h, sizeof(h))) return DecodeResult::Truncated;
  uint8_t flags;
  uint16_t message_type;
  uint32_t request_id;
  size_t payload_len;
  DecodeResult r = DecodeHeader(h, flags, message_type, request_id, payload_len);
  if (r != DecodeResult::Ok) return r;
  out.flags = flags;
  out.message_type = message_type;
  out.request_id = request_id;
  out.payload.assign(payload_len, 0);
  if (payload_len > 0 && !ReadFull(file, out.payload.data(), payload_len)) {
    return DecodeResult::Truncated;
  }
  return DecodeResult::Ok;
}

bool WriteFrame(HANDLE file, const Frame& f) {
  std::vector<uint8_t> wire;
  if (!EncodeFrame(f, wire)) return false;
  return WriteFull(file, wire.data(), wire.size());
}

}  // namespace xnc
