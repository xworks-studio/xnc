// handshake.h - XNIP mutual-proof handshake payloads, C++ mirror of
// proto/ipc/handshake.go (byte-identical, spec 9.3). Flow (payloads are
// fixed binary layouts, little-endian):
//
//   A -> B: HELLO       [pid u32][nonce 16B]
//   B -> A: HELLO_PROOF [pid u32][nonce 16B][hmac32 = HMAC(secret, A.nonce)]
//   A -> B: PROOF       [hmac32 = HMAC(secret, B.nonce)]
//
// The secret is pipe_secret (HMAC key-first, RFC 4231 semantics); PID/image
// validation happens at the connection layer, not here.
#ifndef XNC_NATIVE_CORE_HANDSHAKE_H_
#define XNC_NATIVE_CORE_HANDSHAKE_H_

#include "frame.h"

#include <cstddef>
#include <cstdint>
#include <vector>

namespace xnc {

constexpr size_t kNonceSize = 16, kProofSize = 32;

// HMAC-SHA256(key || data) via CNG (BCrypt). Key-first: key = pipe_secret
// semantics. Returns false on any BCrypt failure.
bool HmacSha256(const uint8_t* key, size_t key_len, const uint8_t* data,
                size_t data_len, uint8_t out[32]);

std::vector<uint8_t> EncodeHello(uint32_t pid, const uint8_t nonce[16]);
// Wrong payload size maps to Truncated (mirrors Go's size-error).
DecodeResult DecodeHello(const Frame& f, uint32_t& pid, uint8_t nonce[16]);

std::vector<uint8_t> EncodeHelloProof(uint32_t pid, const uint8_t nonce[16],
                                      const uint8_t proof[32]);
DecodeResult DecodeHelloProof(const Frame& f, uint32_t& pid, uint8_t nonce[16],
                              uint8_t proof[32]);

std::vector<uint8_t> EncodeProof(const uint8_t proof[32]);
DecodeResult DecodeProof(const Frame& f, uint8_t proof[32]);

}  // namespace xnc

#endif  // XNC_NATIVE_CORE_HANDSHAKE_H_
