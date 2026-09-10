// handshake.cpp - handshake payload codec + HMAC-SHA256 via CNG (BCrypt),
// byte-identical to proto/ipc/handshake.go. HMAC is created by passing the
// secret as pbSecret to BCryptCreateHash (key-first, RFC 4231 semantics:
// key = pipe_secret, data = nonce).
#include "handshake.h"

#include <bcrypt.h>
#include <cstring>

namespace xnc {
namespace {

void PutU32(uint8_t* p, uint32_t v) {
  p[0] = static_cast<uint8_t>(v);
  p[1] = static_cast<uint8_t>(v >> 8);
  p[2] = static_cast<uint8_t>(v >> 16);
  p[3] = static_cast<uint8_t>(v >> 24);
}

uint32_t GetU32(const uint8_t* p) {
  return static_cast<uint32_t>(p[0]) | static_cast<uint32_t>(p[1]) << 8 |
         static_cast<uint32_t>(p[2]) << 16 | static_cast<uint32_t>(p[3]) << 24;
}

}  // namespace

bool HmacSha256(const uint8_t* key, size_t key_len, const uint8_t* data,
                size_t data_len, uint8_t out[32]) {
  BCRYPT_ALG_HANDLE alg = nullptr;
  // BCRYPT_ALG_HANDLE_HMAC_FLAG is required for BCryptCreateHash to accept
  // pbSecret (without it the create call fails STATUS_INVALID_PARAMETER).
  if (!BCRYPT_SUCCESS(BCryptOpenAlgorithmProvider(
          &alg, BCRYPT_SHA256_ALGORITHM, nullptr, BCRYPT_ALG_HANDLE_HMAC_FLAG))) {
    return false;
  }
  bool ok = false;
  BCRYPT_HASH_HANDLE hash = nullptr;
  if (BCRYPT_SUCCESS(BCryptCreateHash(
          alg, &hash, nullptr, 0, const_cast<PUCHAR>(key),
          static_cast<ULONG>(key_len), 0))) {
    if (BCRYPT_SUCCESS(BCryptHashData(hash, const_cast<PUCHAR>(data),
                                      static_cast<ULONG>(data_len), 0)) &&
        BCRYPT_SUCCESS(BCryptFinishHash(hash, out, kProofSize, 0))) {
      ok = true;
    }
    BCryptDestroyHash(hash);
  }
  BCryptCloseAlgorithmProvider(alg, 0);
  return ok;
}

std::vector<uint8_t> EncodeHello(uint32_t pid, const uint8_t nonce[16]) {
  std::vector<uint8_t> p(4 + kNonceSize, 0);
  PutU32(p.data(), pid);
  std::memcpy(p.data() + 4, nonce, kNonceSize);
  return p;
}

DecodeResult DecodeHello(const Frame& f, uint32_t& pid, uint8_t nonce[16]) {
  if (f.payload.size() != 4 + kNonceSize) return DecodeResult::Truncated;
  pid = GetU32(f.payload.data());
  std::memcpy(nonce, f.payload.data() + 4, kNonceSize);
  return DecodeResult::Ok;
}

std::vector<uint8_t> EncodeHelloProof(uint32_t pid, const uint8_t nonce[16],
                                      const uint8_t proof[32]) {
  std::vector<uint8_t> p(4 + kNonceSize + kProofSize, 0);
  PutU32(p.data(), pid);
  std::memcpy(p.data() + 4, nonce, kNonceSize);
  std::memcpy(p.data() + 4 + kNonceSize, proof, kProofSize);
  return p;
}

DecodeResult DecodeHelloProof(const Frame& f, uint32_t& pid, uint8_t nonce[16],
                              uint8_t proof[32]) {
  if (f.payload.size() != 4 + kNonceSize + kProofSize) return DecodeResult::Truncated;
  pid = GetU32(f.payload.data());
  std::memcpy(nonce, f.payload.data() + 4, kNonceSize);
  std::memcpy(proof, f.payload.data() + 4 + kNonceSize, kProofSize);
  return DecodeResult::Ok;
}

std::vector<uint8_t> EncodeProof(const uint8_t proof[32]) {
  return std::vector<uint8_t>(proof, proof + kProofSize);
}

DecodeResult DecodeProof(const Frame& f, uint8_t proof[32]) {
  if (f.payload.size() != kProofSize) return DecodeResult::Truncated;
  std::memcpy(proof, f.payload.data(), kProofSize);
  return DecodeResult::Ok;
}

}  // namespace xnc
