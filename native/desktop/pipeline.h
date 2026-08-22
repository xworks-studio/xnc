// pipeline.h - capture -> FrameCache -> encode -> shaped Annex-B dump loop
// (plan M1-Slice1 Task 5). Pipeline::Run drives one diagnostic capture
// session end to end:
//
//   Acquire (ICapture contract) -> FrameCache state machine -> MfSoftEncoder
//   -> per-AU stream shaping -> fwrite(h264) + per-second counter log,
//   then MfSoftEncoder::FlushTail (lookahead window recovery) at end.
//
// Stream contract per AU (spec §7.10, port of vclNALUs semantics from
// agent/screen-helper/capture_windows.go - same rules, C++ rewrite):
//   - IDR AU = cached SPS+PPS (from the encoder's first IDR) prefixed
//     before the VCL NALUs;
//   - parameter sets (7/8) and AUD (9) are dropped from encoder output
//     (duplicate parameter sets / AUD prefixes make some WebCodecs
//     decoders reject frames);
//   - every NALU is normalized to a 4-byte start code (00 00 00 01).
//
// Warm-up (spec §7.4 as amended 2026-08-22, commit 1a1dd41): CMSH264EncoderMFT
// has ~17 frames of startup lookahead, so the first submitted frame does not
// emerge as an AU until ~17 inputs later. On a static screen (acquire
// timeouts) the pipeline re-feeds the cached base frame WITHOUT re-forcing
// the IDR until the first keyframe AU emerges. Bounds: at most
// min(2 x lookahead window frames, 2 s at target fps) re-feeds; warm-up feed
// counts are logged. ForceNextIdr is never called during warm-up (E2).
//
// stats.json: FormatStatsJson/WriteStatsJson produce the run sidecar next to
// the --out file (same fields as the per-second log + duration/w/h/bitrate).
#ifndef XNC_NATIVE_DESKTOP_PIPELINE_H_
#define XNC_NATIVE_DESKTOP_PIPELINE_H_

#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <string>
#include <vector>

#include "capture.h"       // ICapture, FrameBlob
#include "frame_cache.h"   // FrameCache, FrameCacheCounters
#include "mf_encoder.h"    // MfSoftEncoder

namespace xnc {

struct PipelineOpts {
  uint32_t duration_s = 10;            // run length in seconds (> 0)
  uint32_t fps = 30;                   // target fps (paces submissions)
  uint32_t target_bitrate_bps = 2300000;
};

struct PipelineResult {
  bool ok = true;
  std::string err;                 // fatal message when !ok
  FrameCacheCounters counters;     // final totals (stats.json source)
  uint32_t width = 0, height = 0;
  uint64_t aus_written = 0;        // shaped AUs fwrite'd to out
  uint64_t bytes_written = 0;      // total bytes fwrite'd
};

// Measured lookahead window of CMSH264EncoderMFT on the dev/target machines
// (Task 4: latency exactly 17 frames, then 1 AU per submit in input order).
inline constexpr uint32_t kEncoderLookaheadFrames = 17;

// Warm-up re-feed bound (spec §7.4): min(2 x window frames, 2 s worth of
// frames at the target fps).
inline uint32_t WarmupFeedBound(uint32_t fps) {
  const uint32_t by_window = 2u * kEncoderLookaheadFrames;
  const uint32_t by_time = 2u * (fps > 0 ? fps : 1u);
  return by_window < by_time ? by_window : by_time;
}

// Appends every NALU of `data` except parameter sets (7/8) and AUD (9),
// each re-emitted with a 4-byte start code, trailing zero bytes before the
// next start code dropped, original order preserved (port of vclNALUs -
// agent/screen-helper/capture_windows.go; the reference is read-only, this
// is the clean-room C++ rewrite of its semantics).
inline void VclNalus(const uint8_t* d, size_t n, std::vector<uint8_t>* out) {
  if (!d || !out || n == 0) return;
  static const uint8_t kSc[4] = {0, 0, 0, 1};
  const size_t kNpos = static_cast<size_t>(-1);
  size_t i = 0;
  for (;;) {
    // Next start code at/after i (3- or 4-byte); hdr = NAL header byte.
    size_t hdr = kNpos;
    for (size_t j = i; j + 3 < n; ++j) {
      if (d[j] == 0 && d[j + 1] == 0) {
        if (d[j + 2] == 1) {
          hdr = j + 3;
          break;
        }
        if (d[j + 2] == 0 && j + 4 < n && d[j + 3] == 1) {
          hdr = j + 4;
          break;
        }
      }
    }
    if (hdr == kNpos || hdr >= n) break;
    // NALU ends at the next start code's zero run (or stream end); the
    // leading zeros of that next code belong to the code, not the NALU.
    size_t end = n;
    for (size_t j = hdr + 1; j + 3 <= n; ++j) {
      if (d[j] == 0 && d[j + 1] == 0 &&
          (d[j + 2] == 1 || (j + 4 <= n && d[j + 2] == 0 && d[j + 3] == 1))) {
        end = j;
        break;
      }
    }
    while (end > hdr && d[end - 1] == 0) --end;
    const uint8_t type = static_cast<uint8_t>(d[hdr] & 0x1F);
    if (type != 7 && type != 8 && type != 9 && end > hdr) {
      out->insert(out->end(), kSc, kSc + 4);
      out->insert(out->end(), d + hdr, d + end);
    }
    i = end;
  }
}

// Shapes one raw encoder AU to the stream contract. IDR AUs get the cached
// SPS+PPS (4-byte start codes, from MfSoftEncoder::SpsPps()) prefixed
// before their VCL NALUs; every AU loses its own parameter sets/AUD and is
// normalized to 4-byte start codes. out is cleared first.
inline void ShapeAu(const uint8_t* au, size_t len, bool is_idr,
                    const std::vector<uint8_t>& spspps, std::vector<uint8_t>* out) {
  if (!out) return;
  out->clear();
  if (is_idr && !spspps.empty()) out->assign(spspps.begin(), spspps.end());
  VclNalus(au, len, out);
}

class Pipeline {
 public:
  // Runs the loop described in the header comment for opts.duration_s wall
  // seconds. `out` must be open in binary mode (caller closes it). The
  // encoder must already be Init'ed at the capture's dimensions unless the
  // first Acquire fails fatally (then it is never touched). Returns the
  // counters/totals; counters are also visible in the per-second log lines.
  static PipelineResult Run(ICapture& capture, MfSoftEncoder& encoder, FILE* out,
                            const PipelineOpts& opts);
};

// One-line-per-field JSON for the stats.json sidecar (pure, so the selftest
// can assert the field set without touching the filesystem).
std::string FormatStatsJson(const PipelineResult& r, const PipelineOpts& o);

// Writes "<directory of h264_out_path>\\stats.json". Returns false + *err on
// open/write failure. Used by --console-diag (also on capture/encoder init
// failure, with zeroed counters, so every diag run leaves a sidecar).
bool WriteStatsJson(const std::wstring& h264_out_path, const PipelineResult& r,
                    const PipelineOpts& o, std::wstring* err);

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_PIPELINE_H_
