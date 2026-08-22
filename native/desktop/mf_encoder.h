// mf_encoder.h - Media Foundation software H.264 encoder for xnc-desktop
// (plan M1-Slice1 Task 4, spec §7.10 MfSoftwareEncoder rung). Wraps
// CMSH264EncoderMFT (Microsoft's software H.264 encoder, ships with client
// Windows): compact top-down BGRA in (converted to NV12 via nv12.h),
// Annex-B access units out. COM plumbing lives in mf_encoder.cpp (pimpl, so
// this header needs no windows.h); the Annex-B NAL helpers below are
// header-only pure logic so the selftest covers them without any MFT.
//
// force-key contract (E2 lesson, spec §7.10, HARD):
//   ForceNextIdr() arms a one-shot request. The ICodecAPI force-keyframe
//   property is set EXACTLY ONCE, at the moment the next Encode() input is
//   SUBMITTED, and the pending state is cleared unconditionally - never
//   re-armed because "no keyframe output has been seen yet". While the
//   encoder is cold-start buffering (Encode returns 0 AUs), callers drain
//   with Drain(); re-forcing during that window is what caused the E2
//   keyframe storm. Selftest scenario (b) is the regression test.
//
// Bitstream shaping (SPS/PPS prefixing, 4-byte start codes, AUD drop) is the
// Task 5 pipeline's job; this class returns the MFT's raw output AUs plus
// the SpsPps()/LastWasKey() bookkeeping the pipeline needs.
#ifndef XNC_NATIVE_DESKTOP_MF_ENCODER_H_
#define XNC_NATIVE_DESKTOP_MF_ENCODER_H_

#include <cstddef>
#include <cstdint>
#include <string>
#include <vector>

namespace xnc {

// ---- Annex-B NAL helpers (pure; used by the encoder and selftests) ----

namespace nal_detail {

inline constexpr size_t kNpos = static_cast<size_t>(-1);

// Byte index of the NAL header (the byte after the 3/4-byte start code) of
// the first NALU of `type` at/after `from`, or kNpos.
inline size_t FindTypeFrom(const uint8_t* d, size_t n, size_t from, uint8_t type) {
  size_t i = from;
  while (i + 4 < n) {
    if (d[i] == 0 && d[i + 1] == 0) {
      size_t sc = 0;
      size_t hdr = kNpos;
      if (d[i + 2] == 1) {
        sc = 3;
        hdr = i + 3;
      } else if (d[i + 2] == 0 && d[i + 3] == 1) {
        sc = 4;
        hdr = i + 4;
      }
      if (hdr != kNpos) {
        if (hdr < n && (d[hdr] & 0x1F) == type) return hdr;
        i += sc;  // wrong type: skip this start code, keep scanning
        continue;
      }
    }
    ++i;
  }
  return kNpos;
}

}  // namespace nal_detail

// True when the Annex-B stream contains a NALU of the given type
// (1 = non-IDR slice, 5 = IDR, 6 = SEI, 7 = SPS, 8 = PPS, 9 = AUD).
inline bool NalHasType(const uint8_t* data, size_t len, uint8_t nal_type) {
  if (!data) return false;
  return nal_detail::FindTypeFrom(data, len, 0, nal_type) != nal_detail::kNpos;
}

// Appends every NALU whose type is listed in `types` (scanned type by type,
// in order) to *out, each normalized to a 4-byte start code (00 00 00 01 +
// NAL bytes, trailing zero bytes before the next start code dropped). This
// is the SPS/PPS extraction semantics the IDR-prefixed stream contract
// needs; SpsPps() is built with types {7, 8}.
inline void NalExtractTypes(const uint8_t* data, size_t len, const uint8_t* types,
                            size_t n_types, std::vector<uint8_t>* out) {
  if (!data || !types || !out) return;
  static const uint8_t kStartCode[4] = {0, 0, 0, 1};
  for (size_t t = 0; t < n_types; ++t) {
    const uint8_t want = types[t];
    size_t from = 0;
    for (;;) {
      const size_t hdr = nal_detail::FindTypeFrom(data, len, from, want);
      if (hdr == nal_detail::kNpos) break;
      // NALU ends at the next start code; back off its leading zero bytes.
      size_t end = len;
      for (size_t j = hdr + 1; j + 3 <= len; ++j) {
        if (data[j] == 0 && data[j + 1] == 0 &&
            (data[j + 2] == 1 ||
             (data[j + 2] == 0 && j + 4 <= len && data[j + 3] == 1))) {
          end = j;
          while (end > hdr && data[end - 1] == 0) --end;
          break;
        }
      }
      out->insert(out->end(), kStartCode, kStartCode + 4);
      out->insert(out->end(), data + hdr, data + end);
      from = end;
    }
  }
}

// ---- Encoder ----

class MfSoftEncoder {
 public:
  MfSoftEncoder();
  ~MfSoftEncoder();
  MfSoftEncoder(const MfSoftEncoder&) = delete;
  MfSoftEncoder& operator=(const MfSoftEncoder&) = delete;

