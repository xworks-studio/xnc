// mf_decoder_probe.h - TEST-ONLY pixel verification probe for the native
// desktop selftest (2026-08-26 desktop-media-m0 correctness Task 5).
//
// Decodes one Annex-B access unit (SPS + PPS + IDR, 4-byte start codes) with
// the system Windows H.264 decoder MFT into NV12 and returns the FNV-1a
// 64-bit hash of the luma (Y) plane. This gives the selftest a
// decoded-pixel identity it can compare across encoder sessions WITHOUT
// precomputed raw-color constants (H.264 output varies by driver): the
// post-idle recovery IDR is hashed and compared against a reference IDR for
// the same content encoded + decoded in the same run by the same backend.
//
// Probe-only: nothing in the production pipeline/encoder/capture paths links
// or calls this; it is compiled into the selftest binary (desktop_selftest.cpp
// via build.bat) and nothing else.
#ifndef XNC_NATIVE_DESKTOP_MF_DECODER_PROBE_H_
#define XNC_NATIVE_DESKTOP_MF_DECODER_PROBE_H_

#include <cstdint>
#include <string>
#include <vector>

namespace xnc {

// Decodes `annexb` (SPS/PPS + IDR access unit) with the Windows H.264
// decoder MFT to NV12 and writes the FNV-1a 64 hash of the Y plane to
// *luma_hash. Returns true on success; on failure returns false and, when
// err is non-null, leaves a human-readable description there. *luma_hash is
// zeroed on entry. Empty input, a missing H.264 decoder MFT, a decode
// failure, or a frame whose dimensions cannot be resolved all fail.
bool DecodeAnnexBToLumaHash(const std::vector<uint8_t>& annexb,
                            uint64_t* luma_hash, std::string* err);

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_MF_DECODER_PROBE_H_
