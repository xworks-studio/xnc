// frame.h - XNIP frame codec, C++ mirror of proto/ipc (Go), byte-identical
// wire format per spec 9.2. Header (all little-endian):
//
//   offset len  field
//   0      4    magic "XNIP"
//   4      1    protocolVersion (=1)
//   5      1    flags (bit0=response, bit2=event, bit4=error)
//   6      2    messageType (u16)
//   8      4    requestId (u32)
//   12     4    payloadLength (u32)
//   16     ..   payload (opaque)
//
// Single-frame cap is 9 MiB; exceeding it is a protocol error (disconnect).
#ifndef XNC_NATIVE_CORE_FRAME_H_
#define XNC_NATIVE_CORE_FRAME_H_

#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>

#include <cstddef>
#include <cstdint>
#include <vector>

namespace xnc {

constexpr char kMagic[4] = {'X', 'N', 'I', 'P'};  // "XNIP" (spec 9.2)
constexpr uint8_t kProtocolVersion = 1;
constexpr size_t kHeaderSize = 16;
constexpr size_t kMaxFrameBytes = size_t(9) << 20;  // 9 MiB

// Flags (spec 9.2: bit0=response, bit2=event, bit4=error).
constexpr uint8_t kFlagResponse = 1, kFlagEvent = 4, kFlagError = 16;

// MessageType registry: 0x0001..0x000F are frame-level handshake/keepalive
// with fixed binary payloads; 0x0100+ are protobuf app RPC (M1).
constexpr uint16_t kMsgHello = 0x0001, kMsgHelloProof = 0x0002, kMsgProof = 0x0003,
                   kMsgBye = 0x0004, kMsgPing = 0x0010, kMsgPong = 0x0011;

// A decoded XNIP frame. payload owns a copy of the wire bytes.
struct Frame {
  uint8_t flags;
  uint16_t message_type;
  uint32_t request_id;
  std::vector<uint8_t> payload;
};

enum class DecodeResult { Ok, BadMagic, BadVersion, TooLarge, Truncated };

// Serialize to wire bytes (header + payload copy). Returns false when
// payload exceeds kMaxFrameBytes (Go side panics; C++ side reports).
bool EncodeFrame(const Frame& f, std::vector<uint8_t>& out);

// Parse a complete wire frame (header + payload).
DecodeResult DecodeFrame(const uint8_t* data, size_t len, Frame& out);

// Synchronous named-pipe I/O: read exactly one frame (loop until the 16-byte
// header, then payloadLength bytes, are complete); write the full frame.
// IO failure while reading maps to Truncated; WriteFrame returns false.
DecodeResult ReadFrame(HANDLE file, Frame& out);
bool WriteFrame(HANDLE file, const Frame& f);

}  // namespace xnc

#endif  // XNC_NATIVE_CORE_FRAME_H_
