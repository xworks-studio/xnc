// media_types.h - immutable encoded-frame identity (plan M1 Task 1).
// The one AU schema every consumer below the encoder boundary shares:
//   - FrameIdentity  : the six identity/time fields of spec §5.1/§5.2
//                      (four identity fields + the two mono timestamps),
//                      assigned by the pipeline at capture (epochs,
//                      content_id, source_mono_us) and at successful encoder
//                      submission (encode_seq, present_mono_us). AUs are
//                      consumed through a const ref; the Annex-B payload is
//                      shared const so sinks can fan it out without copying.
//   - AuFlags        : flag bits for EncodedAU::flags (key/IDR today).
//   - FrameIdentityLedger : monotonicity accept/reject gate (spec §5.1/
//                      §5.3): accepts an identity iff it is strictly newer
//                      than the last accepted one. Epochs may not regress;
//                      within one epoch pair content_id may not regress and
//                      encode_seq must strictly increase (exact repeats are
//                      rejected). Separate from SubmissionLedger
//                      (pipeline.h): that type is the encode-FIFO pairing
//                      pinned by the M0 tests, this one only judges
//                      monotonicity.
//
// Header-only on purpose (frame_cache.h/frame_queue.h pattern): the selftest
// and the pipeline include it without adding a translation unit to build.bat.
#ifndef XNC_NATIVE_DESKTOP_MEDIA_TYPES_H_
#define XNC_NATIVE_DESKTOP_MEDIA_TYPES_H_

#include <cstdint>
#include <memory>
#include <vector>

namespace xnc {

// One encoded AU's identity (spec §5.1/§5.2), fixed at assignment and never
// mutated afterward:
//   capture_epoch / codec_epoch - generation counters isolating capture
//     rebuilds and codec (re)configurations from each other;
//   content_id - the pixel content this AU encodes (re-encoding the same
//     static screen keeps the content_id);
//   encode_seq - the successful encoder-input submission that produced this
//     AU (pairs MFT outputs back to their inputs, spec §5.3);
//   source_mono_us - desktop-capture time of these pixels (unchanged when a
//     static frame is re-encoded);
//   present_mono_us - media-timeline time of this encoded result (now() for
//     a re-encode of a static frame, spec §5.2).
struct FrameIdentity {
  uint64_t capture_epoch, codec_epoch, content_id, encode_seq;
  uint64_t source_mono_us, present_mono_us;
};

// Flag bits for EncodedAU::flags. Only the key bit exists today; the
// uint32_t field leaves room for codec/config flags without a wire change.
struct AuFlags {
  static constexpr uint32_t kAuFlagNone = 0;
  static constexpr uint32_t kAuFlagKey = 1u << 0;  // IDR AU (SPS/PPS-prefixed)
};

// Immutable shaped Annex-B AU (spec §10): id is assigned by the pipeline
// (see FrameIdentity), width/height freeze the sink-visible geometry, flags
// carry AuFlags bits, and annexb is the SPS/PPS-prefixed 4-byte-start-code
// payload shared const so the payload can never be mutated in place.
struct EncodedAU {
  FrameIdentity id;
  uint32_t width, height;
  uint32_t flags;
  std::shared_ptr<const std::vector<uint8_t>> annexb;
};

// Monotonicity ledger for published identities (spec §5.1/§5.3).
class FrameIdentityLedger {
 public:
  // Accepts and records `id` when it is strictly newer than the last
  // accepted identity; returns false for repeats and regressions (nothing
  // recorded). Rules: capture_epoch/codec_epoch may advance (advance
  // re-baselines content/seq tracking) but never regress; inside one epoch
  // pair content_id may not regress and encode_seq must strictly increase
  // (a same-content re-encode carries a new encode_seq and is accepted).
  bool Accept(const FrameIdentity& id) {
    if (!has_last_) {
      last_ = id;
      has_last_ = true;
      return true;
    }
    if (id.capture_epoch < last_.capture_epoch) return false;
    if (id.capture_epoch > last_.capture_epoch) {
      last_ = id;
      return true;
    }
    if (id.codec_epoch < last_.codec_epoch) return false;
    if (id.codec_epoch > last_.codec_epoch) {
      last_ = id;
      return true;
    }
    if (id.content_id < last_.content_id) return false;
    if (id.encode_seq <= last_.encode_seq) return false;
    last_ = id;
    return true;
  }

  // True once at least one identity was accepted (a baseline exists).
  bool HasLast() const { return has_last_; }

  // The last accepted identity (only meaningful when HasLast()).
  const FrameIdentity& Last() const { return last_; }

  // Forgets the baseline (fresh epoch tracking; the next Accept seeds it).
  void Reset() {
    has_last_ = false;
    last_ = FrameIdentity{};
  }

 private:
  bool has_last_ = false;
  FrameIdentity last_{};
};

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_MEDIA_TYPES_H_