  // Creates the MFT and negotiates media types. w/h must be even (NV12),
  // fps > 0; bitrate_bps == 0 falls back to 2 Mbps (reference behavior).
  // Media-type frame rate = caller fps (time-base alignment lesson: the
  // attribute is the rate-control clock, PTS stays wall-clock). Low-latency
  // shaping (B-frames 0, low-delay rate control, low-latency mode, long GOP
  // - IDRs are forced on demand) is best-effort via ICodecAPI: rejections
  // are logged and Init continues. On failure *err holds "<step>: hr=0x…".
  bool Init(uint32_t w, uint32_t h, uint32_t fps, uint32_t bitrate_bps, std::string* err);

  // Encodes one compact BGRA frame (len >= w*h*4): converts to NV12,
  // submits, then collects whatever the MFT produced. aus is cleared, then
  // filled with 0..N raw Annex-B AUs (one per output sample; cold-start
  // buffering yields 0). false + *err on submit/hardware failure.
  bool Encode(const uint8_t* bgra, size_t len, std::vector<std::vector<uint8_t>>& aus,
              std::string* err);

  // Arms the one-shot IDR request (see header comment). Pure bookkeeping -
  // the ICodecAPI property is applied at the next Encode submission.
  void ForceNextIdr(const char* reason);

  // True iff the most recent Encode/Drain call that produced >= 1 AU saw an
  // IDR (NAL type 5) in its output. Unchanged by calls that produce nothing.
  bool LastWasKey() const { return last_was_key_; }

  // SPS+PPS (4-byte start codes) cached from the first IDR AU; empty until
  // then. Stable for the encoder instance's lifetime.
  const std::vector<uint8_t>& SpsPps() const { return sps_pps_; }

  // Cold-start buffer flush: repeatedly ProcessOutput until
  // MF_E_TRANSFORM_NEED_MORE_INPUT, appending any AUs to aus (NOT cleared).
  // Never applies or re-arms a force-key request - that is the E2 contract.
  void Drain(std::vector<std::vector<uint8_t>>& aus);

 private:
  bool CollectOutputs(std::vector<std::vector<uint8_t>>& aus, std::string* err);
  void Shutdown();

  struct Impl;  // COM pointers + streaming state (mf_encoder.cpp)
  Impl* impl_ = nullptr;
  uint32_t w_ = 0, h_ = 0, fps_ = 0, bitrate_ = 0;
  bool last_was_key_ = false;
  bool force_pending_ = false;  // one-shot, consumed at input SUBMISSION
  std::vector<uint8_t> nv12_;   // reused conversion buffer (w*h*3/2)
  std::vector<uint8_t> sps_pps_;
};

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_MF_ENCODER_H_
