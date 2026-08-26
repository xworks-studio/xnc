// mf_encoder.h - Media Foundation H.264 encoder for xnc-desktop (plan
// M1-Slice1 Task 4, spec §7.10 MfSoftwareEncoder rung; hw-encode task
// extends it with a hardware-first ladder). Wraps a Windows Media Foundation
// H.264 encoder MFT with compact top-down BGRA in (converted to NV12 via
// nv12.h), Annex-B access units out. COM plumbing lives in mf_encoder.cpp
// (pimpl, so this header needs no windows.h); the Annex-B NAL helpers below
// are header-only pure logic so the selftest covers them without any MFT.
//
// Encoder selection ladder (hw-encode): MFTEnumEx over
// MFT_CATEGORY_VIDEO_ENCODER | MFT_ENUM_FLAG_HARDWARE with NV12-in/H264-out
// filters picks the first hardware encoder (Intel QSV / NVENC / AMF MFTs)
// that fully initializes AND encodes one synthetic frame successfully
// (self-check); only when every hardware candidate fails (or the machine has
// none) does Init fall back to CMSH264EncoderMFT - Microsoft's software
// H.264 encoder that ships with client Windows. SetForceSoftware(true) pins
// the software rung (diagnostic lever, --encoder software). The ladder and
// the backend choice never change the encoder's outward contract.
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

struct IMFTransform;  // opaque; the pimpl cpp includes mftransform.h

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

// Which MFT backend Init selected (hw-encode ladder). Diagnostics only -
// the encoder's outward contract is identical for both.
enum class EncoderBackend : uint8_t { kHardware = 0, kSoftware = 1 };

// One Encode/EncodeNV12 operation has two independently meaningful results:
// ProcessInput may reject the input, or it may accept it and a later
// ProcessOutput collection may fail after appending complete AUs.
struct EncoderSubmitResult {
  bool input_accepted = false;
  bool outputs_ok = false;
  explicit operator bool() const { return input_accepted && outputs_ok; }
};

enum class EncoderOutputStage : uint8_t {
  kSubmit = 0,
  kDrain = 1,
  kFlushTail = 2,
};

enum class EncoderMessage : uint8_t {
  kEndOfStream = 0,
  kDrain = 1,
};

// Optional deterministic seam at the external MFT boundary. Production
// constructs MfSoftEncoder with nullptr; native selftests inject precise
// ProcessInput/ProcessOutput outcomes while exercising the real pipeline.
struct MfEncoderFaultSeam {
  void* ctx = nullptr;
  bool (*process_input)(void* ctx, std::string* err) = nullptr;
  bool (*collect_outputs)(void* ctx, EncoderOutputStage stage,
                          std::vector<std::vector<uint8_t>>* aus,
                          std::string* err) = nullptr;
  bool (*process_message)(void* ctx, EncoderMessage message,
                          std::string* err) = nullptr;
};

class MfSoftEncoder {
 public:
  explicit MfSoftEncoder(const MfEncoderFaultSeam* fault_seam = nullptr);
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
  //
  // Hardware-first (hw-encode): every MFT_ENUM_FLAG_HARDWARE H.264 encoder
  // (NV12-in/H264-out) is tried in merit order - full negotiation + a
  // 1-frame self-check (synthetic content, output non-empty = pass). The
  // first passing candidate is kept; if all fail (or SetForceSoftware was
  // called), Init falls back to CMSH264EncoderMFT (software). Media types
  // are negotiated adaptively for hardware MFTs: the caller's tight-stride
  // NV12/H.264 types are tried first, then the MFT's own enumerated types
  // (dims/rate/bitrate overridden), and a negotiated stride != width is
  // honored by padding the input rows. Backend + FriendlyName + self-check
  // outcome are logged (encoder_backend=…).
  bool Init(uint32_t w, uint32_t h, uint32_t fps, uint32_t bitrate_bps, std::string* err);

  // Diagnostic lever (--encoder software): bypasses the hardware-first
  // ladder and uses the software MFT unconditionally. Sticky across Init
  // calls; default is hardware-first with software fallback.
  void SetForceSoftware(bool force) { force_software_ = force; }

  // Backend selected by the last successful Init (kSoftware before any).
  EncoderBackend backend() const { return backend_; }
  const char* BackendName() const {
    return backend_ == EncoderBackend::kHardware ? "hardware" : "software";
  }
  // FriendlyName of the MFT in use (diagnostic log surface; empty until Init).
  const std::string& FriendlyName() const { return friendly_name_; }

  // Encodes one compact BGRA frame (len >= w*h*4): converts to NV12,
  // submits, then collects whatever the MFT produced. aus is cleared, then
  // filled with 0..N raw Annex-B AUs (one per output sample; cold-start
  // buffering yields 0). The result distinguishes pre-accept rejection from
  // accepted input followed by output-collection failure; complete partial
  // AUs remain in aus in the latter case.
  EncoderSubmitResult Encode(const uint8_t* bgra, size_t len,
                             std::vector<std::vector<uint8_t>>& aus,
                             std::string* err);

  // gpu-readback task: encodes one compact NV12 frame directly (len >=
  // w*h*3/2, tight stride = width) - NO BGRA->NV12 conversion (the DXGI GPU
  // path already produced NV12 in the VideoProcessor). Same submit/output
  // contract as Encode. Invalid dims/len are rejected like Encode.
  EncoderSubmitResult EncodeNV12(const uint8_t* nv12, size_t len,
                                 std::vector<std::vector<uint8_t>>& aus,
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
  // false surfaces collection failure; complete AUs appended first remain.
  bool Drain(std::vector<std::vector<uint8_t>>& aus, std::string* err = nullptr);

  // End-of-stream flush (Task 5; the Task 4 review's deferred minor):
  // sends MFT_MESSAGE_NOTIFY_END_OF_STREAM + MFT_MESSAGE_COMMAND_DRAIN,
  // then collects every remaining output AU (appending to aus, NOT cleared).
  // This recovers the frames still inside the ~17-frame lookahead window
  // when the pipeline stops - Drain alone cannot surface them because the
  // MFT will not emit a buffered frame while more input is still expected.
  // Never touches the force-key state. After FlushTail the encoder is
  // drained - do not feed it again; call Init to reuse the object.
  // false surfaces message/collection failure; complete partial AUs remain.
  bool FlushTail(std::vector<std::vector<uint8_t>>& aus,
                 std::string* err = nullptr);

 private:
  // Full media-type negotiation + streaming start on an existing MFT (no
  // ownership transfer). hardware=true allows the enumerated-type fallbacks.
  // impl_ must be allocated and COM initialized. Stores the negotiated input
  // stride into impl_->in_stride (padding buffer allocated when != w).
  bool InitWithMft(IMFTransform* mft, const std::wstring& friendly, bool hardware,
                   std::string* err);
  // Hardware ladder probe: encodes up to kSelfCheckMaxFrames synthetic frames
  // through the CURRENT impl_ MFT; true iff at least one AU came out.
  bool SelfCheckEncode(std::string* err);
  // Encode()'s submit half over a ready NV12 frame (stride-expands when the
  // negotiated input stride differs from w). Applies the one-shot force-key
  // at submission (E2 contract).
  EncoderSubmitResult SubmitNv12(const uint8_t* nv12, size_t len,
                                 std::vector<std::vector<uint8_t>>& aus,
                                 std::string* err);
  // Drops the current MFT/session (END_STREAMING + release + activate
  // ShutdownObject) WITHOUT tearing down impl_/w_/h_ - used to discard
  // hardware ladder candidates and to reset between probe/live instances.
  // Clears stream-derived state (sps_pps_, last_was_key_, force_pending_).
  void ReleaseMft();
  bool CollectOutputs(std::vector<std::vector<uint8_t>>& aus, std::string* err,
                      EncoderOutputStage stage);
  void Shutdown();

  struct Impl;  // COM pointers + streaming state (mf_encoder.cpp)
  Impl* impl_ = nullptr;
  uint32_t w_ = 0, h_ = 0, fps_ = 0, bitrate_ = 0;
  bool last_was_key_ = false;
  bool force_pending_ = false;  // one-shot, consumed at input SUBMISSION
  bool force_software_ = false; // --encoder software diagnostic pin
  const MfEncoderFaultSeam* fault_seam_ = nullptr;  // non-owning; null in production
  EncoderBackend backend_ = EncoderBackend::kSoftware;
  std::string friendly_name_;
  std::vector<uint8_t> nv12_;   // reused conversion buffer (w*h*3/2)
  std::vector<uint8_t> sps_pps_;
};

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_MF_ENCODER_H_
