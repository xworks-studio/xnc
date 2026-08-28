// desktop_selftest.cpp - native/desktop selftest (Task 2 scope: console-diag
// arg parsing + FrameBlob/ICapture layout; Task 3 scope: BGRA byte math,
// pitch compaction, FNV-1a hash and 256-point sampling helpers from
// dxgi_capture.h; Task 4 scope: BGRA→NV12 BT.601 color math, Annex-B NAL
// parse helpers, and the MfSoftEncoder contract incl. the E2 one-shot
// force-key regression; Task 5 scope: FrameCache state-machine transitions,
// VclNalus/ShapeAu stream-contract shaping, FlushTail tail recovery, and the
// full Pipeline::Run end-to-end over a scripted fake ICapture + the real MF
// encoder; M1-Slice2 Task 2 scope: rt CLI args, fixed-binary pipe message
// codecs, the subscriber drop/merge policy, and the real RtServer over REAL
// pipe handles with fake in-process clients: ① attach -> HOST_HELLO + IDR
// ② static-screen second attach -> fresh IDR reason=sub_join (the Slice1
// carry-forward regression) ③ stuck subscriber -> queue overflow -> delta
// drop + merged IDR ④ detach cleanup). Task 3 fix wave adds the
// --secret-stdin service-path secret (stdin line codec + arg matrix; spec
// 1.5: the secret never rides argv). 2026-08-26 desktop-media-m0
// correctness Task 5 adds the decoded A/B/C end-to-end regression: A/B/C
// then timeouts then sub_join + pli - the recovery IDR must DECODE to C's
// luma hash (mf_decoder_probe.h), not A's (stale-pixel acceptance test).
// 2026-08-26 desktop-media-m2 Task 1 adds the pure lease state machine
// (FREE -> CONVERTING -> SUBMITTED -> RETIRED -> FREE) plus the D3D11
// LatestSurface/Nv12SurfacePool wrappers over a self-created device
// (hardware -> WARP; XNC_D3D_DEBUG=1 gates the debug-layer assertions:
// no ERROR/CORRUPTION D3D messages + zero live leases after teardown).
// Task 2 adds the fake captured-surface ownership test (ReleaseFrame
// immediately after the copy and before any encoder callback; cursor-only
// LastPresentTime==0 never increments content) plus a real-LatestSurface
// seam check inside the device block.
// Pure-logic cases need no desktop;
// the encoder/pipeline scenarios feed synthetic color bars straight into the
// MF software H.264 MFT, so no capture is involved and they run on any
// Windows box that ships CMSH264EncoderMFT (client SKUs). The rt loopback
// uses a permissive TEST-ONLY DACL pipe (precedent: native/core/selftest.cpp
// loopback). Any failure prints "SELFTEST FAIL: <name>" and exits 1; all-pass
// prints "selftest ok". Entry point SelftestMain() is declared by
// xnc-desktop.cpp and reachable via `xnc-desktop.exe --selftest` /
// `build.bat selftest`.
#include "capture.h"
#include "backend_ladder.h"
#include "capture_reset.h"
#include "cursor_manager.h"
#include "desktop_watch.h"
#include "diag.h"
#include "dxgi_capture.h"
#include "frame_cache.h"
#include "frame_queue.h"
#include "gdi_capture.h"
#include "gpu_surface.h"  // M2 Task 1: lease model + LatestSurface/NV12 pool
#include "input_manager.h"
#include "jpeg_wic.h"
#include "media_types.h"  // M1 Task 1: FrameIdentity/EncodedAU/AuFlags ledger
#include "mf_decoder_probe.h"  // Task 5 m0: test-only decode-to-luma-hash probe
#include "mf_encoder.h"
#include "mf_gpu_encoder.h"  // M2 Task 3: IEncoderSession + both rungs
#include "media_pipeline_v2.h"  // M2 Task 4: depth-one GPU media pipeline
#include "nv12.h"
#include "pipeline.h"
#include "rt_pipe_server.h"
#include "scaled_capture.h"
#include "subscribers.h"

#include "../common/handshake.h"

namespace xnc {
// xnc-desktop.cpp: pure XNC_DESKTOP_PIPELINE_V2 value parser (M1 Task 5
// ruling 1d extracted it from DesktopPipelineV2Enabled; startup-only gate,
// no shared header - both TUs link into the same exe).
bool ParsePipelineV2Env(const char* v);
// M2 Task 4: the --desktop-pipeline-v2 argv stripper (wmain calls it
// before ParseDiagArgs; pure so this table pins it without re-execing).
int StripDesktopPipelineV2Flag(int* argc, wchar_t** argv);
}  // namespace xnc

#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>  // GetTempPathW, GetCurrentProcessId, DeleteFileW, pipes
#include <d3d11.h>    // M2 Task 1: device + textures for the surface tests
#include <wrl/client.h>  // ComPtr RAII in the surface tests

#include <cstddef>  // offsetof
#include <cstdio>
#include <algorithm>  // std::find
#include <atomic>
#include <condition_variable>
#include <cstring>
#include <deque>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <type_traits>
#include <vector>

static int fails = 0;
#define CHECK(name, cond) do { if (!(cond)) { std::printf("SELFTEST FAIL: %s\n", name); fails++; } } while (0)

namespace {

// Runs ParseDiagArgs with a fake argv[0] prepended - mirrors wmain's call.
struct ParseOutcome {
  bool ok = false;
  xnc::DiagOptions opt;
  std::wstring err;
};

ParseOutcome Parse(const std::vector<std::wstring>& args) {
  std::vector<wchar_t*> argv;
  argv.reserve(args.size() + 1);
  argv.push_back(const_cast<wchar_t*>(L"xnc-desktop.exe"));
  for (const auto& a : args) argv.push_back(const_cast<wchar_t*>(a.c_str()));
  ParseOutcome p;
  p.ok = xnc::ParseDiagArgs(static_cast<int>(argv.size()), argv.data(), &p.opt, &p.err);
  return p;
}

xnc::FrameBlob MakeSolidFrame(uint64_t mono_us, uint8_t value) {
  xnc::FrameBlob frame;
  frame.bgra = {value, value, value, 0xFF};
  frame.w = 1;
  frame.h = 1;
  frame.pixfmt = xnc::Pixfmt::kBgra;
  frame.mono_us = mono_us;
  frame.gpu_scale_us = 0;
  return frame;
}

// Selftest-only fake backend: proves ICapture is implementable/abstract and
// is reusable by Task 5's synthetic-capture pipeline tests.
struct FakeCapture final : xnc::ICapture {
  bool Acquire(xnc::FrameBlob&, std::string* = nullptr,
               uint32_t = 0) override { return false; }
  uint32_t Width() const override { return 0; }
  uint32_t Height() const override { return 0; }
};

// Synthetic content for the encoder scenarios (a)-(d): vertical color bars
// (bar width = w/8, so 2x2 chroma subsampling never mixes colors) plus a
// moving 4px stripe per frame so consecutive frames differ and P frames
// flow. Deterministic, no desktop or capture involved (plan Task 4: 合成彩条).
class SyntheticBars {
 public:
  SyntheticBars(uint32_t w, uint32_t h) : bgra_((size_t)w * h * 4), w_(w), h_(h) {
    DrawBars();  // frame 0 is the pristine bar pattern
  }
  const uint8_t* Frame(uint32_t i) {
    if (i > 0) {  // redraw base + move the stripe (cheap at selftest sizes)
      DrawBars();
      DrawStripe(i);
    }
    return bgra_.data();
  }
  size_t Bytes() const { return bgra_.size(); }

 private:
  void SetPx(uint32_t x, uint32_t y, uint8_t b, uint8_t g, uint8_t r) {
    uint8_t* px = bgra_.data() + ((size_t)y * w_ + x) * 4;
    px[0] = b; px[1] = g; px[2] = r; px[3] = 0xFF;
  }
  void DrawBars() {
    static const uint8_t kBars[5][3] = {{0, 0, 255}, {0, 255, 0}, {255, 0, 0},
                                        {255, 255, 255}, {0, 0, 0}};  // B,G,R
    const uint32_t bw = w_ / 8;
    for (uint32_t y = 0; y < h_; ++y)
      for (uint32_t x = 0; x < w_; ++x) {
        const uint8_t* c = kBars[(x / bw) % 5];
        SetPx(x, y, c[0], c[1], c[2]);
      }
  }
  void DrawStripe(uint32_t frame) {
    static const uint8_t kStripe[3][3] = {{255, 255, 0}, {255, 0, 255}, {0, 255, 255}};
    const uint8_t* c = kStripe[frame % 3];
    const uint32_t sw = 4;
    const uint32_t x0 = (frame * 9) % (w_ - sw);
    for (uint32_t y = 0; y < h_; ++y)
      for (uint32_t x = x0; x < x0 + sw; ++x) SetPx(x, y, c[0], c[1], c[2]);
  }
  std::vector<uint8_t> bgra_;
  uint32_t w_, h_;
};

// Number of AUs containing a NALU of the given type (5 = IDR; this MFT emits
// at most one VCL NAL set per AU, so AU count == NAL count in practice).
size_t CountAusWithNal(const std::vector<std::vector<uint8_t>>& aus, uint8_t type) {
  size_t n = 0;
  for (const auto& au : aus)
    if (xnc::NalHasType(au.data(), au.size(), type)) ++n;
  return n;
};

// ---- M2 Task 3: minimal SPS/VUI parser (selftest-only) ----
//
// Parses the first SPS (NAL type 7) of an Annex-B AU far enough to read
// the VUI's video_signal_type: video_format / video_full_range_flag /
// colour_primaries / transfer_characteristics / matrix_coefficients.
// That is the §8.4 agreement surface: the encoded metadata must match the
// Nv12ColorForSize rule the VideoProcessor + encoder input type carry.
struct SpsVui {
  bool sps_found = false;
  bool vui_present = false;
  bool video_signal_present = false;
  int video_format = -1;
  int full_range = -1;
  int colour_description = -1;
  int cp = -1;  // colour_primaries
  int tc = -1;  // transfer_characteristics
  int mc = -1;  // matrix_coefficients
};

struct SpsBitReader {
  const uint8_t* d;
  size_t n;
  size_t bit = 0;
  SpsBitReader(const uint8_t* p, size_t sz) : d(p), n(sz) {}
  int U(int bits) {
    int v = 0;
    for (int i = 0; i < bits; ++i) {
      if (bit >= n * 8) return -1;
      v = (v << 1) | ((d[bit >> 3] >> (7 - (bit & 7))) & 1);
      ++bit;
    }
    return v;
  }
  int UE() {  // exp-Golomb
    int zeros = 0;
    for (;;) {
      if (bit >= n * 8) return -1;
      if (((d[bit >> 3] >> (7 - (bit & 7))) & 1) != 0) break;
      ++zeros;
      ++bit;
      if (zeros > 31) return -1;
    }
    ++bit;  // the terminating 1
    int info = 1 << zeros;
    for (int i = 0; i < zeros; ++i) info |= U(1) << (zeros - 1 - i);
    return info - 1;
  }
};

SpsVui ParseSpsVui(const uint8_t* au, size_t len) {
  SpsVui v;
  const size_t hdr = xnc::nal_detail::FindTypeFrom(au, len, 0, 7);
  if (hdr == xnc::nal_detail::kNpos) return v;
  v.sps_found = true;
  size_t end = len;
  for (size_t j = hdr + 1; j + 3 <= len; ++j) {
    if (au[j] == 0 && au[j + 1] == 0 &&
        (au[j + 2] == 1 ||
         (au[j + 2] == 0 && j + 4 <= len && au[j + 3] == 1))) {
      end = j;
      break;
    }
  }
  // Strip emulation prevention (00 00 03 -> 00 00).
  std::vector<uint8_t> rbsp;
  for (size_t i = hdr; i < end; ++i) {
    if (i + 2 < end && au[i] == 0 && au[i + 1] == 0 && au[i + 2] == 3) {
      rbsp.push_back(0);
      rbsp.push_back(0);
      i += 2;
    } else {
      rbsp.push_back(au[i]);
    }
  }
  SpsBitReader br(rbsp.data(), rbsp.size());
  const int profile = br.U(8);
  br.U(8);  // constraint flags + reserved
  br.U(8);  // level_idc
  br.UE();  // seq_parameter_set_id
  if (profile == 100 || profile == 110 || profile == 122 || profile == 244 ||
      profile == 44 || profile == 83 || profile == 86 || profile == 118 ||
      profile == 128 || profile == 138 || profile == 139 || profile == 134 ||
      profile == 135) {
    const int chroma = br.UE();
    if (chroma == 3) br.U(1);  // separate_colour_plane_flag
    br.UE();                   // bit_depth_luma_minus8
    br.UE();                   // bit_depth_chroma_minus8
    br.U(1);                   // qpprime_y_zero_transform_bypass_flag
    if (br.U(1) != 0) return v;  // scaling lists: out of scope
  }
  br.UE();  // log2_max_frame_num_minus4
  const int poc = br.UE();
  if (poc == 0) {
    br.UE();
  } else if (poc == 1) {
    br.U(1);
    br.UE();
    br.UE();
    const int cyc = br.UE();
    for (int i = 0; i < cyc; ++i) br.UE();
  }
  br.UE();  // max_num_ref_frames
  br.U(1);  // gaps_in_frame_num_value_allowed_flag
  br.UE();  // pic_width_in_mbs_minus1
  br.UE();  // pic_height_in_map_units_minus1
  br.U(1);  // frame_mbs_only_flag
  br.U(1);  // direct_8x8_inference_flag
  if (br.U(1) != 0) {  // frame_cropping
    br.UE();
    br.UE();
    br.UE();
    br.UE();
  }
  if (br.U(1) == 0) return v;  // vui_parameters_present_flag
  v.vui_present = true;
  if (br.U(1) != 0) {  // aspect_ratio_info_present
    if (br.U(8) == 255) {
      br.U(16);
      br.U(16);
    }
  }
  if (br.U(1) != 0) br.U(1);  // overscan
  if (br.U(1) != 0) {         // video_signal_type_present_flag
    v.video_signal_present = true;
    v.video_format = br.U(3);
    v.full_range = br.U(1);
    v.colour_description = br.U(1);
    if (v.colour_description == 1) {
      v.cp = br.U(8);
      v.tc = br.U(8);
      v.mc = br.U(8);
    }
  }
  return v;
}

// True when the parsed VUI color fields agree with the §8.4 rule
// (BT.709 {1,1,1} or BT.601/170M family {5,6}x{5,6}x{5,6}, limited range).
bool VuiMatchesColorRule(const SpsVui& vui, const xnc::Nv12ColorConfig& rule) {
  if (!vui.vui_present || !vui.video_signal_present ||
      vui.colour_description != 1)
    return false;
  if (vui.full_range != 0) return false;  // limited range
  if (rule.bt709)
    return vui.cp == 1 && vui.tc == 1 && vui.mc == 1;
  return (vui.cp == 5 || vui.cp == 6) && (vui.tc == 5 || vui.tc == 6) &&
         (vui.mc == 5 || vui.mc == 6);
}



// Task 5 (2026-08-26 desktop-media-m0 correctness): encodes ONE cold-start
// reference IDR for `bgra` through a freshly-Init'ed encoder (same
// dimensions/fps/bitrate as the scenario stream, so the backend ladder lands
// on the same rung) and shapes it to the wire contract (SPS/PPS prefix +
// 4-byte start codes). The scenario decodes the returned AU with
// DecodeAnnexBToLumaHash and compares the pipeline's warm-up IDR (the first
// submission = frame A) against it - never against a precomputed raw-color
// constant (H.264 output varies by driver). For the POST-IDLE recovery IDR
// use EncodeRecoveryReferenceIdr: a mid-session forced IDR quantizes
// differently from a cold-start IDR of the same content.
bool EncodeReferenceIdr(xnc::MfSoftEncoder& enc, uint32_t w, uint32_t h,
                        uint32_t fps, uint32_t bitrate, const uint8_t* bgra,
                        size_t bgra_len, std::vector<uint8_t>* shaped_au,
                        std::string* err) {
  std::string ierr;
  if (!enc.Init(w, h, fps, bitrate, &ierr)) {
    if (err != nullptr) *err = "ref init: " + ierr;
    return false;
  }
  enc.ForceNextIdr("selftest-ref");  // one-shot; rides the first submission
  std::string eerr;
  std::vector<std::vector<uint8_t>> frame_aus;
  std::vector<uint8_t> idr_au;
  // Cold start: the MFT buffers ~kEncoderLookaheadFrames inputs before the
  // first AU emerges (the first AU = the first submitted frame, an IDR).
  for (uint32_t i = 0; i < xnc::kEncoderLookaheadFrames * 2 + 4; ++i) {
    frame_aus.clear();
    if (!enc.Encode(bgra, bgra_len, frame_aus, &eerr)) {
      if (err != nullptr) *err = "ref encode: " + eerr;
      return false;
    }
    for (const auto& au : frame_aus) {
      if (xnc::NalHasType(au.data(), au.size(), 5)) {
        idr_au = au;
        break;
      }
    }
    if (!idr_au.empty()) break;
  }
  if (idr_au.empty()) {
    if (err != nullptr) *err = "no reference IDR AU within the feed bound";
    return false;
  }
  xnc::ShapeAu(idr_au.data(), idr_au.size(), true, enc.SpsPps(), shaped_au);
  if (shaped_au->empty()) {
    if (err != nullptr) *err = "shaped reference AU is empty";
    return false;
  }
  return true;
}

// Task 5 (2026-08-26 desktop-media-m0 correctness): encodes the RECOVERY
// reference IDR for the A/B/C stale-pixel regression. The pipeline's
// post-idle recovery IDR is a FORCED IDR produced mid-session after
// A/B/C + warm-up re-feeds of the idle frame, and the encoder's rate
// control quantizes it differently from a cold-start IDR of the same
// content (measured on the MS software encoder: cold-start C and
// mid-session forced C decode to different luma hashes). The reference
// therefore replays the pipeline's exact submission history: A, B, C,
// then re-feeds of frame `refeed_idx` until the cold-start IDR emerges
// (the warm-up phase), then ForceNextIdr, then more re-feeds until the
// forced recovery IDR emerges. Returns that forced IDR shaped to the wire
// contract. refeed_idx == 2 reproduces the fixed pipeline (idle re-feed
// of the latest frame C); refeed_idx == 0 reproduces the stale bug (idle
// re-feed of A) for the discrimination check. A PRIVATE SyntheticBars is
// built here: Frame(0) returns the ctor-drawn pristine pattern, so each
// call starts from a clean buffer (Frame(i>0) redraws in place).
bool EncodeRecoveryReferenceIdr(xnc::MfSoftEncoder& enc, uint32_t w, uint32_t h,
                                uint32_t fps, uint32_t bitrate,
                                uint32_t refeed_idx,
                                std::vector<uint8_t>* shaped_au,
                                std::string* err) {
  std::string ierr;
  if (!enc.Init(w, h, fps, bitrate, &ierr)) {
    if (err != nullptr) *err = "ref init: " + ierr;
    return false;
  }
  SyntheticBars bars(w, h);  // frame 0 = A, frame 1 = B, frame 2 = C
  // Frame(0) skips the redraw (returns whatever Frame(i>0) drew last), so
  // snapshot the pristine A pattern now for the stale re-feed case.
  std::vector<uint8_t> pristine_a(bars.Bytes());
  std::memcpy(pristine_a.data(), bars.Frame(0), bars.Bytes());
  std::string eerr;
  std::vector<std::vector<uint8_t>> frame_aus;
  // bars.Frame(i) redraws the shared buffer, so feed by index each time.
  auto feed = [&](uint32_t idx) {
    frame_aus.clear();
    const uint8_t* src = (idx == 0) ? pristine_a.data() : bars.Frame(idx);
    return enc.Encode(src, bars.Bytes(), frame_aus, &eerr) ? true : false;
  };
  if (!feed(0) || !feed(1) || !feed(2)) {
    if (err != nullptr) *err = "ref encode: " + eerr;
    return false;
  }
  // Warm-up phase: re-feed the idle frame until the cold-start IDR emerges
  // (mirrors the pipeline's warm-up loop, which stops on the first key).
  bool saw_first = false;
  for (uint32_t i = 0; i < xnc::kEncoderLookaheadFrames * 2 + 4; ++i) {
    if (!feed(refeed_idx)) {
      if (err != nullptr) *err = "ref encode: " + eerr;
      return false;
    }
    for (const auto& au : frame_aus)
      if (xnc::NalHasType(au.data(), au.size(), 5)) {
        saw_first = true;
        break;
      }
    if (saw_first) break;
  }
  if (!saw_first) {
    if (err != nullptr) *err = "no warm-up IDR within the feed bound";
    return false;
  }
  // On-demand IDR: the force rides the next re-feed (mirrors the pipeline's
  // PollIdrRequest + IdleFeed recovery path).
  enc.ForceNextIdr("selftest-ref");
  std::vector<uint8_t> rec_au;
  for (uint32_t i = 0; i < xnc::kEncoderLookaheadFrames * 2 + 4; ++i) {
    if (!feed(refeed_idx)) {
      if (err != nullptr) *err = "ref encode: " + eerr;
      return false;
    }
    for (const auto& au : frame_aus)
      if (xnc::NalHasType(au.data(), au.size(), 5)) {
        rec_au = au;
        break;
      }
    if (!rec_au.empty()) break;
  }
  if (rec_au.empty()) {
    if (err != nullptr) *err = "no recovery reference IDR within the feed bound";
    return false;
  }
  xnc::ShapeAu(rec_au.data(), rec_au.size(), true, enc.SpsPps(), shaped_au);
  if (shaped_au->empty()) {
    if (err != nullptr) *err = "shaped reference AU is empty";
    return false;
  }
  return true;
}

// ---- Task 5 pipeline fixtures ----

// Scripted ICapture for the end-to-end pipeline scenarios: yields
// `total_frames` synthetic color-bar frames (moving stripe so P frames
// flow), then err_timeout forever - "N frames then timeouts" (plan Task 5).
// When `rebuild_at < total_frames`, the Acquire that would return frame
// `rebuild_at` instead fires one "err_rebuilt" first (the capture.h retry
// contract: rebuild consumes one call, no frame), mirroring DxgiCapture's
// ACCESS_LOST behavior.
constexpr uint32_t kNoScriptedRebuild = 0xFFFFFFFFu;
class ScriptedCapture final : public xnc::ICapture {
 public:
  ScriptedCapture(uint32_t w, uint32_t h, uint32_t total_frames,
                  uint32_t rebuild_at = kNoScriptedRebuild)
      : bars_(w, h), w_(w), h_(h), total_(total_frames), rebuild_at_(rebuild_at) {}
  bool Acquire(xnc::FrameBlob& blob, std::string* err = nullptr,
               uint32_t timeout_ms = 0) override {
    (void)timeout_ms;  // fakes never block
    if (err) err->clear();
    if (next_ == rebuild_at_ && !rebuild_fired_) {
      rebuild_fired_ = true;
      ++rebuilds_;
      if (err) *err = "err_rebuilt";  // retryable, no frame this call
      return false;
    }
    if (next_ < total_) {
      const uint8_t* p = bars_.Frame(next_);
      blob.bgra.assign(p, p + bars_.Bytes());
      blob.w = w_;
      blob.h = h_;
      blob.mono_us = xnc::NowMonoUs();  // real clock: pipe_latency_ms needs genuine capture stamps
      ++next_;
      return true;
    }
    if (err) *err = "err_timeout";  // static screen from here on
    return false;
  }
  uint32_t Width() const override { return w_; }
  uint32_t Height() const override { return h_; }
  uint32_t RebuildCount() const override { return rebuilds_; }
  uint32_t yielded() const { return next_ < total_ ? next_ : total_; }

 private:
  SyntheticBars bars_;
  uint32_t w_, h_, total_, rebuild_at_;
  uint32_t next_ = 0, rebuilds_ = 0;
  bool rebuild_fired_ = false;
  uint64_t mono_ = 0;
};

// gpu-readback: fake of the DXGI GPU backend - emits NV12 frames (converted
// from the same synthetic bars, tight stride = width) at pre-scaled dims
// with blob.pixfmt == kNv12, like the VideoProcessor path. Used by the
// ScaledCapture pass-through and the pipeline NV12-routing e2e.
class Nv12ScriptedCapture final : public xnc::ICapture {
 public:
  Nv12ScriptedCapture(uint32_t w, uint32_t h, uint32_t total_frames)
      : bars_(w, h), w_(w), h_(h), total_(total_frames),
        nv12_(xnc::Nv12Bytes(w, h)) {}
  bool Acquire(xnc::FrameBlob& blob, std::string* err = nullptr,
               uint32_t timeout_ms = 0) override {
    (void)timeout_ms;
    if (err) err->clear();
    if (next_ < total_) {
      if (!xnc::BgraToNv12(bars_.Frame(next_), bars_.Bytes(), nv12_.data(),
                           nv12_.size(), w_, h_)) {
        if (err) *err = "nv12 convert failed";
        return false;
      }
      blob.bgra = nv12_;
      blob.w = w_;
      blob.h = h_;
      blob.pixfmt = xnc::Pixfmt::kNv12;
      blob.gpu_scale_us = 2000 + next_ * 7;  // like the real DXGI GPU backend
      blob.mono_us = xnc::NowMonoUs();
      ++next_;
      return true;
    }
    if (err) *err = "err_timeout";  // static screen from here on
    return false;
  }
  uint32_t Width() const override { return w_; }
  uint32_t Height() const override { return h_; }

 private:
  SyntheticBars bars_;
  uint32_t w_, h_, total_, next_ = 0;
  std::vector<uint8_t> nv12_;
};

// ---- M2 Task 2: ICaptureSurface fakes (ownership test, plan Step 1) ----

// Test-local instrumentation of the duplication CONTRACT (controller ruling
// 4: assert the ownership invariants on a scripted fake; the real DXGI path
// is Task 6's device matrix). Every call appends to the event log; held()
// is the AcquireNextFrame-minus-ReleaseFrame balance (0 = nothing is held
// when AcquireSurface returns, which is what "ReleaseFrame immediately
// after CopyResource" guarantees).
class ScriptedDupl {
 public:
  enum class Ev : uint8_t { kAcquire = 0, kCopy, kRelease, kEncoder };
  enum class Acq : uint8_t { kFrame, kTimeout };
  struct Frame {
    uint64_t last_present_us;  // mirrors DXGI_OUTDUPL_FRAME_INFO.LastPresentTime
    bool has_texture;          // mirrors a successful QI(ID3D11Texture2D)
  };
  explicit ScriptedDupl(std::vector<Frame> frames) : frames_(std::move(frames)) {}

  Acq AcquireNextFrame(uint32_t timeout_ms, uint64_t* last_present_us,
                       bool* has_texture) {
    (void)timeout_ms;
    events_.push_back(Ev::kAcquire);
    if (next_ >= frames_.size()) return Acq::kTimeout;
    *last_present_us = frames_[next_].last_present_us;
    *has_texture = frames_[next_].has_texture;
    ++next_;
    ++held_;
    return Acq::kFrame;
  }
  void CopyResource() {
    events_.push_back(Ev::kCopy);
    ++copies_;
  }
  void ReleaseFrame() {
    events_.push_back(Ev::kRelease);
    if (held_ > 0) --held_;
  }
  // Any encoder-visible callback the pipeline fires AFTER AcquireSurface
  // returned (in production no encoder work ever runs between the copy and
  // ReleaseFrame).
  void EncoderWork() { events_.push_back(Ev::kEncoder); }

  int held() const { return held_; }
  int copies() const { return copies_; }
  const std::vector<Ev>& events() const { return events_; }

 private:
  std::vector<Frame> frames_;
  size_t next_ = 0;
  int held_ = 0;
  int copies_ = 0;
  std::vector<Ev> events_;
};

// Fake ICaptureSurface mirroring DxgiCapture::AcquireSurface's control flow
// (acquire -> cursor-only check -> full-resource copy -> IMMEDIATE release
// -> stamp the given identity), so the invariants the test pins are the
// ones the real backend implements. Device-free: the LatestSurface is
// bookkept, not driven (the real CopyFrom/Snapshot seam is covered inside
// the D3D device block below). Implements BOTH interfaces on one object,
// like the real backends do.
class ScriptedSurfaceCapture final : public xnc::ICapture,
                                     public xnc::ICaptureSurface {
 public:
  explicit ScriptedSurfaceCapture(std::vector<ScriptedDupl::Frame> script)
      : dupl_(std::move(script)) {}

  // ICapture: unused here - the surface path is ADDITIVE (the CPU FrameBlob
  // pipeline stays for the software/GDI fallback rung).
  bool Acquire(xnc::FrameBlob&, std::string* err = nullptr,
               uint32_t = 0) override {
    if (err) *err = "err_timeout";
    return false;
  }
  uint32_t Width() const override { return 64; }
  uint32_t Height() const override { return 48; }

  xnc::CaptureStatus AcquireSurface(xnc::LatestSurface& latest,
                                    uint32_t timeout_ms, xnc::FrameIdentity* id,
                                    std::string* err) override {
    (void)latest;  // device-free fake: identity bookkeeping only
    if (err) err->clear();
    uint64_t present_us = 0;
    bool has_texture = false;
    if (dupl_.AcquireNextFrame(timeout_ms, &present_us, &has_texture) ==
        ScriptedDupl::Acq::kTimeout) {
      if (err) *err = "err_timeout";
      if (id) *id = last_id_;
      return xnc::CaptureStatus::kNoChange;  // static screen
    }
    // Cursor/metadata-only (LastPresentTime == 0) or no QI-able texture:
    // NOT content - no copy, no stamp, ReleaseFrame immediately.
    if (present_us == 0 || !has_texture) {
      dupl_.ReleaseFrame();
      if (err) *err = "err_timeout";
      if (id) *id = last_id_;
      return xnc::CaptureStatus::kNoChange;
    }
    dupl_.CopyResource();  // the full-resource copy into the surface
    xnc::FrameIdentity stamp = id != nullptr ? *id : xnc::FrameIdentity{};
    stamp.source_mono_us = xnc::NowMonoUs();  // the acquire timestamp
    stamp.encode_seq = 0;
    stamp.present_mono_us = 0;
    dupl_.ReleaseFrame();  // immediately after the copy, before returning
    last_id_ = stamp;
    if (id) *id = stamp;
    return xnc::CaptureStatus::kFrame;
  }

  ScriptedDupl& dupl() { return dupl_; }
  const ScriptedDupl& dupl() const { return dupl_; }

 private:
  ScriptedDupl dupl_;
  xnc::FrameIdentity last_id_{};
};

// rt 场景 ③ 专用:确定性 LCG 噪声帧(320x240,逐帧全噪声 → 压缩后 AU
// 数 KB 级)。合成的 64x48 彩条压缩后只有 ~160B/AU,永远填不满管道
// 缓冲,队列溢出不可达;噪声帧让卡死订阅者的 64KB 管道缓冲 + 深度 3
// 队列在 ~1s 内确定打满(溢出语义的真实路径测试)。
class NoisyCapture final : public xnc::ICapture {
 public:
  NoisyCapture(uint32_t w, uint32_t h, uint32_t total)
      : bgra_((size_t)w * h * 4), w_(w), h_(h), total_(total) {}
  bool Acquire(xnc::FrameBlob& blob, std::string* err = nullptr,
               uint32_t timeout_ms = 0) override {
    (void)timeout_ms;  // fakes never block
    if (err) err->clear();
    if (next_ < total_) {
      uint32_t st = 0x1234567u + next_ * 7919u;
      for (size_t i = 0; i < bgra_.size(); i += 4) {
        st = st * 1664525u + 1013904223u;
        bgra_[i] = static_cast<uint8_t>(st >> 24);
        bgra_[i + 1] = static_cast<uint8_t>(st >> 16);
        bgra_[i + 2] = static_cast<uint8_t>(st >> 8);
        bgra_[i + 3] = 0xFF;
      }
      blob.bgra = bgra_;
      blob.w = w_;
      blob.h = h_;
      blob.mono_us = xnc::NowMonoUs();  // real clock: pipe_latency_ms needs genuine capture stamps
      ++next_;
      return true;
    }
    if (err) *err = "err_timeout";
    return false;
  }
  uint32_t Width() const override { return w_; }
  uint32_t Height() const override { return h_; }

 private:
  std::vector<uint8_t> bgra_;
  uint32_t w_, h_, total_, next_ = 0;
  uint64_t mono_ = 0;
};

// Reads a whole FILE* back from the start (pipeline output goes to a
// TempBinFile - %TEMP%\xnc-selftest-<pid>-<slot>.bin, auto-removed).
std::vector<uint8_t> ReadAll(FILE* f) {
  std::vector<uint8_t> v;
  if (!f) return v;
  std::fseek(f, 0, SEEK_SET);
  uint8_t buf[4096];
  size_t n = 0;
  while ((n = std::fread(buf, 1, sizeof(buf), f)) > 0) v.insert(v.end(), buf, buf + n);
  return v;
}

// tmpfile() replacement without the MSVC deprecation warning: named file in
// %TEMP%, unique per process/slot, deleted on destruction.
class TempBinFile {
 public:
  bool Open(int slot) {
    wchar_t dir[MAX_PATH] = L"";
    const UINT n = GetTempPathW(MAX_PATH, dir);
    if (n == 0 || n >= MAX_PATH) return false;
    wchar_t path[MAX_PATH];
    if (swprintf_s(path, L"%sxnc-selftest-%lu-%d.bin", dir,
                   static_cast<unsigned long>(GetCurrentProcessId()), slot) < 0)
      return false;
    if (_wfopen_s(&f_, path, L"w+b") != 0 || f_ == nullptr) return false;
    path_ = path;
    return true;
  }
  ~TempBinFile() {
    if (f_ != nullptr) std::fclose(f_);
    if (!path_.empty()) DeleteFileW(path_.c_str());
  }
  FILE* get() const { return f_; }

 private:
  FILE* f_ = nullptr;
  std::wstring path_;
};

// Sequence of NAL types (header byte & 0x1F) in Annex-B order, up to max.
std::vector<uint8_t> StreamNalTypes(const uint8_t* d, size_t n, size_t max_types) {
  std::vector<uint8_t> types;
  if (!d) return types;
  const size_t kNpos = static_cast<size_t>(-1);
  size_t i = 0;
  while (types.size() < max_types) {
    size_t hdr = kNpos;
    for (size_t j = i; j + 3 < n; ++j) {
      if (d[j] == 0 && d[j + 1] == 0) {
        if (d[j + 2] == 1) { hdr = j + 3; break; }
        if (d[j + 2] == 0 && j + 4 < n && d[j + 3] == 1) { hdr = j + 4; break; }
      }
    }
    if (hdr == kNpos || hdr >= n) break;
    types.push_back(static_cast<uint8_t>(d[hdr] & 0x1F));
    i = hdr + 1;
  }
  return types;
}

// Total NALUs of a type in a raw stream (counted via repeated find).
size_t CountNalTypeInStream(const uint8_t* d, size_t n, uint8_t type) {
  size_t cnt = 0, from = 0;
  for (;;) {
    const size_t hdr = xnc::nal_detail::FindTypeFrom(d, n, from, type);
    if (hdr == xnc::nal_detail::kNpos) break;
    ++cnt;
    from = hdr + 1;
  }
  return cnt;
}

// True when the first shaped AU of the stream is a keyframe AU: NAL order
// starts SPS(7), PPS(8) and the first VCL NALU (type 1 or 5) is the IDR (5).
// SEI (6) between PPS and IDR is tolerated (the MFT may prepend one).
bool StreamStartsWithKeyframe(const std::vector<uint8_t>& stream) {
  const std::vector<uint8_t> t = StreamNalTypes(stream.data(), stream.size(), 6);
  if (t.size() < 3 || t[0] != 7 || t[1] != 8) return false;
  for (size_t k = 2; k < t.size(); ++k) {
    if (t[k] == 5) return true;   // IDR before any non-IDR slice
    if (t[k] == 1) return false;
  }
  return false;
}

// ---- M1-Slice2 Task 2: rt pipe server fixtures ----

// Fake in-process subscriber over a REAL pipe handle (core selftest
// loopback pattern): client half of the M0 handshake via the blocking
// common/frame.cpp path, then ATTACH + poll-based frame reads so no
// assertion can hang forever.
class RtTestClient {
 public:
  ~RtTestClient() {
    if (h_ != INVALID_HANDLE_VALUE) CloseHandle(h_);
  }

  // Retries CreateFileW until the server's listening instance shows up
  // (<= 4s), then runs the mutual-proof handshake.
  bool Connect(const wchar_t* name, const uint8_t* secret, size_t secret_len) {
    for (int i = 0; i < 400 && h_ == INVALID_HANDLE_VALUE; ++i) {
      h_ = CreateFileW(name, GENERIC_READ | GENERIC_WRITE, 0, nullptr,
                       OPEN_EXISTING, 0, nullptr);
      if (h_ == INVALID_HANDLE_VALUE) {
        const DWORD e = GetLastError();
        if (e == ERROR_PIPE_BUSY) WaitNamedPipeW(name, 200);
        else Sleep(10);
      }
    }
    if (h_ == INVALID_HANDLE_VALUE) return false;
    uint8_t nonce[16];
    for (int i = 0; i < 16; ++i) nonce[i] = static_cast<uint8_t>(i * 7 + 1);
    if (!xnc::WriteFrame(
            h_, xnc::Frame{0, xnc::kMsgHello, 0,
                           xnc::EncodeHello(GetCurrentProcessId(), nonce)}))
      return false;
    xnc::Frame hp;
    if (!ReadFrameT(hp, 3000) || hp.message_type != xnc::kMsgHelloProof) return false;
    uint32_t spid = 0;
    uint8_t snonce[16], sproof[32], want[32];
    if (xnc::DecodeHelloProof(hp, spid, snonce, sproof) != xnc::DecodeResult::Ok)
      return false;
    if (!xnc::HmacSha256(secret, secret_len, nonce, 16, want) ||
        std::memcmp(want, sproof, 32) != 0)
      return false;
    uint8_t myproof[32];
    if (!xnc::HmacSha256(secret, secret_len, snonce, 16, myproof)) return false;
    return xnc::WriteFrame(h_,
                           xnc::Frame{0, xnc::kMsgProof, 0, xnc::EncodeProof(myproof)});
  }

  // ATTACH then expect HOST_HELLO as the very first frame back (any FRAME
  // before it is an ordering bug; STATE/error means the attach failed).
  bool Attach(uint32_t sub_id) {
    const xnc::AttachPayload ap{sub_id, 30, 1920, 2300000};
    if (!xnc::WriteFrame(h_, xnc::Frame{0, xnc::kMsgAttach, 1, xnc::EncodeAttach(ap)}))
      return false;
    for (;;) {
      xnc::Frame f;
      if (!ReadFrameT(f, 3000)) return false;
      if (f.message_type == xnc::kMsgHostHello) {
        hello_ok_ = xnc::DecodeHostHello(f, &hello_);
        media_protocol_ = 0;
        caps_ = 0;
        if (!hello_ok_)  // M1 Task 2: v2 servers append u32 media_protocol=2
          hello_ok_ = xnc::DecodeHostHelloV2(f, &hello_, &media_protocol_, &caps_);
        return hello_ok_;
      }
      if (f.message_type == xnc::kMsgFrame ||
          f.message_type == xnc::kMsgFrameV2) {
        frame_before_hello_ = true;
        return false;
      }
      if (f.message_type == xnc::kMsgState || (f.flags & xnc::kFlagError) != 0) {
        xnc::StateEventPayload st;
        xnc::DecodeStateEvent(f, &st);
        std::printf("SELFTEST NOTE: attach rejected: state=%s\n", st.code);
        return false;
      }
    }
  }

  bool SendDetach(uint32_t sub_id) {
    return xnc::WriteFrame(h_, xnc::Frame{0, xnc::kMsgDetach, 2, xnc::EncodeDetach(sub_id)});
  }

  // Raw frame send (M1-Slice3 Task 1: 0x0108 input messages from the
  // in-process fake viewer).
  bool SendRaw(uint16_t type, const std::vector<uint8_t>& payload) {
    return xnc::WriteFrame(h_, xnc::Frame{0, type, 0, payload});
  }

  // Reads + counts frames until stop_if() or the deadline. Never blocks
  // past deadline (PeekNamedPipe poll underneath).
  template <typename Pred>
  uint32_t Pump(DWORD timeout_ms, Pred stop_if) {
    const ULONGLONG deadline = GetTickCount64() + timeout_ms;
    uint32_t n = 0;
    for (;;) {
      const ULONGLONG now = GetTickCount64();
      if (now >= deadline) break;
      xnc::Frame f;
      if (!ReadFrameT(f, static_cast<DWORD>(deadline - now))) break;
      ++n;
      CountFrame(f);
      if (stop_if()) break;
    }
    return n;
  }
  uint32_t Pump(DWORD timeout_ms) { return Pump(timeout_ms, [] { return false; }); }

  // Poll-read: PeekNamedPipe until one FULL frame is buffered, then the
  // blocking ReadFrame returns instantly. False on EOF/break or deadline.
  bool ReadFrameT(xnc::Frame& out, DWORD timeout_ms) {
    const ULONGLONG deadline = GetTickCount64() + timeout_ms;
    for (;;) {
      uint8_t hdr[xnc::kHeaderSize];
      DWORD got = 0, total = 0;
      if (!PeekNamedPipe(h_, hdr, sizeof(hdr), &got, &total, nullptr)) return false;
      if (got >= xnc::kHeaderSize) {
        const uint32_t n = static_cast<uint32_t>(hdr[12]) |
                           static_cast<uint32_t>(hdr[13]) << 8 |
                           static_cast<uint32_t>(hdr[14]) << 16 |
                           static_cast<uint32_t>(hdr[15]) << 24;
        if (static_cast<uint64_t>(total) >=
            static_cast<uint64_t>(xnc::kHeaderSize) + n)
          return xnc::ReadFrame(h_, out) == xnc::DecodeResult::Ok;
      }
      if (GetTickCount64() >= deadline) return false;
      Sleep(5);
    }
  }

  void CountFrame(const xnc::Frame& f) {
    if (f.message_type == xnc::kMsgFrame) {
      xnc::FrameEventPayload ev;
      if (xnc::DecodeFrameEvent(f, &ev)) {
        frames_++;
        if (ev.key != 0) {
          keys_++;
          first_key_mono_us_ = first_key_mono_us_ == 0 ? ev.mono_us : first_key_mono_us_;
          last_key_mono_us_ = ev.mono_us;
          last_key_payload_ = ev.au;
        }
      }
    } else if (f.message_type == xnc::kMsgFrameV2) {
      // M1 Task 2: 0x0205 frames decode into a full EncodedAU (identity +
      // dims + flags + Annex-B); key = AuFlags::kAuFlagKey.
      xnc::EncodedAU v2;
      if (xnc::DecodeFrameEventV2(f, &v2) && v2.annexb) {
        frames_++;
        v2_frames_++;
        v2_ids_.push_back(v2.id);  // M1 Task 5 (ruling 1a): delivery-order capture
        // M1 Task 4: 重建信号之后,旧 epoch 的 AU 不得再发布(spec 10.2)。
        if (saw_rebuilt_state_ && v2.id.capture_epoch == 1)
          old_epoch_after_rebuilt_++;
        // M1 Task 4: WAIT_IDR 语义 —— 0x020B 之后的首个 v2 帧必须是所携
        // epoch 对的 IDR(delta = 违反断流契约,记负)。
        if (disc_waiting_idr_) {
          const bool is_key = (v2.flags & xnc::AuFlags::kAuFlagKey) != 0;
          if (is_key && v2.id.capture_epoch == disc_capture_epoch_ &&
              v2.id.codec_epoch == disc_codec_epoch_)
            disc_idr_ok_ = true;
          else if (!is_key)
            disc_delta_seen_ = true;
          disc_waiting_idr_ = false;
        }
        if ((v2.flags & xnc::AuFlags::kAuFlagKey) != 0) {
          keys_++;
          first_key_mono_us_ = first_key_mono_us_ == 0 ? v2.id.present_mono_us
                                                       : first_key_mono_us_;
          last_key_mono_us_ = v2.id.present_mono_us;
          last_key_payload_ = *v2.annexb;
          last_key_id_ = v2.id;
          last_key_id_set_ = true;
          if (!first_key_id_set_) {
            first_key_id_ = v2.id;
            first_key_w_ = v2.width;
            first_key_h_ = v2.height;
            first_key_id_set_ = true;
          }
        }
      }
    } else if (f.message_type == xnc::kMsgStreamDiscontinuity) {
      // M1 Task 4: 0x020B STREAM_DISCONTINUITY(v2 模式;v1 永不发送)。
      xnc::StreamDiscontinuityPayload d;
      if (xnc::DecodeStreamDiscontinuity(f, &d)) {
        discontinuities_++;
        disc_capture_epoch_ = d.capture_epoch;
        disc_codec_epoch_ = d.codec_epoch;
        disc_waiting_idr_ = true;
      }
    } else if (f.message_type == xnc::kMsgState) {
      xnc::StateEventPayload st;
      if (xnc::DecodeStateEvent(f, &st)) {
        if (std::strcmp(st.code, "stream_end") == 0) saw_stream_end_ = true;
        if (std::strcmp(st.code, "capture_rebuilt") == 0) saw_rebuilt_state_ = true;
        state_codes_.emplace_back(st.code);
      }
    } else if (f.message_type == xnc::kMsgCursor) {
      if (xnc::DecodeCursorEvent(f, &cursor_x_, &cursor_y_, &cursor_visible_))
        cursors_++;
    } else if (f.message_type == xnc::kMsgDisplayChanged) {
      xnc::DisplayChangedPayload dc;
      if (xnc::DecodeDisplayChanged(f, &dc)) displays_.push_back(dc);
    }
  }

  // counters / observations
  uint64_t frames_ = 0, keys_ = 0;
  uint64_t first_key_mono_us_ = 0, last_key_mono_us_ = 0;
  std::vector<uint8_t> last_key_payload_;
  uint64_t v2_frames_ = 0;  // M1 Task 2: 0x0205 frames decoded
  std::vector<xnc::FrameIdentity> v2_ids_;  // M1 Task 5: decoded v2 identities, arrival order
  xnc::FrameIdentity first_key_id_{};  // identity of the first v2 key AU
  uint32_t first_key_w_ = 0, first_key_h_ = 0;
  bool first_key_id_set_ = false;
  xnc::FrameIdentity last_key_id_{};  // identity of the most recent v2 key AU
  bool last_key_id_set_ = false;
  uint64_t cursors_ = 0;
  int32_t cursor_x_ = -1, cursor_y_ = -1;
  uint8_t cursor_visible_ = 0xFF;
  std::vector<xnc::DisplayChangedPayload> displays_;
  std::vector<std::string> state_codes_;
  xnc::HostHelloPayload hello_{};
  uint32_t media_protocol_ = 0;  // HOST_HELLO v2 trailing u32 (0 = legacy)
  uint32_t caps_ = 0;            // M3 Task 3: further-trailing capabilities u32
  bool hello_ok_ = false, saw_stream_end_ = false, frame_before_hello_ = false;
  // M1 Task 4: 0x020B STREAM_DISCONTINUITY observations.
  uint64_t discontinuities_ = 0;
  uint64_t disc_capture_epoch_ = 0, disc_codec_epoch_ = 0;
  bool disc_waiting_idr_ = false;   // 0x020B seen; the next v2 frame is judged
  bool disc_idr_ok_ = false;        // ...and it was the expected-epoch IDR
  bool disc_delta_seen_ = false;    // ...or a delta slipped through (contract break)
  bool saw_rebuilt_state_ = false;  // STATE{capture_rebuilt} seen (M1 Task 4)
  uint64_t old_epoch_after_rebuilt_ = 0;  // old-epoch v2 frames after a rebuild (bug)

  bool SawState(const char* code) const {
    return std::find(state_codes_.begin(), state_codes_.end(), std::string(code)) !=
           state_codes_.end();
  }

 private:
  HANDLE h_ = INVALID_HANDLE_VALUE;
};

// Per-scenario rt test secret + unique pipe name (pid + slot).
const uint8_t kRtSecret[16] = {'r', 't', '-', 's', 'e', 'l', 'f', 't',
                               'e', 's', 't', '-', 'k', 'e', 'y', '1'};
const wchar_t* RtPipeNameOf(int slot) {
  static wchar_t names[16][96] = {};
  if (slot >= 0 && slot < 16)
    std::swprintf(names[slot], 96, L"\\\\.\\pipe\\xnc-desktop-rt-selftest-%lu-%d",
                  static_cast<unsigned long>(GetCurrentProcessId()), slot);
  return names[slot];
}

// ---- M1-Slice3 Task 1 fixtures: fake Win32 seams ----
// The InputManager/CursorManager production path calls SendInput & co
// directly; both take the raw function pointers as injectable Opts so this
// headless selftest drives the FULL logic (state tables, janitor, lock
// diffs, coordinate math, seq enforcement) while recording the exact INPUT
// structs that would reach the OS. The real SendInput path stays untouched
// and is exercised by the T6 session-1 probe.

// Records every INPUT the manager built; can be told to fail SendInput
// batches (mode 1 = fail the first batch once, mode 2 = always fail) to
// cover the desktop-rebind-retry-once path.
struct InputRecorder {
  std::vector<INPUT> sent;
  int fail_mode = 0;
  bool failed_once = false;
  UINT Send(UINT n, LPINPUT in, int /*cb*/) {
    if (fail_mode == 2 || (fail_mode == 1 && !failed_once)) {
      failed_once = true;
      return 0;
    }
    for (UINT i = 0; i < n; ++i) sent.push_back(in[i]);
    return n;
  }
  void Reset() {
    sent.clear();
    failed_once = false;
  }
  size_t CountMouse(DWORD flags) const {
    size_t c = 0;
    for (const INPUT& i : sent)
      if (i.type == INPUT_MOUSE && i.mi.dwFlags == flags) ++c;
    return c;
  }
  size_t CountKey(DWORD flags) const {
    size_t c = 0;
    for (const INPUT& i : sent)
      if (i.type == INPUT_KEYBOARD && i.ki.dwFlags == flags) ++c;
    return c;
  }
  const INPUT* FindMouse(DWORD flags) const {
    for (const INPUT& i : sent)
      if (i.type == INPUT_MOUSE && i.mi.dwFlags == flags) return &i;
    return nullptr;
  }
  const INPUT* FindKey(DWORD flags) const {
    for (const INPUT& i : sent)
      if (i.type == INPUT_KEYBOARD && i.ki.dwFlags == flags) return &i;
    return nullptr;
  }
};

InputRecorder* g_input_rec = nullptr;
std::map<int, SHORT> g_vk_state;             // fake GetKeyState table
int g_metrics[128] = {0};                    // fake GetSystemMetrics table
uint64_t g_fake_now = 100000;                // fake clock (janitor tests)
int g_open_desk_calls = 0, g_set_desk_calls = 0;
struct {
  LONG x = 0, y = 0;
  DWORD flags = CURSOR_SHOWING;
  BOOL ok = TRUE;
} g_cursor;

UINT WINAPI FakeSendInput(UINT n, LPINPUT in, int cb) {
  return g_input_rec != nullptr ? g_input_rec->Send(n, in, cb) : n;
}
SHORT WINAPI FakeGetKeyState(int vk) {
  const auto it = g_vk_state.find(vk);
  return it == g_vk_state.end() ? SHORT(0) : it->second;
}
int WINAPI FakeGetSystemMetrics(int i) {
  return (i >= 0 && i < 128) ? g_metrics[i] : 0;
}
ULONGLONG WINAPI FakeClock() { return g_fake_now; }
HDESK WINAPI FakeOpenInputDesktop(DWORD, BOOL, ACCESS_MASK) {
  g_open_desk_calls++;
  return reinterpret_cast<HDESK>(1);
}
BOOL WINAPI FakeSetThreadDesktop(HDESK) {
  g_set_desk_calls++;
  return TRUE;
}
BOOL WINAPI FakeGetCursorInfo(PCURSORINFO ci) {
  if (!g_cursor.ok || ci == nullptr) return FALSE;
  ci->flags = g_cursor.flags;
  ci->hCursor = reinterpret_cast<HCURSOR>(1);
  ci->ptScreenPos.x = g_cursor.x;
  ci->ptScreenPos.y = g_cursor.y;
  return TRUE;
}

// Standard test seams: a 200x100 host stream over a (0,0,200,100) virtual
// desktop (single monitor degenerate case); individual cases override the
// tables for the multi-monitor/offset variants.
xnc::InputManager::Opts TestInputOpts(uint32_t w, uint32_t h) {
  xnc::InputManager::Opts o;
  o.hello_w = w;
  o.hello_h = h;
  o.send_input = &FakeSendInput;
  o.get_key_state = &FakeGetKeyState;
  o.get_system_metrics = &FakeGetSystemMetrics;
  o.clock_ms = &FakeClock;
  o.open_input_desktop = &FakeOpenInputDesktop;
  o.set_thread_desktop = &FakeSetThreadDesktop;
  return o;
}

void ResetInputSeams(uint32_t w, uint32_t h) {
  if (g_input_rec != nullptr) g_input_rec->Reset();
  g_vk_state.clear();
  for (int i = 0; i < 128; ++i) g_metrics[i] = 0;
  g_metrics[SM_XVIRTUALSCREEN] = 0;
  g_metrics[SM_YVIRTUALSCREEN] = 0;
  g_metrics[SM_CXVIRTUALSCREEN] = static_cast<int>(w);
  g_metrics[SM_CYVIRTUALSCREEN] = static_cast<int>(h);
  g_fake_now = 100000;
  g_open_desk_calls = 0;
  g_set_desk_calls = 0;
}

xnc::CursorManager::Opts TestCursorOpts(uint32_t w, uint32_t h) {
  xnc::CursorManager::Opts o;
  o.hello_w = w;
  o.hello_h = h;
  o.get_cursor_info = &FakeGetCursorInfo;
  o.get_system_metrics = &FakeGetSystemMetrics;
  return o;
}

// Polls `pred` until true or the deadline (ms); reader threads are async.
template <typename Pred>
bool WaitUntil(Pred pred, DWORD timeout_ms) {
  const ULONGLONG dl = GetTickCount64() + timeout_ms;
  for (;;) {
    if (pred()) return true;
    if (GetTickCount64() >= dl) return pred();
    Sleep(10);
  }
}

// ---- M2-Slice1 Task 1 fixtures: DesktopWatch fake Win32 seams ----
// The watch's production path is OpenInputDesktop -> GetUserObjectInformation
// (UOI_NAME) -> CloseDesktop on a 500 ms thread; the Opts function pointers
// let the selftest drive the FULL threaded logic with scripted desktop
// names + a logical clock (poll_ms is shrunk to 1 ms real sleep while each
// sample advances the fake clock by 500 ms), proving the transition
// sequence, CloseDesktop pairing and the poll-failure synthetic name.
std::vector<std::string> g_dw_names;  // scripted observations; last repeats
size_t g_dw_idx = 0;
uint64_t g_dw_clock = 0;
int g_dw_opens = 0, g_dw_closes = 0, g_dw_bad_closes = 0;
bool g_dw_open_fail = false;
std::mutex g_dw_ev_mu;  // guards vectors read cross-thread below

HDESK WINAPI DwOpenInputDesktop(DWORD, BOOL, ACCESS_MASK) {
  g_dw_opens++;
  g_dw_clock += 500;  // logical time of THIS sample (see fixtures comment)
  if (g_dw_open_fail) {
    SetLastError(ERROR_ACCESS_DENIED);
    return nullptr;
  }
  return reinterpret_cast<HDESK>(0x584E4357ull);  // 'XNCW' sentinel handle
}
BOOL WINAPI DwGetUserObjectInformation(HANDLE h, int info, void* pv, DWORD len,
                                       LPDWORD needed) {
  if (h != reinterpret_cast<HDESK>(0x584E4357ull) || info != UOI_NAME ||
      pv == nullptr)
    return FALSE;
  const size_t i = g_dw_idx < g_dw_names.size() ? g_dw_idx : g_dw_names.size() - 1;
  ++g_dw_idx;
  const std::string& n = g_dw_names[i];
  wchar_t* w = static_cast<wchar_t*>(pv);
  size_t k = 0;
  for (; k + 1 < len / sizeof(wchar_t) && k < n.size(); ++k)
    w[k] = static_cast<wchar_t>(n[k]);
  w[k] = L'\0';
  if (needed) *needed = static_cast<DWORD>((k + 1) * sizeof(wchar_t));
  return TRUE;
}
BOOL WINAPI DwCloseDesktop(HDESK h) {
  if (h == reinterpret_cast<HDESK>(0x584E4357ull)) {
    g_dw_closes++;
    return TRUE;
  }
  g_dw_bad_closes++;  // double close / foreign handle = leak or corruption
  SetLastError(ERROR_INVALID_HANDLE);
  return FALSE;
}
ULONGLONG WINAPI DwClock() { return g_dw_clock; }

// Builds watch Opts over the fake seams; poll_ms is REAL sleep (1 ms) while
// the logical clock advances 500 ms per sample, so a scripted run of N names
// completes in ~N ms wall time with 500 ms-spaced observation timestamps.
xnc::DesktopWatch::Opts TestWatchOpts(
    std::function<void(const xnc::DesktopTransition&)> cb) {
  xnc::DesktopWatch::Opts o;
  o.poll_ms = 1;
  o.transition_timeout_ms = 2000;
  o.on_transition = std::move(cb);
  o.open_input_desktop = &DwOpenInputDesktop;
  o.get_user_object_info = &DwGetUserObjectInformation;
  o.close_desktop = &DwCloseDesktop;
  o.clock_ms = &DwClock;
  return o;
}
void ResetWatchSeams(std::vector<std::string> names) {
  g_dw_names = std::move(names);
  g_dw_idx = 0;
  g_dw_clock = 0;
  g_dw_opens = 0;
  g_dw_closes = 0;
  g_dw_bad_closes = 0;
  g_dw_open_fail = false;
}

// ---- M2-Slice1 Task 2 fixtures: CaptureReset fake clock/gate, scripted
// resettable capture, recording sink ----

// Deterministic clock for the CaptureReset unit table (g_cr_unit_now is the
// ONLY time source; tests advance it by hand).
uint64_t g_cr_unit_now = 1000;
uint64_t CrUnitClock() { return g_cr_unit_now; }
// Shared gate for the pipeline-driven reset scenarios (main thread flips it
// while the pipeline thread runs; ResetDesktop is a plain enum so reads are
// atomic enough for a test lever).
std::atomic<xnc::ResetDesktop> g_cr_gate{xnc::ResetDesktop::kDefault};
xnc::ResetDesktop CrUnitGate(void*) { return g_cr_gate.load(); }
uint64_t CrRealClock() { return GetTickCount64(); }

// Scripted capture for the reset scenarios (real-encoder pipeline drives
// it). Levers mirror the DXGI behaviors T1 measured on XIAOXIN:
//   LoseAccess()    every Acquire returns "err_access_lost" (secure desktop
//                   revoked the duplication; re-duplication denied)
//   SetResolution() the NEXT yielded frame carries the new size (backend
//                   adopted the mode; the encoder did not)
//   FailRebuilds(n) Rebuild() fails n times before succeeding (denied while
//                   the secure desktop is up / transient failures)
class ResetCapture final : public xnc::ICapture {
 public:
  ResetCapture(uint32_t w, uint32_t h, uint32_t total)
      : buf_((size_t)w * h * 4), w_(w), h_(h), total_(total) {}
  bool Acquire(xnc::FrameBlob& blob, std::string* err = nullptr,
               uint32_t timeout_ms = 0) override {
    (void)timeout_ms;  // fakes never block
    if (err) err->clear();
    if (access_lost_) {
      if (err) *err = "err_access_lost";
      return false;
    }
    if (next_ < total_) {
      Fill(next_);
      blob.bgra = buf_;
      blob.w = w_;
      blob.h = h_;
      blob.mono_us = xnc::NowMonoUs();  // real clock: pipe_latency_ms needs genuine capture stamps
      ++next_;
      return true;
    }
    if (err) *err = "err_timeout";
    return false;
  }
  uint32_t Width() const override { return w_; }
  uint32_t Height() const override { return h_; }
  uint32_t RebuildCount() const override { return rebuilds_; }
  bool Rebuild(std::string* err = nullptr) override {
    ++rebuilds_;
    if (fails_left_ > 0) {
      --fails_left_;
      if (err) *err = "scripted rebuild failure";
      return false;
    }
    access_lost_ = false;  // fresh duplication works again
    return true;
  }
  void LoseAccess() { access_lost_ = true; }
  void SetResolution(uint32_t w, uint32_t h) { pending_w_ = w; pending_h_ = h; }
  void FailRebuilds(uint32_t n) { fails_left_ = n; }
  void MoreFrames(uint32_t n) { total_ += n; }
  uint32_t yielded() const { return next_ < total_ ? next_ : total_; }

 private:
  void Fill(uint32_t frame) {
    if (pending_w_ != 0 && (pending_w_ != w_ || pending_h_ != h_)) {
      w_ = pending_w_;
      h_ = pending_h_;
      buf_.assign((size_t)w_ * h_ * 4, 0);
    }
    const uint8_t base[4] = {0x20, 0x40, 0x60, 0xFF};
    for (size_t i = 0; i + 3 < buf_.size(); i += 4)
      for (int k = 0; k < 4; ++k) buf_[i + k] = base[k];
    if (!buf_.empty()) {  // moving pixel so P frames flow
      const size_t px = (frame * 137) % (buf_.size() / 4);
      buf_[px * 4] = 0xF0;
      buf_[px * 4 + 2] = 0x90;
    }
  }
  std::vector<uint8_t> buf_;
  uint32_t w_, h_, total_, next_ = 0, rebuilds_ = 0;
  uint32_t pending_w_ = 0, pending_h_ = 0, fails_left_ = 0;
  bool access_lost_ = false;
  uint64_t mono_ = 0;
};

// AuSink recorder for the pipeline reset scenarios: counts AUs, records
// STATE codes (+recoverable flag) and DISPLAY_CHANGED events in order.
// Thread-safe: with the capture/encode threads decoupled, OnAu runs on the
// encode thread while OnState/OnDisplayChanged run on the capture thread.
struct RecordingSink final : xnc::AuSink {
  const char* OnAu(const xnc::EncodedAU& au) override {
    std::lock_guard<std::mutex> lk(mu);
    aus++;
    if ((au.flags & xnc::AuFlags::kAuFlagKey) != 0) keys++;
    timestamps.push_back(au.id.present_mono_us);
    ids.push_back(au.id);  // M1 Task 5 (ruling 1a): sink-level identity capture
    return nullptr;
  }
  void OnState(const char* code, bool recoverable) override {
    std::lock_guard<std::mutex> lk(mu);
    states.emplace_back(code != nullptr ? code : "?");
    states_recoverable.push_back(recoverable);
  }
  void OnDisplayChanged(uint32_t w, uint32_t h, const char* reason) override {
    std::lock_guard<std::mutex> lk(mu);
    Disp d;
    d.w = w;
    d.h = h;
    d.reason = reason != nullptr ? reason : "?";
    displays.push_back(d);
  }
  bool Saw(const char* code) const {
    std::lock_guard<std::mutex> lk(mu);
    return std::find(states.begin(), states.end(), std::string(code)) != states.end();
  }
  bool SawRecoverable(const char* code) const {
    std::lock_guard<std::mutex> lk(mu);
    for (size_t i = 0; i < states.size() && i < states_recoverable.size(); ++i)
      if (states[i] == code) return states_recoverable[i];
    return false;
  }
  mutable std::mutex mu;
  uint64_t aus = 0, keys = 0;
  std::vector<uint64_t> timestamps;
  std::vector<xnc::FrameIdentity> ids;  // every delivered AU, in delivery order
  std::vector<std::string> states;
  std::vector<bool> states_recoverable;
  struct Disp {
    uint32_t w, h;
    std::string reason;
  };
  std::vector<Disp> displays;
};

// Delivered-AU identity contract (M1 Task 5, ruling 1a): every AU sequence a
// sink actually received must (a) replay clean through a fresh production
// FrameIdentityLedger - i.e. epochs never regress and within one epoch pair
// content_id never regresses while encode_seq strictly increases - and
// (b) carry source_mono_us <= present_mono_us (capture precedes encoder
// submission; spec §5.1/§5.2). The pipeline ledger-gates every submitted
// identity and delivers FIFO, so any real scenario's captured AUs must
// satisfy this; asserting it end-to-end catches publish-order regressions.
bool DeliveredIdentitiesValid(const std::vector<xnc::FrameIdentity>& ids) {
  if (ids.empty()) return false;
  xnc::FrameIdentityLedger replay;
  for (const xnc::FrameIdentity& id : ids) {
    if (id.source_mono_us > id.present_mono_us) return false;
    if (!replay.Accept(id)) return false;
  }
  return true;
}

// Deterministic MFT-boundary fault plan for submission-ledger failure tests.
// The pipeline, ledger, shaping, sink delivery, and cleanup remain real; only
// external ProcessInput/ProcessOutput outcomes are supplied by this seam.
struct EncoderFaultPlan {
  struct CollectStep {
    bool ok = true;
    size_t aus = 0;
    size_t emit_on_call = 0;  // zero = every call
    const char* err = nullptr;
    size_t calls = 0;
  };
  struct MessageStep {
    bool ok = true;
    const char* err = nullptr;
    size_t calls = 0;
  };

  size_t reject_input_call = static_cast<size_t>(-1);  // one-based
  size_t input_calls = 0;
  CollectStep submit;
  CollectStep drain;
  CollectStep flush_tail;
  MessageStep end_of_stream;
  MessageStep drain_message;
  std::mutex input_mu;
  std::condition_variable input_cv;

  static bool ProcessInput(void* opaque, std::string* err) {
    auto* p = static_cast<EncoderFaultPlan*>(opaque);
    size_t call = 0;
    {
      std::lock_guard<std::mutex> lk(p->input_mu);
      call = ++p->input_calls;
    }
    p->input_cv.notify_all();
    if (call != p->reject_input_call) return true;
    if (err != nullptr) *err = "fault_pre_accept";
    return false;
  }

  size_t InputCalls() {
    std::lock_guard<std::mutex> lk(input_mu);
    return input_calls;
  }

  void WaitForInputCalls(size_t count) {
    std::unique_lock<std::mutex> lk(input_mu);
    input_cv.wait(lk, [&] { return input_calls >= count; });
  }

  static bool CollectOutputs(void* opaque, xnc::EncoderOutputStage stage,
                             std::vector<std::vector<uint8_t>>* aus,
                             std::string* err) {
    auto* p = static_cast<EncoderFaultPlan*>(opaque);
    CollectStep* step = stage == xnc::EncoderOutputStage::kSubmit
                            ? &p->submit
                            : (stage == xnc::EncoderOutputStage::kDrain
                                   ? &p->drain
                                   : &p->flush_tail);
    ++step->calls;
    static const uint8_t kCompletePFrame[] = {0, 0, 0, 1, 0x41, 0x80};
    if (step->emit_on_call == 0 || step->emit_on_call == step->calls) {
      for (size_t i = 0; i < step->aus; ++i)
        aus->emplace_back(kCompletePFrame,
                          kCompletePFrame + sizeof(kCompletePFrame));
    }
    if (!step->ok && err != nullptr)
      *err = step->err != nullptr ? step->err : "fault_collect";
    return step->ok;
  }

  static bool ProcessMessage(void* opaque, xnc::EncoderMessage message,
                             std::string* err) {
    auto* p = static_cast<EncoderFaultPlan*>(opaque);
    MessageStep* step = message == xnc::EncoderMessage::kEndOfStream
                            ? &p->end_of_stream
                            : &p->drain_message;
    ++step->calls;
    if (!step->ok && err != nullptr)
      *err = step->err != nullptr ? step->err : "fault_message";
    return step->ok;
  }

  xnc::MfEncoderFaultSeam Seam() {
    xnc::MfEncoderFaultSeam seam;
    seam.ctx = this;
    seam.process_input = &EncoderFaultPlan::ProcessInput;
    seam.collect_outputs = &EncoderFaultPlan::CollectOutputs;
    seam.process_message = &EncoderFaultPlan::ProcessMessage;
    return seam;
  }
};

// Produces exactly N frames and requests pipeline stop while returning the
// last one. No test-side sleeps or wall-clock races are needed.
class StopAfterCapture final : public xnc::ICapture {
 public:
  StopAfterCapture(uint32_t total, std::atomic<bool>* stop,
                   EncoderFaultPlan* plan)
      : buf_(64u * 48u * 4u, 0x20), total_(total), stop_(stop), plan_(plan) {}
  bool Acquire(xnc::FrameBlob& blob, std::string* err = nullptr,
               uint32_t = 0) override {
    if (err != nullptr) err->clear();
    if (next_ >= total_) {
      if (err != nullptr) *err = "err_timeout";
      return false;
    }
    // Keep at most one frame ahead of the encoder. This is a state-driven
    // handoff, not a timing sleep, and makes the 64->65 overflow deterministic.
    if (next_ != 0 && plan_ != nullptr) plan_->WaitForInputCalls(next_);
    buf_[next_ % buf_.size()] = static_cast<uint8_t>(0x40 + next_);
    blob.bgra = buf_;
    blob.w = Width();
    blob.h = Height();
    blob.pixfmt = xnc::Pixfmt::kBgra;
    blob.mono_us = 100 + static_cast<uint64_t>(next_) * 100;
    ++next_;
    if (next_ == total_ && stop_ != nullptr) stop_->store(true);
    return true;
  }
  uint32_t Width() const override { return 64; }
  uint32_t Height() const override { return 48; }

 private:
  std::vector<uint8_t> buf_;
  uint32_t total_ = 0, next_ = 0;
  std::atomic<bool>* stop_ = nullptr;
  EncoderFaultPlan* plan_ = nullptr;
};

// Two encoder lifecycles in one Pipeline::Run: the first accepted input stays
// delayed, ACCESS_LOST rebuilds at a new dimension (forcing encoder Init), and
// the second lifecycle emits exactly one AU. Success proves the old pending
// identity was cleared; without the lifecycle clear stream end sees one extra.
class LedgerLifecycleCapture final : public xnc::ICapture {
 public:
  LedgerLifecycleCapture(EncoderFaultPlan* plan, std::atomic<bool>* stop)
      : plan_(plan), stop_(stop), buf_(64u * 48u * 4u, 0x31) {}

  bool Acquire(xnc::FrameBlob& blob, std::string* err = nullptr,
               uint32_t = 0) override {
    if (err != nullptr) err->clear();
    if (event_ == 0) {
      ++event_;
      Fill(blob);
      return true;
    }
    if (event_ == 1) {
      plan_->WaitForInputCalls(1);
      ++event_;
      if (err != nullptr) *err = "err_access_lost";
      return false;
    }
    if (event_ == 2 && rebuilt_) {
      ++event_;
      Fill(blob);
      if (stop_ != nullptr) stop_->store(true);
      return true;
    }
    if (err != nullptr) *err = "err_timeout";
    return false;
  }

  uint32_t Width() const override { return w_; }
  uint32_t Height() const override { return h_; }
  bool Rebuild(std::string* err = nullptr) override {
    if (err != nullptr) err->clear();
    w_ = 66;
    buf_.assign(static_cast<size_t>(w_) * h_ * 4u, 0x52);
    rebuilt_ = true;
    return true;
  }

 private:
  void Fill(xnc::FrameBlob& blob) {
    blob.bgra = buf_;
    blob.w = w_;
    blob.h = h_;
    blob.pixfmt = xnc::Pixfmt::kBgra;
    blob.mono_us = 100 + event_ * 100;
  }

  EncoderFaultPlan* plan_ = nullptr;
  std::atomic<bool>* stop_ = nullptr;
  std::vector<uint8_t> buf_;
  uint32_t w_ = 64, h_ = 48, event_ = 0;
  bool rebuilt_ = false;
};

xnc::PipelineResult RunEncoderFaultScenario(uint32_t frames,
                                            EncoderFaultPlan* plan,
                                            xnc::AuSink& sink,
                                            bool* init_ok) {
  const xnc::MfEncoderFaultSeam seam = plan->Seam();
  xnc::MfSoftEncoder enc(&seam);
  enc.SetForceSoftware(true);
  std::string err;
  const bool initialized = enc.Init(64, 48, 30, 500000, &err);
  if (init_ok != nullptr) *init_ok = initialized;
  if (!initialized) {
    xnc::PipelineResult r;
    r.ok = false;
    r.err = "fault-test init: " + err;
    return r;
  }
  std::atomic<bool> stop{false};
  StopAfterCapture cap(frames, &stop, plan);
  xnc::PipelineOpts opt;
  opt.duration_s = frames > 64 ? 10 : 2;
  opt.fps = 30;
  opt.target_bitrate_bps = 500000;
  opt.stop = &stop;
  return xnc::Pipeline::Run(cap, enc, sink, opt);
}

xnc::PipelineResult RunLedgerLifecycleScenario(EncoderFaultPlan* plan,
                                               xnc::AuSink& sink,
                                               bool* init_ok) {
  const xnc::MfEncoderFaultSeam seam = plan->Seam();
  xnc::MfSoftEncoder enc(&seam);
  enc.SetForceSoftware(true);
  std::string err;
  const bool initialized = enc.Init(64, 48, 30, 500000, &err);
  if (init_ok != nullptr) *init_ok = initialized;
  if (!initialized) {
    xnc::PipelineResult r;
    r.ok = false;
    r.err = "fault-test init: " + err;
    return r;
  }
  std::atomic<bool> stop{false};
  LedgerLifecycleCapture cap(plan, &stop);
  xnc::CaptureReset reset;
  xnc::PipelineOpts opt;
  opt.duration_s = 5;
  opt.fps = 30;
  opt.target_bitrate_bps = 500000;
  opt.stop = &stop;
  opt.reset = &reset;
  return xnc::Pipeline::Run(cap, enc, sink, opt);
}

struct FailingAuSink final : xnc::AuSink {
  const char* OnAu(const xnc::EncodedAU& au) override {
    timestamps.push_back(au.id.present_mono_us);
    return timestamps.size() == fail_on ? "fault_sink_mid_vector" : nullptr;
  }
  size_t fail_on = 1;
  std::vector<uint64_t> timestamps;
};

// ---- M2-Slice1 Task 3 fixtures: fake ladder backends + state log ----

// Records EmitBackendChanged callbacks ("backend/reason" pairs, in order).
struct LadderStateLog {
  std::vector<std::string> entries;
  void Record(const char* backend, const char* reason) {
    std::string e = backend != nullptr ? backend : "?";
    e += "/";
    e += reason != nullptr ? reason : "?";
    entries.push_back(std::move(e));
  }
  bool Has(const char* backend, const char* reason) const {
    std::string want = std::string(backend) + "/" + reason;
    return std::find(entries.begin(), entries.end(), want) != entries.end();
  }
};

// LadderOpts::on_switch thunk.
void LadderLogThunk(void* ctx, const char* backend, const char* reason) {
  static_cast<LadderStateLog*>(ctx)->Record(backend, reason);
}

// Fake DXGI rung: yields `frames` moving frames, then `hard_errors` fatal-
// family errors, then err_timeout forever. Rebuild() fails
// `rebuild_fails` times before succeeding. acquire_calls/rebuild_calls
// observe the ladder's driving.
class FakeDxgiRung final : public xnc::ICapture {
 public:
  FakeDxgiRung(uint32_t w, uint32_t h, uint32_t frames, uint32_t hard_errors,
               uint32_t rebuild_fails = 0)
      : buf_((size_t)w * h * 4, 0x11), w_(w), h_(h),
        frames_(frames), hard_errors_(hard_errors), rebuild_fails_(rebuild_fails) {}
  bool Acquire(xnc::FrameBlob& blob, std::string* err = nullptr,
               uint32_t timeout_ms = 0) override {
    (void)timeout_ms;  // fakes never block
    ++acquire_calls_;
    if (err) err->clear();
    if (next_ < frames_) {
      buf_[next_ % buf_.size()] = 0x80 + (next_ & 0x3F);  // moving content
      blob.bgra = buf_;
      blob.w = w_;
      blob.h = h_;
      blob.mono_us = xnc::NowMonoUs();  // real clock: pipe_latency_ms needs genuine capture stamps
      ++next_;
      return true;
    }
    if (errors_ < hard_errors_) {
      ++errors_;
      if (err) *err = "AcquireNextFrame: hr=0x80004005";  // fatal family
      return false;
    }
    if (err) *err = "err_timeout";
    return false;
  }
  uint32_t Width() const override { return w_; }
  uint32_t Height() const override { return h_; }
  uint32_t RebuildCount() const override { return rebuild_oks_; }
  bool Rebuild(std::string* err = nullptr) override {
    ++rebuild_calls_;
    if (fails_left_ > 0) {
      --fails_left_;
      if (err) *err = "D3D11CreateDevice: hr=0x80004005";
      return false;
    }
    ++rebuild_oks_;
    return true;
  }
  uint32_t acquire_calls() const { return acquire_calls_; }
  uint32_t rebuild_calls() const { return rebuild_calls_; }

 private:
  std::vector<uint8_t> buf_;
  uint32_t w_, h_, frames_, hard_errors_, rebuild_fails_;
  uint32_t next_ = 0, errors_ = 0, fails_left_ = rebuild_fails_;
  uint32_t rebuild_oks_ = 0, acquire_calls_ = 0, rebuild_calls_ = 0;
  uint64_t mono_ = 0;
};

// Fake GDI rung: frames forever, never fails (the rung of last resort).
class FakeGdiRung final : public xnc::ICapture {
 public:
  explicit FakeGdiRung(uint32_t w = 96, uint32_t h = 64)
      : buf_((size_t)w * h * 4, 0x22), w_(w), h_(h) {}
  bool Acquire(xnc::FrameBlob& blob, std::string* err = nullptr,
               uint32_t timeout_ms = 0) override {
    (void)timeout_ms;  // fakes never block
    if (err) err->clear();
    buf_[next_ % buf_.size()] = 0x90 + (next_ & 0xF);
    ++next_;
    blob.bgra = buf_;
    blob.w = w_;
    blob.h = h_;
    blob.mono_us = xnc::NowMonoUs();  // real clock: pipe_latency_ms needs genuine capture stamps
    return true;
  }
  uint32_t Width() const override { return w_; }
  uint32_t Height() const override { return h_; }
  bool Rebuild(std::string* err = nullptr) override {
    if (err) err->clear();
    return true;  // the rung of last resort always rebuilds
  }

 private:
  std::vector<uint8_t> buf_;
  uint32_t w_, h_, next_ = 0;
  uint64_t mono_ = 0;
};

// Fake factory contexts: the ladder calls make_dxgi from BOTH the probe
// thread and Rebuild (pipeline thread) - construction counts are atomic and
// each call returns an independent instance. `fail_after` models DXGI
// breaking again between the probe and the swap (creation N fails).
struct FakeDxgiFactory {
  uint32_t w = 64, h = 48, frames = 1000, hard_errors = 0, rebuild_fails = 0;
  int64_t fail_after = -1;  // <0 = never; else creations > fail_after fail
  std::atomic<uint32_t> created{0};
  std::string fail_err = "D3D11CreateDevice: hr=0x887A0007";
  std::unique_ptr<xnc::ICapture> Make(uint32_t max_w, std::string* err) {
    (void)max_w;  // fake rungs are BGRA; the GPU pipeline is not fake-able
    const uint32_t n = ++created;
    if (fail_after >= 0 && static_cast<int64_t>(n) > fail_after) {
      if (err) *err = fail_err;
      return nullptr;
    }
    return std::make_unique<FakeDxgiRung>(w, h, frames, hard_errors, rebuild_fails);
  }
};

struct FakeGdiFactory {
  uint32_t w = 96, h = 64;
  std::atomic<uint32_t> created{0};
  std::unique_ptr<xnc::ICapture> Make(uint32_t max_w, std::string* err) {
    (void)max_w;
    ++created;
    if (err) err->clear();
    return std::make_unique<FakeGdiRung>(w, h);
  }
};

// Thunks bridging the fn-pointer factories to file-scope fixture pointers
// (each test points them at its own fixture BEFORE constructing the ladder;
// the probe thread is joined in ~LadderCapture, so the pointers never
// dangle while a ladder is alive).
FakeDxgiFactory* g_ld_dxgi = nullptr;
FakeGdiFactory* g_ld_gdi = nullptr;
std::unique_ptr<xnc::ICapture> LdMakeDxgi(uint32_t max_w, std::string* err) {
  return g_ld_dxgi != nullptr ? g_ld_dxgi->Make(max_w, err) : nullptr;
}
std::unique_ptr<xnc::ICapture> LdMakeGdi(uint32_t max_w, std::string* err) {
  return g_ld_gdi != nullptr ? g_ld_gdi->Make(max_w, err) : nullptr;
}

}  // namespace

int SelftestMain(bool desktop_pipeline_v2) {
  if (desktop_pipeline_v2)
    std::printf("SELFTEST NOTE: --desktop-pipeline-v2: MediaPipelineV2 "
                "scenarios ON\n");
  { // M2 Task 4: the --desktop-pipeline-v2 argv stripper (pure matrix).
    const std::vector<std::wstring> args = {L"--selftest",
                                            L"--desktop-pipeline-v2"};
    std::vector<wchar_t*> argv;
    argv.push_back(const_cast<wchar_t*>(L"xnc-desktop.exe"));
    for (const auto& a : args) argv.push_back(const_cast<wchar_t*>(a.c_str()));
    int argc = static_cast<int>(argv.size());
    CHECK("v2-strip-removes",
          xnc::StripDesktopPipelineV2Flag(&argc, argv.data()) == 1 &&
              argc == 2 && std::wcscmp(argv[1], L"--selftest") == 0);
    CHECK("v2-strip-idempotent",
          xnc::StripDesktopPipelineV2Flag(&argc, argv.data()) == 0 && argc == 2);
    // Absent flag: nothing stripped, order preserved.
    const std::vector<std::wstring> args2 = {L"--console-rt", L"--fps",
                                             L"15"};
    std::vector<wchar_t*> argv2;
    argv2.push_back(const_cast<wchar_t*>(L"xnc-desktop.exe"));
    for (const auto& a : args2) argv2.push_back(const_cast<wchar_t*>(a.c_str()));
    int argc2 = static_cast<int>(argv2.size());
    CHECK("v2-strip-absent",
          xnc::StripDesktopPipelineV2Flag(&argc2, argv2.data()) == 0 &&
              argc2 == 4 && std::wcscmp(argv2[3], L"15") == 0);
    // Flag among others keeps the rest in order.
    const std::vector<std::wstring> args3 = {L"--fps", L"--desktop-pipeline-v2",
                                             L"30", L"--desktop-pipeline-v2"};
    std::vector<wchar_t*> argv3;
    argv3.push_back(const_cast<wchar_t*>(L"xnc-desktop.exe"));
    for (const auto& a : args3) argv3.push_back(const_cast<wchar_t*>(a.c_str()));
    int argc3 = static_cast<int>(argv3.size());
    CHECK("v2-strip-keeps-order",
          xnc::StripDesktopPipelineV2Flag(&argc3, argv3.data()) == 2 &&
              argc3 == 3 && std::wcscmp(argv3[1], L"--fps") == 0 &&
              std::wcscmp(argv3[2], L"30") == 0);
    // End-to-end shape: the stripped argv parses as the plain mode.
    ParseOutcome po = Parse({L"--selftest"});
    CHECK("v2-strip-parse", po.ok && po.opt.selftest);
  }
  { // M1 Task 1: immutable frame identity - compile-time field order plus
    // the monotonicity ledger's accept/reject rules (spec §5.1/§5.3).
    // The first three checks are the plan's binding test vector.
    xnc::FrameIdentity id{1, 2, 3, 4, 500, 600};
    CHECK("identity-fields", id.capture_epoch == 1 && id.encode_seq == 4);
    xnc::FrameIdentityLedger l;
    CHECK("identity-accept", l.Accept(id));
    CHECK("identity-reject-repeat", !l.Accept(id));
    // Same epochs: new content accepted; same-content re-encode needs a
    // strictly new encode_seq; content/seq regressions and repeats rejected.
    xnc::FrameIdentity next = id;
    next.content_id = 4;
    next.encode_seq = 5;
    CHECK("identity-accept-new-content", l.Accept(next));
    xnc::FrameIdentity reencode = next;
    reencode.encode_seq = 6;
    CHECK("identity-accept-reencode-new-seq", l.Accept(reencode));
    CHECK("identity-reject-seq-repeat", !l.Accept(reencode));
    xnc::FrameIdentity regress = reencode;
    regress.content_id = 3;
    regress.encode_seq = 7;
    CHECK("identity-reject-content-regress", !l.Accept(regress));
    CHECK("identity-last-tracked",
          l.Last().content_id == 4 && l.Last().encode_seq == 6);
    // Epoch advance re-baselines (capture/codec generation change);
    // epoch regression is rejected.
    const xnc::FrameIdentity gen2{2, 1, 1, 8, 700, 800};
    CHECK("identity-accept-epoch-advance", l.Accept(gen2));
    CHECK("identity-reject-epoch-regress", !l.Accept(id));
    CHECK("identity-reset-forgets", (l.Reset(), l.Accept(id)));
    // Codec-epoch edges WITHIN one capture epoch (M1 Task 5, ruling 1b - the
    // checks above only ever move codec_epoch alongside a capture advance):
    // a codec_epoch advance alone re-baselines (content/seq may restart from
    // 1), a codec_epoch regress in the same capture epoch rejects, and the
    // re-baselined pair then enforces the usual in-epoch monotonicity.
    xnc::FrameIdentityLedger cl;
    CHECK("identity-codec-advance-baseline",
          cl.Accept(xnc::FrameIdentity{5, 3, 10, 100, 1, 2}));
    CHECK("identity-codec-advance-rebaseline",
          cl.Accept(xnc::FrameIdentity{5, 4, 1, 1, 3, 4}));
    CHECK("identity-codec-advance-repeat-rejected",
          !cl.Accept(xnc::FrameIdentity{5, 4, 1, 1, 3, 4}));
    CHECK("identity-codec-regress-rejected",
          !cl.Accept(xnc::FrameIdentity{5, 3, 20, 200, 5, 6}));
    CHECK("identity-codec-rebaseline-monotonic",
          cl.Accept(xnc::FrameIdentity{5, 4, 2, 2, 7, 8}) &&
              !cl.Accept(xnc::FrameIdentity{5, 4, 1, 9, 7, 8}));
    // EncodedAU: AuFlags key bit and the immutable shared Annex-B payload.
    CHECK("au-flag-none-zero", xnc::AuFlags::kAuFlagNone == 0);
    xnc::EncodedAU au;
    au.id = id;
    au.width = 1920;
    au.height = 1080;
    au.flags = xnc::AuFlags::kAuFlagKey;
    au.annexb = std::make_shared<const std::vector<uint8_t>>(
        std::vector<uint8_t>{0, 0, 0, 1, 0x65});
    CHECK("au-key-flag", (au.flags & xnc::AuFlags::kAuFlagKey) != 0);
    CHECK("au-payload-immutable-shared",
          au.annexb != nullptr && au.annexb->size() == 5 &&
              au.annexb->data()[4] == 0x65);
  }
  { // M2 Task 1: the PURE lease state machine (FREE -> CONVERTING ->
    // SUBMITTED -> RETIRED -> FREE, plan ruling 1 - constructible without
    // any D3D type). The first three checks are the plan's binding test
    // vector; the rest pin the remaining transitions.
    xnc::SurfaceLeaseModel m(3);
    auto a = m.Acquire(); auto b = m.Acquire(); auto c = m.Acquire();
    CHECK("pool-bounded", a && b && c && !m.Acquire());
    a->Submit(10);
    CHECK("submitted-not-reusable", !m.ReleaseFree(a->index()));
    CHECK("output-releases", m.Complete(10) && m.Acquire());
    // Complete parks the slot in RETIRED (observable); the NEXT Acquire
    // sweeps retired slots back to FREE before allocating (that sweep is
    // what makes output-releases hold).
    xnc::SurfaceLeaseModel m2(3);
    CHECK("lease-slot-count", m2.slot_count() == 3);
    auto l0 = m2.Acquire();
    CHECK("lease-acquire-converting",
          l0 && m2.State(l0->index()) == xnc::SurfaceLeaseState::kConverting);
    CHECK("lease-submit", l0->Submit(77));
    CHECK("lease-submitted-state",
          m2.State(l0->index()) == xnc::SurfaceLeaseState::kSubmitted);
    CHECK("lease-complete-retires",
          m2.Complete(77) &&
              m2.State(l0->index()) == xnc::SurfaceLeaseState::kRetired);
    CHECK("lease-retired-counted-live",
          m2.LiveLeases() == 1 && m2.RetiredCount() == 1 && m2.FreeCount() == 2);
    CHECK("lease-explicit-sweep", m2.FreeRetired() == 1);
    CHECK("lease-swept-free",
          m2.LiveLeases() == 0 && m2.FreeCount() == 3);
    CHECK("lease-complete-unknown-id", !m2.Complete(9999));
    // ReleaseFree: the CONVERTING abandon path; every other state rejects.
    xnc::SurfaceLeaseModel m3(3);
    auto r0 = m3.Acquire();
    CHECK("lease-release-free", m3.ReleaseFree(r0->index()) && m3.FreeCount() == 3);
    CHECK("lease-release-free-rejected", !m3.ReleaseFree(r0->index()));
    auto r1 = m3.Acquire();
    CHECK("lease-submit-ok", r1->Submit(1));
    CHECK("lease-release-submitted-rejected", !m3.ReleaseFree(r1->index()));
    CHECK("lease-double-submit-rejected", !r1->Submit(2));
    // Submit ids stay unique among SUBMITTED slots (Complete is by id).
    auto r2 = m3.Acquire();
    CHECK("lease-duplicate-submit-id-rejected", !r2->Submit(1));
    // RetireAll: bulk CONVERTING/SUBMITTED -> RETIRED (flush/teardown), then
    // the sweep frees them; exhaustion stays bounded at slot_count.
    CHECK("lease-retireall-count", m3.RetireAll() == 2);
    CHECK("lease-retireall-states",
          m3.RetiredCount() == 2 && m3.LiveLeases() == 2);
    CHECK("lease-retireall-idempotent", m3.RetireAll() == 0);
    CHECK("lease-retireall-sweep-frees",
          m3.FreeRetired() == 2 && m3.LiveLeases() == 0 && m3.FreeCount() == 3 &&
              m3.Acquire() != nullptr);
  }
  { // M2 Task 1: the D3D wrappers - LatestSurface (owned persistent BGRA
    // "latest complete desktop") + the 3-slot NV12 Nv12SurfacePool over the
    // same lease model. Device: hardware -> WARP fallback with BGRA+VIDEO
    // flags (RDP/WARP-safe, the dxgi_capture.cpp pattern). XNC_D3D_DEBUG=1
    // adds D3D11_CREATE_DEVICE_DEBUG and FAILS on any ERROR/CORRUPTION D3D
    // message observed during these surface tests (ruling 3); when the debug
    // layer is not installed those assertions are loudly SKIPPED (SELFTEST
    // NOTE), never silently passed. Zero live leases after teardown is
    // asserted either way. No Map()/readback: GPU stays GPU.
    bool want_debug = false;
    {
      char dbuf[8];
      const DWORD dlen =
          GetEnvironmentVariableA("XNC_D3D_DEBUG", dbuf, sizeof(dbuf));
      want_debug = dlen > 0 && dlen < sizeof(dbuf) && dbuf[0] != '0';
    }
    UINT create_flags =
        D3D11_CREATE_DEVICE_VIDEO_SUPPORT | D3D11_CREATE_DEVICE_BGRA_SUPPORT;
    if (want_debug) create_flags |= D3D11_CREATE_DEVICE_DEBUG;
    Microsoft::WRL::ComPtr<ID3D11Device> dev;
    Microsoft::WRL::ComPtr<ID3D11DeviceContext> dev_ctx;
    D3D_FEATURE_LEVEL fl{};
    const char* via = "hardware";
    auto try_create = [&dev, &dev_ctx, &fl](UINT f, D3D_DRIVER_TYPE dt) {
      dev.Reset();
      dev_ctx.Reset();
      return D3D11CreateDevice(nullptr, dt, nullptr, f, nullptr, 0,
                               D3D11_SDK_VERSION, &dev, &fl, &dev_ctx);
    };
    HRESULT hr = try_create(create_flags, D3D_DRIVER_TYPE_HARDWARE);
    if (FAILED(hr)) {
      via = "warp";
      hr = try_create(create_flags, D3D_DRIVER_TYPE_WARP);
    }
    bool debug_layer = want_debug && SUCCEEDED(hr);
    if (want_debug && !debug_layer) {
      std::printf(
          "SELFTEST NOTE: XNC_D3D_DEBUG=1 but the D3D11 debug layer is not "
          "installed (debug device creation failed) - D3D debug assertions "
          "SKIPPED\n");
      create_flags &= ~D3D11_CREATE_DEVICE_DEBUG;
      via = "hardware";
      hr = try_create(create_flags, D3D_DRIVER_TYPE_HARDWARE);
      if (FAILED(hr)) {
        via = "warp";
        hr = try_create(create_flags, D3D_DRIVER_TYPE_WARP);
      }
    }
    CHECK("gpu-surface-device", SUCCEEDED(hr));
    if (SUCCEEDED(hr)) {
      std::printf("SELFTEST NOTE: gpu-surface device driver=%s debug_layer=%d\n",
                  via, debug_layer ? 1 : 0);
      Microsoft::WRL::ComPtr<ID3D11InfoQueue> iq;
      if (debug_layer && SUCCEEDED(dev.As(&iq)))
        iq->ClearStoredMessages();  // only surface-test messages count
      // ---- LatestSurface ----
      xnc::LatestSurface latest;
      std::string err;
      const uint32_t lw = 64, lh = 48;
      CHECK("latest-init", latest.Init(dev.Get(), lw, lh, &err));
      CHECK("latest-dims", latest.width() == lw && latest.height() == lh);
      CHECK("latest-invalid-before-copy", !latest.valid());
      xnc::FrameIdentity sid{};
      ID3D11Texture2D* snap = nullptr;
      CHECK("latest-snapshot-before-copy-fails", !latest.Snapshot(&sid, &snap));
      // Source BGRA texture with known content (DEFAULT usage, like the
      // duplication textures CopyFrom will see in production).
      D3D11_TEXTURE2D_DESC sd{};
      sd.Width = lw;
      sd.Height = lh;
      sd.MipLevels = 1;
      sd.ArraySize = 1;
      sd.SampleDesc.Count = 1;
      sd.Format = DXGI_FORMAT_B8G8R8A8_UNORM;
      sd.Usage = D3D11_USAGE_DEFAULT;
      std::vector<uint8_t> px(static_cast<size_t>(lw) * lh * 4);
      for (size_t i = 0; i + 3 < px.size(); i += 4) {
        px[i] = 0x11;
        px[i + 1] = 0x22;
        px[i + 2] = 0x33;
        px[i + 3] = 0xFF;
      }
      D3D11_SUBRESOURCE_DATA srd{px.data(), lw * 4, 0};
      Microsoft::WRL::ComPtr<ID3D11Texture2D> src;
      CHECK("latest-src-create",
            SUCCEEDED(dev->CreateTexture2D(&sd, &srd, &src)));
      const xnc::FrameIdentity id1{2, 1, 30, 1, 1000, 1100};
      CHECK("latest-copy", latest.CopyFrom(dev_ctx.Get(), src.Get(), id1, &err));
      CHECK("latest-valid-after-copy", latest.valid());
      ID3D11Texture2D* snap_a = nullptr;
      CHECK("latest-snapshot-ok",
            latest.Snapshot(&sid, &snap_a) && snap_a != nullptr);
      CHECK("latest-snapshot-identity",
            sid.capture_epoch == 2 && sid.content_id == 30 &&
                sid.encode_seq == 1 && sid.source_mono_us == 1000 &&
                sid.present_mono_us == 1100);
      D3D11_TEXTURE2D_DESC gd{};
      if (snap_a) snap_a->GetDesc(&gd);
      CHECK("latest-snapshot-desc",
            snap_a && gd.Width == lw && gd.Height == lh &&
                gd.Format == DXGI_FORMAT_B8G8R8A8_UNORM);
      // Snapshot is AddRef'd: two live snapshots of the same image.
      xnc::FrameIdentity sid_b{};
      ID3D11Texture2D* snap_b = nullptr;
      CHECK("latest-snapshot-stable",
            latest.Snapshot(&sid_b, &snap_b) && snap_b == snap_a &&
                sid_b.encode_seq == 1);
      if (snap_a) snap_a->Release();
      if (snap_b) snap_b->Release();
      // Desc mismatches are rejected before CopyResource (the debug layer
      // would flag an invalid copy).
      D3D11_TEXTURE2D_DESC sd2 = sd;
      sd2.Width = 32;
      sd2.Height = 24;
      Microsoft::WRL::ComPtr<ID3D11Texture2D> src_small;
      if (SUCCEEDED(dev->CreateTexture2D(&sd2, nullptr, &src_small)))
        CHECK("latest-copy-dims-mismatch",
              !latest.CopyFrom(dev_ctx.Get(), src_small.Get(), id1, &err));
      // Invalidate -> Snapshot fails until a new CopyFrom completes (ruling 2).
      latest.Invalidate();
      ID3D11Texture2D* snap_c = nullptr;
      CHECK("latest-invalidated-snapshot-fails",
            !latest.valid() && !latest.Snapshot(&sid, &snap_c));
      const xnc::FrameIdentity id2{2, 1, 31, 2, 2000, 2100};
      CHECK("latest-recopy-after-invalidate",
            latest.CopyFrom(dev_ctx.Get(), src.Get(), id2, &err));
      CHECK("latest-new-identity",
            latest.Snapshot(&sid, &snap_c) && snap_c != nullptr &&
                sid.encode_seq == 2 && sid.content_id == 31);
      if (snap_c) snap_c->Release();
      // ---- Nv12SurfacePool ----
      xnc::Nv12SurfacePool pool;
      CHECK("pool-init", pool.Init(dev.Get(), lw, lh, &err));
      CHECK("pool-dims", pool.width() == lw && pool.height() == lh);
      CHECK("pool-starts-free",
            pool.FreeCount() == xnc::Nv12SurfacePool::kSlotCount &&
                pool.LiveLeases() == 0);
      CHECK("pool-odd-dims-rejected", [&] {
        xnc::Nv12SurfacePool odd;
        return !odd.Init(dev.Get(), 33, lh, &err);
      }());
      auto* p0 = pool.Acquire();
      auto* p1 = pool.Acquire();
      auto* p2 = pool.Acquire();
      CHECK("pool-three-leases", p0 && p1 && p2 && pool.LiveLeases() == 3);
      CHECK("pool-fourth-rejected", pool.Acquire() == nullptr);
      ID3D11Texture2D* t0 = p0 ? p0->texture() : nullptr;
      ID3D11Texture2D* t1 = p1 ? p1->texture() : nullptr;
      ID3D11Texture2D* t2 = p2 ? p2->texture() : nullptr;
      D3D11_TEXTURE2D_DESC pd{};
      if (t0) t0->GetDesc(&pd);
      CHECK("pool-slot-textures",
            t0 != nullptr && t1 != nullptr && t2 != nullptr && t0 != t1 &&
                t0 != t2 && t1 != t2 && pd.Width == lw && pd.Height == lh &&
                pd.Format == DXGI_FORMAT_NV12);
      CHECK("pool-lease-converting",
            p0->state() == xnc::SurfaceLeaseState::kConverting &&
                pool.State(p0->index()) == xnc::SurfaceLeaseState::kConverting);
      // Submit -> only Complete/RetireAll may move the slot on.
      CHECK("pool-submit", p0->Submit(1000));
      CHECK("pool-submit-state",
            p0->state() == xnc::SurfaceLeaseState::kSubmitted);
      CHECK("pool-release-submitted-rejected", !p0->Release());
      CHECK("pool-double-submit-rejected", !p0->Submit(1001));
      CHECK("pool-duplicate-id-rejected", !p1->Submit(1000));
      CHECK("pool-submit-second", p1->Submit(1001));
      // Abandon: a CONVERTING slot releases without ever being submitted.
      CHECK("pool-abandon-release",
            p2->Release() && pool.FreeCount() == 1);
      auto* p3 = pool.Acquire();
      CHECK("pool-reacquire-after-abandon",
            p3 != nullptr && p3->index() == p2->index() &&
                pool.Acquire() == nullptr);
      // Output completion: Complete retires; the next Acquire sweeps to FREE.
      CHECK("pool-complete",
            pool.Complete(1000) &&
                pool.State(p0->index()) == xnc::SurfaceLeaseState::kRetired);
      auto* p4 = pool.Acquire();
      CHECK("pool-reacquire-after-complete",
            p4 != nullptr &&
                pool.State(p0->index()) == xnc::SurfaceLeaseState::kConverting);
      CHECK("pool-complete-unknown-id", !pool.Complete(4242));
      // Teardown (ruling 3): retire everything outstanding, sweep, and the
      // pool must report zero live leases.
      CHECK("pool-retireall", pool.RetireAll() == 3 && pool.LiveLeases() == 3);
      CHECK("pool-retireall-sweep", pool.FreeRetired() == 3);
      CHECK("pool-zero-live-after-teardown",
            pool.LiveLeases() == 0 &&
                pool.FreeCount() == xnc::Nv12SurfacePool::kSlotCount);
      // ---- M2 Task 2: the surface seam over the REAL LatestSurface ----
      // A minimal ICaptureSurface backend (the exact Init / CopyFrom /
      // Snapshot wiring DXGI/GDI implement, minus their duplication
      // plumbing): the caller-owned surface is Init'ed by the backend, the
      // full-resource copy stamps the GIVEN identity, and Snapshot leases
      // the AddRef'd texture + that identity back - the concrete
      // CapturedSurface shape from capture.h.
      class DeviceSurfaceCapture final : public xnc::ICaptureSurface {
       public:
        DeviceSurfaceCapture(ID3D11Device* dev, ID3D11DeviceContext* ctx,
                             ID3D11Texture2D* src, uint32_t w, uint32_t h)
            : dev_(dev), ctx_(ctx), src_(src), w_(w), h_(h) {}
        xnc::CaptureStatus AcquireSurface(xnc::LatestSurface& latest,
                                          uint32_t timeout_ms,
                                          xnc::FrameIdentity* id,
                                          std::string* e) override {
          (void)timeout_ms;
          if (e) e->clear();
          // Backend-owned Init: an uninitialized or resized caller surface
          // is (re)created on the backend's device, exactly like
          // DxgiCapture::EnsureLatestSurface.
          if (latest.width() != w_ || latest.height() != h_) {
            if (!latest.Init(dev_, w_, h_, e))
              return xnc::CaptureStatus::kFatal;
          }
          xnc::FrameIdentity stamp = id != nullptr ? *id : xnc::FrameIdentity{};
          stamp.source_mono_us = xnc::NowMonoUs();
          stamp.encode_seq = 0;
          stamp.present_mono_us = 0;
          if (!latest.CopyFrom(ctx_, src_, stamp, e))
            return xnc::CaptureStatus::kFatal;
          if (id != nullptr) *id = stamp;
          return xnc::CaptureStatus::kFrame;
        }

       private:
        ID3D11Device* dev_;
        ID3D11DeviceContext* ctx_;
        ID3D11Texture2D* src_;
        uint32_t w_, h_;
      };
      DeviceSurfaceCapture dsc(dev.Get(), dev_ctx.Get(), src.Get(), lw, lh);
      xnc::LatestSurface owned;                    // caller-owned surface
      xnc::FrameIdentity seam_id{7, 4, 901, 0, 0, 0};  // caller-assigned
      std::string serr;
      CHECK("surface-seam-frame",
            dsc.AcquireSurface(owned, 16, &seam_id, &serr) ==
                xnc::CaptureStatus::kFrame);
      xnc::CapturedSurface got;  // the plan's conceptual return, filled
      CHECK("surface-seam-snapshot",
            owned.Snapshot(&got.id, &got.texture) && got.texture != nullptr);
      got.changed = true;  // kFrame - the changed half of CapturedSurface
      CHECK("surface-seam-identity",
            got.id.capture_epoch == 7 && got.id.codec_epoch == 4 &&
                got.id.content_id == 901 && got.id.source_mono_us != 0 &&
                got.id.encode_seq == 0 && got.id.present_mono_us == 0);
      D3D11_TEXTURE2D_DESC gotd{};
      if (got.texture) got.texture->GetDesc(&gotd);
      CHECK("surface-seam-texture",
            got.texture != nullptr && gotd.Width == lw && gotd.Height == lh &&
                gotd.Format == DXGI_FORMAT_B8G8R8A8_UNORM);
      if (got.texture) got.texture->Release();
      // A second acquire re-stamps: the snapshot identity follows the copy.
      xnc::FrameIdentity sid2{7, 4, 902, 0, 0, 0};
      CHECK("surface-seam-second-frame",
            dsc.AcquireSurface(owned, 16, &sid2, &serr) ==
                xnc::CaptureStatus::kFrame);
      xnc::FrameIdentity leased2{};
      ID3D11Texture2D* leased2_tex = nullptr;
      CHECK("surface-seam-restamp",
            owned.Snapshot(&leased2, &leased2_tex) && leased2_tex != nullptr &&
                leased2.content_id == 902 && leased2.source_mono_us != 0);
      if (leased2_tex) leased2_tex->Release();
      // Ruling 3 verdict: no ERROR/CORRUPTION D3D debug messages during the
      // surface tests (only when the debug layer actually came up).
      if (debug_layer && iq) {
        bool bad = false;
        const UINT64 nmsgs = iq->GetNumStoredMessages();
        for (UINT64 i = 0; i < nmsgs; ++i) {
          SIZE_T len = 0;
          if (FAILED(iq->GetMessage(i, nullptr, &len)) || len == 0) continue;
          std::vector<char> mbuf(len);
          D3D11_MESSAGE* msg = reinterpret_cast<D3D11_MESSAGE*>(mbuf.data());
          if (FAILED(iq->GetMessage(i, msg, &len))) continue;
          if (msg->Severity == D3D11_MESSAGE_SEVERITY_ERROR ||
              msg->Severity == D3D11_MESSAGE_SEVERITY_CORRUPTION) {
            bad = true;
            std::printf("SELFTEST NOTE: d3d sev=%u id=%u desc=%s\n",
                        static_cast<unsigned>(msg->Severity),
                        static_cast<unsigned>(msg->ID),
                        msg->pDescription ? msg->pDescription : "");
          }
        }
        CHECK("gpu-surface-no-d3d-errors", !bad);
      }
    }
  }
  { // M2 Task 3: the PURE session vocabulary - SubmitResult/ShutdownMode/
    // EncoderOutput field round-trip, the OutputIdentityTracker's 1:1
    // output<->input mapping rules (the three encoder_identity_mismatch
    // failure modes), and the §8.4 color rule Nv12ColorForSize.
    xnc::SubmitResult ok = xnc::SubmitResult::kOk;
    CHECK("submit-result-enum", ok == xnc::SubmitResult::kOk &&
                                    xnc::SubmitResult::kRejected !=
                                        xnc::SubmitResult::kIdentityFault);
    xnc::ShutdownMode drain = xnc::ShutdownMode::kDrain;
    CHECK("shutdown-mode-enum",
          drain == xnc::ShutdownMode::kDrain &&
              xnc::ShutdownMode::kImmediate != xnc::ShutdownMode::kDrain);
    xnc::EncoderOutput eo;
    eo.id = xnc::FrameIdentity{9, 8, 7, 6, 5, 4};
    eo.submit_id = 6;
    eo.slot = 2;
    eo.sample_time = 333333;
    eo.key = true;
    eo.au = {0, 0, 0, 1, 0x65};
    CHECK("encoder-output-fields",
          eo.id.encode_seq == 6 && eo.submit_id == 6 && eo.slot == 2 &&
              eo.sample_time == 333333 && eo.key &&
              xnc::NalHasType(eo.au.data(), eo.au.size(), 5));
    // Tracker: register strictly increasing times, consume by EXACT time.
    xnc::OutputIdentityTracker t;
    xnc::OutputIdentityRecord r1{};
    r1.id = xnc::FrameIdentity{1, 1, 10, 1, 0, 0};
    r1.submit_id = 1;
    r1.slot = 0;
    xnc::OutputIdentityRecord r2 = r1;
    r2.id.encode_seq = 2;
    r2.submit_id = 2;
    r2.slot = 1;
    CHECK("tracker-register", t.Register(333333, r1) && t.Register(666666, r2));
    CHECK("tracker-register-not-increasing",
          !t.Register(666666, r1) && !t.Register(100000, r1));
    CHECK("tracker-register-increasing-ok", t.Register(666667, r1));
    xnc::OutputIdentityRecord got{};
    CHECK("tracker-consume-ok",
          t.Consume(true, 666666, &got) == xnc::OutputConsume::kOk &&
              got.id.encode_seq == 2 && got.submit_id == 2 && got.slot == 1);
    CHECK("tracker-consume-duplicated",
          t.Consume(true, 666666, &got) ==
          xnc::OutputConsume::kTimeDuplicated);
    CHECK("tracker-consume-unknown",
          t.Consume(true, 999999, &got) == xnc::OutputConsume::kTimeUnknown);
    CHECK("tracker-consume-missing-time",
          t.Consume(false, 333333, &got) == xnc::OutputConsume::kTimeMissing);
    CHECK("tracker-pending-accounting",
          t.PendingCount() == 2 && t.consumed_count() == 1 &&
              t.last_registered_time() == 666667);
    CHECK("tracker-rollback-newest",
          (t.RollbackNewest(), t.last_registered_time() == 666666 &&
                                   t.PendingCount() == 1));
    CHECK("tracker-consume-after-rollback",
          t.Consume(true, 666667, &got) == xnc::OutputConsume::kTimeUnknown);
    CHECK("tracker-clear", (t.Clear(), t.PendingCount() == 0));
    // Bounded memory (review fix): a healthy register->consume stream
    // prunes the consumed FRONT, so retained stays tiny however many
    // submissions flow; a consumed entry behind the oldest PENDING record
    // is retained until that front resolves (reorder tolerance); and the
    // retained cap is a HARD failure (>= kMaxTracked outputs owed).
    {
      xnc::OutputIdentityTracker big;
      xnc::OutputIdentityRecord br{};
      size_t max_retained = 0;
      bool flow_ok = true;
      for (int64_t i = 1; i <= 4000; ++i) {
        br.id.encode_seq = static_cast<uint64_t>(i);
        br.submit_id = static_cast<uint64_t>(i);
        if (!big.Register(i * 33333, br)) {
          flow_ok = false;
          break;
        }
        xnc::OutputIdentityRecord got{};
        if (big.Consume(true, i * 33333, &got) != xnc::OutputConsume::kOk ||
            got.submit_id != static_cast<uint64_t>(i)) {
          flow_ok = false;
          break;
        }
        if (big.retained() > max_retained) max_retained = big.retained();
      }
      CHECK("tracker-bounded-pruning",
            flow_ok && big.consumed_count() == 4000 && max_retained <= 2 &&
                big.PendingCount() == 0);
      // Reorder: consuming a NON-front entry retains it until the front
      // resolves; then the whole consumed prefix prunes at once.
      xnc::OutputIdentityTracker ro;
      for (int64_t i = 1; i <= 3; ++i) {
        br.id.encode_seq = static_cast<uint64_t>(i);
        br.submit_id = static_cast<uint64_t>(i);
        ro.Register(i * 33333, br);
      }
      xnc::OutputIdentityRecord got{};
      CHECK("tracker-reorder-keeps-entries",
            ro.Consume(true, 2 * 33333, &got) == xnc::OutputConsume::kOk &&
                ro.retained() == 3 && ro.PendingCount() == 2);
      CHECK("tracker-reorder-prunes-prefix",
            ro.Consume(true, 33333, &got) == xnc::OutputConsume::kOk &&
                ro.retained() == 1 && ro.PendingCount() == 1);
      // Cap: kMaxTracked retained entries is the ceiling; the next
      // Register is a hard failure and stays one until something is
      // consumed (the cap follows RETAINED, not lifetime submissions).
      xnc::OutputIdentityTracker capped;
      bool failed_at_cap = false;
      for (size_t i = 1; i <= xnc::OutputIdentityTracker::kMaxTracked + 2;
           ++i) {
        br.id.encode_seq = i;
        br.submit_id = i;
        if (!capped.Register(static_cast<int64_t>(i) * 33333, br)) {
          failed_at_cap = true;
          break;
        }
      }
      CHECK("tracker-cap-hard-failure",
            failed_at_cap &&
                capped.retained() ==
                    xnc::OutputIdentityTracker::kMaxTracked &&
                capped.PendingCount() ==
                    xnc::OutputIdentityTracker::kMaxTracked);
      CHECK("tracker-cap-recovers",
            capped.Consume(true, 33333, &got) == xnc::OutputConsume::kOk &&
                capped.retained() ==
                    xnc::OutputIdentityTracker::kMaxTracked - 1 &&
                capped.Register(
                    (static_cast<int64_t>(
                         xnc::OutputIdentityTracker::kMaxTracked) +
                     1) *
                        33333,
                    br));
    }
    // §8.4 color rule: BT.709 limited at 720p+, BT.601 limited below.
    const xnc::Nv12ColorConfig c720 = xnc::Nv12ColorForSize(720);
    const xnc::Nv12ColorConfig c1080 = xnc::Nv12ColorForSize(1080);
    const xnc::Nv12ColorConfig c480 = xnc::Nv12ColorForSize(480);
    const xnc::Nv12ColorConfig c719 = xnc::Nv12ColorForSize(719);
    CHECK("color-rule-709-at-720p",
          c720.bt709 && c720.matrix == 1 && c720.primaries == 2 &&
              c720.transfer == 5 && c720.nominal_range == 1);
    CHECK("color-rule-709-at-1080p",
          c1080.bt709 && c1080.matrix == 1 && c1080.primaries == 2);
    CHECK("color-rule-601-below-720p",
          !c480.bt709 && c480.matrix == 2 && c480.primaries == 5 &&
              c480.transfer == 5 && c480.nominal_range == 1);
    CHECK("color-rule-boundary-719", !c719.bt709 && c719.matrix == 2);
  }
  { // M2 Task 3: D3D11 color agreement on the ENCODER side (spec §8.4):
    // the negotiated input type must carry Nv12ColorForSize's matrix, and
    // the encoded SPS VUI - when the backend writes color metadata - must
    // agree with the same rule. Measured on this machine: the MS software
    // H.264 encoder ACCEPTS the color attributes on its input type (the
    // matrix reads back exactly) but leaves VUI color UNSPECIFIED (its
    // AVEncVideo*Color* codec APIs are E_NOTIMPL), so on the software
    // rung the input-type agreement is the proof and the VUI absence is
    // reported with a loud NOTE (unspecified color in SPS + our
    // resolution-follows-standard-practice matrix selection is the
    // coherent combination; a hardware rung writing VUI must match).
    for (uint32_t rep = 0; rep < 2; ++rep) {
      const uint32_t w = rep == 0 ? 1280 : 640;
      const uint32_t h = rep == 0 ? 720 : 480;
      const xnc::Nv12ColorConfig rule = xnc::Nv12ColorForSize(h);
      xnc::MfSoftEncoder enc;
      std::string err;
      const bool init_ok = enc.Init(w, h, 30, 2000000, &err);
      if (!init_ok)
        std::printf("SELFTEST NOTE: color-init(%u) err=%s\n", h, err.c_str());
      CHECK(rep == 0 ? "color-encoder-init-720" : "color-encoder-init-480",
            init_ok);
      if (!init_ok) continue;
      char name_matrix[48], name_vui[48];
      std::snprintf(name_matrix, sizeof(name_matrix), "color-input-matrix-%u",
                    h);
      std::snprintf(name_vui, sizeof(name_vui), "color-sps-vui-%u", h);
      CHECK(name_matrix, enc.negotiated_input_matrix() == rule.matrix);
      // Feed synthetic bars until the cold-start IDR surfaces, then parse
      // its SPS VUI (EncodeReferenceIdr's feed shape, inlined so the
      // negotiated matrix above comes from THIS Init).
      SyntheticBars bars(w, h);
      enc.ForceNextIdr("color-selftest");
      SpsVui vui;
      bool saw_idr = false;
      std::vector<std::vector<uint8_t>> aus;
      for (uint32_t i = 0;
           i < xnc::kEncoderLookaheadFrames * 2 + 4 && !saw_idr; ++i) {
        aus.clear();
        if (!enc.Encode(bars.Frame(i % 3), bars.Bytes(), aus, &err)) break;
        for (const auto& au : aus) {
          if (xnc::NalHasType(au.data(), au.size(), 5)) {
            vui = ParseSpsVui(au.data(), au.size());
            saw_idr = true;
            break;
          }
        }
      }
      CHECK(rep == 0 ? "color-idr-encoded-720" : "color-idr-encoded-480",
            saw_idr && vui.sps_found);
      if (saw_idr) {
        if (vui.vui_present && vui.video_signal_present &&
            vui.colour_description == 1) {
          CHECK(name_vui, VuiMatchesColorRule(vui, rule));
          std::printf("SELFTEST NOTE: color-sps-vui-%u cp=%d tc=%d mc=%d "
                      "range=%d (matches rule bt709=%d)\n",
                      h, vui.cp, vui.tc, vui.mc, vui.full_range,
                      rule.bt709 ? 1 : 0);
        } else {
          // Loud NOTE, never a silent pass: this backend leaves color
          // unspecified in the bitstream; agreement is proven at the
          // input-type level above.
          std::printf("SELFTEST NOTE: color-sps-vui-%u ABSENT (backend does "
                      "not signal color metadata; agreement proven via the "
                      "negotiated input matrix=%u)\n",
                      h, enc.negotiated_input_matrix());
          CHECK(name_vui, enc.negotiated_input_matrix() == rule.matrix);
        }
      }
    }
  }
  { // M2 Task 3: the encoder SESSION interface + both real rungs over the
    // Task 1 NV12 pool. Device hardware->WARP (the gpu-surface pattern).
    //   (a) fake-session contract: a 17-submission output delay (the CPU
    //       MFT's lookahead shape) must return every output paired to ITS
    //       input - identity AND lease token (submit-id/slot) - with no
    //       cross-wiring, leases completed exactly once;
    //   (b) the GPU-shape fake: leases held until output bound the pool at
    //       3 in-flight (spec §9) and Shutdown completes them all;
    //   (c) the REAL CPU session (MfCpuEncoder over MfSoftEncoder): the
    //       eight-frame pixel probe - distinct content ids, forced IDRs at
    //       inputs 0 and 7, 1:1 time-keyed mapping, decoded luma
    //       signatures vs per-input references computed through the SAME
    //       session type, input discrimination;
    //   (d) the REAL GPU session (MfGpuEncoder): D3D11-aware hardware
    //       ladder + startup probe; skip-clean with a loud NOTE when this
    //       machine's RDP session cannot complete hardware encode work
    //       (measured: QSV negotiates after MF_TRANSFORM_ASYNC_UNLOCK but
    //       never emits under RDP).
    UINT create_flags =
        D3D11_CREATE_DEVICE_VIDEO_SUPPORT | D3D11_CREATE_DEVICE_BGRA_SUPPORT;
    Microsoft::WRL::ComPtr<ID3D11Device> dev;
    Microsoft::WRL::ComPtr<ID3D11DeviceContext> dev_ctx;
    D3D_FEATURE_LEVEL fl{};
    const char* via = "hardware";
    auto try_create = [&dev, &dev_ctx, &fl](UINT f, D3D_DRIVER_TYPE dt) {
      dev.Reset();
      dev_ctx.Reset();
      return D3D11CreateDevice(nullptr, dt, nullptr, f, nullptr, 0,
                               D3D11_SDK_VERSION, &dev, &fl, &dev_ctx);
    };
    HRESULT hr = try_create(create_flags, D3D_DRIVER_TYPE_HARDWARE);
    if (FAILED(hr)) {
      via = "warp";
      hr = try_create(create_flags, D3D_DRIVER_TYPE_WARP);
    }
    CHECK("gpu-session-device", SUCCEEDED(hr));
    if (SUCCEEDED(hr)) {
      std::printf("SELFTEST NOTE: gpu-session device driver=%s\n", via);
      const uint32_t sw = 640, sh = 480;  // even; §8.4 BT.601 regime
      // Two deterministic NV12 contents (A/B) for the pixel probe: luma
      // gradients that differ everywhere (discriminating inputs).
      std::vector<uint8_t> nv12_a(static_cast<size_t>(sw) * sh * 3 / 2);
      std::vector<uint8_t> nv12_b(static_cast<size_t>(sw) * sh * 3 / 2);
      for (uint32_t y = 0; y < sh; ++y)
        for (uint32_t x = 0; x < sw; ++x) {
          const size_t i = static_cast<size_t>(y) * sw + x;
          nv12_a[i] = static_cast<uint8_t>((x + y) & 0xFF);
          nv12_b[i] = static_cast<uint8_t>((255 - x + y) & 0xFF);
        }
      for (size_t i = static_cast<size_t>(sw) * sh;
           i < nv12_a.size(); i += 2) {
        nv12_a[i] = 128; nv12_a[i + 1] = 128;
        nv12_b[i] = 96; nv12_b[i + 1] = 160;
      }
      auto upload = [&](ID3D11Texture2D* tex, const std::vector<uint8_t>& nv12) {
        dev_ctx->UpdateSubresource(tex, 0, nullptr, nv12.data(), sw, 0);
      };
      // ---- (a) FakeDelaySession: outputs lag submissions by 17 ----
      class FakeDelaySession final : public xnc::IEncoderSession {
       public:
        explicit FakeDelaySession(xnc::Nv12SurfacePool* pool, size_t delay)
            : pool_(pool), delay_(delay) {}
        xnc::SubmitResult Submit(const xnc::FrameIdentity& id,
                                 xnc::SurfaceLease&& lease,
                                 bool force_idr) override {
          (void)force_idr;
          ++submits_;
          const uint64_t sid = id.encode_seq;
          const size_t slot = lease.index();
          if (!lease.Submit(sid)) {
            lease.Release();
            return xnc::SubmitResult::kRejected;
          }
          // CPU shape: the input is consumed at Submit (bytes secured) -
          // the lease completes now; only the OUTPUT identity lags.
          if (!pool_->Complete(sid)) ++double_completes_;
          ++completes_;
          Rec p{};
          p.id = id;
          p.sid = sid;
          p.slot = slot;
          fifo_.push_back(p);
          if (fifo_.size() > delay_) {
            ready_.push_back(fifo_.front());
            fifo_.erase(fifo_.begin());
          }
          return xnc::SubmitResult::kOk;
        }
        bool TakeOutput(xnc::EncoderOutput* out, uint32_t) override {
          if (out == nullptr || ready_.empty()) return false;
          const Rec& p = ready_.front();
          out->id = p.id;
          out->submit_id = p.sid;
          out->slot = p.slot;
          out->sample_time = static_cast<int64_t>(p.sid) * 33333;
          out->key = true;
          out->au = {0, 0, 0, 1, 0x65};
          ready_.erase(ready_.begin());
          ++returned_;
          return true;
        }
        bool Reconfigure(uint32_t bitrate, uint32_t fps) override {
          reconf_bitrate_ = bitrate;
          reconf_fps_ = fps;
          return true;
        }
        void Shutdown(xnc::ShutdownMode) override {
          // CPU shape completed every lease at consumption already.
          shutdown_pending_ = fifo_.size();
          fifo_.clear();
          ready_.clear();
        }
        size_t submits_ = 0, completes_ = 0, double_completes_ = 0;
        size_t returned_ = 0, shutdown_pending_ = 0;
        uint32_t reconf_bitrate_ = 0, reconf_fps_ = 0;

       private:
        struct Rec {
          xnc::FrameIdentity id{};
          uint64_t sid = 0;
          size_t slot = 0;
        };
        xnc::Nv12SurfacePool* pool_;
        size_t delay_;
        std::vector<Rec> fifo_, ready_;
      };
      xnc::Nv12SurfacePool pool;
      std::string err;
      CHECK("session-pool-init", pool.Init(dev.Get(), sw, sh, &err));
      constexpr size_t kFakeDelay = 17;
      constexpr size_t kFakeSubmits = 24;
      FakeDelaySession fake(&pool, kFakeDelay);
      std::vector<size_t> slots(kFakeSubmits, 999);
      bool pre_delay_true = true;  // TakeOutput must fail before input #18
      for (size_t i = 0; i < kFakeSubmits; ++i) {
        xnc::SurfaceLease* lease = pool.Acquire();
        CHECK("fake-lease-available", lease != nullptr);
        if (lease == nullptr) break;
        upload(lease->texture(), (i % 2) ? nv12_b : nv12_a);
        slots[i] = lease->index();
        const xnc::FrameIdentity id{1, 1, 100 + i, i + 1, 0, 0};
        const xnc::SubmitResult r =
            fake.Submit(id, std::move(*lease), i == 0);
        CHECK("fake-submit-ok", r == xnc::SubmitResult::kOk);
        xnc::EncoderOutput out;
        if (i + 1 <= kFakeDelay && fake.TakeOutput(&out, 1))
          pre_delay_true = false;
      }
      CHECK("fake-no-output-before-delay",
            pre_delay_true && fake.submits_ == kFakeSubmits);
      CHECK("fake-completes-exactly-once",
            fake.completes_ == kFakeSubmits && fake.double_completes_ == 0);
      // Every returned output pairs to ITS input: identity + lease token.
      bool pairing_ok = true;
      size_t expect = kFakeSubmits - kFakeDelay;  // 7 by now
      xnc::EncoderOutput out;
      while (fake.TakeOutput(&out, 1)) {
        const size_t i = fake.returned_ - 1;  // FIFO: output j <-> input j
        if (out.id.encode_seq != i + 1 || out.id.content_id != 100 + i ||
            out.submit_id != i + 1 || out.slot != slots[i] || !out.key)
          pairing_ok = false;
      }
      CHECK("fake-identity-lease-pairing", pairing_ok);
      CHECK("fake-output-count", fake.returned_ == expect);
      CHECK("fake-reconfigure",
            fake.Reconfigure(1500000, 24) && fake.reconf_bitrate_ == 1500000 &&
                fake.reconf_fps_ == 24);
      fake.Shutdown(xnc::ShutdownMode::kDrain);
      pool.FreeRetired();  // sweep any last consumption-retired slot
      CHECK("fake-shutdown-zero-live",
            pool.LiveLeases() == 0 &&
                pool.FreeCount() == xnc::Nv12SurfacePool::kSlotCount);
      // ---- (b) FakeHoldSession: GPU shape (leases held until output) ----
      class FakeHoldSession final : public xnc::IEncoderSession {
       public:
        explicit FakeHoldSession(xnc::Nv12SurfacePool* pool,
                                 size_t delay)
            : pool_(pool), delay_(delay) {}
        xnc::SubmitResult Submit(const xnc::FrameIdentity& id,
                                 xnc::SurfaceLease&& lease,
                                 bool force_idr) override {
          (void)force_idr;
          if (!lease.Submit(id.encode_seq)) {
            lease.Release();
            return xnc::SubmitResult::kRejected;
          }
          Rec p{};
          p.id = id;
          p.sid = id.encode_seq;
          p.slot = lease.index();
          fifo_.push_back(p);
          ++submits_;
          return xnc::SubmitResult::kOk;
        }
        bool TakeOutput(xnc::EncoderOutput* out, uint32_t) override {
          if (out == nullptr || fifo_.size() <= delay_) return false;
          const Rec p = fifo_.front();
          fifo_.erase(fifo_.begin());
          if (!pool_->Complete(p.sid)) ++double_completes_;
          ++completes_;
          out->id = p.id;
          out->submit_id = p.sid;
          out->slot = p.slot;
          return true;
        }
        bool Reconfigure(uint32_t, uint32_t) override { return true; }
        void Shutdown(xnc::ShutdownMode) override {
          for (const auto& p : fifo_) {
            if (!pool_->Complete(p.sid)) ++double_completes_;
            ++completes_;
          }
          fifo_.clear();
        }
        size_t submits_ = 0, completes_ = 0, double_completes_ = 0;

       private:
        struct Rec {
          xnc::FrameIdentity id{};
          uint64_t sid = 0;
          size_t slot = 0;
        };
        xnc::Nv12SurfacePool* pool_;
        size_t delay_;
        std::vector<Rec> fifo_;
      };
      FakeHoldSession hold(&pool, kFakeDelay);
      size_t held = 0;
      for (; held < 6; ++held) {  // try 6; only 3 slots may be in flight
        xnc::SurfaceLease* lease = pool.Acquire();
        if (lease == nullptr) break;
        upload(lease->texture(), nv12_a);
        const xnc::FrameIdentity id{1, 1, 200 + held, 50 + held, 0, 0};
        CHECK("hold-submit-ok",
              hold.Submit(id, std::move(*lease), false) ==
              xnc::SubmitResult::kOk);
      }
      CHECK("hold-pool-bounded-at-three",
            held == 3 && pool.Acquire() == nullptr &&
                pool.LiveLeases() == 3);
      xnc::EncoderOutput hout;
      CHECK("hold-no-output", !hold.TakeOutput(&hout, 1));
      hold.Shutdown(xnc::ShutdownMode::kImmediate);
      CHECK("hold-shutdown-completes-all",
            hold.completes_ == 3 && hold.double_completes_ == 0);
      CHECK("hold-zero-live-after-shutdown",
            pool.FreeRetired() == 3 && pool.LiveLeases() == 0 &&
                pool.FreeCount() == xnc::Nv12SurfacePool::kSlotCount);
      CHECK("hold-complete-unknown-after",
            !pool.Complete(50));  // already retired: exactly-once evidence
      // ---- (c) the REAL eight-frame pixel probe over a session ----
      // Fixed 30-input sequence (deterministic; identical across the probe
      // and the reference run): inputs 0..7 alternate A/B with distinct
      // content ids and forced IDRs at 0 and 7; 9..30 continue the
      // alternation so the software rung's ~17-input warm-up surfaces the
      // first outputs (its lookahead is structural - see the NOTE below).
      struct ProbeOutcome {
        bool ok = false;
        int first_output_inputs = -1;
        uint32_t first_output_ms = 0;
        std::vector<xnc::EncoderOutput> outputs;
        uint64_t hash_a = 0, hash_b = 0;
        bool decoded_a = false, decoded_b = false;
        std::string err;
      };
      auto run_probe = [&](xnc::IEncoderSession& sess) {
        ProbeOutcome po;
        const ULONGLONG t0 = GetTickCount64();
        for (uint32_t i = 0; i < 30; ++i) {
          xnc::SurfaceLease* lease = pool.Acquire();
          if (lease == nullptr) {
            po.err = "probe: pool exhausted (lease leak?)";
            return po;
          }
          upload(lease->texture(), (i % 2) ? nv12_b : nv12_a);
          const xnc::FrameIdentity id{1, 1, 300 + i, i + 1, 0, 0};
          const bool force = (i == 0 || i == 7);
          const xnc::SubmitResult r = sess.Submit(id, std::move(*lease), force);
          if (r != xnc::SubmitResult::kOk) {
            po.err = "probe: submit rejected at input " + std::to_string(i);
            return po;
          }
          xnc::EncoderOutput out;
          while (sess.TakeOutput(&out, 4)) {
            if (po.first_output_inputs < 0) {
              po.first_output_inputs = static_cast<int>(i) + 1;
              po.first_output_ms =
                  static_cast<uint32_t>(GetTickCount64() - t0);
            }
            po.outputs.push_back(std::move(out));
          }
        }
        xnc::EncoderOutput out;
        while (sess.TakeOutput(&out, 4)) po.outputs.push_back(std::move(out));
        // Decode the two forced-IDR outputs (input 0 = content A,
        // input 7 = content B) through the MF decoder probe.
        std::string derr;
        for (const auto& o : po.outputs) {
          if (!(o.key && (o.id.encode_seq == 1 || o.id.encode_seq == 8)))
            continue;
          uint64_t hash = 0;
          if (!xnc::DecodeAnnexBToLumaHash(o.au, &hash, &derr)) continue;
          if (o.id.encode_seq == 1) {
            po.hash_a = hash;
            po.decoded_a = true;
          } else {
            po.hash_b = hash;
            po.decoded_b = true;
          }
        }
        if (!po.decoded_a)
          po.err = "probe: no decodable IDR for input 0" +
                   (derr.empty() ? std::string() : " (" + derr + ")");
        else if (!po.decoded_b)
          po.err = "probe: no decodable IDR for input 7";
        po.ok = po.err.empty();
        return po;
      };
      xnc::MfCpuEncoder cpu;
      bool cpu_ok = cpu.Init(dev.Get(), &pool, sw, sh, 30, 2000000, &err);
      if (!cpu_ok)
        std::printf("SELFTEST NOTE: cpu-session-init err=%s\n", err.c_str());
      CHECK("cpu-session-init", cpu_ok);
      if (cpu_ok) {
        std::printf("SELFTEST NOTE: cpu-session backend=%s friendly=\"%s\"\n",
                    cpu.BackendName(), cpu.FriendlyName().c_str());
        ProbeOutcome p1 = run_probe(cpu);
        if (!p1.ok)
          std::printf("SELFTEST NOTE: cpu-probe err=%s\n", p1.err.c_str());
        CHECK("cpu-probe-ok", p1.ok);
        if (p1.ok) {
          // 1:1 mapping: inputs 1..8 each produced exactly one output,
          // paired by identity + lease token; reordering is allowed (the
          // software MFT emits AUs out of input order - time-keyed
          // mapping, not FIFO position).
          bool map_ok = p1.outputs.size() >= 8;
          for (uint64_t seq = 1; seq <= 8 && map_ok; ++seq) {
            size_t n = 0;
            for (const auto& o : p1.outputs) {
              if (o.id.encode_seq == seq) {
                ++n;
                map_ok = map_ok && o.submit_id == seq && o.slot < 3;
              }
            }
            map_ok = map_ok && n == 1;
          }
          CHECK("cpu-probe-1to1-mapping", map_ok);
          // First-output bound: the STRICT §8.3 bound (two inputs or
          // 100 ms) is a HARDWARE gate; the software rung's structural
          // ~17-input lookahead (measured; CBR/low-latency does not
          // remove it) gets the loud-NOTE treatment here.
          std::printf("SELFTEST NOTE: cpu-probe first_output_inputs=%d "
                      "first_output_ms=%u outputs=%zu (software rung: the "
                      "strict 2-input/100ms bound is hardware-only; "
                      "spec 8.3 items 1/3/4/5 all asserted)\n",
                      p1.first_output_inputs, p1.first_output_ms,
                      p1.outputs.size());
          CHECK("cpu-probe-first-output-bounded",
                p1.first_output_inputs > 0 &&
                    p1.first_output_inputs <= 30);
          CHECK("cpu-probe-discriminates",
                p1.hash_a != 0 && p1.hash_b != 0 && p1.hash_a != p1.hash_b);
          // Hot reconfigure (spec §8.2): rate change keeps the session
          // encoding with intact identity mapping.
          CHECK("cpu-reconfigure", cpu.Reconfigure(1200000, 30));
          {
            xnc::SurfaceLease* lease = pool.Acquire();
            CHECK("cpu-post-reconf-lease", lease != nullptr);
            if (lease != nullptr) {
              upload(lease->texture(), nv12_b);
              const xnc::FrameIdentity id{1, 1, 400, 31, 0, 0};
              CHECK("cpu-post-reconf-submit",
                    cpu.Submit(id, std::move(*lease), false) ==
                        xnc::SubmitResult::kOk);
              bool got31 = false;
              xnc::EncoderOutput out;
              uint64_t fill_seq = 31;
              const ULONGLONG dl = GetTickCount64() + 3000;
              while (GetTickCount64() < dl) {
                if (cpu.TakeOutput(&out, 4) && out.id.encode_seq == 31) {
                  got31 = true;
                  break;
                }
                xnc::SurfaceLease* fill = pool.Acquire();
                if (fill == nullptr) {
                  Sleep(4);
                  continue;
                }
                upload(fill->texture(), nv12_a);
                const xnc::FrameIdentity fid{1, 1, 401, ++fill_seq, 0, 0};
                cpu.Submit(fid, std::move(*fill), false);
              }
              CHECK("cpu-post-reconf-output-maps", got31);
            }
          }
          // Reference regime: a PRISTINE session of the SAME type, fed the
          // IDENTICAL 30-input sequence, must decode to the same per-input
          // signatures (decode-side determinism via the same backend -
          // the abc-scenario reference discipline).
          xnc::MfCpuEncoder cpu2;
          const bool cpu2_ok =
              cpu2.Init(dev.Get(), &pool, sw, sh, 30, 2000000, &err);
          CHECK("cpu-ref-session-init", cpu2_ok);
          if (cpu2_ok) {
            ProbeOutcome p2 = run_probe(cpu2);
            CHECK("cpu-ref-probe-ok", p2.ok);
            if (p2.ok) {
              CHECK("cpu-ref-signature-a", p2.hash_a == p1.hash_a);
              CHECK("cpu-ref-signature-b", p2.hash_b == p1.hash_b);
            }
            cpu2.Shutdown(xnc::ShutdownMode::kDrain);
            pool.FreeRetired();  // sweep consumption-retired slots
            CHECK("cpu-ref-zero-live",
                  pool.LiveLeases() == 0 &&
                      pool.FreeCount() == xnc::Nv12SurfacePool::kSlotCount);
          }
        }
        cpu.Shutdown(xnc::ShutdownMode::kDrain);
        pool.FreeRetired();
        CHECK("cpu-shutdown-zero-live",
              pool.LiveLeases() == 0 &&
                  pool.FreeCount() == xnc::Nv12SurfacePool::kSlotCount);
        CHECK("cpu-shutdown-takeoutput-notready",
              [&] {
                xnc::EncoderOutput out;
                return !cpu.TakeOutput(&out, 1) &&
                       cpu.last_error() ==
                           xnc::EncoderSessionError::kNotReady;
              }());
        {
          xnc::SurfaceLease* lease = pool.Acquire();
          CHECK("cpu-shutdown-submit-lease", lease != nullptr);
          if (lease != nullptr) {
            const size_t idx = lease->index();
            xnc::FrameIdentity id{};
            id.capture_epoch = 1;
            id.codec_epoch = 1;
            id.content_id = 500;
            id.encode_seq = 60;
            const xnc::SubmitResult r =
                cpu.Submit(id, std::move(*lease), false);
            CHECK("cpu-shutdown-submit-releases-lease",
                  r == xnc::SubmitResult::kNotReady &&
                      pool.State(idx) == xnc::SurfaceLeaseState::kFree);
          }
        }
      }
      // ---- (d) the REAL GPU session (D3D11-aware hardware ladder) ----
      xnc::MfGpuEncoder gpu;
      const bool gpu_ok = gpu.Init(dev.Get(), &pool, sw, sh, 30, 2000000, &err);
      if (!gpu_ok) {
        // Skip-clean (plan ruling 5): no fail, no silent pass. The error
        // carries the measured machine shape (async-unlockable QSV that
        // never completes encode work under RDP, or no hardware encoder).
        std::printf("SELFTEST NOTE: gpu-session hardware path UNAVAILABLE "
                    "on this machine - err=\"%s\" - hardware session "
                    "scenarios SKIPPED (the software path above is the "
                    "exercised rung)\n",
                    err.c_str());
        CHECK("gpu-session-skip-reason",
              err.find("startup probe") != std::string::npos ||
                  err.find("hardware H.264 encoder") != std::string::npos ||
                  err.find("D3D11-aware") != std::string::npos ||
                  err.find("MFTEnumEx") != std::string::npos ||
                  err.find("MFCreateDXGIDeviceManager") != std::string::npos);
        CHECK("gpu-session-skip-not-initialized", !gpu.initialized());
        xnc::SurfaceLease* lease = pool.Acquire();
        CHECK("gpu-skip-lease", lease != nullptr);
        if (lease != nullptr) {
          const size_t idx = lease->index();
          const xnc::FrameIdentity id{1, 1, 600, 70, 0, 0};
          CHECK("gpu-skip-submit-notready",
                gpu.Submit(id, std::move(*lease), false) ==
                    xnc::SubmitResult::kNotReady);
          CHECK("gpu-skip-lease-returned",
                pool.State(idx) == xnc::SurfaceLeaseState::kFree);
        }
        xnc::EncoderOutput out;
        CHECK("gpu-skip-takeoutput-notready",
              !gpu.TakeOutput(&out, 1) && gpu.last_error() ==
                                              xnc::EncoderSessionError::
                                                  kNotReady);
        CHECK("gpu-skip-reconfigure", !gpu.Reconfigure(1, 30));
        CHECK("gpu-skip-matrix-zero", gpu.negotiated_input_matrix() == 0);
        gpu.Shutdown(xnc::ShutdownMode::kImmediate);
        pool.FreeRetired();
        CHECK("gpu-skip-zero-live",
              pool.LiveLeases() == 0 &&
                  pool.FreeCount() == xnc::Nv12SurfacePool::kSlotCount);
      } else {
        // Hardware initialized (console session / capable RDP): run the
        // same probe with the STRICT §8.3 bounds.
        std::printf("SELFTEST NOTE: gpu-session hardware INITIALIZED "
                    "friendly=\"%s\" - running the strict probe\n",
                    gpu.FriendlyName().c_str());
        ProbeOutcome p1 = run_probe(gpu);
        CHECK("gpu-probe-ok", p1.ok);
        if (p1.ok) {
          bool map_ok = p1.outputs.size() >= 8;
          for (uint64_t seq = 1; seq <= 8 && map_ok; ++seq) {
            size_t n = 0;
            for (const auto& o : p1.outputs) {
              if (o.id.encode_seq == seq) {
                ++n;
                map_ok = map_ok && o.submit_id == seq && o.slot < 3;
              }
            }
            map_ok = map_ok && n == 1;
          }
          CHECK("gpu-probe-1to1-mapping", map_ok);
          // §8.3 item 2 as documented (and as the probe itself gates):
          // first output within TWO INPUTS OR 100 ms. Measured on Arc/QSV
          // (driver 32.0.101.8801): the encoder's structural ~4-5-input
          // emit depth (every low-latency knob wedges or is rejected - see
          // mf_gpu_encoder.cpp) lands the first output at input ~5 but
          // 11-23 ms wall - the 100 ms latency half of the bound is the
          // binding gate and it holds with wide margin.
          CHECK("gpu-probe-first-output-strict",
                p1.first_output_inputs > 0 &&
                    (p1.first_output_inputs <= 2 ||
                     p1.first_output_ms <= 100));
          CHECK("gpu-probe-discriminates", p1.hash_a != p1.hash_b);
          // §8.4 on hardware is a HARD check (review fix): the negotiated
          // input type's matrix must equal the rule UNCONDITIONALLY
          // (MfGpuEncoder::negotiated_input_matrix readback - there is no
          // VUI-escape anymore), and a VUI that carries color metadata
          // must agree with the same rule.
          const xnc::Nv12ColorConfig rule = xnc::Nv12ColorForSize(sh);
          CHECK("gpu-input-matrix-agrees",
                gpu.negotiated_input_matrix() == rule.matrix);
          bool vui_checked = false, vui_ok = false, saw_idr_vui = false;
          for (const auto& o : p1.outputs) {
            if (!o.key) continue;
            const SpsVui vui = ParseSpsVui(o.au.data(), o.au.size());
            if (!vui.sps_found) continue;
            saw_idr_vui = true;
            if (vui.vui_present && vui.video_signal_present &&
                vui.colour_description == 1) {
              vui_checked = true;
              vui_ok = VuiMatchesColorRule(vui, rule);
            }
          }
          if (vui_checked) {
            CHECK("gpu-probe-vui-agrees", vui_ok);
          } else if (saw_idr_vui) {
            // Loud NOTE, not an escape: for a backend that does not write
            // VUI color, the unconditional negotiated-matrix assertion
            // above IS the hard §8.4 check.
            std::printf("SELFTEST NOTE: gpu-probe VUI color unspecified "
                        "(backend does not signal; the hard check is the "
                        "negotiated input matrix=%u vs rule=%u above)\n",
                        gpu.negotiated_input_matrix(), rule.matrix);
          }
        }
        gpu.Shutdown(xnc::ShutdownMode::kDrain);
        pool.FreeRetired();
        CHECK("gpu-shutdown-zero-live",
              pool.LiveLeases() == 0 &&
                  pool.FreeCount() == xnc::Nv12SurfacePool::kSlotCount);
      }
    }
  }
  { // M2 Task 2 (plan Step 1): fake captured-surface OWNERSHIP. Pins the
    // AcquireSurface contract on a scripted fake (controller ruling 4):
    //   (a) ReleaseFrame fires immediately after the full-resource copy and
    //       BEFORE the method returns / any encoder callback (the
    //       duplication is never held into encoder work);
    //   (b) cursor-only frames (LastPresentTime == 0, with or without a
    //       QI-able texture) do not copy and do not count as changed, so
    //       the CALLER-side content_id never increments for them (exactly
    //       one component - the caller - assigns content);
    //   (c) the backend stamps the identity it was GIVEN (caller-assigned
    //       epochs + content_id), completed with the acquire timestamp - it
    //       never invents content_id/epochs.
    // Device-free (the ScriptedCapture pattern).
    std::vector<ScriptedDupl::Frame> script{
        {1, true},   // content frame A
        {0, true},   // cursor-only move: LastPresentTime == 0
        {0, false},  // cursor-only again, no QI-able texture
        {2, true},   // content frame B
    };
    ScriptedSurfaceCapture cap(script);
    xnc::LatestSurface latest;  // caller-owned (the fake only bookkeeps it)
    std::string err;
    uint64_t next_content = 0;            // the CALLER's content authority
    std::vector<uint64_t> committed_ids;  // content ids committed on kFrame
    auto acquire = [&](xnc::FrameIdentity* id_out) {
      xnc::FrameIdentity id{};
      id.capture_epoch = 5;             // caller-owned identity fields
      id.codec_epoch = 3;
      id.content_id = next_content + 1;  // SPECULATIVE: committed on kFrame
      const xnc::CaptureStatus st = cap.AcquireSurface(latest, 16, &id, &err);
      if (st == xnc::CaptureStatus::kFrame) {
        next_content = id.content_id;
        committed_ids.push_back(id.content_id);
      }
      *id_out = id;
      return st;
    };
    using Ev = ScriptedDupl::Ev;
    auto tail_is = [&](size_t mark, std::vector<Ev> want) {
      const auto& ev = cap.dupl().events();
      return ev.size() == mark + want.size() &&
             std::equal(ev.begin() + mark, ev.end(), want.begin());
    };
    size_t mark = 0;
    xnc::FrameIdentity id{};

    // Frame A: acquire -> copy -> release; nothing held at the return.
    CHECK("surface-frame-a", acquire(&id) == xnc::CaptureStatus::kFrame);
    CHECK("surface-frame-a-order",
          tail_is(mark, {Ev::kAcquire, Ev::kCopy, Ev::kRelease}));
    CHECK("surface-frame-a-not-held", cap.dupl().held() == 0);
    CHECK("surface-frame-a-given-identity",
          id.capture_epoch == 5 && id.codec_epoch == 3 && id.content_id == 1 &&
              id.encode_seq == 0 && id.present_mono_us == 0 &&
              id.source_mono_us != 0);
    const uint64_t src_a = id.source_mono_us;
    cap.dupl().EncoderWork();  // the encoder callback fires after the return
    mark = cap.dupl().events().size();

    // Cursor-only move: no copy, no change, no content increment; the
    // surface identity echoes the last STAMPED frame (the speculative id 2
    // was never committed).
    CHECK("surface-cursor-move",
          acquire(&id) == xnc::CaptureStatus::kNoChange && err == "err_timeout");
    CHECK("surface-cursor-move-order",
          tail_is(mark, {Ev::kAcquire, Ev::kRelease}));
    CHECK("surface-cursor-move-no-copy", cap.dupl().copies() == 1);
    CHECK("surface-cursor-move-no-content",
          committed_ids.size() == 1 && next_content == 1 && id.content_id == 1);
    mark = cap.dupl().events().size();

    // Cursor-only without a texture: identical no-change semantics.
    CHECK("surface-cursor-notext",
          acquire(&id) == xnc::CaptureStatus::kNoChange && err == "err_timeout");
    CHECK("surface-cursor-notext-order",
          tail_is(mark, {Ev::kAcquire, Ev::kRelease}));
    CHECK("surface-cursor-notext-no-content", next_content == 1);
    mark = cap.dupl().events().size();

    // Frame B: content again - the caller's next id commits, and the new
    // acquire timestamp is stamped.
    CHECK("surface-frame-b", acquire(&id) == xnc::CaptureStatus::kFrame);
    CHECK("surface-frame-b-order",
          tail_is(mark, {Ev::kAcquire, Ev::kCopy, Ev::kRelease}));
    CHECK("surface-frame-b-not-held", cap.dupl().held() == 0);
    CHECK("surface-frame-b-content",
          id.content_id == 2 && committed_ids.size() == 2 &&
              committed_ids[1] == 2 && id.source_mono_us >= src_a);
    cap.dupl().EncoderWork();
    mark = cap.dupl().events().size();

    // Script exhausted: static screen -> no change, identity still echoed.
    CHECK("surface-static",
          acquire(&id) == xnc::CaptureStatus::kNoChange && err == "err_timeout");
    CHECK("surface-static-order", tail_is(mark, {Ev::kAcquire}));
    CHECK("surface-static-echoes-last",
          id.content_id == 2 && id.source_mono_us != 0);

    // The whole session, end to end: every ReleaseFrame precedes the
    // encoder work that follows it; cursor-only acquires never copied.
    const auto& evs = cap.dupl().events();
    CHECK("surface-session-events", evs.size() == 13);
    size_t last_release = 0, last_encoder = 0;
    for (size_t i = 0; i < evs.size(); ++i) {
      if (evs[i] == Ev::kRelease) last_release = i;
      if (evs[i] == Ev::kEncoder) last_encoder = i;
    }
    CHECK("surface-release-before-encoder", last_release < last_encoder);
    CHECK("surface-total-copies", cap.dupl().copies() == 2);
    // The err-string -> status mapping both real backends share.
    CHECK("surface-status-map-timeout",
          xnc::CaptureStatusFromErr("err_timeout") ==
              xnc::CaptureStatus::kNoChange);
    CHECK("surface-status-map-rebuilt",
          xnc::CaptureStatusFromErr("err_rebuilt") == xnc::CaptureStatus::kRetry);
    CHECK("surface-status-map-access-lost",
          xnc::CaptureStatusFromErr("err_access_lost") ==
              xnc::CaptureStatus::kAccessLost);
    CHECK("surface-status-map-fatal",
          xnc::CaptureStatusFromErr("AcquireNextFrame: hr=0x80004005") ==
              xnc::CaptureStatus::kFatal);
  }
  { // Delayed encoder output must retain submission order, not call order.
    xnc::SubmissionLedger ledger;
    for (uint64_t t : {100, 200, 300})
      CHECK("ledger-submit", ledger.Submit(t));
    uint64_t out = 0;
    CHECK("ledger-first", ledger.Take(&out) && out == 100);
    CHECK("ledger-second", ledger.Take(&out) && out == 200);
    CHECK("ledger-third", ledger.Take(&out) && out == 300);
    CHECK("ledger-empty", !ledger.Take(&out));
    CHECK("ledger-empty-null", !ledger.Take(nullptr));
  }
  { // The measured MFT window is bounded; overflow must fail explicitly.
    xnc::SubmissionLedger ledger;
    bool first_64 = true;
    for (uint64_t t = 0; t < 64; ++t) first_64 &= ledger.Submit(t);
    CHECK("ledger-cap-64", first_64 && ledger.Pending() == 64);
    CHECK("ledger-overflow-rejected", !ledger.Submit(64) && ledger.Pending() == 64);
    uint64_t out = 99;
    CHECK("ledger-overflow-preserves-oldest", ledger.Take(&out) && out == 0);
    CHECK("ledger-space-reopens", ledger.Submit(64) && ledger.Pending() == 64);
    ledger.Clear();
    CHECK("ledger-clear", ledger.Pending() == 0 && !ledger.Take(&out));
    CHECK("ledger-clear-new-lifecycle-submit", ledger.Submit(888));
    CHECK("ledger-clear-new-lifecycle-take",
          ledger.Take(&out) && out == 888 && ledger.Pending() == 0);
  }
  { // Failed input registration rollback removes only the newest item.
    xnc::SubmissionLedger ledger;
    CHECK("ledger-rollback-submit-old", ledger.Submit(10));
    CHECK("ledger-rollback-submit-new", ledger.Submit(20));
    CHECK("ledger-rollback-newest", ledger.RollbackNewest(20));
    uint64_t out = 0;
    CHECK("ledger-rollback-preserves-old", ledger.Pending() == 1 &&
                                                ledger.Take(&out) && out == 10);
    CHECK("ledger-rollback-empty", !ledger.RollbackNewest(10));
  }
  { // Pre-accept rejection rolls back only the newest registration.
    EncoderFaultPlan plan;
    plan.reject_input_call = 2;  // input 1 stays accepted and pending
    RecordingSink sink;
    bool init_ok = false;
    const xnc::PipelineResult res =
        RunEncoderFaultScenario(2, &plan, sink, &init_ok);
    CHECK("ledger-fault-preaccept-init", init_ok);
    CHECK("ledger-fault-preaccept-aborts",
          !res.ok && res.err == "fault_pre_accept");
    CHECK("ledger-fault-preaccept-branches",
          plan.InputCalls() == 2 && plan.submit.calls == 1 &&
              plan.drain.calls == 0 && plan.flush_tail.calls == 0);
    CHECK("ledger-fault-preaccept-no-au", sink.aus == 0);
  }
  { // Accepted input + partial collection failure keeps and consumes identity.
    EncoderFaultPlan plan;
    plan.submit.ok = false;
    plan.submit.aus = 1;
    plan.submit.err = "fault_submit_collect";
    RecordingSink sink;
    bool init_ok = false;
    const xnc::PipelineResult res =
        RunEncoderFaultScenario(1, &plan, sink, &init_ok);
    CHECK("ledger-fault-postaccept-init", init_ok);
    CHECK("ledger-fault-postaccept-aborts",
          !res.ok && res.err.find("encoder_output_collection_failed") == 0 &&
              res.err.find("fault_submit_collect") != std::string::npos);
    CHECK("ledger-fault-postaccept-partial-delivered",
          sink.aus == 1 && sink.timestamps.size() == 1);
    CHECK("ledger-fault-postaccept-no-tail",
          plan.submit.calls == 1 && plan.drain.calls == 0 &&
              plan.flush_tail.calls == 0);
  }
  { // Sink failure is primary even when the same collection also failed.
    EncoderFaultPlan plan;
    plan.submit.ok = false;
    plan.submit.aus = 1;
    plan.submit.err = "fault_compound_collect";
    FailingAuSink sink;
    bool init_ok = false;
    const xnc::PipelineResult res =
        RunEncoderFaultScenario(1, &plan, sink, &init_ok);
    CHECK("ledger-fault-compound-sink-init", init_ok);
    CHECK("ledger-fault-compound-sink-primary",
          !res.ok && res.err == "fault_sink_mid_vector" &&
              sink.timestamps.size() == 1 && plan.submit.calls == 1);
  }
  { // Missing identity is primary even when collection returned failure.
    EncoderFaultPlan plan;
    plan.submit.ok = false;
    plan.submit.aus = 2;
    plan.submit.err = "fault_compound_collect";
    RecordingSink sink;
    bool init_ok = false;
    const xnc::PipelineResult res =
        RunEncoderFaultScenario(1, &plan, sink, &init_ok);
    CHECK("ledger-fault-compound-mismatch-init", init_ok);
    CHECK("ledger-fault-compound-mismatch-primary",
          !res.ok && res.err == "encoder_identity_mismatch" &&
              sink.aus == 1 && plan.submit.calls == 1);
  }
  { // Drain failure surfaces after its complete partial AUs are delivered.
    EncoderFaultPlan plan;
    plan.drain.ok = false;
    plan.drain.aus = 1;
    plan.drain.err = "fault_drain_collect";
    RecordingSink sink;
    bool init_ok = false;
    const xnc::PipelineResult res =
        RunEncoderFaultScenario(1, &plan, sink, &init_ok);
    CHECK("ledger-fault-drain-init", init_ok);
    CHECK("ledger-fault-drain-aborts",
          !res.ok && res.err.find("encoder_drain_failed") == 0 &&
              res.err.find("fault_drain_collect") != std::string::npos);
    CHECK("ledger-fault-drain-partial-delivered", sink.aus == 1);
    CHECK("ledger-fault-drain-skips-flush",
          plan.drain.calls == 1 && plan.flush_tail.calls == 0);
  }
  { // FlushTail failure surfaces; one complete AU consumes one of two inputs.
    EncoderFaultPlan plan;
    plan.flush_tail.ok = false;
    plan.flush_tail.aus = 1;
    plan.flush_tail.err = "fault_flush_collect";
    RecordingSink sink;
    bool init_ok = false;
    const xnc::PipelineResult res =
        RunEncoderFaultScenario(2, &plan, sink, &init_ok);
    CHECK("ledger-fault-flush-init", init_ok);
    CHECK("ledger-fault-flush-aborts",
          !res.ok && res.err.find("encoder_flush_tail_failed") == 0 &&
              res.err.find("fault_flush_collect") != std::string::npos);
    CHECK("ledger-fault-flush-partial-delivered", sink.aus == 1);
    CHECK("ledger-fault-flush-branches",
          plan.drain.calls == 1 && plan.flush_tail.calls == 1);
  }
  { // Successful drain/tail with an unresolved input is identity mismatch.
    EncoderFaultPlan plan;
    RecordingSink sink;
    bool init_ok = false;
    const xnc::PipelineResult res =
        RunEncoderFaultScenario(1, &plan, sink, &init_ok);
    CHECK("ledger-fault-pending-init", init_ok);
    CHECK("ledger-fault-pending-mismatch",
          !res.ok && res.err == "encoder_identity_mismatch" && sink.aus == 0);
  }
  { // Real pipeline ledger reaches 64; submission 65 aborts before ProcessInput.
    EncoderFaultPlan plan;
    RecordingSink sink;
    bool init_ok = false;
    const xnc::PipelineResult res =
        RunEncoderFaultScenario(65, &plan, sink, &init_ok);
    CHECK("ledger-fault-pipeline-overflow-init", init_ok);
    CHECK("ledger-fault-pipeline-overflow-error",
          !res.ok && res.err == "encoder_submission_overflow");
    CHECK("ledger-fault-pipeline-overflow-boundary",
          plan.InputCalls() == 64 && plan.submit.calls == 64 && sink.aus == 0 &&
              plan.drain.calls == 0 && plan.flush_tail.calls == 0);
  }
  { // Re-init clears lifecycle 1's pending identity before lifecycle 2 output.
    EncoderFaultPlan plan;
    plan.submit.aus = 1;
    plan.submit.emit_on_call = 2;
    RecordingSink sink;
    bool init_ok = false;
    const xnc::PipelineResult res =
        RunLedgerLifecycleScenario(&plan, sink, &init_ok);
    CHECK("ledger-fault-lifecycle-init", init_ok);
    CHECK("ledger-fault-lifecycle-reuse-clean",
          res.ok && res.err.empty() && res.resets == 1 && res.width == 66 &&
              sink.aus == 1 && plan.InputCalls() == 2);
  }
  { // Sink failure mid-vector remains the primary abort error during cleanup.
    EncoderFaultPlan plan;
    plan.drain.aus = 2;
    FailingAuSink sink;
    bool init_ok = false;
    const xnc::PipelineResult res =
        RunEncoderFaultScenario(2, &plan, sink, &init_ok);
    CHECK("ledger-fault-sink-init", init_ok);
    CHECK("ledger-fault-sink-preserved",
          !res.ok && res.err == "fault_sink_mid_vector" &&
              sink.timestamps.size() == 1);
    CHECK("ledger-fault-sink-skips-flush", plan.flush_tail.calls == 0);
  }
  { // An overproducing output vector aborts on the first missing identity.
    EncoderFaultPlan plan;
    plan.drain.aus = 2;
    RecordingSink sink;
    bool init_ok = false;
    const xnc::PipelineResult res =
        RunEncoderFaultScenario(1, &plan, sink, &init_ok);
    CHECK("ledger-fault-mismatch-init", init_ok);
    CHECK("ledger-fault-mismatch-preserved",
          !res.ok && res.err == "encoder_identity_mismatch" && sink.aus == 1);
    CHECK("ledger-fault-mismatch-skips-flush", plan.flush_tail.calls == 0);
  }
  { // EOS ProcessMessage failure surfaces after partial tail AU delivery.
    EncoderFaultPlan plan;
    plan.end_of_stream.ok = false;
    plan.end_of_stream.err = "fault_flush_eos_message";
    plan.flush_tail.aus = 1;
    RecordingSink sink;
    bool init_ok = false;
    const xnc::PipelineResult res =
        RunEncoderFaultScenario(1, &plan, sink, &init_ok);
    CHECK("ledger-fault-eos-message-init", init_ok);
    CHECK("ledger-fault-eos-message-surfaced",
          !res.ok && res.err.find("encoder_flush_tail_failed") == 0 &&
              res.err.find("fault_flush_eos_message") != std::string::npos);
    CHECK("ledger-fault-eos-message-partial-delivered",
          sink.aus == 1 && plan.end_of_stream.calls == 1 &&
              plan.drain_message.calls == 1 && plan.flush_tail.calls == 1);
  }
  { // Drain ProcessMessage failure also surfaces after partial tail output.
    EncoderFaultPlan plan;
    plan.drain_message.ok = false;
    plan.drain_message.err = "fault_flush_drain_message";
    plan.flush_tail.aus = 1;
    RecordingSink sink;
    bool init_ok = false;
    const xnc::PipelineResult res =
        RunEncoderFaultScenario(1, &plan, sink, &init_ok);
    CHECK("ledger-fault-drain-message-init", init_ok);
    CHECK("ledger-fault-drain-message-surfaced",
          !res.ok && res.err.find("encoder_flush_tail_failed") == 0 &&
              res.err.find("fault_flush_drain_message") != std::string::npos);
    CHECK("ledger-fault-drain-message-partial-delivered",
          sink.aus == 1 && plan.end_of_stream.calls == 1 &&
              plan.drain_message.calls == 1 && plan.flush_tail.calls == 1);
  }
  { // 默认值(plan Task 2 接口):--console-diag 只给 --out → fps=30 duration=10
    auto p = Parse({L"--console-diag", L"--out", L"t.h264"});
    CHECK("args-ok-defaults", p.ok);
    CHECK("args-default-fps", p.ok && p.opt.fps == 30);
    CHECK("args-default-duration", p.ok && p.opt.duration_s == 10);
    CHECK("args-default-out", p.ok && p.opt.out_path == L"t.h264");
    CHECK("args-default-mode", p.ok && p.opt.console_diag);
  }
  { // 显式值全解析
    auto p = Parse({L"--console-diag", L"--duration", L"3", L"--out", L"x.h264", L"--fps", L"15"});
    CHECK("args-explicit-ok", p.ok);
    CHECK("args-explicit-values", p.ok && p.opt.duration_s == 3 && p.opt.fps == 15 && p.opt.out_path == L"x.h264");
  }
  { // --encoder(hw-encode 梯子 + M2-Slice2 Task 3 crash-loop 降参契约):
    // hardware(默认,硬编优先软编回退)与 software(强制软编)合法;其它值
    // 拒绝。
    auto hw = Parse({L"--console-diag", L"--out", L"t", L"--encoder", L"hardware"});
    CHECK("args-encoder-hardware-ok", hw.ok);
    auto sw = Parse({L"--console-diag", L"--out", L"t", L"--encoder", L"software"});
    CHECK("args-encoder-software-ok", sw.ok);
    CHECK("args-encoder-default-hardware",
          Parse({L"--console-diag", L"--out", L"t"}).ok &&
          Parse({L"--console-diag", L"--out", L"t"}).opt.encoder == xnc::DiagEncoder::kHardware);
    auto bad = Parse({L"--console-diag", L"--out", L"t", L"--encoder", L"nvenc"});
    CHECK("args-encoder-only-software", !bad.ok);
    CHECK("args-encoder-missing-value", !Parse({L"--console-diag", L"--out", L"t", L"--encoder"}).ok);
  }
  { // out 缺失/空 → 参数错(Task 6 spawn 契约要求显式 --out)
    auto p = Parse({L"--console-diag"});
    CHECK("args-out-missing", !p.ok);
    CHECK("args-out-missing-err", !p.ok && p.err.find(L"--out") != std::wstring::npos);
    CHECK("args-out-empty", !Parse({L"--console-diag", L"--out", L""}).ok);
  }
  { // duration/fps 校验:必须 >0 的纯数字(拒绝 0、负数、垃圾、尾随字符、缺值)
    auto diag = [](std::vector<std::wstring> tail) {
      tail.insert(tail.begin(), {L"--console-diag", L"--out", L"t"});
      return Parse(tail);
    };
    CHECK("args-duration-zero", !diag({L"--duration", L"0"}).ok);
    CHECK("args-duration-negative", !diag({L"--duration", L"-1"}).ok);
    CHECK("args-duration-garbage", !diag({L"--duration", L"abc"}).ok);
    CHECK("args-duration-trailing", !diag({L"--duration", L"1x"}).ok);
    CHECK("args-duration-missing-value", !diag({L"--duration"}).ok);
    CHECK("args-duration-ok-boundary", diag({L"--duration", L"1"}).ok);
    CHECK("args-fps-zero", !diag({L"--fps", L"0"}).ok);
    CHECK("args-fps-missing-value", !diag({L"--fps"}).ok);
  }
  { // 模式选择与未知参数
    auto st = Parse({L"--selftest"});
    CHECK("args-selftest", st.ok && st.opt.selftest);
    auto h = Parse({L"--help"});
    CHECK("args-help", h.ok && h.opt.help);
    CHECK("args-no-mode", !Parse({}).ok);
    CHECK("args-unknown-flag", !Parse({L"--bogus"}).ok);
    CHECK("args-mode-exclusive", !Parse({L"--selftest", L"--console-diag", L"--out", L"t"}).ok);
  }
  { // FrameBlob 布局(MSVC x64 ABI):bgra(vector 24B)+w(4)+h(4)+pixfmt(1)+
    // 对齐填充(3)+mono_us(8)+gpu_scale_us(8) 紧凑 56B(gpu-readback 任务
    // 新增 pixfmt 布局字段与 gpu_scale_us 计时段)。M1 Task 1 在末尾追加四个
    // uint64 内容身份字段(总 88B):capture_epoch/codec_epoch/content_id/
    // source_mono_us - 身份随帧穿过 handoff 队列(见 capture.h)。
    CHECK("frameblob-sizeof", sizeof(xnc::FrameBlob) == 88);
    CHECK("frameblob-off-bgra", offsetof(xnc::FrameBlob, bgra) == 0);
    CHECK("frameblob-off-w", offsetof(xnc::FrameBlob, w) == 24);
    CHECK("frameblob-off-h", offsetof(xnc::FrameBlob, h) == 28);
    CHECK("frameblob-off-pixfmt", offsetof(xnc::FrameBlob, pixfmt) == 32);
    CHECK("frameblob-off-mono", offsetof(xnc::FrameBlob, mono_us) == 40);
    CHECK("frameblob-off-gpu-scale", offsetof(xnc::FrameBlob, gpu_scale_us) == 48);
    CHECK("frameblob-off-capture-epoch", offsetof(xnc::FrameBlob, capture_epoch) == 56);
    CHECK("frameblob-off-codec-epoch", offsetof(xnc::FrameBlob, codec_epoch) == 64);
    CHECK("frameblob-off-content-id", offsetof(xnc::FrameBlob, content_id) == 72);
    CHECK("frameblob-off-source-mono", offsetof(xnc::FrameBlob, source_mono_us) == 80);
    CHECK("frameblob-field-sizes", sizeof(xnc::FrameBlob::w) == 4 && sizeof(xnc::FrameBlob::h) == 4 &&
                                  sizeof(xnc::FrameBlob::mono_us) == 8 &&
                                  sizeof(xnc::FrameBlob::gpu_scale_us) == 8 &&
                                  sizeof(xnc::FrameBlob::capture_epoch) == 8 &&
                                  sizeof(xnc::FrameBlob::codec_epoch) == 8 &&
                                  sizeof(xnc::FrameBlob::content_id) == 8 &&
                                  sizeof(xnc::FrameBlob::source_mono_us) == 8 &&
                                  sizeof(xnc::FrameBlob::pixfmt) == 1);
    xnc::FrameBlob fb;
    CHECK("frameblob-default", fb.bgra.empty() && fb.w == 0 && fb.h == 0 &&
                               fb.mono_us == 0 && fb.gpu_scale_us == 0 &&
                               fb.pixfmt == xnc::Pixfmt::kBgra &&
                               fb.capture_epoch == 0 && fb.codec_epoch == 0 &&
                               fb.content_id == 0 && fb.source_mono_us == 0);
  }
  { // ICapture 形状:抽象基类(Acquire/Width/Height 纯虚),虚析构可 delete
    static_assert(std::is_abstract<xnc::ICapture>::value, "ICapture must stay abstract");
    CHECK("icapture-abstract", std::is_abstract<xnc::ICapture>::value);
    FakeCapture fc;
    xnc::ICapture* iface = &fc;
    xnc::FrameBlob fb;
    std::string acq_err;
    CHECK("icapture-virtual-dispatch",
          !iface->Acquire(fb, &acq_err) && iface->Width() == 0 && iface->Height() == 0);
    CHECK("icapture-default-acquire-err-optional", !iface->Acquire(fb));  // err 参数可省
    CHECK("icapture-default-rebuilds", iface->RebuildCount() == 0);
  }
  { // FrameBlob 尺寸算术(w*h*4,溢出护栏):Task 3 blob 大小契约
    CHECK("bgra-bytes-64x32", xnc::BgraBytes(64, 32) == 64u * 32u * 4u);
    CHECK("bgra-bytes-1080p", xnc::BgraBytes(1920, 1080) == 8294400u);
    CHECK("bgra-bytes-zero-dim", xnc::BgraBytes(0, 100) == 0 && xnc::BgraBytes(100, 0) == 0);
    CHECK("bgra-bytes-overflow-guard", xnc::BgraBytes(0xFFFFFFFFu, 0xFFFFFFFFu) == 0);
    // 真实 FrameBlob 尺寸对齐:resize(BgraBytes) 后 size == w*h*4
    xnc::FrameBlob fb;
    fb.w = 1920; fb.h = 1080;
    fb.bgra.resize(xnc::BgraBytes(fb.w, fb.h));
    CHECK("blob-size-matches-math", fb.bgra.size() == (size_t)fb.w * fb.h * 4);
  }
  CHECK("staging-read-is-current-0", xnc::StagingReadIndex(0) == 0);
  CHECK("staging-read-is-current-1", xnc::StagingReadIndex(1) == 1);
  { // 行距压缩(Map RowPitch > w*4 是常态):合成 pitched 数据逐行核对
    const uint32_t w = 8, h = 6;
    const size_t tight = (size_t)w * 4;      // 32
    const size_t pitch = tight + 16;         // GPU 加长行距
    std::vector<uint8_t> src(pitch * h + 7, 0xAB);  // +7 尾部哨兵
    for (uint32_t r = 0; r < h; ++r) {
      uint8_t* row = src.data() + r * pitch;
      for (size_t i = 0; i < tight; ++i) row[i] = static_cast<uint8_t>(r * 8 + (i % 8));
      for (size_t i = tight; i < pitch; ++i) row[i] = 0xCD;  // 行内 padding 垃圾
    }
    std::vector<uint8_t> dst(w * h * 4, 0);
    xnc::CompactBgraRows(src.data(), pitch, dst.data(), w, h);
    bool rows_ok = true;
    for (uint32_t r = 0; r < h && rows_ok; ++r)
      for (size_t i = 0; i < tight; ++i)
        if (dst[(size_t)r * tight + i] != ((r * 8 + (i % 8)) & 0xFF)) { rows_ok = false; break; }
    CHECK("compact-rows-content", rows_ok);
    CHECK("compact-rows-no-padding", std::find(dst.begin(), dst.end(), 0xCD) == dst.end());
    CHECK("compact-rows-size", dst.size() == xnc::BgraBytes(w, h));
    // 退化:RowPitch == w*4(无 padding)也必须逐行正确
    std::vector<uint8_t> src2(tight * h, 0x11);
    for (uint32_t r = 0; r < h; ++r)
      for (size_t i = 0; i < tight; ++i) src2[r * tight + i] = static_cast<uint8_t>(0x40 + r);
    std::vector<uint8_t> dst2(w * h * 4, 0);
    xnc::CompactBgraRows(src2.data(), tight, dst2.data(), w, h);
    bool tight_ok = dst2.size() == tight * h;
    for (uint32_t r = 0; r < h && tight_ok; ++r)
      if (dst2[r * tight] != 0x40 + r) tight_ok = false;
    CHECK("compact-rows-tight-pitch", tight_ok);
  }
  { // FNV-1a 64 已知向量(诊断首帧哈希工具)
    CHECK("fnv1a64-empty", xnc::Fnv1a64(nullptr, 0) == 0xcbf29ce484222325ull);
    const uint8_t a[] = {'a'};
    CHECK("fnv1a64-a", xnc::Fnv1a64(a, 1) == 0xaf63dc4c8601ec8cull);
    const uint8_t foobar[] = {'f', 'o', 'o', 'b', 'a', 'r'};
    CHECK("fnv1a64-foobar", xnc::Fnv1a64(foobar, 6) == 0x85944171f73967e8ull);
  }
  { // gpu-readback: VideoProcessor 输出尺寸(缩放+旋转+偶对齐,纯函数)。
    // 与 ScaledDims 相同的 2880x1800 -> 1920x1200 缩放;90/270 先交换边长
    uint32_t ow = 0, oh = 0;
    CHECK("gpu-sd-2880x1800", xnc::GpuScaledDims(2880, 1800, xnc::Rotate::kNone, 1920, &ow, &oh) &&
          ow == 1920 && oh == 1200);
    CHECK("gpu-sd-identity", xnc::GpuScaledDims(1280, 720, xnc::Rotate::kNone, 1920, &ow, &oh) &&
          ow == 1280 && oh == 720);
    CHECK("gpu-sd-zero-clamp", xnc::GpuScaledDims(2880, 1800, xnc::Rotate::kNone, 0, &ow, &oh) &&
          ow == 2880 && oh == 1800);
    CHECK("gpu-sd-even-snap-w", xnc::GpuScaledDims(1365, 768, xnc::Rotate::kNone, 1920, &ow, &oh) &&
          ow == 1364 && oh == 768);
    CHECK("gpu-sd-even-snap-h", xnc::GpuScaledDims(1920, 1081, xnc::Rotate::kNone, 1920, &ow, &oh) &&
          ow == 1920 && oh == 1080);
    // 90 度:2880x1800 横屏旋转后 1800x2880(竖宽 1800 <= 1920 → 不缩放)
    CHECK("gpu-sd-rot90-no-scale", xnc::GpuScaledDims(2880, 1800, xnc::Rotate::k90, 1920, &ow, &oh) &&
          ow == 1800 && oh == 2880);
    // 90 度 + 缩放:1440x3440 旋转后 3440x1440 → 1920 宽,高 = ceil(1440*1920/3440)=804
    CHECK("gpu-sd-rot90-scaled", xnc::GpuScaledDims(1440, 3440, xnc::Rotate::k90, 1920, &ow, &oh) &&
          ow == 1920 && oh == 804);
    CHECK("gpu-sd-rot270-swap", xnc::GpuScaledDims(1800, 2880, xnc::Rotate::k270, 1920, &ow, &oh) &&
          ow == 1920 && oh == 1200);
    CHECK("gpu-sd-rot180-keeps-dims", xnc::GpuScaledDims(2880, 1800, xnc::Rotate::k180, 1920, &ow, &oh) &&
          ow == 1920 && oh == 1200);
    CHECK("gpu-sd-reject-zero", !xnc::GpuScaledDims(0, 100, xnc::Rotate::kNone, 1920, &ow, &oh));
    CHECK("gpu-sd-rot-dxgi-mapping",
          xnc::RotateFromDxgi(0) == xnc::Rotate::kNone &&  // UNSPECIFIED
          xnc::RotateFromDxgi(1) == xnc::Rotate::kNone &&  // IDENTITY
          xnc::RotateFromDxgi(2) == xnc::Rotate::k90 &&
          xnc::RotateFromDxgi(3) == xnc::Rotate::k180 &&
          xnc::RotateFromDxgi(4) == xnc::Rotate::k270);
  }
  { // gpu-readback: NV12 单子资源行距压缩(D3D11 quirk:Map(0) 返回 Y 行 +
    // 紧接的 UV 行,共用同一 RowPitch;Map(1) 报 E_INVALIDARG - 2026-08-25
    // 在 Intel 驱动实测)。与 BGRA 版 CompactBgraRows 对照。
    const uint32_t w = 16, h = 10;
    const size_t pitch = w + 8;  // 对齐加长行距(Y/UV 共用)
    // 填充标记取 0xFE:Y/UV 合法值最大 0xAF/0xE4,不会误撞
    std::vector<uint8_t> src((size_t)pitch * h + (size_t)pitch * (h / 2), 0xFE);
    for (uint32_t r = 0; r < h; ++r)
      for (size_t i = 0; i < w; ++i) src[(size_t)r * pitch + i] = static_cast<uint8_t>(0x10 + r * 16 + i);
    uint8_t* uv = src.data() + (size_t)pitch * h;  // Y 平面之后
    for (uint32_t r = 0; r < h / 2; ++r)
      for (size_t i = 0; i < w; ++i) uv[(size_t)r * pitch + i] = static_cast<uint8_t>(0xE0 + r);
    std::vector<uint8_t> dst(xnc::Nv12Bytes(w, h), 0);
    xnc::CompactNv12Rows(src.data(), pitch, dst.data(), w, h);
    bool planes_ok = true;
    for (uint32_t r = 0; r < h && planes_ok; ++r)
      for (size_t i = 0; i < w; ++i)
        if (dst[(size_t)r * w + i] != static_cast<uint8_t>(0x10 + r * 16 + i)) { planes_ok = false; break; }
    for (uint32_t r = 0; r < h / 2 && planes_ok; ++r)
      for (size_t i = 0; i < w; ++i)
        if (dst[(size_t)w * h + (size_t)r * w + i] != static_cast<uint8_t>(0xE0 + r)) { planes_ok = false; break; }
    CHECK("nv12-planes-content", planes_ok);
    CHECK("nv12-planes-size", dst.size() == (size_t)w * h * 3 / 2);
    CHECK("nv12-planes-no-padding", std::find(dst.begin(), dst.end(), 0xFE) == dst.end());
  }
  { // gpu-readback: NV12 Y 平面 256 点非全等判定(NV12 版 first_frame 检查)
    const uint32_t w = 64, h = 48;
    std::vector<uint8_t> black((size_t)w * h * 3 / 2, 0);
    CHECK("nv12-y-uniform-black", !xnc::Nv12YNotUniform(black.data(), w, h));
    std::vector<uint8_t> same((size_t)w * h * 3 / 2, 0x66);
    CHECK("nv12-y-uniform-nonblack", !xnc::Nv12YNotUniform(same.data(), w, h));
    std::vector<uint8_t> grad((size_t)w * h * 3 / 2, 0);
    for (uint32_t y2 = 0; y2 < h; ++y2)
      for (uint32_t x = 0; x < w; ++x) grad[(size_t)y2 * w + x] = static_cast<uint8_t>(x);
    CHECK("nv12-y-gradient-nonuniform", xnc::Nv12YNotUniform(grad.data(), w, h));
  }
  { // 256 点采样非全等判定(诊断非全黑检查)
    const uint32_t w = 64, h = 48;
    std::vector<uint8_t> black((size_t)w * h * 4, 0);
    CHECK("sample-uniform-black", !xnc::SamplePointsNotUniform(black.data(), w, h));
    std::vector<uint8_t> same((size_t)w * h * 4, 0x77);  // 均一非黑仍全等
    CHECK("sample-uniform-nonblack", !xnc::SamplePointsNotUniform(same.data(), w, h));
    std::vector<uint8_t> grad((size_t)w * h * 4, 0);
    for (uint32_t y = 0; y < h; ++y)
      for (uint32_t x = 0; x < w; ++x) {
        uint8_t* px = grad.data() + ((size_t)y * w + x) * 4;
        px[0] = static_cast<uint8_t>(x); px[1] = static_cast<uint8_t>(y);
        px[2] = 0x40; px[3] = 0xFF;
      }
    CHECK("sample-gradient-nonuniform", xnc::SamplePointsNotUniform(grad.data(), w, h));
    // 单像素变化落在采样点 (i=128 → x=32,y=24) 上即可检出
    std::vector<uint8_t> one = black;
    uint8_t* px = one.data() + ((size_t)24 * w + 32) * 4;
    px[2] = 0xFF;
    CHECK("sample-single-sample-point-change", xnc::SamplePointsNotUniform(one.data(), w, h));
    CHECK("sample-degenerate-1x1-safe", !xnc::SamplePointsNotUniform(black.data(), 1, 1));
  }
  { // BGRA→NV12 (BT.601 有限范围,整数近似):合成 5 色竖条,条宽 8px
    // (2x2 色度子采样永不跨色),定点抽查 Y/U/V;期望值由 pixel_windows.go
    // 公式手算(red Y=82 U=90 V=240 / green Y=144 U=54 V=34 /
    // blue Y=41 U=240 V=110 / white Y=235 U=V=128 / black Y=16 U=V=128)
    const uint32_t w = 40, h = 8;
    const uint8_t bars[5][3] = {{0, 0, 255}, {0, 255, 0}, {255, 0, 0},
                                {255, 255, 255}, {0, 0, 0}};  // B,G,R
    std::vector<uint8_t> bgra((size_t)w * h * 4);
    for (uint32_t y = 0; y < h; ++y)
      for (uint32_t x = 0; x < w; ++x) {
        const uint8_t* c = bars[(x / 8) % 5];
        uint8_t* px = bgra.data() + ((size_t)y * w + x) * 4;
        px[0] = c[0]; px[1] = c[1]; px[2] = c[2]; px[3] = 0xFF;
      }
    CHECK("nv12-bytes", xnc::Nv12Bytes(w, h) == (size_t)w * h * 3 / 2);
    CHECK("nv12-bytes-odd-dims", xnc::Nv12Bytes(41, 8) == 0 && xnc::Nv12Bytes(40, 7) == 0);
    CHECK("nv12-bytes-overflow", xnc::Nv12Bytes(0xFFFFFFFFu, 0xFFFFFFFFu) == 0);
    std::vector<uint8_t> nv12((size_t)w * h * 3 / 2, 0xEE);
    CHECK("nv12-convert-ok", xnc::BgraToNv12(bgra.data(), bgra.size(), nv12.data(), nv12.size(), w, h));
    CHECK("nv12-convert-short-src", !xnc::BgraToNv12(bgra.data(), bgra.size() - 1, nv12.data(), nv12.size(), w, h));
    CHECK("nv12-convert-short-dst", !xnc::BgraToNv12(bgra.data(), bgra.size(), nv12.data(), nv12.size() - 1, w, h));
    const int exp[5][3] = {{82, 90, 240}, {144, 54, 34}, {41, 240, 110},
                           {235, 128, 128}, {16, 128, 128}};  // Y,U,V
    bool bars_ok = true;
    for (uint32_t k = 0; k < 5; ++k) {
      const size_t yidx = (size_t)4 * w + k * 8 + 4;         // 像素 (k*8+4, 4)
      const size_t uvidx = (size_t)w * h + 2 * w + (k * 4 + 2) * 2;  // 色度 (k*4+2, 2)
      const int y = nv12[yidx], u = nv12[uvidx], v = nv12[uvidx + 1];
      if (y < exp[k][0] - 3 || y > exp[k][0] + 3 || u < exp[k][1] - 3 ||
          u > exp[k][1] + 3 || v < exp[k][2] - 3 || v > exp[k][2] + 3) {
        std::printf("SELFTEST NOTE: bar %u Y=%d U=%d V=%d (want %d/%d/%d)\n",
                    k, y, u, v, exp[k][0], exp[k][1], exp[k][2]);
        bars_ok = false;
      }
    }
    CHECK("nv12-color-bars-yuv", bars_ok);
    { // 2x2 混色块:像素级竖条(偶 x=红,奇 x=绿)→ 单个色度块内
      // 2 红 + 2 绿,RGB 均值(127,127,0)→ U≈72 V≈137;Y 逐像素保留
      const uint32_t mw = 4, mh = 4;
      std::vector<uint8_t> m((size_t)mw * mh * 4);
      for (uint32_t y = 0; y < mh; ++y)
        for (uint32_t x = 0; x < mw; ++x) {
          uint8_t* px = m.data() + ((size_t)y * mw + x) * 4;
          px[3] = 0xFF;
          if ((x % 2) == 0) { px[2] = 255; }  // red
          else              { px[1] = 255; }  // green
        }
      std::vector<uint8_t> mn((size_t)mw * mh * 3 / 2);
      CHECK("nv12-mixed-convert", xnc::BgraToNv12(m.data(), m.size(), mn.data(), mn.size(), mw, mh));
      const int u = mn[mw * mh], v = mn[mw * mh + 1];
      CHECK("nv12-mixed-chroma-avg", u >= 70 && u <= 74 && v >= 135 && v <= 139);
      CHECK("nv12-mixed-y-red-green", mn[0] >= 79 && mn[0] <= 85 && mn[1] >= 141 && mn[1] <= 147);
    }
  }
  { // Annex-B NAL 解析(纯逻辑,与 encode_windows_test.go 同一向量):
    // SPS(7)+PPS(8)+IDR(5)+非 IDR(1)
    const uint8_t stream[] = {0, 0, 0, 1, 0x67, 0xAA, 0, 0, 0, 1, 0x68, 0xBB,
                              0, 0, 0, 1, 0x65, 0xCC, 0, 0, 0, 1, 0x41, 0xDD};
    const size_t n = sizeof(stream);
    CHECK("nal-has-idr", xnc::NalHasType(stream, n, 5));
    CHECK("nal-has-sps", xnc::NalHasType(stream, n, 7));
    CHECK("nal-has-pps", xnc::NalHasType(stream, n, 8));
    CHECK("nal-has-aud-absent", !xnc::NalHasType(stream, n, 9));
    const uint8_t types[] = {7, 8};
    std::vector<uint8_t> spspps;
    xnc::NalExtractTypes(stream, n, types, 2, &spspps);
    const uint8_t want[] = {0, 0, 0, 1, 0x67, 0xAA, 0, 0, 0, 1, 0x68, 0xBB};
    CHECK("nal-extract-spspps", spspps.size() == sizeof(want) &&
                               std::equal(spspps.begin(), spspps.end(), want));
    // 3 字节起始码归一化为 4 字节;IDR 提取只含 IDR
    const uint8_t sc3[] = {0x99, 0, 0, 1, 0x65, 0xCC};
    std::vector<uint8_t> idr;
    const uint8_t t5[] = {5};
    xnc::NalExtractTypes(sc3, sizeof(sc3), t5, 1, &idr);
    const uint8_t want2[] = {0, 0, 0, 1, 0x65, 0xCC};
    CHECK("nal-extract-normalizes-4b-startcode", idr.size() == sizeof(want2) &&
                                                std::equal(idr.begin(), idr.end(), want2));
  }
  // ---- MfSoftEncoder 场景 (a)-(d)(合成彩条 → MF 软编,无桌面依赖) ----
  const uint32_t kEncW = 128, kEncH = 96, kEncFps = 15, kEncBitrate = 500000;
  { // 参数护栏:未 Init 编码拒绝;短帧拒绝(不崩溃)
    xnc::MfSoftEncoder enc;
    std::vector<std::vector<uint8_t>> aus;
    std::string err;
    CHECK("mf-encode-before-init", !enc.Encode(nullptr, 0, aus, &err) && !err.empty());
    CHECK("mf-drain-before-init-noop", enc.Drain(aus) && aus.empty());
    xnc::MfSoftEncoder enc2;
    std::string ierr;
    CHECK("mf-init-odd-dims-rejected", !enc2.Init(127, 96, kEncFps, kEncBitrate, &ierr) && !ierr.empty());
  }
  { // (a) 编码 60 帧合成彩条 → ≥1 输出、首输出含 SPS/PPS+IDR;每个 AU 都有
    // VCL NAL;(d) SpsPps 首 IDR 后就绪且稳定、LastWasKey 与输出一致
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(kEncW, kEncH, kEncFps, kEncBitrate, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: mf-init-a err=%s\n", err.c_str());
    CHECK("mf-init-a", init_ok);
    if (init_ok) {
      SyntheticBars bars(kEncW, kEncH);
      std::vector<std::vector<uint8_t>> aus;
      bool ok = true, all_vcl = true, lastkey_consistent = true, p_only_seen = false;
      std::vector<uint8_t> spspps_at_first_idr;
      std::string e2;
      for (uint32_t i = 0; i < 60; ++i) {
        std::vector<std::vector<uint8_t>> frame_aus;
        if (!enc.Encode(bars.Frame(i), bars.Bytes(), frame_aus, &e2)) { ok = false; break; }
        bool call_has_idr = false, call_vcl = frame_aus.empty();
        for (const auto& au : frame_aus) {
          aus.push_back(au);
          const bool idr = xnc::NalHasType(au.data(), au.size(), 5);
          call_has_idr = call_has_idr || idr;
          call_vcl = idr || xnc::NalHasType(au.data(), au.size(), 1);
          if (idr && spspps_at_first_idr.empty() && !enc.SpsPps().empty())
            spspps_at_first_idr = enc.SpsPps();
        }
        if (!frame_aus.empty()) {
          lastkey_consistent = lastkey_consistent && (enc.LastWasKey() == call_has_idr);
          if (!call_has_idr) p_only_seen = true;
        }
        all_vcl = all_vcl && call_vcl;
      }
      CHECK("mf-a-encode-ok", ok);
      CHECK("mf-a-has-output", aus.size() >= 1);
      if (!aus.empty()) {
        CHECK("mf-a-first-sps", xnc::NalHasType(aus[0].data(), aus[0].size(), 7));
        CHECK("mf-a-first-pps", xnc::NalHasType(aus[0].data(), aus[0].size(), 8));
        CHECK("mf-a-first-idr", xnc::NalHasType(aus[0].data(), aus[0].size(), 5));
      }
      CHECK("mf-a-every-au-vcl", all_vcl);
      // (d) LastWasKey/SpsPps 一致性
      CHECK("mf-d-spspps-ready", spspps_at_first_idr.size() > 10);
      CHECK("mf-d-spspps-4b-sps-head",
            spspps_at_first_idr.size() >= 5 && spspps_at_first_idr[0] == 0 &&
            spspps_at_first_idr[1] == 0 && spspps_at_first_idr[2] == 0 &&
            spspps_at_first_idr[3] == 1 && (spspps_at_first_idr[4] & 0x1F) == 7);
      CHECK("mf-d-spspps-has-pps",
            xnc::NalHasType(spspps_at_first_idr.data(), spspps_at_first_idr.size(), 8));
      CHECK("mf-d-spspps-no-vcl",
            !xnc::NalHasType(spspps_at_first_idr.data(), spspps_at_first_idr.size(), 5) &&
            !xnc::NalHasType(spspps_at_first_idr.data(), spspps_at_first_idr.size(), 1));
      CHECK("mf-d-spspps-stable", enc.SpsPps() == spspps_at_first_idr);
      CHECK("mf-d-lastkey-consistent", lastkey_consistent);
      CHECK("mf-d-p-only-call-seen", p_only_seen);
      std::printf("SELFTEST NOTE: mf-a aus=%zu idrs=%zu\n", aus.size(),
                  CountAusWithNal(aus, 5));
    }
  }
  { // (g) GOP 行为探针:生产发现 ~1 IDR/s(编码器无视 GOP=300?)。纯合成
    // 图案(fps=30, 350 帧 > GOP=300)隔离内容因素:若 GOP 生效,IDR 只应
    // 出现在帧 1 与 ~301;若 ~1/s(≈30 帧一个),则编码器忽略/钳位 GOP。
    xnc::MfSoftEncoder enc;
    std::string err;
    const uint32_t gop_w = 128, gop_h = 96, gop_fps = 30, gop_bitrate = 2000000;
    const bool init_ok = enc.Init(gop_w, gop_h, gop_fps, gop_bitrate, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: mf-init-gop err=%s\n", err.c_str());
    CHECK("mf-init-gop", init_ok);
    if (init_ok) {
      SyntheticBars bars(gop_w, gop_h);
      std::vector<uint32_t> idr_positions;
      std::string e2;
      for (uint32_t i = 0; i < 350; ++i) {
        std::vector<std::vector<uint8_t>> frame_aus;
        if (!enc.Encode(bars.Frame(i), bars.Bytes(), frame_aus, &e2)) break;
        for (const auto& au : frame_aus)
          if (xnc::NalHasType(au.data(), au.size(), 5)) idr_positions.push_back(i);
      }
      std::printf("SELFTEST NOTE: mf-gop-probe fps=%u frames=350 idrs=%zu at=[",
                  gop_fps, idr_positions.size());
      for (size_t k = 0; k < idr_positions.size(); ++k)
        std::printf("%s%u", k ? " " : "", idr_positions[k]);
      std::printf("]\n");
      CHECK("mf-gop-probe-ran", !idr_positions.empty());
    }
  }
  { // (b) force-key 契约(E2 关键帧风暴回归):冷启动缓冲期对连续 5 帧只
    // 调一次 ForceNextIdr + Drain 排空 → 输出中 IDR 恰 1 个。若契约破坏
    // (以「未见关键帧输出」为由每次 Encode 重复置位)→ 多个 IDR → FAIL。
    // 注:CMSH264EncoderMFT 有 ~17 帧内部缓冲(实测),5 帧内无输出;Drain
    // 排不穿前瞻窗口,故在零额外 force 的前提下补喂帧直至首输出再 Drain
    // (本机实测:此 MFT 输出严格 1:1 按输入顺序,首个 AU=首帧)。
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(kEncW, kEncH, kEncFps, kEncBitrate, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: mf-init-b err=%s\n", err.c_str());
    CHECK("mf-init-b", init_ok);
    if (init_ok) {
      enc.ForceNextIdr("selftest-cold-start");  // 唯一一次
      SyntheticBars bars(kEncW, kEncH);
      std::vector<std::vector<uint8_t>> aus;    // Encode 追加 + Drain 追加
      std::string e2;
      bool ok = true;
      for (uint32_t i = 0; i < 5 && ok; ++i) {  // 计划规定的连续 5 帧
        std::vector<std::vector<uint8_t>> frame_aus;
        if (!enc.Encode(bars.Frame(i), bars.Bytes(), frame_aus, &e2)) { ok = false; break; }
        aus.insert(aus.end(), frame_aus.begin(), frame_aus.end());
      }
      // 冷启动仍无输出(前瞻缓冲):继续喂帧,绝不二次 force
      for (uint32_t i = 5; ok && i < 45 && aus.empty(); ++i) {
        std::vector<std::vector<uint8_t>> frame_aus;
        if (!enc.Encode(bars.Frame(i), bars.Bytes(), frame_aus, &e2)) { ok = false; break; }
        aus.insert(aus.end(), frame_aus.begin(), frame_aus.end());
      }
      CHECK("mf-b-encode-ok", ok);
      if (ok) {
        ok = enc.Drain(aus, &e2);  // 重复 ProcessOutput 至 NEED_MORE_INPUT
        CHECK("mf-b-drain-ok", ok);
        // 首输出后再喂 5 帧(仍零额外 force):正确实现只应追 P 帧
        for (uint32_t i = 45; ok && i < 50; ++i) {
          std::vector<std::vector<uint8_t>> frame_aus;
          if (!enc.Encode(bars.Frame(i), bars.Bytes(), frame_aus, &e2)) { ok = false; break; }
          aus.insert(aus.end(), frame_aus.begin(), frame_aus.end());
        }
        CHECK("mf-b-postdrain-encode-ok", ok);
        CHECK("mf-b-has-output", !aus.empty());
        const size_t idrs = CountAusWithNal(aus, 5);
        std::printf("SELFTEST NOTE: mf-b aus=%zu idrs=%zu\n", aus.size(), idrs);
        CHECK("mf-b-exactly-one-idr", idrs == 1);
      }
    }
  }
  { // (c) 稳态第 30 帧提交前 ForceNextIdr → 该帧自己的 AU(0 基第 30 个
    // 输出,1:1 顺序映射)= IDR,且整个 50 帧无第三个 IDR(一次性消费)
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(kEncW, kEncH, kEncFps, kEncBitrate, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: mf-init-c err=%s\n", err.c_str());
    CHECK("mf-init-c", init_ok);
    if (init_ok) {
      SyntheticBars bars(kEncW, kEncH);
      std::vector<std::vector<uint8_t>> aus;
      size_t idrs = 0;
      bool lastkey_captured = false, lastkey_on_forced = false, ok = true;
      std::string e2;
      for (uint32_t i = 0; i < 50; ++i) {
        if (i == 30) enc.ForceNextIdr("selftest-steady-30");
        std::vector<std::vector<uint8_t>> frame_aus;
        if (!enc.Encode(bars.Frame(i), bars.Bytes(), frame_aus, &e2)) { ok = false; break; }
        for (const auto& au : frame_aus) {
          aus.push_back(au);
          if (xnc::NalHasType(au.data(), au.size(), 5)) ++idrs;
        }
        // 第二个 IDR 出现的那个 Encode 调用:LastWasKey 必须为 true
        if (!frame_aus.empty() && idrs == 2 && !lastkey_captured) {
          lastkey_on_forced = enc.LastWasKey();
          lastkey_captured = true;
        }
      }
      CHECK("mf-c-encode-ok", ok);
      CHECK("mf-c-min-aus-for-mapping", aus.size() >= 32);  // AU#30 及邻帧已产出
      const size_t want_idx = 30;
      CHECK("mf-c-idr-total-2", idrs == 2);
      if (aus.size() > want_idx + 1) {
        const bool idr30 = xnc::NalHasType(aus[want_idx].data(), aus[want_idx].size(), 5);
        const bool idr29 = xnc::NalHasType(aus[29].data(), aus[29].size(), 5);
        const bool idr31 = xnc::NalHasType(aus[31].data(), aus[31].size(), 5);
        if (!(idr30 && !idr29 && !idr31) || idrs != 2)
          std::printf("SELFTEST NOTE: mf-c aus=%zu idrs=%zu au29=%d au30=%d au31=%d\n",
                      aus.size(), idrs, idr29 ? 1 : 0, idr30 ? 1 : 0, idr31 ? 1 : 0);
        // 下一输出(= 第 30 帧的 AU)为 IDR,邻帧不是
        CHECK("mf-c-forced-frame-au-is-idr", idr30 && !idr29 && !idr31);
      }
      CHECK("mf-c-lastkey-on-forced-idr", lastkey_on_forced);
      // (d) 稳态下 SpsPps 持续可用且不变
      CHECK("mf-c-spspps-stable-nonempty", enc.SpsPps().size() > 10);
      std::printf("SELFTEST NOTE: mf-c aus=%zu idrs=%zu\n", aus.size(), idrs);
    }
  }
  // ---- Task 5:FrameCache 状态机(spec §7.5;纯逻辑,无编码器) ----
  {
    xnc::FrameCache c;
    CHECK("fc-init-state", c.state() == xnc::FrameCache::State::kInit &&
                               std::strcmp(c.StateName(), "INIT") == 0 && c.NeedsBaseFrame());
    CHECK("fc-no-pending-before-rebuild", c.TakePendingIdrReason() == nullptr);
    CHECK("fc-counters-zero-default", c.counters().captured == 0 && c.counters().encoded == 0 &&
          c.counters().keyframes == 0 && c.counters().timeouts == 0 &&
          c.counters().warmup_feeds == 0 && c.counters().rebuilds == 0);
    c.Start();
    CHECK("fc-start-wait-base", c.state() == xnc::FrameCache::State::kWaitBaseFrame &&
                                    std::strcmp(c.StateName(), "WAIT_BASE_FRAME") == 0 &&
                                    c.NeedsBaseFrame());
    CHECK("fc-start-idempotent-after-init", (c.Start(), c.state() == xnc::FrameCache::State::kWaitBaseFrame));
    CHECK("fc-first-frame-is-base", c.OnCapturedFrame() &&
                                        c.state() == xnc::FrameCache::State::kHaveBase &&
                                        std::strcmp(c.StateName(), "HAVE_BASE") == 0 &&
                                        !c.NeedsBaseFrame() && c.counters().captured == 1);
    CHECK("fc-second-frame-incremental", !c.OnCapturedFrame() &&
                                             c.state() == xnc::FrameCache::State::kIncremental &&
                                             std::strcmp(c.StateName(), "INCREMENTAL") == 0);
    c.OnTimeout();
    c.OnTimeout();
    CHECK("fc-timeout-counts-no-state-change",
          c.counters().timeouts == 2 && c.state() == xnc::FrameCache::State::kIncremental);
    // warm-up bookkeeping invariant: encoded == captured + warmup_feeds
    c.OnEncoded();
    c.OnWarmupFeed();
    CHECK("fc-warmup-feed-counts-encoded",
          c.counters().encoded == 2 && c.counters().warmup_feeds == 1);
    CHECK("fc-no-keyframe-yet", !c.HaveKeyframe());
    c.OnKeyframeAu();
    CHECK("fc-keyframe-ends-warmup", c.HaveKeyframe() && c.counters().keyframes == 1);
    // rebuild:回 WAIT_BASE_FRAME + 一次性 "rebuild" IDR 请求;计数累计保留
    c.OnRebuild();
    CHECK("fc-rebuild-rewinds-state", c.state() == xnc::FrameCache::State::kWaitBaseFrame &&
                                          c.NeedsBaseFrame() && !c.HaveKeyframe());
    CHECK("fc-rebuild-counters-cumulative",
          c.counters().rebuilds == 1 && c.counters().captured == 2 &&
          c.counters().timeouts == 2 && c.counters().keyframes == 1);
    const char* reason = c.TakePendingIdrReason();
    CHECK("fc-rebuild-idr-reason", reason != nullptr && std::strcmp(reason, "rebuild") == 0);
    CHECK("fc-rebuild-idr-reason-one-shot", c.TakePendingIdrReason() == nullptr);
    CHECK("fc-post-rebuild-frame-is-base",
          c.OnCapturedFrame() && c.state() == xnc::FrameCache::State::kHaveBase);
    // 防御:未 Start 直接喂帧 → 仍按 base 处理(首帧全量语义不依赖调用顺序)
    xnc::FrameCache c2;
    CHECK("fc-unstarted-first-frame-base", c2.OnCapturedFrame());
    // CaptureReset 二连发:每次重建都重新武装一次性请求
    xnc::FrameCache c3;
    c3.Start();
    c3.OnCapturedFrame();
    c3.OnRebuild();
    c3.OnRebuild();
    CHECK("fc-double-rebuild-counts", c3.counters().rebuilds == 2);
    CHECK("fc-double-rebuild-one-reason", c3.TakePendingIdrReason() != nullptr &&
                                              c3.TakePendingIdrReason() == nullptr);
  }
  {
    xnc::LatestFrameStore latest;
    latest.Update(MakeSolidFrame(1, 0x11));
    latest.Update(MakeSolidFrame(2, 0x22));
    latest.Update(MakeSolidFrame(3, 0x33));
    xnc::FrameBlob got;
    CHECK("latest-frame-snapshot", latest.Snapshot(&got) &&
                                       got.mono_us == 3 && got.bgra[0] == 0x33);
    uint64_t generation = 0;
    CHECK("latest-frame-generation-snapshot",
          latest.Snapshot(&got, &generation));
    latest.Invalidate();
    CHECK("latest-frame-reset-blocks-feed", !latest.Snapshot(&got));
    latest.Update(MakeSolidFrame(4, 0x44));
    CHECK("latest-frame-old-generation-rejected",
          !latest.IsCurrent(generation));
  }
  { // pipeline-decouple:FrameQueue 交接队列(有界深度 2,满则丢最旧保最新,
    // FIFO 顺序不重排;Shutdown 唤醒等待者且排空后才拒绝)
    xnc::FrameQueue q(2);
    auto mk = [](uint64_t mono) {
      xnc::FrameBlob f;
      f.mono_us = mono;
      f.bgra.assign(4, static_cast<uint8_t>(mono));
      return f;
    };
    CHECK("fq-empty-initially", q.Empty() && q.Size() == 0 && !q.Done());
    xnc::FrameBlob out;
    CHECK("fq-pop-empty-fails", !q.TryPop(&out));
    q.Push(mk(1));
    q.Push(mk(2));
    q.Push(mk(3));  // depth 2: frame 1 is evicted - the LATEST is kept
    CHECK("fq-bounded-depth", q.Size() == 2);
    CHECK("fq-drop-oldest", q.TryPop(&out) && out.mono_us == 2);
    CHECK("fq-second", q.TryPop(&out) && out.mono_us == 3);
    CHECK("fq-drained", q.Empty());
    // Drop 只发生在前端(最旧),保序:FIFO 弹序单调
    q.Push(mk(4));
    q.Push(mk(5));
    q.Push(mk(6));
    q.Push(mk(7));  // evicts 4 then 5
    CHECK("fq-order-preserved", q.TryPop(&out) && out.mono_us == 6 &&
                                    q.TryPop(&out) && out.mono_us == 7);
    // Shutdown:唤醒等待者;已排队帧仍可弹出(排空语义),空后返回 false
    q.Shutdown();
    CHECK("fq-done-flag", q.Done());
    q.Push(mk(8));  // 关闭后仍入队(编码线程排空阶段)
    CHECK("fq-drain-after-shutdown", q.TryPop(&out) && out.mono_us == 8 &&
                                         q.WaitPop(&out, 10) == false && q.Empty());
  }
  { // Task 5:VclNalus/ShapeAu 码流整形(vclNALUs 语义移植:丢 7/8/9,
    // 4 字节起始码归一,尾零回退;IDR AU = 缓存 SpsPps + VCL)
    const uint8_t au[] = {0, 0, 0, 1, 0x09, 0xF0,                     // AUD(9) 4B
                          0, 0, 1, 0x67, 0xAA,                          // SPS(7) 3B
                          0, 0, 0, 1, 0x68, 0xBB,                       // PPS(8) 4B
                          0, 0, 0, 1, 0x65, 0xCC, 0, 0,                 // IDR(5) + 尾零
                          0, 0, 1, 0x06, 0xDD};                         // SEI(6) 3B
    std::vector<uint8_t> vcl;
    xnc::VclNalus(au, sizeof(au), &vcl);
    const uint8_t want[] = {0, 0, 0, 1, 0x65, 0xCC, 0, 0, 0, 1, 0x06, 0xDD};
    CHECK("vcl-drops-aud-sps-pps-keeps-idr-sei",
          vcl.size() == sizeof(want) && std::equal(vcl.begin(), vcl.end(), want));
    std::vector<uint8_t> empty;
    const uint8_t none[] = {0x11, 0x22, 0x33};
    xnc::VclNalus(none, sizeof(none), &empty);
    CHECK("vcl-no-startcode-empty", empty.empty());
    xnc::VclNalus(nullptr, 0, &empty);
    CHECK("vcl-null-noop", empty.empty());
    std::vector<uint8_t> appended;
    appended.push_back(0xEE);  // 追加语义(与 NalExtractTypes 一致)
    const uint8_t p[] = {0, 0, 1, 0x41, 0x05};
    xnc::VclNalus(p, sizeof(p), &appended);
    CHECK("vcl-appends-and-normalizes",
          appended.size() == 7 && appended[0] == 0xEE && appended[1] == 0 &&
          appended[2] == 0 && appended[3] == 0 && appended[4] == 1 && appended[5] == 0x41 &&
          appended[6] == 0x05);
    // ShapeAu:IDR → 缓存参数集前置 + VCL;非 IDR → 仅 VCL
    std::vector<uint8_t> spspps = {0, 0, 0, 1, 0x67, 0xAA, 0, 0, 0, 1, 0x68, 0xBB};
    std::vector<uint8_t> shaped;
    xnc::ShapeAu(au, sizeof(au), true, spspps, &shaped);
    const uint8_t want_idr[] = {0, 0, 0, 1, 0x67, 0xAA, 0, 0, 0, 1, 0x68, 0xBB,
                                0, 0, 0, 1, 0x65, 0xCC, 0, 0, 0, 1, 0x06, 0xDD};
    CHECK("shape-au-idr-prefixed", shaped.size() == sizeof(want_idr) &&
                                      std::equal(shaped.begin(), shaped.end(), want_idr));
    xnc::ShapeAu(au, sizeof(au), false, spspps, &shaped);
    CHECK("shape-au-nonidr-no-prefix",
          shaped.size() == 12 && shaped[4] == 0x65 && shaped[10] == 0x06);
  }
  { // Task 5:stats.json 字段(同每秒日志字段 + duration/w/h/bitrate)
    xnc::PipelineResult r;
    r.counters.captured = 3;
    r.counters.encoded = 5;
    r.counters.keyframes = 1;
    r.counters.timeouts = 7;
    r.counters.warmup_feeds = 2;
    r.counters.rebuilds = 1;
    r.width = 64;
    r.height = 48;
    r.aus_written = 6;
    r.bytes_written = 1234;
    r.resets = 2;
    xnc::PipelineOpts o;
    o.duration_s = 2;
    o.fps = 15;
    o.target_bitrate_bps = 2300000;
    const std::string j = xnc::FormatStatsJson(r, o);
    CHECK("statsjson-duration", j.find("\"duration_s\": 2") != std::string::npos);
    CHECK("statsjson-dims", j.find("\"width\": 64") != std::string::npos &&
                                j.find("\"height\": 48") != std::string::npos);
    CHECK("statsjson-bitrate", j.find("\"bitrate_bps\": 2300000") != std::string::npos);
    CHECK("statsjson-counters", j.find("\"captured\": 3") != std::string::npos &&
                                    j.find("\"encoded\": 5") != std::string::npos &&
                                    j.find("\"keyframes\": 1") != std::string::npos &&
                                    j.find("\"timeouts\": 7") != std::string::npos &&
                                    j.find("\"warmup_feeds\": 2") != std::string::npos &&
                                    j.find("\"rebuilds\": 1") != std::string::npos);
    CHECK("statsjson-aus-bytes", j.find("\"aus_written\": 6") != std::string::npos &&
                                     j.find("\"bytes_written\": 1234") != std::string::npos);
    CHECK("statsjson-resets", j.find("\"resets\": 2") != std::string::npos);
    // 文件系统路径(也是捕获/编码器初始化失败分支写零计数 sidecar 的路径):
    // sidecar 落在 h264 路径同目录、名字固定 stats.json,内容可回读
    wchar_t dir[MAX_PATH] = L"";
    const UINT dn = GetTempPathW(MAX_PATH, dir);
    CHECK("statsjson-tempdir", dn > 0 && dn < MAX_PATH);
    if (dn > 0 && dn < MAX_PATH) {
      wchar_t hp[MAX_PATH];
      if (swprintf_s(hp, L"%sxnc-selftest-%lu-stats.h264", dir,
                     static_cast<unsigned long>(GetCurrentProcessId())) > 0) {
        std::wstring werr;
        CHECK("statsjson-write-ok", xnc::WriteStatsJson(hp, r, o, &werr));
        wchar_t sp[MAX_PATH];
        swprintf_s(sp, L"%sstats.json", dir);
        FILE* sf = nullptr;
        if (_wfopen_s(&sf, sp, L"rb") == 0 && sf != nullptr) {
          const std::vector<uint8_t> content = ReadAll(sf);
          std::fclose(sf);
          DeleteFileW(sp);
          const std::string text(content.begin(), content.end());
          CHECK("statsjson-sidecar-content",
                text.find("\"keyframes\": 1") != std::string::npos &&
                    text.find("\"duration_s\": 2") != std::string::npos &&
                    !text.empty() && text.back() == '\n');
        } else {
          CHECK("statsjson-sidecar-open", false);
        }
      }
    }
  }
  { // M2 Task 6: bounded stage histograms (pipeline.h). Pure math on
    // synthetic samples: nearest-rank percentiles, the ring's
    // keep-newest bound, zero-sample ABSENCE (never fake-zero), and the
    // log/sidecar rendering. Runs in BOTH selftest modes - the V2
    // scenarios only wire the same helper into the media loop.
    uint64_t v = 0;
    xnc::StageHistogram s("s_us", 128);
    CHECK("hist-empty", s.Empty() && s.Count() == 0);
    CHECK("hist-empty-percentile-rejected", !s.Percentile(50.0, &v));
    CHECK("hist-empty-summary-invalid", !s.Summary().has_samples);
    for (uint64_t i = 1; i <= 100; ++i) s.Add(i);
    CHECK("hist-count", s.Count() == 100);
    // Nearest-rank: rank(p) = ceil(p*n/100), value = rank-th smallest.
    // n=100: p50 -> 50th (50), p95 -> 95th (95), p99 -> 99th (99), p100 -> max.
    CHECK("hist-p50-nearest-rank", s.Percentile(50.0, &v) && v == 50);
    CHECK("hist-p95-nearest-rank", s.Percentile(95.0, &v) && v == 95);
    CHECK("hist-p99-nearest-rank", s.Percentile(99.0, &v) && v == 99);
    CHECK("hist-p100-max", s.Percentile(100.0, &v) && v == 100);
    // Unsorted input must not matter.
    xnc::StageHistogram u("u_us", 128);
    for (const uint64_t x : {5ull, 1ull, 4ull, 2ull, 3ull}) u.Add(x);
    // n=5: p50 -> ceil(2.5)=3rd (3); p95 -> ceil(4.75)=5th (5).
    CHECK("hist-unsorted-p50", u.Percentile(50.0, &v) && v == 3);
    CHECK("hist-unsorted-p95", u.Percentile(95.0, &v) && v == 5);
    CHECK("hist-unsorted-monotone-summary",
          [&] {
            const auto p = u.Summary();
            return p.has_samples && p.n == 5 && p.p50 <= p.p95 &&
                   p.p95 <= p.p99 && p.p50 == 3 && p.p95 == 5 && p.p99 == 5;
          }());
    // Single sample: every percentile is that sample.
    xnc::StageHistogram one("one_us", 4);
    one.Add(7);
    CHECK("hist-single", one.Percentile(50.0, &v) && v == 7 &&
                             one.Percentile(99.0, &v) && v == 7);
    // Capacity bound: the ring keeps the NEWEST samples (drop-oldest).
    xnc::StageHistogram ring("ring_us", 4);
    for (uint64_t i = 1; i <= 6; ++i) ring.Add(i);  // retains 3,4,5,6
    CHECK("hist-ring-keeps-newest",
          ring.Count() == 4 && ring.Percentile(50.0, &v) && v == 4 &&
              ring.Percentile(95.0, &v) && v == 6);
    ring.Reset();
    CHECK("hist-reset", ring.Empty() && !ring.Percentile(50.0, &v));
    // Rendering: log line + sidecar members; zero-sample stages ABSENT.
    xnc::StageHistogram a("a_us", 8), b("b_us", 8), z("z_us", 8);
    for (const uint64_t x : {10ull, 20ull, 30ull, 40ull}) a.Add(x);
    b.Add(1);
    b.Add(2);
    const xnc::StageStat stats[] = {
        {"a_us", a.Summary()}, {"z_us", z.Summary()}, {"b_us", b.Summary()}};
    const std::string log_line = xnc::FormatStageLog(stats, 3);
    CHECK("hist-log-omits-zero-sample",
          log_line.find("z_us") == std::string::npos &&
              log_line.find("a_us=20/40/40(n=4)") != std::string::npos &&
              log_line.find("b_us=1/2/2(n=2)") != std::string::npos);
    const std::string json = xnc::FormatStagesJson(stats, 3, 7, "software");
    CHECK("hist-json-absent-when-zero",
          json.find("z_us") == std::string::npos);
    CHECK("hist-json-members",
          json.find("\"a_us\": { \"n\": 4, \"p50\": 20, \"p95\": 40, \"p99\": 40 }") !=
              std::string::npos &&
              json.find("\"b_us\": { \"n\": 2, \"p50\": 1, \"p95\": 2, \"p99\": 2 }") !=
                  std::string::npos);
    CHECK("hist-json-block-shape",
          json.find("\"stages\": {") != std::string::npos &&
              json.find("},") != std::string::npos);
    CHECK("hist-json-readback-count",
          json.find("\"cpu_readbacks\": 7") != std::string::npos);
    // Fix round 1: the sidecar must carry the semantics + rung labels in
    // the DURABLE output - a stage_semantics member (wait-inclusive /
    // enqueue-cost wording from StageSemanticsNote) and the encoder_backend
    // that makes cpu_readbacks interpretable.
    CHECK("hist-json-encoder-backend",
          json.find("\"encoder_backend\": \"software\"") != std::string::npos);
    CHECK("hist-json-stage-semantics",
          json.find("\"stage_semantics\": ") != std::string::npos &&
              json.find(xnc::StageSemanticsNote()) != std::string::npos &&
              std::strstr(xnc::StageSemanticsNote(), "wait-inclusive") !=
                  nullptr &&
              std::strstr(xnc::StageSemanticsNote(), "enqueue") != nullptr);
    // The multi-stage serialization is pinned byte-for-byte: the substring
    // checks above cannot see comma/brace shape (a trailing comma before
    // the closing brace is invisible to find()), exact equality is not.
    const std::string golden =
        std::string("  \"stages\": {\n"
                    "    \"stage_semantics\": \"") +
        xnc::StageSemanticsNote() +
        "\",\n"
        "    \"a_us\": { \"n\": 4, \"p50\": 20, \"p95\": 40, \"p99\": 40 },\n"
        "    \"b_us\": { \"n\": 2, \"p50\": 1, \"p95\": 2, \"p99\": 2 }\n"
        "  },\n"
        "  \"cpu_readbacks\": 7,\n"
        "  \"encoder_backend\": \"software\",\n";
    CHECK("hist-json-golden", json == golden);
    // Sidecar splice: stages land BEFORE "ok"; an empty block keeps the
    // M0 stats.json byte-identical (no "stages" key at all).
    xnc::PipelineResult r2;
    xnc::PipelineOpts o2;
    r2.stages_json = xnc::FormatStagesJson(stats, 3, 0, "factory");
    const std::string with = xnc::FormatStatsJson(r2, o2);
    CHECK("statsjson-stages-spliced",
          with.find("\"stages\": {") != std::string::npos &&
              with.find("\"ok\"") > with.find("\"stages\""));
    CHECK("statsjson-backend-spliced",
          with.find("\"encoder_backend\": \"factory\"") != std::string::npos);
    r2.stages_json.clear();
    CHECK("statsjson-m0-unchanged",
          xnc::FormatStatsJson(r2, o2).find("\"stages\"") ==
              std::string::npos);
  }
  { // Task 5:致命 Acquire 错误 → Run 立即失败(未初始化编码器也安全)
    struct FatalCapture final : xnc::ICapture {
      bool Acquire(xnc::FrameBlob&, std::string* err = nullptr,
                   uint32_t = 0) override {
        if (err) *err = "err_fatal_probe";
        return false;
      }
      uint32_t Width() const override { return 64; }
      uint32_t Height() const override { return 48; }
    };
    FatalCapture cap;
    xnc::MfSoftEncoder uninit;  // fatal 在首次提交前发生,编码器从未被调用
    xnc::PipelineOpts o;
    o.duration_s = 1;
    o.fps = 15;
    TempBinFile tf;
    CHECK("pipe-fatal-tmpfile", tf.Open(0));
    if (tf.get() != nullptr) {
      xnc::PipelineResult res = xnc::Pipeline::Run(cap, uninit, tf.get(), o);
      CHECK("pipe-fatal-not-ok", !res.ok);
      CHECK("pipe-fatal-err-propagated", res.err.find("err_fatal_probe") != std::string::npos);
      CHECK("pipe-fatal-no-aus", res.aus_written == 0 && res.bytes_written == 0);
      CHECK("pipe-fatal-timeout-zero", res.counters.timeouts == 0);
    }
  }
  // ---- Task 5:FlushTail(尾帧不丢;Task 4 评审遗留项) ----
  {
    xnc::MfSoftEncoder uninit;
    std::vector<std::vector<uint8_t>> noop;
    CHECK("mf-flush-before-init-noop",
          uninit.FlushTail(noop) && noop.empty());  // 与 Drain 同样的防御
  }
  {
    // 直接喂 20 帧(无 force、无 Drain):冷启动 ~17 帧前瞻 → 期间至多 3 AU
    // 自然产出;FlushTail 排空剩余 → 总数应恢复到全部帧(尾帧不丢)。
    const uint32_t w = 64, h = 48, fps = 15;
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(w, h, fps, 500000, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: mf-init-flush err=%s\n", err.c_str());
    CHECK("mf-flush-init", init_ok);
    if (init_ok) {
      SyntheticBars bars(w, h);
      std::vector<std::vector<uint8_t>> aus;
      std::string e2;
      bool ok = true;
      for (uint32_t i = 0; i < 20 && ok; ++i) {
        std::vector<std::vector<uint8_t>> frame_aus;
        if (!enc.Encode(bars.Frame(i), bars.Bytes(), frame_aus, &e2)) { ok = false; break; }
        aus.insert(aus.end(), frame_aus.begin(), frame_aus.end());
      }
      CHECK("mf-flush-feed-ok", ok);
      if (ok) {
        const size_t during = aus.size();
        const bool flush_ok = enc.FlushTail(aus, &e2);  // 追加
        CHECK("mf-flush-tail-ok", flush_ok);
        const size_t flushed = aus.size() - during;
        std::printf("SELFTEST NOTE: mf-flush during=%zu flushed=%zu total=%zu\n",
                    during, flushed, aus.size());
        CHECK("mf-flush-total-not-dropped", aus.size() >= 3);  // 计划下限 20-17
        // 实测(与本 selftest 同机型/SDK):CMSH264EncoderMFT 严格 1:1 顺序
        // 输出(Task 4),20 帧喂入 → 恰 20 AU;FlushTail 丢失任何尾帧即 FAIL
        CHECK("mf-flush-total-exact-1to1", aus.size() == 20);
        CHECK("mf-flush-total-bounded", aus.size() <= 20);
        // 首个输出 AU 含 SPS+PPS+IDR(任务措辞=包含;原始 AU 的 NAL 顺序
        // 由 MFT 决定(AUD/SEI 可能前置),顺序契约由整形后的管线流断言)
        CHECK("mf-flush-first-au-keyframe", !aus.empty() &&
                  xnc::NalHasType(aus[0].data(), aus[0].size(), 7) &&
                  xnc::NalHasType(aus[0].data(), aus[0].size(), 8) &&
                  xnc::NalHasType(aus[0].data(), aus[0].size(), 5));
        const std::vector<uint8_t> first_types =
            StreamNalTypes(aus[0].data(), aus[0].size(), 8);
        std::printf("SELFTEST NOTE: mf-flush first-au-nal=%zu types:",
                    first_types.size());
        for (const uint8_t t : first_types) std::printf(" %u", t);
        std::printf("\n");
        size_t vcl = 0;
        for (const auto& au : aus)
          if (xnc::NalHasType(au.data(), au.size(), 1) ||
              xnc::NalHasType(au.data(), au.size(), 5)) ++vcl;
        CHECK("mf-flush-every-au-vcl", vcl == aus.size());
      }
    }
  }
  { // (e) gpu-readback: EncodeNV12 直接编码 NV12(合成 NV12 帧,跳过
    // BGRA->NV12)——输出契约与 Encode 一致:首 AU 含 SPS+PPS+IDR、全 VCL;
    // 短帧/未初始化拒绝;与 Encode 的 BGRA 路径产出等价(同源彩条)。
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(kEncW, kEncH, kEncFps, kEncBitrate, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: mf-init-e err=%s\n", err.c_str());
    CHECK("mf-init-e", init_ok);
    if (init_ok) {
      // 未初始化拒绝(与 Encode 的 mf-encode-before-init 同一契约)
      xnc::MfSoftEncoder uninit;
      std::vector<std::vector<uint8_t>> guard_aus;
      std::string guard_err;
      CHECK("mf-e-nv12-before-init-guard",
            !uninit.EncodeNV12(nullptr, 0, guard_aus, &guard_err) && !guard_err.empty());
      SyntheticBars bars(kEncW, kEncH);
      std::vector<uint8_t> nv12(xnc::Nv12Bytes(kEncW, kEncH));
      std::vector<std::vector<uint8_t>> aus;
      bool ok = true, all_vcl = true;
      std::string e2;
      for (uint32_t i = 0; i < 30; ++i) {
        std::vector<std::vector<uint8_t>> frame_aus;
        if (!xnc::BgraToNv12(bars.Frame(i), bars.Bytes(), nv12.data(), nv12.size(), kEncW, kEncH)) {
          ok = false; break;
        }
        if (!enc.EncodeNV12(nv12.data(), nv12.size(), frame_aus, &e2)) { ok = false; break; }
        for (const auto& au : frame_aus) {
          aus.push_back(au);
          all_vcl = all_vcl && (xnc::NalHasType(au.data(), au.size(), 5) ||
                                xnc::NalHasType(au.data(), au.size(), 1));
        }
      }
      CHECK("mf-e-encode-ok", ok);
      CHECK("mf-e-has-output", aus.size() >= 1);
      if (!aus.empty()) {
        CHECK("mf-e-first-sps", xnc::NalHasType(aus[0].data(), aus[0].size(), 7));
        CHECK("mf-e-first-pps", xnc::NalHasType(aus[0].data(), aus[0].size(), 8));
        CHECK("mf-e-first-idr", xnc::NalHasType(aus[0].data(), aus[0].size(), 5));
      }
      CHECK("mf-e-every-au-vcl", all_vcl);
      // NOTE 必须在短帧拒绝检查之前:EncodeNV12 会先清空目标 aus
      std::printf("SELFTEST NOTE: mf-e aus=%zu idrs=%zu\n", aus.size(),
                  CountAusWithNal(aus, 5));
      // 短帧/越界拒绝(用独立 scratch,不污染已收集的 aus)
      std::vector<std::vector<uint8_t>> scratch;
      CHECK("mf-e-short-frame-rejected",
            !enc.EncodeNV12(nv12.data(), nv12.size() - 1, scratch, &e2) && !e2.empty());
      CHECK("mf-e-short-frame-rejected-empty", scratch.empty());
    }
  }
  // ---- Task 5:Pipeline 端到端(FAKE ICapture + 真 MfSoftEncoder,64x48@15) ----
  {
    // 场景 1 warm-up(§7.4 修订):3 帧后静止 → 无关键帧输出前重喂 base,
    // 上限内收敛,绝不二次 force → 恰 1 个 IDR;首 AU = SPS+PPS+IDR
    const uint32_t w = 64, h = 48, fps = 15;
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(w, h, fps, 500000, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: mf-init-pipe-warm err=%s\n", err.c_str());
    CHECK("pipe-warm-init", init_ok);
    if (init_ok) {
      ScriptedCapture cap(w, h, 3);
      xnc::PipelineOpts o;
      o.duration_s = 2;
      o.fps = fps;
      o.target_bitrate_bps = 500000;
      TempBinFile tf;
      CHECK("pipe-warm-tmpfile", tf.Open(1));
      if (tf.get() != nullptr) {
        RecordingSink sink;
        xnc::PipelineResult res = xnc::Pipeline::Run(cap, enc, tf.get(), sink, o);
        const xnc::FrameCacheCounters& c = res.counters;
        std::printf("SELFTEST NOTE: pipe-warm ok=%d captured=%llu encoded=%llu feeds=%llu timeouts=%llu keyframes=%llu aus=%llu bytes=%llu\n",
                    res.ok ? 1 : 0, (unsigned long long)c.captured,
                    (unsigned long long)c.encoded, (unsigned long long)c.warmup_feeds,
                    (unsigned long long)c.timeouts, (unsigned long long)c.keyframes,
                    (unsigned long long)res.aus_written, (unsigned long long)res.bytes_written);
        CHECK("pipe-warm-ok", res.ok);
        CHECK("pipe-warm-captured", c.captured == 3);
        CHECK("pipe-warm-feeds-happened", c.warmup_feeds >= 1);
        CHECK("pipe-warm-feeds-bounded", c.warmup_feeds <= xnc::WarmupFeedBound(fps));
        CHECK("pipe-warm-encoded-invariant", c.encoded == c.captured + c.warmup_feeds);
        CHECK("pipe-warm-exactly-one-idr", c.keyframes == 1);  // E2 风暴回归(管线级)
        CHECK("pipe-warm-aus-written", res.aus_written >= 1);
        bool timestamps_ordered = sink.timestamps.size() == res.aus_written;
        for (size_t i = 1; i < sink.timestamps.size(); ++i)
          timestamps_ordered &= sink.timestamps[i - 1] < sink.timestamps[i];
        CHECK("pipe-warm-delayed-tail-timestamps-ordered", timestamps_ordered);
        const std::vector<uint8_t> stream = ReadAll(tf.get());
        CHECK("pipe-warm-bytes-match", stream.size() == (size_t)res.bytes_written);
        CHECK("pipe-warm-first-au-sps-pps-idr", StreamStartsWithKeyframe(stream));
        CHECK("pipe-warm-stream-idr-count",
              CountNalTypeInStream(stream.data(), stream.size(), 5) == c.keyframes);
      }
    }
  }
  {
    // 场景 2 重建:第 12 帧处 err_rebuilt → 状态机回 WAIT_BASE_FRAME、
    // 恰一次 "rebuild" force(新 base 提交时消费)→ 流中共 2 个 IDR
    //(第二个 IDR 落在尾窗内,只有 FlushTail 能把它带出来)
    const uint32_t w = 64, h = 48, fps = 15;
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(w, h, fps, 500000, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: mf-init-pipe-reb err=%s\n", err.c_str());
    CHECK("pipe-reb-init", init_ok);
    if (init_ok) {
      ScriptedCapture cap(w, h, 25, 12);
      xnc::PipelineOpts o;
      o.duration_s = 3;
      o.fps = fps;
      o.target_bitrate_bps = 500000;
      TempBinFile tf;
      CHECK("pipe-reb-tmpfile", tf.Open(2));
      if (tf.get() != nullptr) {
        xnc::PipelineResult res = xnc::Pipeline::Run(cap, enc, tf.get(), o);
        const xnc::FrameCacheCounters& c = res.counters;
        std::printf("SELFTEST NOTE: pipe-reb ok=%d captured=%llu encoded=%llu feeds=%llu keyframes=%llu timeouts=%llu rebuilds=%u aus=%llu\n",
                    res.ok ? 1 : 0, (unsigned long long)c.captured,
                    (unsigned long long)c.encoded, (unsigned long long)c.warmup_feeds,
                    (unsigned long long)c.keyframes, (unsigned long long)c.timeouts,
                    c.rebuilds, (unsigned long long)res.aus_written);
        CHECK("pipe-reb-ok", res.ok);
        CHECK("pipe-reb-rebuild-counted", c.rebuilds == 1 && cap.RebuildCount() == 1);
        CHECK("pipe-reb-captured", c.captured == 25);
        CHECK("pipe-reb-encoded-invariant", c.encoded == c.captured + c.warmup_feeds);
        CHECK("pipe-reb-two-idrs", c.keyframes == 2);  // 自然首 IDR + 重建 IDR 各一次
        const std::vector<uint8_t> stream = ReadAll(tf.get());
        CHECK("pipe-reb-stream-idr-count",
              CountNalTypeInStream(stream.data(), stream.size(), 5) == 2);
        CHECK("pipe-reb-first-au-keyframe", StreamStartsWithKeyframe(stream));
      }
    }
  }
  { // 场景 3 (gpu-readback): NV12 blob 全管线路由 - Nv12ScriptedCapture
    // (pixfmt=kNv12, GPU 后端形态)直入 Pipeline:编码线程走 EncodeNV12,
    // 暖启动重喂 base 保持 NV12,流契约不变(1 IDR + SPS/PPS 前置 + VCL)
    const uint32_t w = 64, h = 48, fps = 15;
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(w, h, fps, 500000, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: mf-init-pipe-nv12 err=%s\n", err.c_str());
    CHECK("pipe-nv12-init", init_ok);
    if (init_ok) {
      Nv12ScriptedCapture cap(w, h, 3);  // 3 帧后静止 -> 触发暖启动重喂
      xnc::PipelineOpts o;
      o.duration_s = 2;
      o.fps = fps;
      o.target_bitrate_bps = 500000;
      TempBinFile tf;
      CHECK("pipe-nv12-tmpfile", tf.Open(3));
      if (tf.get() != nullptr) {
        xnc::PipelineResult res = xnc::Pipeline::Run(cap, enc, tf.get(), o);
        const xnc::FrameCacheCounters& c = res.counters;
        std::printf("SELFTEST NOTE: pipe-nv12 ok=%d captured=%llu encoded=%llu feeds=%llu keyframes=%llu aus=%llu bytes=%llu\n",
                    res.ok ? 1 : 0, (unsigned long long)c.captured,
                    (unsigned long long)c.encoded, (unsigned long long)c.warmup_feeds,
                    (unsigned long long)c.keyframes, (unsigned long long)res.aus_written,
                    (unsigned long long)res.bytes_written);
        CHECK("pipe-nv12-ok", res.ok);
        CHECK("pipe-nv12-captured", c.captured == 3);
        CHECK("pipe-nv12-feeds-happened", c.warmup_feeds >= 1);
        CHECK("pipe-nv12-encoded-invariant", c.encoded == c.captured + c.warmup_feeds);
        CHECK("pipe-nv12-exactly-one-idr", c.keyframes == 1);
        CHECK("pipe-nv12-aus-written", res.aus_written >= 1);
        const std::vector<uint8_t> stream = ReadAll(tf.get());
        CHECK("pipe-nv12-bytes-match", stream.size() == (size_t)res.bytes_written);
        CHECK("pipe-nv12-first-au-sps-pps-idr", StreamStartsWithKeyframe(stream));
        CHECK("pipe-nv12-stream-idr-count",
              CountNalTypeInStream(stream.data(), stream.size(), 5) == c.keyframes);
      }
    }
  }
  // ---- M1-Slice2 Task 2: rt CLI args ----
  { // --console-rt 默认值:pipe 默认名、secret 必填、max_subs=4
    auto p = Parse({L"--console-rt", L"--secret", L"0011ff"});
    CHECK("rt-args-ok-defaults", p.ok);
    CHECK("rt-args-default-pipe",
          p.ok && p.opt.pipe_name == xnc::kDefaultRtPipe);
    CHECK("rt-args-secret-bytes",
          p.ok && p.opt.secret.size() == 3 && p.opt.secret[0] == 0x00 &&
                p.opt.secret[1] == 0x11 && p.opt.secret[2] == 0xFF);
    CHECK("rt-args-default-max-subs", p.ok && p.opt.max_subs == 4);
    CHECK("rt-args-mode", p.ok && p.opt.console_rt);
  }
  { // secret 必填/校验:缺失、空、奇数长度、非 hex 一律参数错(exit 2 路径)
    const auto ms = Parse({L"--console-rt"});
    CHECK("rt-secret-missing", !ms.ok);
    CHECK("rt-secret-missing-err",
          !ms.ok && ms.err.find(L"--secret") != std::wstring::npos);
    CHECK("rt-secret-empty", !Parse({L"--console-rt", L"--secret", L""}).ok);
    CHECK("rt-secret-odd", !Parse({L"--console-rt", L"--secret", L"ABC"}).ok);
    CHECK("rt-secret-garbage", !Parse({L"--console-rt", L"--secret", L"GG"}).ok);
    CHECK("rt-secret-0x-prefix-rejected",
          !Parse({L"--console-rt", L"--secret", L"0x00"}).ok);
    CHECK("rt-secret-missing-value", !Parse({L"--console-rt", L"--secret"}).ok);
    CHECK("rt-secret-hex-case-ok", Parse({L"--console-rt", L"--secret", L"aAbB"}).ok);
  }
  { // --pipe 显式覆盖;--max-subs 边界 1..4;模式互斥;diag+pipe+secret 组合
    auto p = Parse({L"--console-rt", L"--secret", L"00", L"--pipe", L"\\\\.\\pipe\\x",
                    L"--max-subs", L"2", L"--fps", L"15"});
    CHECK("rt-args-explicit", p.ok && p.opt.pipe_name == L"\\\\.\\pipe\\x" &&
                                  p.opt.max_subs == 2 && p.opt.fps == 15);
    CHECK("rt-max-subs-zero", !Parse({L"--console-rt", L"--secret", L"00",
                                      L"--max-subs", L"0"}).ok);
    CHECK("rt-max-subs-over", !Parse({L"--console-rt", L"--secret", L"00",
                                      L"--max-subs", L"5"}).ok);
    CHECK("rt-max-subs-ok-bounds",
          Parse({L"--console-rt", L"--secret", L"00", L"--max-subs", L"1"}).ok &&
              Parse({L"--console-rt", L"--secret", L"00", L"--max-subs", L"4"}).ok);
    CHECK("rt-mode-exclusive-with-diag",
          !Parse({L"--console-rt", L"--secret", L"00", L"--console-diag",
                  L"--out", L"t"}).ok);
    CHECK("rt-mode-exclusive-with-selftest",
          !Parse({L"--console-rt", L"--secret", L"00", L"--selftest"}).ok);
    auto d = Parse({L"--console-diag", L"--out", L"t.h264", L"--pipe",
                    L"\\\\.\\pipe\\y", L"--secret", L"beef"});
    CHECK("rt-diag-combo-ok", d.ok && d.opt.console_diag && d.opt.secret.size() == 2);
    CHECK("rt-diag-combo-secret-required",
          !Parse({L"--console-diag", L"--out", L"t", L"--pipe", L"\\\\.\\pipe\\y"}).ok);
  }
  { // --secret-stdin(服务路径,spec 1.5:secret 不走 argv):rt 模式
    // 二选一 —— stdin 注入或交互 --secret;两者同给 = 参数错
    auto p = Parse({L"--console-rt", L"--secret-stdin"});
    CHECK("rt-stdin-flag", p.ok && p.opt.console_rt && p.opt.secret_stdin &&
                                p.opt.secret.empty());
    CHECK("rt-stdin-default-pipe",
          p.ok && p.opt.pipe_name == xnc::kDefaultRtPipe);
    CHECK("rt-stdin-with-pipe-and-opts",
          Parse({L"--console-rt", L"--secret-stdin", L"--pipe",
                 L"\\\\.\\pipe\\x", L"--max-subs", L"2", L"--fps", L"15"}).ok);
    CHECK("rt-both-secret-channels-exclusive",
          !Parse({L"--console-rt", L"--secret", L"00", L"--secret-stdin"}).ok);
    CHECK("rt-no-secret-channel",
          !Parse({L"--console-rt", L"--pipe", L"\\\\.\\pipe\\x"}).ok);
    // --secret-stdin 不带值:紧随的值 token 按未知参数拒绝
    CHECK("rt-stdin-takes-no-value",
          !Parse({L"--console-rt", L"--secret-stdin", L"0011"}).ok);
    auto d2 = Parse({L"--console-diag", L"--out", L"t.h264", L"--pipe",
                     L"\\\\.\\pipe\\y", L"--secret-stdin"});
    CHECK("rt-diag-combo-stdin", d2.ok && d2.opt.secret_stdin && d2.opt.secret.empty());
    CHECK("rt-diag-stdin-standalone",
          Parse({L"--console-diag", L"--out", L"t", L"--secret-stdin"}).ok);
  }
  { // ParseSecretStdinLine(纯逻辑):固定 64 hex chars = 32B,可选尾随换行
    std::string hex64, hex64up;
    std::vector<uint8_t> want;
    for (int i = 0; i < 32; ++i) {
      char lo[3], up[3];
      sprintf_s(lo, 3, "%02x", (i + 1) & 0xFF);
      sprintf_s(up, 3, "%02X", (i + 1) & 0xFF);
      hex64 += lo;
      hex64up += up;
      want.push_back(static_cast<uint8_t>((i + 1) & 0xFF));
    }
    std::vector<uint8_t> out;
    CHECK("stdin-line-plain",
          xnc::ParseSecretStdinLine(hex64.c_str(), &out) && out == want);
    CHECK("stdin-line-lf",
          xnc::ParseSecretStdinLine((hex64 + "\n").c_str(), &out) && out == want);
    CHECK("stdin-line-crlf",
          xnc::ParseSecretStdinLine((hex64 + "\r\n").c_str(), &out) && out == want);
    CHECK("stdin-line-cr",
          xnc::ParseSecretStdinLine((hex64 + "\r").c_str(), &out) && out == want);
    CHECK("stdin-line-uppercase",
          xnc::ParseSecretStdinLine(hex64up.c_str(), &out) && out == want);
    CHECK("stdin-line-63-chars",
          !xnc::ParseSecretStdinLine(hex64.substr(0, 63).c_str(), &out));
    CHECK("stdin-line-65-chars",
          !xnc::ParseSecretStdinLine((hex64 + "0").c_str(), &out));
    CHECK("stdin-line-nonhex",
          !xnc::ParseSecretStdinLine(("g" + hex64.substr(1)).c_str(), &out));
    CHECK("stdin-line-empty", !xnc::ParseSecretStdinLine("", &out));
    CHECK("stdin-line-leading-newline",
          !xnc::ParseSecretStdinLine(("\n" + hex64).c_str(), &out));
    CHECK("stdin-line-second-line",
          !xnc::ParseSecretStdinLine((hex64 + "\n" + hex64).c_str(), &out));
    CHECK("stdin-line-double-newline",
          !xnc::ParseSecretStdinLine((hex64 + "\n\n").c_str(), &out));
    CHECK("stdin-line-null-args",
          !xnc::ParseSecretStdinLine(nullptr, &out) &&
          !xnc::ParseSecretStdinLine(hex64.c_str(), nullptr));
  }
  // ---- M1-Slice2 Task 2:固定二进制消息 codec(精确字节向量) ----
  {
    const xnc::AttachPayload ap{7, 30, 1920, 2300000};
    const std::vector<uint8_t> aw = xnc::EncodeAttach(ap);
    const uint8_t want_a[16] = {7, 0, 0, 0, 30, 0, 0, 0, 0x80, 0x07, 0, 0,
                                0x60, 0x18, 0x23, 0x00};  // 2300000 = 0x231860
    CHECK("codec-attach-bytes",
          aw.size() == 16 && std::equal(aw.begin(), aw.end(), want_a));
    xnc::AttachPayload ap2;
    CHECK("codec-attach-rt",
          xnc::DecodeAttach(xnc::Frame{0, xnc::kMsgAttach, 0, aw}, &ap2) &&
              ap2.sub_id == 7 && ap2.max_fps == 30 && ap2.max_w == 1920 &&
              ap2.bitrate == 2300000);
    CHECK("codec-attach-bad-len",
          !xnc::DecodeAttach(xnc::Frame{0, xnc::kMsgAttach, 0, {1, 2, 3}}, &ap2));
    CHECK("codec-attach-zero-sub-rejected",
          !xnc::DecodeAttach(
              xnc::Frame{0, xnc::kMsgAttach, 0, xnc::EncodeAttach({0, 30, 0, 0})},
              &ap2));
    const std::vector<uint8_t> dw = xnc::EncodeDetach(9);
    CHECK("codec-detach-bytes", dw.size() == 4 && dw[0] == 9 && dw[1] == 0 &&
                                    dw[2] == 0 && dw[3] == 0);
    const std::vector<uint8_t> kw = xnc::EncodeKeyframeReq(3, "pli");
    CHECK("codec-keyframe-req-size", kw.size() == 36 && kw[0] == 3);
    CHECK("codec-keyframe-req-pad", kw[4] == 'p' && kw[5] == 'l' && kw[6] == 'i' &&
                                        kw[7] == 0 && kw[35] == 0);
    xnc::KeyframeReqPayload kr;
    CHECK("codec-keyframe-req-rt",
          xnc::DecodeKeyframeReq(xnc::Frame{0, xnc::kMsgKeyframeReq, 0, kw}, &kr) &&
              kr.sub_id == 3 && std::strcmp(kr.reason, "pli") == 0);
    const xnc::HostHelloPayload hh{1, 64, 48, 15, 4};
    const std::vector<uint8_t> hw = xnc::EncodeHostHello(hh);
    // M2-S3 Task 5: displays 扩展后空表仍多 4 字节 count=0(兼容前缀不变)
    const uint8_t want_h[24] = {1, 0, 0, 0, 64, 0, 0, 0, 48, 0, 0, 0,
                                15, 0, 0, 0, 4, 0, 0, 0, 0, 0, 0, 0};
    CHECK("codec-hello-bytes",
          hw.size() == 24 && std::equal(hw.begin(), hw.end(), want_h));
    xnc::HostHelloPayload hh2;
    CHECK("codec-hello-rt",
          xnc::DecodeHostHello(xnc::Frame{0, xnc::kMsgHostHello, 0, hw}, &hh2) &&
              hh2.gen == 1 && hh2.w == 64 && hh2.h == 48 && hh2.fps == 15 &&
              hh2.max_subs == 4);
    const std::vector<uint8_t> sw = xnc::EncodeStateEvent("capture_rebuilt", true);
    CHECK("codec-state-size", sw.size() == 33 && sw[32] == 1 &&
                                  std::memcmp(sw.data(), "capture_rebuilt", 15) == 0);
    xnc::StateEventPayload st;
    CHECK("codec-state-rt",
          xnc::DecodeStateEvent(xnc::Frame{0, xnc::kMsgState, 0, sw}, &st) &&
              std::strcmp(st.code, "capture_rebuilt") == 0 && st.recoverable == 1);
    const uint8_t au3[3] = {0xAA, 0xBB, 0xCC};
    const std::vector<uint8_t> fw = xnc::EncodeFrameEvent(0x0102030405060708ull, true,
                                                          au3, sizeof(au3));
    const uint8_t want_f[20] = {0, 0, 0, 0, 0x08, 0x07, 0x06, 0x05, 0x04, 0x03,
                                0x02, 0x01, 1, 3, 0, 0, 0, 0xAA, 0xBB, 0xCC};
    CHECK("codec-frame-bytes",
          fw.size() == 20 && std::equal(fw.begin(), fw.end(), want_f));
    xnc::FrameEventPayload fe;
    CHECK("codec-frame-rt",
          xnc::DecodeFrameEvent(xnc::Frame{0, xnc::kMsgFrame, 0, fw}, &fe) &&
              fe.target_sub_id == 0 && fe.key == 1 &&
              fe.mono_us == 0x0102030405060708ull && fe.au.size() == 3);
    CHECK("codec-frame-bad-len",
          !xnc::DecodeFrameEvent(xnc::Frame{0, xnc::kMsgFrame, 0, {0, 0, 0}}, &fe));
    CHECK("codec-frame-len-mismatch",
          !xnc::DecodeFrameEvent(
              xnc::Frame{0, xnc::kMsgFrame, 0,
                         std::vector<uint8_t>(fw.begin(), fw.begin() + 19)},
              &fe));
    // AU bound constants + frame-cap guard: AUs above 8MiB are dropped by
    // RtServer::OnAu (kMaxAuBytes); payloads above the XNIP 9MiB frame cap
    // encode to empty.
    CHECK("codec-au-bound-8mib", xnc::kMaxAuBytes == (size_t(8) << 20));
    std::vector<uint8_t> big(xnc::kMaxFrameBytes, 0);  // 9 MiB > cap - 17
    CHECK("codec-frame-oversize-empty",
          xnc::EncodeFrameEvent(1, false, big.data(), big.size()).empty());
    std::vector<uint8_t> ok_au(1000, 0xAB);
    CHECK("codec-frame-normal-nonempty",
          !xnc::EncodeFrameEvent(1, false, ok_au.data(), ok_au.size()).empty());
  }
  { // ---- M1 Task 2: Pipe v2 codec (0x0205) golden vectors + validation ----
    // CRC32C (Castagnoli) check value: the standard iSCSI test vector.
    CHECK("crc32c-check-value",
          xnc::Crc32c(reinterpret_cast<const uint8_t*>("123456789"), 9, 0) ==
              0xE3069283u);
    CHECK("crc32c-null-data", xnc::Crc32c(nullptr, 0, 0) == 0);
    CHECK("crc32c-zero-len", xnc::Crc32c(reinterpret_cast<const uint8_t*>(""), 0, 0) == 0);
    // The binding golden AU: IDs 1..6, 1920x1080, key flag, payload
    // {0,0,0,1,0x65} (the same AU the plan pins).
    xnc::EncodedAU au;
    au.id = xnc::FrameIdentity{1, 2, 3, 4, 5, 6};
    au.width = 1920;
    au.height = 1080;
    au.flags = xnc::AuFlags::kAuFlagKey;
    au.annexb = std::make_shared<const std::vector<uint8_t>>(
        std::vector<uint8_t>{0, 0, 0, 1, 0x65});
    const std::vector<uint8_t> w2 = xnc::EncodeFrameEventV2(au);
    CHECK("v2-size", w2.size() == xnc::kV2HeaderBytes + 5);
    CHECK("v2-header-bytes", xnc::rt_detail::GetU32(w2.data()) == 72);
    CHECK("v2-id-capture-epoch", xnc::rt_detail::GetU64(w2.data() + 4) == 1);
    CHECK("v2-id-codec-epoch", xnc::rt_detail::GetU64(w2.data() + 12) == 2);
    CHECK("v2-id-content-id", xnc::rt_detail::GetU64(w2.data() + 20) == 3);
    CHECK("v2-id-encode-seq", xnc::rt_detail::GetU64(w2.data() + 28) == 4);
    CHECK("v2-id-source-mono-us", xnc::rt_detail::GetU64(w2.data() + 36) == 5);
    CHECK("v2-id-present-mono-us", xnc::rt_detail::GetU64(w2.data() + 44) == 6);
    CHECK("v2-dims-flags", xnc::rt_detail::GetU32(w2.data() + 52) == 1920 &&
                               xnc::rt_detail::GetU32(w2.data() + 56) == 1080 &&
                               xnc::rt_detail::GetU32(w2.data() + 60) == 1);
    CHECK("v2-payload-len", xnc::rt_detail::GetU32(w2.data() + 64) == 5);
    CHECK("v2-payload-bytes",
          w2[72] == 0 && w2[73] == 0 && w2[74] == 0 && w2[75] == 1 &&
              w2[76] == 0x65);
    // CRC = Crc32c over header-with-zero-crc plus payload: hash [0,68),
    // then the four zeroed crc bytes, then the payload - exactly the
    // encoder's input while the crc field was still zero.
    static const uint8_t kZero4[4] = {0, 0, 0, 0};
    const uint32_t want_crc =
        xnc::Crc32c(w2.data() + 72, 5,
                    xnc::Crc32c(kZero4, 4, xnc::Crc32c(w2.data(), 68, 0)));
    CHECK("v2-crc-self-consistent",
          xnc::rt_detail::GetU32(w2.data() + 68) == want_crc);
    // Full byte-for-byte golden vector (ruling 4 - the Task 3 Go test uses
    // exactly these bytes). 72-byte header + {0,0,0,1,0x65} payload, LE
    // CRC32C stored as 0xE7B136AB.
    const uint8_t want_v2[77] = {
        0x48, 0x00, 0x00, 0x00,                          // header_bytes = 72
        0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,  // capture_epoch = 1
        0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,  // codec_epoch = 2
        0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,  // content_id = 3
        0x04, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,  // encode_seq = 4
        0x05, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,  // source_mono_us = 5
        0x06, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,  // present_mono_us = 6
        0x80, 0x07, 0x00, 0x00,                          // width = 1920
        0x38, 0x04, 0x00, 0x00,                          // height = 1080
        0x01, 0x00, 0x00, 0x00,                          // flags = kAuFlagKey
        0x05, 0x00, 0x00, 0x00,                          // payload_len = 5
        0xAB, 0x36, 0xB1, 0xE7,                          // crc32c (LE)
        0x00, 0x00, 0x00, 0x01, 0x65,                    // Annex-B AU
    };
    CHECK("v2-golden-exact-bytes",
          w2.size() == sizeof(want_v2) &&
              std::equal(w2.begin(), w2.end(), want_v2));
    // Round-trip: every identity field, dims, flags and the payload.
    xnc::EncodedAU rt;
    CHECK("v2-roundtrip",
          xnc::DecodeFrameEventV2(xnc::Frame{0, xnc::kMsgFrameV2, 0, w2}, &rt) &&
              rt.id.capture_epoch == 1 && rt.id.codec_epoch == 2 &&
              rt.id.content_id == 3 && rt.id.encode_seq == 4 &&
              rt.id.source_mono_us == 5 && rt.id.present_mono_us == 6 &&
              rt.width == 1920 && rt.height == 1080 &&
              rt.flags == xnc::AuFlags::kAuFlagKey && rt.annexb != nullptr &&
              rt.annexb->size() == 5 && rt.annexb->data()[4] == 0x65);
    // CRC rejection after one flipped byte (payload and header field).
    std::vector<uint8_t> flip = w2;
    flip[76] ^= 1;
    CHECK("v2-crc-reject-payload-flip",
          !xnc::DecodeFrameEventV2(xnc::Frame{0, xnc::kMsgFrameV2, 0, flip}, &rt));
    flip = w2;
    flip[4] ^= 0xFF;  // capture_epoch low byte
    CHECK("v2-crc-reject-header-flip",
          !xnc::DecodeFrameEventV2(xnc::Frame{0, xnc::kMsgFrameV2, 0, flip}, &rt));
    // Truncated header: 71 bytes < the 72-byte fixed header -> reject.
    CHECK("v2-truncated-header-rejected",
          !xnc::DecodeFrameEventV2(
              xnc::Frame{0, xnc::kMsgFrameV2, 0, std::vector<uint8_t>(71, 0)}, &rt));
    // header_bytes must equal the 72-byte v2 layout.
    std::vector<uint8_t> badh = w2;
    xnc::rt_detail::PutU32(badh.data(), 73);
    CHECK("v2-header-bytes-mismatch-rejected",
          !xnc::DecodeFrameEventV2(xnc::Frame{0, xnc::kMsgFrameV2, 0, badh}, &rt));
    // Declared payload_len past the AU bound is rejected WITHOUT a payload
    // present (the cap check runs before any allocation).
    std::vector<uint8_t> caph(72, 0);
    xnc::rt_detail::PutU32(caph.data(), 72);
    xnc::rt_detail::PutU32(caph.data() + 64, static_cast<uint32_t>(xnc::kMaxAuBytes + 1));
    CHECK("v2-decode-8mib-plus-1-rejected",
          !xnc::DecodeFrameEventV2(xnc::Frame{0, xnc::kMsgFrameV2, 0, caph}, &rt));
    // Exact-length rejectors (M1 Task 5, ruling 1c): a truncated payload
    // (header declares payload_len=5, only 2 bytes present) and trailing
    // extra bytes past header+payload_len are both rejected.
    std::vector<uint8_t> shortp = w2;
    shortp.resize(xnc::kV2HeaderBytes + 2);
    CHECK("v2-decode-truncated-payload-rejected",
          !xnc::DecodeFrameEventV2(xnc::Frame{0, xnc::kMsgFrameV2, 0, shortp}, &rt));
    std::vector<uint8_t> extra = w2;
    extra.push_back(0xEE);
    CHECK("v2-decode-trailing-extra-rejected",
          !xnc::DecodeFrameEventV2(xnc::Frame{0, xnc::kMsgFrameV2, 0, extra}, &rt));
    // Encode side: an 8 MiB + 1 AU yields an empty vector (caller drops).
    std::vector<uint8_t> big(xnc::kMaxAuBytes + 1, 0xAB);
    xnc::EncodedAU bigau = au;
    bigau.annexb = std::make_shared<const std::vector<uint8_t>>(big);
    CHECK("v2-encode-8mib-plus-1-empty", xnc::EncodeFrameEventV2(bigau).empty());
    // Boundary: an exactly-8 MiB AU is accepted and round-trips.
    std::vector<uint8_t> exact(xnc::kMaxAuBytes, 0xCD);
    xnc::EncodedAU exau = au;
    exau.annexb = std::make_shared<const std::vector<uint8_t>>(std::move(exact));
    const std::vector<uint8_t> wex = xnc::EncodeFrameEventV2(exau);
    xnc::EncodedAU rtex;
    CHECK("v2-8mib-accepted-roundtrip",
          !wex.empty() && wex.size() == xnc::kV2HeaderBytes + xnc::kMaxAuBytes &&
              xnc::DecodeFrameEventV2(xnc::Frame{0, xnc::kMsgFrameV2, 0, wex}, &rtex) &&
              rtex.annexb != nullptr && rtex.annexb->size() == xnc::kMaxAuBytes);
    CHECK("v2-null-out-rejected",
          !xnc::DecodeFrameEventV2(xnc::Frame{0, xnc::kMsgFrameV2, 0, w2}, nullptr));
    // HOST_HELLO v2: the 24-byte payload + trailing u32 media_protocol=2.
    const xnc::HostHelloPayload hh2{1, 64, 48, 15, 4};
    const std::vector<uint8_t> hv2 = xnc::EncodeHostHelloV2(hh2);
    CHECK("v2-hello-size", hv2.size() == 28);
    CHECK("v2-hello-base-prefix",
          hv2[0] == 1 && hv2[4] == 64 && hv2[8] == 48 && hv2[12] == 15 &&
              hv2[16] == 4);
    CHECK("v2-hello-trailing-protocol",
          xnc::rt_detail::GetU32(hv2.data() + 24) == xnc::kMediaProtocolV2);
    xnc::HostHelloPayload hback;
    uint32_t mp = 0;
    CHECK("v2-hello-decode",
          xnc::DecodeHostHelloV2(xnc::Frame{0, xnc::kMsgHostHello, 0, hv2}, &hback,
                                 &mp) &&
              mp == xnc::kMediaProtocolV2 && hback.gen == 1 && hback.w == 64 &&
              hback.h == 48 && hback.fps == 15 && hback.max_subs == 4);
    CHECK("v2-hello-truncated-rejected",
          !xnc::DecodeHostHelloV2(
              xnc::Frame{0, xnc::kMsgHostHello, 0, std::vector<uint8_t>(27, 0)},
              &hback, &mp));
    std::vector<uint8_t> wrongmp = hv2;
    xnc::rt_detail::PutU32(wrongmp.data() + 24, 1);
    CHECK("v2-hello-wrong-protocol-rejected",
          !xnc::DecodeHostHelloV2(xnc::Frame{0, xnc::kMsgHostHello, 0, wrongmp},
                                  &hback, &mp));
    // Full golden frame dump for the Task 3 Go test vector (ruling 4).
    std::printf("SELFTEST NOTE: v2-golden ");
    for (uint8_t b : w2) std::printf("%02x", b);
    std::printf("\n");
  }
  { // M1 Task 5 (ruling 1d): XNC_DESKTOP_PIPELINE_V2 value matrix on the
    // pure parser extracted from DesktopPipelineV2Enabled into
    // xnc::ParsePipelineV2Env (xnc-desktop.cpp; both TUs link into the same
    // exe, no shared header). The wrapper's env side stays startup-only:
    // unset (len=0) and over-long (>= 32 chars) values never reach the
    // parser - the matrix pins the semantics it delegates to, plus that
    // over-long runs must not accidentally match.
    CHECK("v2env-one", xnc::ParsePipelineV2Env("1"));
    CHECK("v2env-true", xnc::ParsePipelineV2Env("true"));
    CHECK("v2env-true-upper", xnc::ParsePipelineV2Env("TRUE"));
    CHECK("v2env-true-mixed", xnc::ParsePipelineV2Env("tRuE"));
    CHECK("v2env-zero", !xnc::ParsePipelineV2Env("0"));
    CHECK("v2env-false", !xnc::ParsePipelineV2Env("false"));
    CHECK("v2env-false-upper", !xnc::ParsePipelineV2Env("False"));
    CHECK("v2env-empty", !xnc::ParsePipelineV2Env(""));
    CHECK("v2env-garbage", !xnc::ParsePipelineV2Env("garbage"));
    CHECK("v2env-yes", !xnc::ParsePipelineV2Env("yes"));
    CHECK("v2env-null", !xnc::ParsePipelineV2Env(nullptr));
    CHECK("v2env-no-prefix-suffix",
          !xnc::ParsePipelineV2Env("true1") && !xnc::ParsePipelineV2Env("1x"));
    CHECK("v2env-overlong-ignored",
          !xnc::ParsePipelineV2Env("1111111111111111111111111111111111111111") &&
              !xnc::ParsePipelineV2Env("truetruetruetruetruetruetruetrue"));
  }
  { // M2-Slice1 Task 2:0x010A DISPLAY_CHANGED codec(精确字节向量)
    const xnc::DisplayChangedPayload dc{3, 1920, 1080, "resolution"};
    const std::vector<uint8_t> w = xnc::EncodeDisplayChanged(dc);
    const uint8_t want_dc[36] = {3, 0, 0, 0,                         // gen
                                 0x80, 0x07, 0, 0,                   // w = 1920
                                 0x38, 0x04, 0, 0,                   // h = 1080
                                 'r', 'e', 's', 'o', 'l', 'u', 't', 'i', 'o', 'n',
                                 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0};  // pad 24
    CHECK("codec-dc-bytes",
          w.size() == 36 && std::equal(w.begin(), w.end(), want_dc));
    xnc::DisplayChangedPayload rt;
    CHECK("codec-dc-roundtrip",
          xnc::DecodeDisplayChanged(xnc::Frame{0, xnc::kMsgDisplayChanged, 0, w}, &rt) &&
                rt.gen == 3 && rt.w == 1920 && rt.h == 1080 &&
                std::strcmp(rt.reason, "resolution") == 0);
    CHECK("codec-dc-bad-len-35",
          !xnc::DecodeDisplayChanged(
              xnc::Frame{0, xnc::kMsgDisplayChanged, 0, std::vector<uint8_t>(35, 0)}, &rt));
    CHECK("codec-dc-bad-len-37",
          !xnc::DecodeDisplayChanged(
              xnc::Frame{0, xnc::kMsgDisplayChanged, 0, std::vector<uint8_t>(37, 0)}, &rt));
    CHECK("codec-dc-null-out",
          !xnc::DecodeDisplayChanged(xnc::Frame{0, xnc::kMsgDisplayChanged, 0, w}, nullptr));
    // reason 截断到 23 字符 + NUL 填充(desktop_switch 恰 14 字符全保留)
    const xnc::DisplayChangedPayload ds{7, 64, 48, "desktop_switch"};
    const std::vector<uint8_t> ws = xnc::EncodeDisplayChanged(ds);
    CHECK("codec-dc-reason-fits",
          ws.size() == 36 && ws[12 + 14] == 0 && ws[35] == 0 &&
                std::memcmp(ws.data() + 12, "desktop_switch", 14) == 0);
    char long_reason[40];
    std::memset(long_reason, 'x', sizeof(long_reason) - 1);
    long_reason[sizeof(long_reason) - 1] = '\0';
    xnc::DisplayChangedPayload lr;
    lr.gen = 1;
    lr.w = 8;
    lr.h = 6;
    xnc::CopyReason(lr.reason, sizeof(lr.reason), long_reason);
    const std::vector<uint8_t> wl = xnc::EncodeDisplayChanged(lr);
    xnc::DisplayChangedPayload rl;
    CHECK("codec-dc-reason-truncated",
          xnc::DecodeDisplayChanged(xnc::Frame{0, xnc::kMsgDisplayChanged, 0, wl}, &rl) &&
                std::strlen(rl.reason) == 23);
  }
  // ---- M1-Slice2 Task 2:订阅者丢弃/合并策略(纯逻辑) ----
  {
    using A = xnc::SubSendQueue::AuAction;
    xnc::SubscriberTable t;
    auto q1 = std::make_shared<xnc::SubSendQueue>();
    auto q2 = std::make_shared<xnc::SubSendQueue>();
    CHECK("sub-attach", t.Attach(1, q1) && t.size() == 1);
    CHECK("sub-attach-dup-rejected", !t.Attach(1, q2) && t.size() == 1);
    CHECK("sub-attach-marks-needs", q1->needs_keyframe());
    CHECK("sub-attach-sets-sub-join-reason",
          t.pending_reason() != nullptr &&
              std::strcmp(t.pending_reason(), "sub_join") == 0);
    const xnc::Frame delta{xnc::kFlagEvent, xnc::kMsgFrame, 0, {1}};
    const xnc::Frame key{xnc::kFlagEvent, xnc::kMsgFrame, 0, {2}};
    // joiner 语义:needs_keyframe 期间 delta 直接丢(从 IDR 入流)
    CHECK("sub-delta-dropped-while-needs",
          q1->PushAu(false, delta) == A::kDroppedNeedKey);
    // key 入队并解除 needs
    CHECK("sub-key-enqueued-clears-needs", q1->PushAu(true, key) == A::kEnqueued);
    CHECK("sub-needs-cleared", !q1->needs_keyframe());
    CHECK("sub-pending-cleared-after-key", t.pending_reason() == nullptr);
    // 队列深度 3:从空队列起填 3 个 delta,第 4 个丢弃 + 标记 needs
    xnc::Frame drained;
    while (q1->Pop(&drained)) {}  // 清掉刚入队的 key,从空队列开始
    CHECK("sub-fill", q1->PushAu(false, delta) == A::kEnqueued &&
                          q1->PushAu(false, delta) == A::kEnqueued &&
                          q1->PushAu(false, delta) == A::kEnqueued &&
                          q1->video_depth() == 3);
    CHECK("sub-overflow-drops-delta",
          q1->PushAu(false, delta) == A::kDroppedQueueFull && q1->needs_keyframe());
    t.MarkNeedsKeyframe(1, "queue_overflow");
    CHECK("sub-overflow-reason",
          t.pending_reason() != nullptr &&
              std::strcmp(t.pending_reason(), "queue_overflow") == 0);
    // key 永不丢:挤掉旧 delta
    CHECK("sub-key-displaces-deltas",
          q1->PushAu(true, key) == A::kEnqueuedDisplacingDeltas &&
              q1->video_depth() == 1 && !q1->needs_keyframe());
    // 控制消息不丢(独立队列,先于视频)
    bool ctrl_push_ok = true;
    for (int i = 0; i < 10; ++i)
      if (!q1->PushControl(xnc::Frame{xnc::kFlagEvent, xnc::kMsgState, 0, {}}))
        ctrl_push_ok = false;
    CHECK("sub-ctrl-never-dropped", ctrl_push_ok);
    xnc::Frame popped;
    bool ctrl_first = true;
    int ctrl_seen = 0, video_seen = 0;
    while (q1->Pop(&popped)) {
      if (popped.message_type == xnc::kMsgState) {
        ++ctrl_seen;
        if (video_seen != 0) ctrl_first = false;
      } else {
        ++video_seen;
      }
    }
    CHECK("sub-ctrl-count", ctrl_seen == 10);
    CHECK("sub-ctrl-before-video", ctrl_first && video_seen == 1);
    // 合并语义:第二个订阅者的 needs 也汇入同一 pending reason
    CHECK("sub-second-attach", t.Attach(2, q2) && t.size() == 2);
    CHECK("sub-merged-pending", t.pending_reason() != nullptr);
    CHECK("sub-detach", t.Detach(1) && t.size() == 1);
    CHECK("sub-detach-unknown", !t.Detach(99));
    CHECK("sub-find", t.Find(2) == q2.get() && t.Find(1) == nullptr);
    // 控制背压上限:超过即 false(连接判死)
    xnc::SubSendQueue q3;
    bool never_false = true;
    for (uint32_t i = 0; i <= xnc::kCtrlBacklogMax + 1; ++i)
      if (!q3.PushControl(delta)) never_false = false;
    CHECK("sub-ctrl-backlog-bound", !never_false);
    // 深度参数边界:0 视为 1(key 可入,delta 即溢出)
    xnc::SubSendQueue q4(0);
    CHECK("sub-depth-zero-clamped-key", q4.PushAu(true, delta) == A::kEnqueued);
    CHECK("sub-depth-zero-clamped-overflow",
          q4.PushAu(false, delta) == A::kDroppedQueueFull);
  }
  { // M1 Task 4: v2 每订阅者 WAIT_IDR 状态机(纯逻辑;v1 路径不经过它)。
    // 契约:join/溢出/断流/epoch 变化 → 清空队列 + kWaitIdr;WAIT_IDR 期间
    // 不投递任何 delta;仅「期望 epoch 对」的 IDR 恢复 kLive;epoch 前进
    // 标记 discontinuity(调用方随后推 0x020B);key 永不阻塞(挤掉旧帧)。
    using A = xnc::SubSendQueue::AuAction;
    using V = xnc::SubSendQueue::VideoState;
    xnc::SubSendQueue q;
    const xnc::Frame delta{xnc::kFlagEvent, xnc::kMsgFrameV2, 0, {1}};
    const xnc::Frame key{xnc::kFlagEvent, xnc::kMsgFrameV2, 0, {2}};
    // join:kWaitIdr + needs(表侧 sub_join 合并);无 epoch 基线。
    CHECK("v2sm-join-waitidr", q.video_state() == V::kWaitIdr && q.needs_keyframe());
    // joiner 从 IDR 入流:基线前的 delta 直接丢,状态不变。
    CHECK("v2sm-join-delta-dropped",
          q.PushAuV2(false, 1, 1, delta).action == A::kDroppedNeedKey &&
              q.video_state() == V::kWaitIdr && q.video_depth() == 0);
    // 首个 IDR 入队并进入 kLive(join 不发 0x020B:客户端尚无一帧)。
    const auto first = q.PushAuV2(true, 1, 1, key);
    CHECK("v2sm-first-idr-live",
          first.action == A::kEnqueued && !first.discontinuity &&
              q.video_state() == V::kLive && !q.needs_keyframe() &&
              q.video_depth() == 1);
    // 深度 3:填满后第 4 个 delta 溢出 → 清空队列 + kWaitIdr + needs。
    xnc::Frame drained;
    while (q.Pop(&drained)) {}
    CHECK("v2sm-fill", q.PushAuV2(false, 1, 1, delta).action == A::kEnqueued &&
                           q.PushAuV2(false, 1, 1, delta).action == A::kEnqueued &&
                           q.PushAuV2(false, 1, 1, delta).action == A::kEnqueued &&
                           q.video_depth() == 3);
    CHECK("v2sm-overflow-waitidr",
          q.PushAuV2(false, 1, 1, delta).action == A::kDroppedQueueFull &&
              q.video_state() == V::kWaitIdr && q.video_depth() == 0 &&
              q.needs_keyframe());
    // WAIT_IDR 期间不投递任何后续 delta(收到即丢)。
    CHECK("v2sm-waitidr-suppresses-delta",
          q.PushAuV2(false, 1, 1, delta).action == A::kDroppedNeedKey &&
              q.video_depth() == 0);
    // 旧 epoch 的 IDR(回退)不得复活:保持 kWaitIdr(上游身份账本已挡此路,
    // 这里是防御)。
    CHECK("v2sm-stale-idr-rejected",
          q.PushAuV2(true, 0, 1, key).action == A::kDroppedNeedKey &&
              q.video_state() == V::kWaitIdr && q.video_depth() == 0);
    // 期望 epoch 对的 IDR:恢复 kLive,合并 needs 清位。
    CHECK("v2sm-expected-idr-live",
          q.PushAuV2(true, 1, 1, key).action == A::kEnqueued &&
              q.video_state() == V::kLive && !q.needs_keyframe() &&
              q.video_depth() == 1);
    // epoch 前进(kLive):断流标记 + 清空 + 新 epoch 的 IDR 直接恢复。
    CHECK("v2sm-live-fill-2", q.PushAuV2(false, 1, 1, delta).action == A::kEnqueued);
    const auto disc = q.PushAuV2(true, 2, 1, key);
    CHECK("v2sm-epoch-change-discontinuity",
          disc.action == A::kEnqueued && disc.discontinuity &&
              disc.capture_epoch == 2 && disc.codec_epoch == 1 &&
              q.video_state() == V::kLive && q.video_depth() == 1);
    // 新 epoch 的 delta 正常投递。
    CHECK("v2sm-new-epoch-delta",
          q.PushAuV2(false, 2, 1, delta).action == A::kEnqueued);
    // 重建断流(捕获线程信号):清空 + kWaitIdr;旧 epoch 的一切(含 IDR)
    // 被抑制;首个更新 epoch 的 AU 触发 0x020B 路径。floor = 重建前服务端
    // 最后扇出的 epoch 对(此处基线 (2,1))。
    q.OnRebuildDiscontinuity(2, 1);
    CHECK("v2sm-rebuild-waitidr",
          q.video_state() == V::kWaitIdr && q.video_depth() == 0);
    CHECK("v2sm-rebuild-suppresses-old-delta",
          q.PushAuV2(false, 2, 1, delta).action == A::kDroppedNeedKey &&
              q.video_depth() == 0);
    CHECK("v2sm-rebuild-suppresses-old-idr",
          q.PushAuV2(true, 2, 1, key).action == A::kDroppedNeedKey &&
              q.video_state() == V::kWaitIdr && q.video_depth() == 0);
    // 旧 epoch 的 delta 即便回到 kLive 也绝不投递(epoch 回退防御)。
    // 新 epoch 首 AU 不是 IDR(前瞻延迟)→ 断流 + 合并请求位再武装。
    const auto disc2 = q.PushAuV2(false, 3, 2, delta);
    CHECK("v2sm-rebuild-new-epoch-disc",
          disc2.action == A::kDroppedNeedKey && disc2.discontinuity &&
              disc2.capture_epoch == 3 && disc2.codec_epoch == 2 &&
              q.video_state() == V::kWaitIdr && q.needs_keyframe());
    CHECK("v2sm-regressed-delta-dropped",
          q.PushAuV2(false, 3, 1, delta).action == A::kDroppedNeedKey &&
              q.video_depth() == 0);
    CHECK("v2sm-recovery-idr-live",
          q.PushAuV2(true, 3, 2, key).action == A::kEnqueued &&
              q.video_state() == V::kLive);
    // kLive 且队列满时 key 挤掉旧 delta,永不阻塞、不丢 key。
    while (q.Pop(&drained)) {}
    CHECK("v2sm-key-never-dropped-fill",
          q.PushAuV2(false, 3, 2, delta).action == A::kEnqueued &&
              q.PushAuV2(false, 3, 2, delta).action == A::kEnqueued &&
              q.PushAuV2(false, 3, 2, delta).action == A::kEnqueued);
    CHECK("v2sm-key-displaces-deltas",
          q.PushAuV2(true, 3, 2, key).action == A::kEnqueuedDisplacingDeltas &&
              q.video_state() == V::kLive && q.video_depth() == 1);
    // 合并请求清位(v1 PushAu 契约):每一个「入队」的 IDR 都必须清
    // needs_keyframe——否则 sub_join / queue_overflow / explicit 的合并
    // reason 永不清除,PendingIdrReason() 恒非空,pipeline 每 500ms 重臂
    // ForceNextIdr(无谓的 IDR 洪流)。两处此前漏清:epoch 前进的 IDR、
    // kLive 同 epoch 的 IDR(显式 0x0104 请求的应答路径)。
    xnc::SubSendQueue q5;
    q5.PushAuV2(false, 1, 1, delta);  // joiner delta:丢弃,needs 保持
    CHECK("v2sm-epoch-idr-clears-needs",
          q5.PushAuV2(true, 2, 1, key).action == A::kEnqueued &&
              q5.video_state() == V::kLive && !q5.needs_keyframe());
    CHECK("v2sm-live-fill-3",
          q5.PushAuV2(false, 2, 1, delta).action == A::kEnqueued &&
              q5.PushAuV2(false, 2, 1, delta).action == A::kEnqueued &&
              q5.video_depth() == 3);
    q5.MarkNeedsKeyframe();  // 显式 0x0104 请求(kLive 期间)
    CHECK("v2sm-live-idr-clears-needs",
          q5.PushAuV2(true, 2, 1, key).action == A::kEnqueuedDisplacingDeltas &&
              !q5.needs_keyframe());
    // 重建时「尚无基线」的订阅者(刚 attach、一个 AU 都没收到):floor
    // 同样必须挡住重建前的编码器前瞻尾 —— 旧 epoch 的一切(含 IDR)被抑制,
    // 首个过 floor 的 AU 走 0x020B 断流路径(它错过的那次重建就是断流)。
    xnc::SubSendQueue q6;
    q6.OnRebuildDiscontinuity(1, 1);  // floor = 重建前最后扇出的 epoch 对
    CHECK("v2sm-rebuild-no-baseline-suppress",
          q6.PushAuV2(true, 1, 1, key).action == A::kDroppedNeedKey &&
              q6.video_state() == V::kWaitIdr && q6.video_depth() == 0);
    const auto d3 = q6.PushAuV2(true, 2, 1, key);
    CHECK("v2sm-rebuild-no-baseline-disc",
          d3.discontinuity && d3.capture_epoch == 2 && d3.codec_epoch == 1 &&
              d3.action == A::kEnqueued && q6.video_state() == V::kLive);
  }
  { // M1 Task 4: 0x020B STREAM_DISCONTINUITY wire codec(精确字节)
    const std::vector<uint8_t> w =
        xnc::EncodeStreamDiscontinuity(2, 3, xnc::kStreamDiscontinuityReason);
    CHECK("disc-enc-size", w.size() == 48);
    const uint8_t want_disc[48] = {2, 0, 0, 0, 0, 0, 0, 0,   // capture_epoch=2
                                   3, 0, 0, 0, 0, 0, 0, 0,   // codec_epoch=3
                                   0};                       // reason NUL 填充
    std::vector<uint8_t> want(48, 0);
    std::memcpy(want.data(), want_disc, 16);
    std::memcpy(want.data() + 16, xnc::kStreamDiscontinuityReason,
                std::strlen(xnc::kStreamDiscontinuityReason));
    CHECK("disc-enc-bytes",
          std::equal(w.begin(), w.end(), want.begin()));
    xnc::StreamDiscontinuityPayload d;
    CHECK("disc-enc-rt",
          xnc::DecodeStreamDiscontinuity(xnc::Frame{0, xnc::kMsgStreamDiscontinuity, 0, w},
                                         &d) &&
              d.capture_epoch == 2 && d.codec_epoch == 3 &&
              std::strcmp(d.reason, xnc::kStreamDiscontinuityReason) == 0);
    CHECK("disc-dec-badsize",
          !xnc::DecodeStreamDiscontinuity(
              xnc::Frame{0, xnc::kMsgStreamDiscontinuity, 0, {1, 2, 3}}, &d));
    CHECK("disc-dec-null", !xnc::DecodeStreamDiscontinuity(
                               xnc::Frame{0, xnc::kMsgStreamDiscontinuity, 0, w},
                               nullptr));
    // reason 截断到 31 字符 + NUL 填充(长 reason 只保留前缀)
    const std::vector<uint8_t> wl = xnc::EncodeStreamDiscontinuity(1, 1, "r");
    xnc::StreamDiscontinuityPayload dl;
    CHECK("disc-reason-nulpadded",
          xnc::DecodeStreamDiscontinuity(xnc::Frame{0, xnc::kMsgStreamDiscontinuity, 0, wl},
                                         &dl) &&
              std::strcmp(dl.reason, "r") == 0);
  }
  // ---- M1-Slice2 Task 2:AuSink 默认行为 + TeeAuSink ----
  {
    // defaults: a sink implementing only OnAu inherits no-op IDR/state hooks
    struct MinimalSink final : xnc::AuSink {
      const char* OnAu(const xnc::EncodedAU&) override { return nullptr; }
    };
    MinimalSink m;
    CHECK("sink-default-no-pending", m.PendingIdrReason() == nullptr);
    m.ConsumePendingIdr("sub_join");  // no-op, must not crash
    m.OnState("capture_rebuilt", true);
    CHECK("sink-default-noop-ok", true);
    struct CounterSink final : xnc::AuSink {
      const char* OnAu(const xnc::EncodedAU& au) override {
        aus++;
        if ((au.flags & xnc::AuFlags::kAuFlagKey) != 0) keys++;
        return nullptr;
      }
      const char* PendingIdrReason() override { return want_idr ? "sub_join" : nullptr; }
      void ConsumePendingIdr(const char* r) override { consumed.push_back(r); }
      void OnState(const char* c, bool) override { states.push_back(c); }
      int aus = 0, keys = 0;
      bool want_idr = false;
      std::vector<const char*> consumed, states;
    };
    CounterSink a, b;
    xnc::TeeAuSink tee(&a, &b);
    xnc::EncodedAU tee_au;  // key AU, present_mono_us = 42 (M1 Task 1 shape)
    tee_au.id.present_mono_us = 42;
    tee_au.flags = xnc::AuFlags::kAuFlagKey;
    const char* e = tee.OnAu(tee_au);
    CHECK("tee-onau-both", e == nullptr && a.aus == 1 && b.aus == 1 && a.keys == 1);
    CHECK("tee-pending-none", tee.PendingIdrReason() == nullptr);
    b.want_idr = true;
    CHECK("tee-pending-from-b", tee.PendingIdrReason() != nullptr);
    a.want_idr = true;
    tee.ConsumePendingIdr("sub_join");
    CHECK("tee-consume-both", a.consumed.size() == 1 && b.consumed.size() == 1);
    tee.OnState("stream_end", false);
    CHECK("tee-state-both", a.states.size() == 1 && b.states.size() == 1);
    // fatal error short-circuit: a fails -> b gets nothing
    struct FailingSink final : xnc::AuSink {
      const char* OnAu(const xnc::EncodedAU&) override { return "boom"; }
    };
    FailingSink f;
    CounterSink c2;
    xnc::TeeAuSink tee2(&f, &c2);
    CHECK("tee-fatal-first-wins",
          std::strcmp(tee2.OnAu(tee_au), "boom") == 0 && c2.aus == 0);
  }
  // ---- M1-Slice2 Task 2:RtServer 端到端(真 pipe + 真 MF 编码器 + 合成采集)----
  // 管线跑在子线程(有界 duration),fake 订阅者在主线程轮询读;每个场景
  // 独立 encoder/RtServer/pipe 名(slot N)。DACL = selftest 专用宽松 Everyone
  // (native/core/selftest.cpp loopback 先例;生产 DACL 在 rt_pipe_server.cpp)。
  const uint32_t kRtW = 64, kRtH = 48, kRtFps = 15, kRtBitrate = 500000;
  auto rt_opts = [=](int slot) {
    xnc::RtServer::Opts ro;
    ro.pipe_name = RtPipeNameOf(slot);
    ro.secret = kRtSecret;
    ro.secret_len = sizeof(kRtSecret);
    ro.max_subs = 4;
    ro.fps = kRtFps;
    ro.bitrate_bps = kRtBitrate;
    ro.sddl_override = L"D:P(A;;GA;;;WD)";  // TEST-ONLY permissive DACL
    return ro;
  };
  { // 场景 ①:attach → 立即 HOST_HELLO(字段正确)→ 首帧 FRAME = IDR
    //(SPS/PPS 前置整形);结束时收到 STATE{stream_end}
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(kRtW, kRtH, kRtFps, kRtBitrate, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: rt1-init err=%s\n", err.c_str());
    CHECK("rt1-init", init_ok);
    if (init_ok) {
      xnc::RtServer rt;
      const xnc::RtServer::Opts ro = rt_opts(0);
      CHECK("rt1-start", rt.Start(ro, kRtW, kRtH));
      ScriptedCapture cap(kRtW, kRtH, 3);  // 3 帧后静止
      xnc::PipelineOpts po;
      po.duration_s = 3;
      po.fps = kRtFps;
      po.target_bitrate_bps = kRtBitrate;
      xnc::PipelineResult res;
      std::thread pipe_th([&] { res = xnc::Pipeline::Run(cap, enc, rt, po); });
      RtTestClient a;
      CHECK("rt1-connect", a.Connect(ro.pipe_name.c_str(), kRtSecret, sizeof(kRtSecret)));
      CHECK("rt1-attach-hello", a.Attach(7));
      CHECK("rt1-no-frame-before-hello", !a.frame_before_hello_);
      CHECK("rt1-hello-fields",
            a.hello_ok_ && a.hello_.w == kRtW && a.hello_.h == kRtH &&
                a.hello_.fps == kRtFps && a.hello_.max_subs == 4 && a.hello_.gen == 1);
      a.Pump(2800, [&a] { return a.keys_ >= 1; });
      pipe_th.join();
      a.Pump(700);  // let the sender flush the stream_end STATE
      rt.Shutdown();
      CHECK("rt1-first-key", a.keys_ >= 1);
      CHECK("rt1-frames-received", a.frames_ >= 1);
      CHECK("rt1-key-payload-shaped", StreamStartsWithKeyframe(a.last_key_payload_));
      CHECK("rt1-key-mono-us", a.last_key_mono_us_ >= 1);
      CHECK("rt1-stream-end-state", a.saw_stream_end_);
      CHECK("rt1-pipeline-ok", res.ok);
      CHECK("rt1-encoded-invariant",
            res.counters.encoded == res.counters.captured + res.counters.warmup_feeds);
      CHECK("rt1-warmup-bounded",
            res.counters.warmup_feeds <= xnc::WarmupFeedBound(kRtFps));
      const xnc::RtServer::Stats st = rt.stats();
      CHECK("rt1-stats-attach", st.attaches == 1 && st.detaches == 0);
      std::printf("SELFTEST NOTE: rt1 keys=%llu frames=%llu emitted=%llu enq=%llu drop=%llu sub_join=%llu\n",
                  (unsigned long long)a.keys_, (unsigned long long)a.frames_,
                  (unsigned long long)st.aus_emitted, (unsigned long long)st.frames_enqueued,
                  (unsigned long long)st.frames_dropped, (unsigned long long)st.idr_sub_join);
    }
  }
  { // feat/rt-scale 场景①s:rt 管线 + ScaledCapture(合成 2880x1800 源 →
    // 1920x1200 流)。编码器按缩放尺寸 Init;HOST_HELLO 携带缩放尺寸
    // (1920x1200);管线宽度/编码计数以缩放帧为准。
    const uint32_t kRsW = 2880, kRsH = 1800, kRsw = 1920, kRsh = 1200;
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(kRsw, kRsh, kRtFps, kRtBitrate, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: rts-init err=%s\n", err.c_str());
    CHECK("rts-init", init_ok);
    if (init_ok) {
      xnc::RtServer rt;
      const xnc::RtServer::Opts ro = rt_opts(6);
      CHECK("rts-start", rt.Start(ro, kRsw, kRsh));
      xnc::ScaledCapture sc(std::make_unique<ScriptedCapture>(kRsW, kRsH, 3), kRsw);
      CHECK("rts-sc-dims", sc.Width() == kRsw && sc.Height() == kRsh);
      xnc::PipelineOpts po;
      po.duration_s = 3;
      po.fps = kRtFps;
      po.target_bitrate_bps = kRtBitrate;
      xnc::PipelineResult res;
      std::thread pipe_th([&] { res = xnc::Pipeline::Run(sc, enc, rt, po); });
      RtTestClient a;
      CHECK("rts-connect", a.Connect(ro.pipe_name.c_str(), kRtSecret, sizeof(kRtSecret)));
      CHECK("rts-attach-hello", a.Attach(8));
      CHECK("rts-hello-fields",
            a.hello_ok_ && a.hello_.w == kRsw && a.hello_.h == kRsh &&
                a.hello_.fps == kRtFps && a.hello_.gen == 1);
      a.Pump(2800, [&a] { return a.keys_ >= 1; });
      pipe_th.join();
      a.Pump(700);
      rt.Shutdown();
      CHECK("rts-first-key", a.keys_ >= 1);
      CHECK("rts-frames-received", a.frames_ >= 1);
      CHECK("rts-pipeline-ok", res.ok);
      CHECK("rts-pipeline-dims", res.width == kRsw && res.height == kRsh);
      CHECK("rts-encoded-invariant",
            res.counters.encoded == res.counters.captured + res.counters.warmup_feeds);
      std::printf("SELFTEST NOTE: rts w=%ux%u keys=%llu frames=%llu encoded=%llu captured=%llu\n",
                  kRsw, kRsh, (unsigned long long)a.keys_, (unsigned long long)a.frames_,
                  (unsigned long long)res.counters.encoded,
                  (unsigned long long)res.counters.captured);
    }
  }
  { // 场景 ②(承接语义回归):静止桌面 + 第二订阅者 → 管线重喂 base 产出
    // 新 IDR(reason=sub_join);恰 2 个 IDR、无风暴、无溢出误报
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(kRtW, kRtH, kRtFps, kRtBitrate, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: rt2-init err=%s\n", err.c_str());
    CHECK("rt2-init", init_ok);
    if (init_ok) {
      xnc::RtServer rt;
      const xnc::RtServer::Opts ro = rt_opts(1);
      CHECK("rt2-start", rt.Start(ro, kRtW, kRtH));
      ScriptedCapture cap(kRtW, kRtH, 3);  // 3 帧后永久静止
      xnc::PipelineOpts po;
      po.duration_s = 6;
      po.fps = kRtFps;
      po.target_bitrate_bps = kRtBitrate;
      xnc::PipelineResult res;
      std::thread pipe_th([&] { res = xnc::Pipeline::Run(cap, enc, rt, po); });
      RtTestClient a;
      CHECK("rt2-a-connect", a.Connect(ro.pipe_name.c_str(), kRtSecret, sizeof(kRtSecret)));
      CHECK("rt2-a-attach", a.Attach(7));
      a.Pump(4500, [&a] { return a.keys_ >= 1; });  // 等 A 的首个 IDR(初始 warm-up)
      CHECK("rt2-a-first-key", a.keys_ >= 1);
      // 静止期第二订阅者加入(B 只可能拿到一个"新"IDR:旧 IDR 无回填)
      RtTestClient b;
      CHECK("rt2-b-connect", b.Connect(ro.pipe_name.c_str(), kRtSecret, sizeof(kRtSecret)));
      CHECK("rt2-b-attach", b.Attach(9));
      const ULONGLONG t_end = GetTickCount64() + 4500;
      while (GetTickCount64() < t_end && (b.keys_ < 1 || a.keys_ < 2)) {
        a.Pump(80);
        b.Pump(80);
      }
      pipe_th.join();
      a.Pump(500);
      b.Pump(500);
      rt.Shutdown();
      CHECK("rt2-b-got-idr", b.keys_ >= 1);              // THE carry-forward assertion
      CHECK("rt2-b-first-frame-is-key", b.keys_ >= 1 && b.frames_ >= b.keys_);
      CHECK("rt2-a-second-key", a.keys_ >= 2);           // broadcast reached the old sub too
      CHECK("rt2-exactly-two-idrs", res.counters.keyframes == 2);
      CHECK("rt2-encoded-invariant",
            res.counters.encoded == res.counters.captured + res.counters.warmup_feeds);
      CHECK("rt2-on-demand-feeds-bounded",
            res.counters.warmup_feeds <= 2 * xnc::WarmupFeedBound(kRtFps));
      const xnc::RtServer::Stats st = rt.stats();
      CHECK("rt2-reason-sub_join-once", st.idr_sub_join == 1);
      CHECK("rt2-no-spurious-reasons",
            st.idr_queue_overflow == 0 && st.idr_explicit == 0 && st.idr_other == 0);
      // 注:运行期无溢出合并 IDR(上一条)。帧级 dropped 计数不做断言 ——
      // 结束时 FlushTail 一次吐 ~17 个 AU,超过深度 3 的队列属预期丢弃
      //(joiner 语义的 needkey 丢弃同理由 B 在拿到 IDR 前产生)。
      CHECK("rt2-two-attaches", st.attaches == 2);
      std::printf("SELFTEST NOTE: rt2 a_keys=%llu b_keys=%llu keyframes=%llu feeds=%llu sub_join=%llu\n",
                  (unsigned long long)a.keys_, (unsigned long long)b.keys_,
                  (unsigned long long)res.counters.keyframes,
                  (unsigned long long)res.counters.warmup_feeds,
                  (unsigned long long)st.idr_sub_join);
    }
  }
  { // Task 5 (2026-08-26 desktop-media-m0 correctness): A/B/C then timeouts
    // then sub_join + pli -> the recovery IDR must DECODE to C's luma
    // signature, not A's (stale-pixel acceptance test). All references are
    // encoded + decoded in THIS run by the same encoder instance and the
    // same config (w/h/fps/bitrate), never against precomputed raw-color
    // constants. The recovery reference replays the pipeline's submission
    // history (A/B/C + idle re-feeds + forced IDR) because the encoder's
    // rate control quantizes a mid-session forced IDR differently from a
    // cold-start IDR of the same content (measured; see
    // EncodeRecoveryReferenceIdr). The recovery AU's mono_us is the idle
    // re-feed's re-stamp (now), not C's capture time (Task 2 approved
    // behavior), so no mono_us identity assertion - the binding assertion
    // is decoded-pixel identity.
    //
    // Backend pin (reviewer finding, fix round 1): the replay feeds
    // back-to-back while the real pipeline paces submissions to spf with
    // wall-clock sample times, and the kEncoderLookaheadFrames feed bound
    // is measured for the software MFT. A hardware rung (QSV/NVENC/AMF)
    // with a timestamp-sensitive rate controller or a longer lookahead
    // could quantize the replay differently from the pipeline and fail the
    // binding assertion spuriously. The instance is therefore pinned to
    // the software rung - sticky across Init calls, so the references AND
    // the live pipeline stream share one backend and stay comparable. This
    // does not weaken the proof: the fix under test lives in the
    // pipeline's capture/cache/ledger logic (idle re-encode of the LATEST
    // captured frame), which is encoder-agnostic, and the other rt/mf
    // scenarios keep exercising the hardware-first ladder with unpinned
    // instances.
    xnc::MfSoftEncoder enc;
    enc.SetForceSoftware(true);  // sticky; applies to refs and pipeline alike
    std::string err;
    const bool init_ok = enc.Init(kRtW, kRtH, kRtFps, kRtBitrate, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: abc-init err=%s\n", err.c_str());
    CHECK("abc-init", init_ok);
    CHECK("abc-backend-pinned-software",
          enc.backend() == xnc::EncoderBackend::kSoftware);
    if (init_ok) {
      SyntheticBars bars(kRtW, kRtH);  // frame 0 = A, frame 2 = C (bars differ)
      std::vector<uint8_t> ref_a_au, ref_c_rec_au, ref_a_rec_au;
      uint64_t hash_a = 0, hash_c_rec = 0, hash_a_rec = 0;
      std::string perr;
      // Cold-start IDR of A: what the pipeline's warm-up IDR must be.
      const bool ref_a_ok =
          EncodeReferenceIdr(enc, kRtW, kRtH, kRtFps, kRtBitrate, bars.Frame(0),
                             bars.Bytes(), &ref_a_au, &err);
      if (!ref_a_ok) std::printf("SELFTEST NOTE: abc-ref-a err=%s\n", err.c_str());
      CHECK("abc-ref-a-encoded", ref_a_ok);
      if (ref_a_ok) {
        const bool dec = xnc::DecodeAnnexBToLumaHash(ref_a_au, &hash_a, &perr);
        if (!dec) std::printf("SELFTEST NOTE: abc-ref-a-decode err=%s\n", perr.c_str());
        CHECK("abc-ref-a-decoded", dec);
      }
      // Recovery-regime reference C: replay the pipeline's history with C
      // as the idle re-feed, then force the on-demand IDR (fixed pipeline).
      const bool ref_c_ok =
          EncodeRecoveryReferenceIdr(enc, kRtW, kRtH, kRtFps, kRtBitrate, 2,
                                     &ref_c_rec_au, &err);
      if (!ref_c_ok) std::printf("SELFTEST NOTE: abc-ref-c-recovery err=%s\n", err.c_str());
      CHECK("abc-ref-c-recovery-encoded", ref_c_ok);
      if (ref_c_ok) {
        const bool dec = xnc::DecodeAnnexBToLumaHash(ref_c_rec_au, &hash_c_rec, &perr);
        if (!dec) std::printf("SELFTEST NOTE: abc-ref-c-recovery-decode err=%s\n", perr.c_str());
        CHECK("abc-ref-c-recovery-decoded", dec);
      }
      // Recovery-regime reference A: the SAME history with A as the idle
      // re-feed (the stale-pixel bug). Must decode differently from C.
      const bool ref_a_rec_ok =
          EncodeRecoveryReferenceIdr(enc, kRtW, kRtH, kRtFps, kRtBitrate, 0,
                                     &ref_a_rec_au, &err);
      if (!ref_a_rec_ok) std::printf("SELFTEST NOTE: abc-ref-a-recovery err=%s\n", err.c_str());
      CHECK("abc-ref-a-recovery-encoded", ref_a_rec_ok);
      if (ref_a_rec_ok) {
        const bool dec = xnc::DecodeAnnexBToLumaHash(ref_a_rec_au, &hash_a_rec, &perr);
        if (!dec) std::printf("SELFTEST NOTE: abc-ref-a-recovery-decode err=%s\n", perr.c_str());
        CHECK("abc-ref-a-recovery-decoded", dec);
      }
      // Probe discriminative power (binding preflight 5): in the SAME
      // recovery regime, A and C must have different luma signatures, or
      // "recovery == C and not A" is unprovable.
      CHECK("abc-probe-discriminates",
            ref_c_ok && ref_a_rec_ok && hash_c_rec != hash_a_rec);
      // Scenario stream: re-Init the same encoder instance (the reference
      // sessions consumed their first IDRs and left lookahead buffered;
      // without a re-Init the scenario stream would chain onto the
      // reference stream's P frames - no keyframe all round).
      const bool scen_ok = enc.Init(kRtW, kRtH, kRtFps, kRtBitrate, &err);
      if (!scen_ok) std::printf("SELFTEST NOTE: abc-scenario-init err=%s\n", err.c_str());
      CHECK("abc-scenario-init", scen_ok);
      if (scen_ok) {
        xnc::RtServer rt;
        const xnc::RtServer::Opts ro = rt_opts(7);
        CHECK("abc-start", rt.Start(ro, kRtW, kRtH));
        ScriptedCapture cap(kRtW, kRtH, 3);  // A/B/C, then err_timeout forever
        xnc::PipelineOpts po;
        po.duration_s = 6;
        po.fps = kRtFps;
        po.target_bitrate_bps = kRtBitrate;
        xnc::PipelineResult res;
        std::thread pipe_th([&] { res = xnc::Pipeline::Run(cap, enc, rt, po); });
        RtTestClient a;
        CHECK("abc-connect", a.Connect(ro.pipe_name.c_str(), kRtSecret, sizeof(kRtSecret)));
        CHECK("abc-attach", a.Attach(7));
        a.Pump(4500, [&a] { return a.keys_ >= 1; });  // warm-up IDR (frame A)
        CHECK("abc-first-key", a.keys_ >= 1);
        const std::vector<uint8_t> first_key_au = a.last_key_payload_;
        // Static-screen second subscriber (sub_join) + explicit pli:
        // merged into one on-demand IDR.
        RtTestClient b;
        CHECK("abc-b-connect", b.Connect(ro.pipe_name.c_str(), kRtSecret, sizeof(kRtSecret)));
        CHECK("abc-b-attach", b.Attach(9));
        CHECK("abc-pli-sent",
              a.SendRaw(xnc::kMsgKeyframeReq, xnc::EncodeKeyframeReq(7, "pli")));
        const ULONGLONG t_end = GetTickCount64() + 4500;
        while (GetTickCount64() < t_end && a.keys_ < 2) {
          a.Pump(80);
          b.Pump(80);
        }
        pipe_th.join();
        a.Pump(500);
        b.Pump(500);
        rt.Shutdown();
        CHECK("abc-b-got-idr", b.keys_ >= 1);  // carry-forward (sub_join path)
        CHECK("abc-recovery-idr", a.keys_ >= 2);
        // The warm-up IDR must be A (first submission = first frame): this
        // also proves the probe decodes the pipeline's broadcast shaped AU
        // format on the same decode path as the recovery assertion.
        uint64_t hash_first = 0, hash_recovery = 0;
        const bool first_dec = !first_key_au.empty() &&
                               xnc::DecodeAnnexBToLumaHash(first_key_au, &hash_first, &perr);
        if (!first_dec) std::printf("SELFTEST NOTE: abc-first-key-decode err=%s\n", perr.c_str());
        CHECK("abc-first-key-decoded", first_dec);
        CHECK("abc-first-key-is-a", first_dec && hash_first == hash_a);
        const bool rec_dec = !a.last_key_payload_.empty() &&
                             xnc::DecodeAnnexBToLumaHash(a.last_key_payload_,
                                                         &hash_recovery, &perr);
        if (!rec_dec) std::printf("SELFTEST NOTE: abc-recovery-decode err=%s\n", perr.c_str());
        CHECK("abc-recovery-decoded", rec_dec);
        CHECK("abc-recovery-is-c", rec_dec && hash_recovery == hash_c_rec);  // THE binding assertion
        CHECK("abc-recovery-not-stale-a", rec_dec && hash_recovery != hash_a_rec);
        const xnc::RtServer::Stats st = rt.stats();
        CHECK("abc-demand-idr-reason", st.idr_sub_join + st.idr_explicit >= 1);
        CHECK("abc-pipeline-ok", res.ok);
        std::printf("SELFTEST NOTE: abc hash_a=%016llx hash_c_rec=%016llx "
                    "hash_a_rec=%016llx first=%016llx recovery=%016llx keys=%llu "
                    "sub_join=%llu explicit=%llu\n",
                    (unsigned long long)hash_a, (unsigned long long)hash_c_rec,
                    (unsigned long long)hash_a_rec,
                    (unsigned long long)hash_first, (unsigned long long)hash_recovery,
                    (unsigned long long)a.keys_, (unsigned long long)st.idr_sub_join,
                    (unsigned long long)st.idr_explicit);
      }
    }
  }
  { // 场景 ③:队列溢出 —— 卡死订阅者(attach 后从不读)→ 管道缓冲 +
    // 发送队列满 → 丢 delta + needsKeyframe → 合并 IDR(reason=queue_overflow);
    // 健康订阅者收到第二个关键帧。噪声帧(320x240)保证 AU 足够大、
    // 溢出路径确定触发(64x48 彩条 AU 仅 ~160B,打不满 64KB 管道缓冲)。
    const uint32_t nw = 320, nh = 240, nfps = 15, nbitrate = 2300000;
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(nw, nh, nfps, nbitrate, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: rt3-init err=%s\n", err.c_str());
    CHECK("rt3-init", init_ok);
    if (init_ok) {
      xnc::RtServer rt;
      xnc::RtServer::Opts ro = rt_opts(2);
      ro.fps = nfps;
      ro.bitrate_bps = nbitrate;
      CHECK("rt3-start", rt.Start(ro, nw, nh));
      NoisyCapture cap(nw, nh, 130);  // ~8.7s 连续噪声帧
      xnc::PipelineOpts po;
      po.duration_s = 9;
      po.fps = nfps;
      po.target_bitrate_bps = nbitrate;
      xnc::PipelineResult res;
      std::thread pipe_th([&] { res = xnc::Pipeline::Run(cap, enc, rt, po); });
      RtTestClient a;  // 健康订阅者
      CHECK("rt3-a-connect", a.Connect(ro.pipe_name.c_str(), kRtSecret, sizeof(kRtSecret)));
      CHECK("rt3-a-attach", a.Attach(7));
      RtTestClient b;  // 卡死订阅者:attach 后一个字节都不读
      CHECK("rt3-b-connect", b.Connect(ro.pipe_name.c_str(), kRtSecret, sizeof(kRtSecret)));
      CHECK("rt3-b-attach", b.Attach(9));
      a.Pump(8500, [&a] { return a.keys_ >= 2; });
      pipe_th.join();
      a.Pump(500);
      rt.Shutdown();
      CHECK("rt3-a-two-keys", a.keys_ >= 2);  // 溢出合并的 IDR 也广播给了健康订阅者
      CHECK("rt3-pipeline-ok", res.ok);
      const xnc::RtServer::Stats st = rt.stats();
      CHECK("rt3-deltas-dropped", st.frames_dropped_overflow >= 1);
      CHECK("rt3-overflow-merged-idr", st.idr_queue_overflow >= 1);
      std::printf("SELFTEST NOTE: rt3 a_keys=%llu drop_of=%llu drop_nk=%llu overflow_idr=%llu emitted=%llu\n",
                  (unsigned long long)a.keys_,
                  (unsigned long long)st.frames_dropped_overflow,
                  (unsigned long long)st.frames_dropped_needkey,
                  (unsigned long long)st.idr_queue_overflow,
                  (unsigned long long)st.aus_emitted);
    }
  }
  { // 场景 ④:DETACH 清理 —— 表项即刻移除、发送队列排空后断连(EOF)、
    // 线程全部回收(Shutdown 不挂)
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(kRtW, kRtH, kRtFps, kRtBitrate, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: rt4-init err=%s\n", err.c_str());
    CHECK("rt4-init", init_ok);
    if (init_ok) {
      xnc::RtServer rt;
      const xnc::RtServer::Opts ro = rt_opts(3);
      CHECK("rt4-start", rt.Start(ro, kRtW, kRtH));
      ScriptedCapture cap(kRtW, kRtH, 40);
      xnc::PipelineOpts po;
      po.duration_s = 4;
      po.fps = kRtFps;
      po.target_bitrate_bps = kRtBitrate;
      xnc::PipelineResult res;
      std::thread pipe_th([&] { res = xnc::Pipeline::Run(cap, enc, rt, po); });
      RtTestClient a;
      CHECK("rt4-a-connect", a.Connect(ro.pipe_name.c_str(), kRtSecret, sizeof(kRtSecret)));
      CHECK("rt4-a-attach", a.Attach(5));
      a.Pump(800);
      CHECK("rt4-detach-send", a.SendDetach(5));
      bool zero = false;
      for (int i = 0; i < 100 && !zero; ++i) {
        if (rt.SubscriberCount() == 0) zero = true;
        else Sleep(10);
      }
      CHECK("rt4-sub-count-zero", zero);
      // 排空在途帧后服务器断连:客户端读到 EOF
      bool eof_seen = false;
      const ULONGLONG dl = GetTickCount64() + 2500;
      while (GetTickCount64() < dl) {
        xnc::Frame f;
        if (!a.ReadFrameT(f, 200)) {
          eof_seen = true;
          break;
        }
        a.CountFrame(f);
      }
      CHECK("rt4-eof-after-detach", eof_seen);
      pipe_th.join();
      rt.Shutdown();  // must not hang: all threads reaped
      const xnc::RtServer::Stats st = rt.stats();
      CHECK("rt4-detach-counted", st.detaches == 1 && st.attaches == 1);
      CHECK("rt4-final-subs-zero", rt.SubscriberCount() == 0);
      std::printf("SELFTEST NOTE: rt4 frames=%llu detaches=%u\n",
                  (unsigned long long)a.frames_, st.detaches);
    }
  }
  // ---- M1-Slice3 Task 1:0x0108/0x0109 固定二进制 codec(精确字节) ----
  {
    // MOVE: [u32 sub][u64 seq][u8 1][s32 x][s32 y][u16 buttons]
    xnc::InputMsg m;
    m.sub_id = 7;
    m.seq = 0x0102030405ull;
    m.type = xnc::kInputMove;
    m.x = -100;
    m.y = 200;
    m.buttons = 0x0013;  // L+M+X2
    const std::vector<uint8_t> w = xnc::EncodeInputMsg(m);
    const uint8_t want_move[23] = {7, 0, 0, 0, 5, 4, 3, 2, 1, 0, 0, 0, 1, 0x9C,
                                   0xFF, 0xFF, 0xFF, 0xC8, 0, 0, 0, 0x13, 0};
    CHECK("incodec-move-bytes",
          w.size() == 23 && std::equal(w.begin(), w.end(), want_move));
    xnc::InputMsg r;
    CHECK("incodec-move-rt",
          xnc::DecodeInputMsg(xnc::Frame{0, xnc::kMsgInput, 0, w}, &r) &&
                r.sub_id == 7 && r.seq == 0x0102030405ull && r.type == xnc::kInputMove &&
                r.x == -100 && r.y == 200 && r.buttons == 0x13);
    // BUTTON: [u32][u64][u8 2][u8 btn=4(X1)][u8 down=1]
    xnc::InputMsg b;
    b.sub_id = 7;
    b.seq = 6;
    b.type = xnc::kInputButton;
    b.btn = 8;
    b.down = 1;
    const std::vector<uint8_t> wb = xnc::EncodeInputMsg(b);
    const uint8_t want_btn[15] = {7, 0, 0, 0, 6, 0, 0, 0, 0, 0, 0, 0, 2, 8, 1};
    CHECK("incodec-button-bytes",
          wb.size() == 15 && std::equal(wb.begin(), wb.end(), want_btn));
    xnc::InputMsg rb;
    CHECK("incodec-button-rt",
          xnc::DecodeInputMsg(xnc::Frame{0, xnc::kMsgInput, 0, wb}, &rb) &&
                rb.type == xnc::kInputButton && rb.btn == 8 && rb.down == 1);
    // WHEEL: [u32][u64][u8 3][s32 dx][s32 dy][u8 trackpad]
    xnc::InputMsg h;
    h.sub_id = 7;
    h.seq = 7;
    h.type = xnc::kInputWheel;
    h.x = -2;   // dx
    h.y = -3;   // dy
    h.trackpad = 1;
    const std::vector<uint8_t> wh = xnc::EncodeInputMsg(h);
    const uint8_t want_wh[22] = {7, 0, 0, 0, 7, 0, 0, 0, 0, 0, 0, 0, 3, 0xFE, 0xFF,
                                 0xFF, 0xFF, 0xFD, 0xFF, 0xFF, 0xFF, 1};
    CHECK("incodec-wheel-bytes",
          wh.size() == 22 && std::equal(wh.begin(), wh.end(), want_wh));
    xnc::InputMsg rh;
    CHECK("incodec-wheel-rt",
          xnc::DecodeInputMsg(xnc::Frame{0, xnc::kMsgInput, 0, wh}, &rh) &&
                rh.type == xnc::kInputWheel && rh.x == -2 && rh.y == -3 &&
                rh.trackpad == 1);
    // KEY: [u32][u64][u8 4][u16 scan][u8 down][u8 extended]
    xnc::InputMsg k;
    k.sub_id = 7;
    k.seq = 8;
    k.type = xnc::kInputKey;
    k.scan = 0x1D;
    k.down = 1;
    k.extended = 0;
    const std::vector<uint8_t> wk = xnc::EncodeInputMsg(k);
    const uint8_t want_key[17] = {7, 0, 0, 0, 8, 0, 0, 0, 0, 0, 0, 0, 4, 0x1D, 0, 1, 0};
    CHECK("incodec-key-bytes",
          wk.size() == 17 && std::equal(wk.begin(), wk.end(), want_key));
    xnc::InputMsg rk;
    CHECK("incodec-key-rt",
          xnc::DecodeInputMsg(xnc::Frame{0, xnc::kMsgInput, 0, wk}, &rk) &&
                rk.type == xnc::kInputKey && rk.scan == 0x1D && rk.down == 1 &&
                rk.extended == 0);
    // TEXT: [u32][u64][u8 5][u16 len][utf16le units] (surrogate pair U+1D11E)
    xnc::InputMsg t;
    t.sub_id = 7;
    t.seq = 9;
    t.type = xnc::kInputText;
    t.text = {0xD834, 0xDD1E, 0x0041};
    const std::vector<uint8_t> wt = xnc::EncodeInputMsg(t);
    const uint8_t want_tx[21] = {7, 0, 0, 0, 9, 0, 0, 0, 0, 0, 0, 0, 5,
                                 3, 0, 0x34, 0xD8, 0x1E, 0xDD, 0x41, 0x00};
    CHECK("incodec-text-bytes",
          wt.size() == 21 && std::equal(wt.begin(), wt.end(), want_tx));
    xnc::InputMsg rt2;
    CHECK("incodec-text-rt",
          xnc::DecodeInputMsg(xnc::Frame{0, xnc::kMsgInput, 0, wt}, &rt2) &&
                rt2.type == xnc::kInputText && rt2.text.size() == 3 &&
                rt2.text[0] == 0xD834 && rt2.text[1] == 0xDD1E && rt2.text[2] == 0x0041);
    // LOCK: [u32][u64][u8 6][u8 caps][u8 num]
    xnc::InputMsg l;
    l.sub_id = 7;
    l.seq = 10;
    l.type = xnc::kInputLock;
    l.caps = 1;
    l.num = 0;
    const std::vector<uint8_t> wl = xnc::EncodeInputMsg(l);
    const uint8_t want_lk[15] = {7, 0, 0, 0, 10, 0, 0, 0, 0, 0, 0, 0, 6, 1, 0};
    CHECK("incodec-lock-bytes",
          wl.size() == 15 && std::equal(wl.begin(), wl.end(), want_lk));
    xnc::InputMsg rl;
    CHECK("incodec-lock-rt",
          xnc::DecodeInputMsg(xnc::Frame{0, xnc::kMsgInput, 0, wl}, &rl) &&
                rl.type == xnc::kInputLock && rl.caps == 1 && rl.num == 0);
    // 拒绝矩阵:形状/类型/域校验
    auto dec = [](const std::vector<uint8_t>& p) {
      xnc::InputMsg o;
      return xnc::DecodeInputMsg(xnc::Frame{0, xnc::kMsgInput, 0, p}, &o);
    };
    CHECK("incodec-short-header", !dec({1, 2, 3, 4, 5}));
    CHECK("incodec-type-zero", !dec(std::vector<uint8_t>{7, 0, 0, 0, 1, 0, 0, 0,
                                                         0, 0, 0, 0, 0}));
    CHECK("incodec-type-unknown", !dec(std::vector<uint8_t>{7, 0, 0, 0, 1, 0, 0, 0,
                                                            0, 0, 0, 0, 7, 0, 0}));
    CHECK("incodec-zero-sub", !dec(std::vector<uint8_t>{0, 0, 0, 0, 1, 0, 0, 0,
                                                        0, 0, 0, 0, 6, 0, 0}));
    CHECK("incodec-move-bad-size", !dec(std::vector<uint8_t>(22, 0)));
    {
      std::vector<uint8_t> p = w;  // MOVE with an illegal button bit (0x20)
      p[21] = 0x20;
      CHECK("incodec-move-bad-button-bit", !dec(p));
    }
    {
      std::vector<uint8_t> p = wb;  // btn 3 is not a single valid mask bit
      p[13] = 3;
      CHECK("incodec-button-bad-mask", !dec(p));
      p[13] = 32;  // 0x20 not in 1/2/4/8/16
      CHECK("incodec-button-bad-high", !dec(p));
      p[13] = 0;
      CHECK("incodec-button-zero", !dec(p));
      p = wb;
      p[14] = 2;  // down must be 0/1
      CHECK("incodec-button-bad-down", !dec(p));
    }
    {
      std::vector<uint8_t> p = wk;
      p[13] = 0;  // scan 0
      CHECK("incodec-key-zero-scan", !dec(p));
      p = wk;
      p[15] = 2;  // down not 0/1
      CHECK("incodec-key-bad-down", !dec(p));
      p = wk;
      p[16] = 2;  // extended not 0/1
      CHECK("incodec-key-bad-ext", !dec(p));
    }
    {
      std::vector<uint8_t> p = wh;
      p[21] = 2;  // trackpad not 0/1
      CHECK("incodec-wheel-bad-trackpad", !dec(p));
    }
    CHECK("incodec-text-len-zero",
          !dec(std::vector<uint8_t>{7, 0, 0, 0, 9, 0, 0, 0, 0, 0, 0, 0, 5, 0, 0}));
    {
      std::vector<uint8_t> p = wt;
      p[13] = 4;  // len 4 but only 3 units on the wire
      CHECK("incodec-text-len-mismatch", !dec(p));
      std::vector<uint8_t> big(13 + 2 + 2 * (xnc::kMaxTextUnits + 1), 0);
      big[12] = 5;
      big[13] = static_cast<uint8_t>((xnc::kMaxTextUnits + 1) & 0xFF);
      big[14] = static_cast<uint8_t>((xnc::kMaxTextUnits + 1) >> 8);
      CHECK("incodec-text-over-bound", !dec(big));
    }
    {
      std::vector<uint8_t> p = wl;
      p[13] = 2;  // caps not 0/1
      CHECK("incodec-lock-bad-caps", !dec(p));
    }
    // 光标 0x0109:[s32 x][s32 y][u8 visible]
    const std::vector<uint8_t> wc = xnc::EncodeCursorEvent(-1920, 1079, 1);
    const uint8_t want_cur[9] = {0x80, 0xF8, 0xFF, 0xFF, 0x37, 0x04, 0, 0, 1};
    CHECK("incodec-cursor-bytes",
          wc.size() == 9 && std::equal(wc.begin(), wc.end(), want_cur));
    int32_t cx = 0, cy = 0;
    uint8_t cv = 0;
    CHECK("incodec-cursor-rt",
          xnc::DecodeCursorEvent(xnc::Frame{0, xnc::kMsgCursor, 0, wc}, &cx, &cy,
                                 &cv) &&
                cx == -1920 && cy == 1079 && cv == 1);
    CHECK("incodec-cursor-bad-size",
          !xnc::DecodeCursorEvent(xnc::Frame{0, xnc::kMsgCursor, 0, {1, 2}}, &cx, &cy,
                                  &cv));
    CHECK("incodec-cursor-null", !xnc::DecodeCursorEvent(
                                     xnc::Frame{0, xnc::kMsgCursor, 0, wc}, nullptr,
                                     &cy, &cv));
  }
  // ---- M1-Slice3 Task 1:坐标归一化数学(伪虚拟桌面指标) ----
  {
    // 单显示器退化:hello == 虚拟桌面 → abs = nx*65536(钳 65535)
    CHECK("map-abs-origin", xnc::MapMoveToAbs(0, 1920, 0, 1920) == 0);
    CHECK("map-abs-mid", xnc::MapMoveToAbs(960, 1920, 0, 1920) == 32768);
    CHECK("map-abs-max-clamped", xnc::MapMoveToAbs(1920, 1920, 0, 1920) == 65535);
    CHECK("map-abs-last-pixel",
          xnc::MapMoveToAbs(1919, 1920, 0, 1920) == 65502);  // 65536-34.13 → 65502
    CHECK("map-abs-neg-clamped", xnc::MapMoveToAbs(-5, 1920, 0, 1920) == 0);
    CHECK("map-abs-over-clamped", xnc::MapMoveToAbs(5000, 1920, 0, 1920) == 65535);
    // 虚拟桌面起点为负(多屏):归一化穿过起点,原点抵消
    CHECK("map-abs-negative-origin",
          xnc::MapMoveToAbs(960, 1920, -1920, 3840) == 32768);
    CHECK("map-abs-negative-origin-left",
          xnc::MapMoveToAbs(0, 1920, -1920, 3840) == 0);
    // 流宽 != 虚拟桌面宽:按归一化等比缩放
    CHECK("map-abs-scaled", xnc::MapMoveToAbs(25, 100, 0, 200) == 16384);
    CHECK("map-abs-quarter",
          xnc::MapMoveToAbs(480, 1920, 0, 3840) == 16384);  // nx=.25
    // 退化护栏
    CHECK("map-abs-zero-dims", xnc::MapMoveToAbs(10, 0, 0, 1920) == 0 &&
                                   xnc::MapMoveToAbs(10, 1920, 0, 0) == 0);
    // 光标逆映射:虚拟桌面 px → HOST_HELLO 流 px
    CHECK("map-cursor-left-edge", xnc::MapCursorToStream(-1920, 3840, -1920, 3840) == 0);
    CHECK("map-cursor-mid", xnc::MapCursorToStream(0, 3840, -1920, 3840) == 1920);
    CHECK("map-cursor-right-clamped",
          xnc::MapCursorToStream(9999, 3840, -1920, 3840) == 3840);
    CHECK("map-cursor-left-clamped",
          xnc::MapCursorToStream(-9999, 3840, -1920, 3840) == 0);
    CHECK("map-cursor-third", xnc::MapCursorToStream(66, 100, 0, 200) == 33);
    CHECK("map-cursor-zero-dims",
          xnc::MapCursorToStream(10, 0, 0, 100) == 0 &&
                xnc::MapCursorToStream(10, 100, 0, 0) == 0);
  }
  // ---- M1-Slice3 Task 1:InputManager 注入逻辑(fake SendInput 记录器) ----
  {
    ResetInputSeams(200, 100);
    InputRecorder rec;
    g_input_rec = &rec;
    xnc::InputManager im(TestInputOpts(200, 100));
    CHECK("im-marker-const", xnc::kInputExtraInfoMarker == 0x584E4301ull);
    // MOVE:绝对虚拟桌面 + buttons 位 → DOWN/UP 状态差
    xnc::InputMsg mv;
    mv.sub_id = 7;
    mv.seq = 1;
    mv.type = xnc::kInputMove;
    mv.x = 100;
    mv.y = 50;
    mv.buttons = xnc::kBtnL;
    CHECK("im-move-ok",
          im.Inject(mv) == xnc::InputManager::Result::kInjected);
    CHECK("im-move-recorded", rec.CountMouse(MOUSEEVENTF_MOVE | MOUSEEVENTF_ABSOLUTE |
                                                 MOUSEEVENTF_VIRTUALDESK) == 1);
    const INPUT* mi = rec.FindMouse(MOUSEEVENTF_MOVE | MOUSEEVENTF_ABSOLUTE |
                                    MOUSEEVENTF_VIRTUALDESK);
    CHECK("im-move-abs-coords",
          mi != nullptr && mi->mi.dx == 32768 && mi->mi.dy == 32768);
    CHECK("im-move-marker", mi != nullptr &&
                                  mi->mi.dwExtraInfo == xnc::kInputExtraInfoMarker);
    CHECK("im-move-leftdown-after-move",
          rec.CountMouse(MOUSEEVENTF_LEFTDOWN) == 1 &&
                rec.sent.size() == 2 &&
                rec.sent[0].mi.dwFlags == (MOUSEEVENTF_MOVE | MOUSEEVENTF_ABSOLUTE |
                                           MOUSEEVENTF_VIRTUALDESK) &&
                rec.sent[1].mi.dwFlags == MOUSEEVENTF_LEFTDOWN);
    CHECK("im-held-buttons", im.HeldButtons() == 1);
    // 释放:UP 事件在 MOVE 之前(在旧位置释放,再到新位置按下)
    rec.Reset();
    mv.seq = 2;
    mv.buttons = 0;
    mv.x = 0;
    mv.y = 0;
    CHECK("im-move-release-ok", im.Inject(mv) == xnc::InputManager::Result::kInjected);
    CHECK("im-move-leftup-before-move",
          rec.CountMouse(MOUSEEVENTF_LEFTUP) == 1 && rec.sent.size() == 2 &&
                rec.sent[0].mi.dwFlags == MOUSEEVENTF_LEFTUP &&
                rec.sent[1].mi.dwFlags == (MOUSEEVENTF_MOVE | MOUSEEVENTF_ABSOLUTE |
                                           MOUSEEVENTF_VIRTUALDESK));
    CHECK("im-held-buttons-zero", im.HeldButtons() == 0);
    // 多按钮位一次 MOVE 差分:M+X1 按下 + L 保持 0
    rec.Reset();
    mv.seq = 3;
    mv.buttons = xnc::kBtnM | xnc::kBtnX1;
    im.Inject(mv);
    CHECK("im-move-multi-down",
          rec.CountMouse(MOUSEEVENTF_MIDDLEDOWN) == 1 &&
                rec.CountMouse(MOUSEEVENTF_XDOWN) == 1);
    const INPUT* xd = rec.FindMouse(MOUSEEVENTF_XDOWN);
    CHECK("im-move-xdown-data", xd != nullptr && xd->mi.mouseData == XBUTTON1);
    CHECK("im-held-buttons-two", im.HeldButtons() == 2);
    // BUTTON 消息:显式 down/up,重复幂等(seq 仍须递增)
    rec.Reset();
    xnc::InputMsg bt;
    bt.sub_id = 7;
    bt.seq = 4;
    bt.type = xnc::kInputButton;
    bt.btn = xnc::kBtnR;
    bt.down = 1;
    CHECK("im-button-down", im.Inject(bt) == xnc::InputManager::Result::kInjected &&
                                  rec.CountMouse(MOUSEEVENTF_RIGHTDOWN) == 1);
    rec.Reset();
    bt.seq = 5;
    CHECK("im-button-down-idempotent",
          im.Inject(bt) == xnc::InputManager::Result::kInjected &&
                rec.sent.empty());  // 重复 down 不再注入(保持原按住时间)
    bt.down = 0;
    bt.seq = 6;
    rec.Reset();
    CHECK("im-button-up", im.Inject(bt) == xnc::InputManager::Result::kInjected &&
                                 rec.CountMouse(MOUSEEVENTF_RIGHTUP) == 1);
    rec.Reset();
    bt.seq = 7;
    CHECK("im-button-up-idempotent",
          im.Inject(bt) == xnc::InputManager::Result::kInjected && rec.sent.empty());
    // WHEEL:notch × WHEEL_DELTA / trackpad 原样;水平 HWHEEL
    rec.Reset();
    xnc::InputMsg wh;
    wh.sub_id = 7;
    wh.seq = 8;
    wh.type = xnc::kInputWheel;
    wh.y = -2;  // 2 notches down
    wh.x = 0;
    CHECK("im-wheel-notch",
          im.Inject(wh) == xnc::InputManager::Result::kInjected &&
                rec.CountMouse(MOUSEEVENTF_WHEEL) == 1);
    {
      const INPUT* e = rec.FindMouse(MOUSEEVENTF_WHEEL);
      CHECK("im-wheel-notch-scaled",
            e != nullptr && e->mi.mouseData == static_cast<DWORD>(-2 * WHEEL_DELTA));
    }
    rec.Reset();
    wh.seq = 9;
    wh.y = 0;
    wh.x = 3;  // horizontal, trackpad granularity
    wh.trackpad = 1;
    CHECK("im-wheel-trackpad-hwheel",
          im.Inject(wh) == xnc::InputManager::Result::kInjected &&
                rec.CountMouse(MOUSEEVENTF_HWHEEL) == 1);
    {
      const INPUT* e = rec.FindMouse(MOUSEEVENTF_HWHEEL);
      CHECK("im-wheel-trackpad-raw", e != nullptr && e->mi.mouseData == 3u);
    }
    rec.Reset();
    wh.seq = 10;
    wh.x = 0;
    wh.y = 0;
    CHECK("im-wheel-zero-noop",
          im.Inject(wh) == xnc::InputManager::Result::kInjected && rec.sent.empty());
    // KEY:SCANCODE + extended;重复 down 幂等;UP 带 KEYUP
    rec.Reset();
    xnc::InputMsg key;
    key.sub_id = 7;
    key.seq = 11;
    key.type = xnc::kInputKey;
    key.scan = 0x1D;  // Ctrl
    key.down = 1;
    CHECK("im-key-down",
          im.Inject(key) == xnc::InputManager::Result::kInjected &&
                rec.CountKey(KEYEVENTF_SCANCODE) == 1);
    {
      const INPUT* e = rec.FindKey(KEYEVENTF_SCANCODE);
      CHECK("im-key-down-fields",
            e != nullptr && e->ki.wScan == 0x1D && e->ki.wVk == 0 &&
                  e->ki.dwExtraInfo == xnc::kInputExtraInfoMarker);
    }
    CHECK("im-held-keys", im.HeldKeys() == 1);
    rec.Reset();
    key.seq = 12;
    CHECK("im-key-down-idempotent",
          im.Inject(key) == xnc::InputManager::Result::kInjected && rec.sent.empty());
    rec.Reset();
    key.seq = 13;
    key.scan = 0x47;  // NumpadHome-ish; E0-prefixed variant needs extended
    key.extended = 1;
    CHECK("im-key-ext-down",
          im.Inject(key) == xnc::InputManager::Result::kInjected &&
                rec.CountKey(KEYEVENTF_SCANCODE | KEYEVENTF_EXTENDEDKEY) == 1);
    CHECK("im-held-keys-two", im.HeldKeys() == 2);
    rec.Reset();
    key.seq = 14;
    key.down = 0;
    CHECK("im-key-ext-up",
          im.Inject(key) == xnc::InputManager::Result::kInjected &&
                rec.CountKey(KEYEVENTF_SCANCODE | KEYEVENTF_EXTENDEDKEY |
                             KEYEVENTF_KEYUP) == 1);
    CHECK("im-held-keys-one", im.HeldKeys() == 1);
    // TEXT:KEYEVENTF_UNICODE 每码位 down+up;代理对 = 两个码元
    rec.Reset();
    xnc::InputMsg tx;
    tx.sub_id = 7;
    tx.seq = 15;
    tx.type = xnc::kInputText;
    tx.text = {0xD834, 0xDD1E};
    CHECK("im-text-ok",
          im.Inject(tx) == xnc::InputManager::Result::kInjected &&
                rec.sent.size() == 4);
    if (rec.sent.size() == 4) {
      bool ok = true;
      const WORD want_scans[4] = {0xD834, 0xD834, 0xDD1E, 0xDD1E};
      const DWORD want_flags[4] = {KEYEVENTF_UNICODE,
                                   KEYEVENTF_UNICODE | KEYEVENTF_KEYUP,
                                   KEYEVENTF_UNICODE,
                                   KEYEVENTF_UNICODE | KEYEVENTF_KEYUP};
      for (int i = 0; i < 4; ++i)
        if (rec.sent[i].type != INPUT_KEYBOARD ||
            rec.sent[i].ki.wScan != want_scans[i] ||
            rec.sent[i].ki.dwFlags != want_flags[i] ||
            rec.sent[i].ki.dwExtraInfo != xnc::kInputExtraInfoMarker)
          ok = false;
      CHECK("im-text-surrogate-events", ok);
    }
    // LOCK:与 fake GetKeyState 差异才注入;CapsLock 0x3A / NumLock 0x45
    // 都为 plain 扫描码(T6 实测:E0 前缀的 NumLock 不翻转 VK_NUMLOCK)。
    rec.Reset();
    g_vk_state[VK_CAPITAL] = 0;
    g_vk_state[VK_NUMLOCK] = 0;
    xnc::InputMsg lk;
    lk.sub_id = 7;
    lk.seq = 16;
    lk.type = xnc::kInputLock;
    lk.caps = 1;
    lk.num = 1;
    CHECK("im-lock-both",
          im.Inject(lk) == xnc::InputManager::Result::kInjected &&
                rec.CountKey(KEYEVENTF_SCANCODE) == 2 &&           // caps+num down
                rec.CountKey(KEYEVENTF_SCANCODE | KEYEVENTF_KEYUP) == 2);  // ups
    {
      bool num_plain = false, caps_plain = false;
      for (const INPUT& i : rec.sent) {
        if (i.type != INPUT_KEYBOARD) continue;
        if (i.ki.dwFlags == KEYEVENTF_SCANCODE && i.ki.wScan == 0x45) num_plain = true;
        if (i.ki.dwFlags == KEYEVENTF_SCANCODE && i.ki.wScan == 0x3A) caps_plain = true;
      }
      CHECK("im-lock-numlock-plain", num_plain);
      CHECK("im-lock-capslock-plain", caps_plain);
      CHECK("im-lock-no-extended",
            rec.CountKey(KEYEVENTF_SCANCODE | KEYEVENTF_EXTENDEDKEY) == 0);
    }
    rec.Reset();
    lk.seq = 17;
    g_vk_state[VK_CAPITAL] = 1;  // 注入已生效(fake 表模拟真实翻转)
    g_vk_state[VK_NUMLOCK] = 1;
    CHECK("im-lock-match-noop",
          im.Inject(lk) == xnc::InputManager::Result::kInjected && rec.sent.empty());
    rec.Reset();
    lk.seq = 18;
    lk.caps = 0;
    lk.num = 0;
    CHECK("im-lock-off-diff",
          im.Inject(lk) == xnc::InputManager::Result::kInjected &&
                rec.sent.size() == 4);
    g_input_rec = nullptr;
  }
  // ---- M1-Slice3 Task 1:seq 单调(每 sub_id 严格递增) ----
  {
    ResetInputSeams(200, 100);
    InputRecorder rec;
    g_input_rec = &rec;
    xnc::InputManager im(TestInputOpts(200, 100));
    xnc::InputMsg m;
    m.sub_id = 7;
    m.type = xnc::kInputButton;
    m.btn = xnc::kBtnL;
    m.down = 1;
    m.seq = 5;
    CHECK("seq-first-ok", im.Inject(m) == xnc::InputManager::Result::kInjected);
    m.seq = 5;  // 重放
    CHECK("seq-replay-dropped",
          im.Inject(m) == xnc::InputManager::Result::kStaleSeq && rec.sent.size() == 1);
    m.seq = 4;  // 回退
    CHECK("seq-backward-dropped",
          im.Inject(m) == xnc::InputManager::Result::kStaleSeq);
    m.seq = 6;
    CHECK("seq-next-ok", im.Inject(m) == xnc::InputManager::Result::kInjected);
    // 不同 sub_id 独立基线
    m.sub_id = 8;
    m.seq = 1;
    CHECK("seq-per-sub-independent",
          im.Inject(m) == xnc::InputManager::Result::kInjected);
    // ForgetSub 清基线:同 sub_id 重连后 seq 从头可接受
    im.ForgetSub(8);
    CHECK("seq-forgotten-restart",
          im.Inject(m) == xnc::InputManager::Result::kInjected);
    im.ForgetSub(99);  // 未知 sub:无害
    CHECK("seq-forget-unknown-ok", true);
    // 非法类型(解码层之后防御)
    xnc::InputMsg bad;
    bad.sub_id = 7;
    bad.seq = 100;
    bad.type = 42;
    CHECK("seq-invalid-type",
          im.Inject(bad) == xnc::InputManager::Result::kInvalid);
    xnc::InputManager::Stats st = im.stats();
    CHECK("seq-stats", st.stale_seq == 2 && st.invalid == 1 && st.injected == 4);
    g_input_rec = nullptr;
  }
  // ---- M1-Slice3 Task 1:卡键 janitor(注入时钟:10s 扫描 / 30s 强制释放) ----
  {
    CHECK("janitor-consts", xnc::kJanitorScanMs == 10000 && xnc::kStuckReleaseMs == 30000);
    ResetInputSeams(200, 100);
    InputRecorder rec;
    g_input_rec = &rec;
    xnc::InputManager im(TestInputOpts(200, 100));
    xnc::InputMsg key;
    key.sub_id = 7;
    key.seq = 1;
    key.type = xnc::kInputKey;
    key.scan = 0x2A;  // left shift
    key.down = 1;
    im.Inject(key);
    xnc::InputMsg bt;
    bt.sub_id = 7;
    bt.seq = 2;
    bt.type = xnc::kInputButton;
    bt.btn = xnc::kBtnM;
    bt.down = 1;
    im.Inject(bt);
    rec.Reset();
    im.JanitorSweep(g_fake_now + 29000);  // 未到 30s:不动
    CHECK("janitor-under-30s-held",
          rec.sent.empty() && im.HeldKeys() == 1 && im.HeldButtons() == 1);
    im.JanitorSweep(g_fake_now + 31000);  // 超时:强制 KeyUp/按钮 UP
    CHECK("janitor-forced-release",
          rec.CountKey(KEYEVENTF_SCANCODE | KEYEVENTF_KEYUP) == 1 &&
                rec.CountMouse(MOUSEEVENTF_MIDDLEUP) == 1 && im.HeldKeys() == 0 &&
                im.HeldButtons() == 0);
    CHECK("janitor-stats", im.stats().janitor_released == 2);
    // ReleaseAll:混合键钮全部强制释放(断连/drain 语义)
    rec.Reset();
    g_fake_now += 40000;
    key.seq = 3;
    bt.seq = 4;
    im.Inject(key);
    im.Inject(bt);
    im.ReleaseAll();
    CHECK("release-all-sends-ups",
          rec.CountKey(KEYEVENTF_SCANCODE | KEYEVENTF_KEYUP) == 1 &&
                rec.CountMouse(MOUSEEVENTF_MIDDLEUP) == 1 && im.HeldKeys() == 0 &&
                im.HeldButtons() == 0);
    CHECK("release-all-stats", im.stats().release_all == 1);
    rec.Reset();
    im.ReleaseAll();  // 空表幂等
    CHECK("release-all-idempotent", rec.sent.empty());
    g_input_rec = nullptr;
  }
  // ---- M1-Slice3 Task 1:SendInput 失败 → 重绑 input desktop → 重试一次 ----
  {
    ResetInputSeams(200, 100);
    InputRecorder rec;
    g_input_rec = &rec;
    xnc::InputManager im(TestInputOpts(200, 100));
    rec.fail_mode = 1;  // 首批失败,重试成功
    xnc::InputMsg key;
    key.sub_id = 7;
    key.seq = 1;
    key.type = xnc::kInputKey;
    key.scan = 0x1E;
    key.down = 1;
    CHECK("rebind-retry-ok",
          im.Inject(key) == xnc::InputManager::Result::kInjected);
    CHECK("rebind-calls", g_open_desk_calls == 1 && g_set_desk_calls == 1);
    CHECK("rebind-retried-batch",
          rec.CountKey(KEYEVENTF_SCANCODE) == 1);  // 重试批次送达
    rec.Reset();
    rec.fail_mode = 2;  // 永远失败
    key.seq = 2;
    key.scan = 0x1F;
    CHECK("rebind-still-fails",
          im.Inject(key) == xnc::InputManager::Result::kSendFailed);
    xnc::InputManager::Stats st = im.stats();
    CHECK("rebind-stats",
          st.desktop_rebinds == 2 && st.desktop_mismatch == 1 && st.send_failures == 1);
    g_input_rec = nullptr;
  }
  // ---- M1-Slice3 Task 1:CursorManager(8ms 轮询,变化才发) ----
  {
    ResetInputSeams(200, 100);
    xnc::CursorManager cm(TestCursorOpts(200, 100));
    CHECK("cursor-not-running", !cm.running());
    int32_t ev_x = -1, ev_y = -1;
    uint8_t ev_vis = 0xFF;
    int fired = 0;
    auto sink = [&](int32_t x, int32_t y, uint8_t v) {
      ev_x = x;
      ev_y = y;
      ev_vis = v;
      fired++;
    };
    g_cursor.x = 100;
    g_cursor.y = 50;
    g_cursor.flags = CURSOR_SHOWING;
    g_cursor.ok = TRUE;
    cm.SetSink(sink);
    CHECK("cursor-first-sample-fires", cm.PollOnce() && fired == 1 &&
                                             ev_x == 100 && ev_y == 50 && ev_vis == 1);
    CHECK("cursor-unchanged-quiet", !cm.PollOnce() && fired == 1);
    g_cursor.x = 150;
    CHECK("cursor-move-fires", cm.PollOnce() && fired == 2 && ev_x == 150);
    g_cursor.flags = 0;  // hidden
    CHECK("cursor-visibility-fires",
          cm.PollOnce() && fired == 3 && ev_vis == 0);
    g_cursor.ok = FALSE;  // GetCursorInfo 失败(如无桌面):静默跳过
    CHECK("cursor-fail-quiet", !cm.PollOnce() && fired == 3);
    g_cursor.ok = TRUE;
    g_cursor.flags = CURSOR_SHOWING;
    g_cursor.x = -400;  // 虚拟桌面外 → 钳到 0
    CHECK("cursor-clamped", cm.PollOnce() && fired == 4 && ev_x == 0);
    // 多屏指标:vd (-1920,3840),hello 3840 → 中点 1920
    g_metrics[SM_XVIRTUALSCREEN] = -1920;
    g_metrics[SM_CXVIRTUALSCREEN] = 3840;
    g_metrics[SM_YVIRTUALSCREEN] = 0;
    g_metrics[SM_CYVIRTUALSCREEN] = 1080;
    xnc::CursorManager::Opts mo = TestCursorOpts(3840, 1080);
    xnc::CursorManager cm2(mo);
    g_cursor.x = 0;
    g_cursor.y = 540;
    int fired2 = 0;
    int32_t x2 = -1, y2 = -1;
    auto sink2 = [&](int32_t x, int32_t y, uint8_t) {
      x2 = x;
      y2 = y;
      fired2++;
    };
    cm2.SetSink(sink2);
    CHECK("cursor-multi-monitor-map", cm2.PollOnce() && fired2 == 1 &&
                                          x2 == 1920 && y2 == 540);
    CHECK("cursor-poll-default-8ms", xnc::CursorManager::Opts().poll_ms == 8);
    // 真线程生命周期冒烟:Start → running → Stop(无桌面时轮询静默失败)
    xnc::CursorManager cm3(TestCursorOpts(200, 100));
    int smoked = 0;
    cm3.Start([&smoked](int32_t, int32_t, uint8_t) { smoked++; });
    CHECK("cursor-start-running", cm3.running());
    Sleep(50);
    cm3.Start([&smoked](int32_t, int32_t, uint8_t) { smoked++; });  // 幂等
    cm3.Stop();
    CHECK("cursor-stopped", !cm3.running());
    cm3.Stop();  // 幂等
    // Slice3 Task 3 顺带修(T1 review carry):并发 Start/Stop 紧循环回归门
    // —— 旧实现里 Stop 换 running_ 但尚未 join 时 Start 的 th_ move-assign
    // 落在 joinable 线程上会 std::terminate 带崩进程(≤8ms 交换窗口)。
    xnc::CursorManager cm4(TestCursorOpts(200, 100));
    std::atomic<int> hammered{0};
    auto sink4 = [&hammered](int32_t, int32_t, uint8_t) { hammered++; };
    std::thread starter([&cm4, &sink4] {
      for (int i = 0; i < 300; ++i) cm4.Start(sink4);
    });
    std::thread stopper([&cm4] {
      for (int i = 0; i < 300; ++i) cm4.Stop();
    });
    starter.join();
    stopper.join();
    cm4.Stop();
    CHECK("cursor-start-stop-race-survived", !cm4.running());
  }
  // ---- M1-Slice3 Task 1:端到端(fake 订阅者 → 0x0108 → InputManager;
  //      CursorManager → 0x0109 → fake 订阅者)----
  {
    ResetInputSeams(200, 100);
    InputRecorder rec;
    g_input_rec = &rec;
    xnc::InputManager im(TestInputOpts(200, 100));
    xnc::CursorManager cm(TestCursorOpts(200, 100));
    g_cursor.x = 100;
    g_cursor.y = 50;
    g_cursor.flags = CURSOR_SHOWING;
    g_cursor.ok = TRUE;
    xnc::RtServer rt;
    xnc::RtServer::Opts ro;
    ro.pipe_name = RtPipeNameOf(4);
    ro.secret = kRtSecret;
    ro.secret_len = sizeof(kRtSecret);
    ro.max_subs = 4;
    ro.fps = 15;
    ro.bitrate_bps = 500000;
    ro.sddl_override = L"D:P(A;;GA;;;WD)";
    ro.input = &im;
    ro.cursor = &cm;
    CHECK("rt5-start", rt.Start(ro, 200, 100));
    RtTestClient a;
    CHECK("rt5-connect", a.Connect(ro.pipe_name.c_str(), kRtSecret, sizeof(kRtSecret)));
    CHECK("rt5-attach", a.Attach(11));
    // 光标:首个订阅者 → 轮询启动 → 初始事件(映射到 HOST_HELLO 空间)
    a.Pump(1000, [&a] { return a.cursors_ >= 1; });
    CHECK("rt5-cursor-initial-event",
          a.cursors_ >= 1 && a.cursor_x_ == 100 && a.cursor_y_ == 50 &&
              a.cursor_visible_ == 1);
    uint64_t cur_first = a.cursors_;
    g_cursor.x = 150;
    g_cursor.y = 60;
    a.Pump(1000, [&a, cur_first] { return a.cursors_ >= cur_first + 1; });
    CHECK("rt5-cursor-move-event",
          a.cursors_ >= cur_first + 1 && a.cursor_x_ == 150 && a.cursor_y_ == 60);
    // 输入:MOVE 注入到 InputManager(seq 1)
    xnc::InputMsg mv;
    mv.sub_id = 11;
    mv.seq = 1;
    mv.type = xnc::kInputMove;
    mv.x = 100;
    mv.y = 50;
    CHECK("rt5-input-send", a.SendRaw(xnc::kMsgInput, xnc::EncodeInputMsg(mv)));
    CHECK("rt5-input-injected",
          WaitUntil([&rec] {
            return rec.CountMouse(MOUSEEVENTF_MOVE | MOUSEEVENTF_ABSOLUTE |
                                  MOUSEEVENTF_VIRTUALDESK) >= 1;
          }, 2000));
    {
      const INPUT* e = rec.FindMouse(MOUSEEVENTF_MOVE | MOUSEEVENTF_ABSOLUTE |
                                     MOUSEEVENTF_VIRTUALDESK);
      CHECK("rt5-input-abs", e != nullptr && e->mi.dx == 32768 && e->mi.dy == 32768);
    }
    // seq 重放 → 服务端丢弃计数
    CHECK("rt5-input-replay-send", a.SendRaw(xnc::kMsgInput, xnc::EncodeInputMsg(mv)));
    // 非法按钮 → 解码拒绝
    xnc::InputMsg bad;
    bad.sub_id = 11;
    bad.seq = 2;
    bad.type = xnc::kInputButton;
    bad.btn = 3;
    bad.down = 1;
    CHECK("rt5-input-badbtn-send", a.SendRaw(xnc::kMsgInput, xnc::EncodeInputMsg(bad)));
    // sub_id 不匹配(他人 sub)→ 拒绝
    xnc::InputMsg alien;
    alien.sub_id = 99;
    alien.seq = 1;
    alien.type = xnc::kInputButton;
    alien.btn = xnc::kBtnL;
    alien.down = 1;
    CHECK("rt5-input-alien-send",
          a.SendRaw(xnc::kMsgInput, xnc::EncodeInputMsg(alien)));
    CHECK("rt5-input-stats",
          WaitUntil([&rt] {
            const xnc::RtServer::Stats st = rt.stats();
            return st.input_received >= 4 && st.input_dropped >= 1 &&
                   st.input_rejected >= 2;
          }, 2000));
    // 按住键 → 断开(最后一个订阅者)→ ReleaseAll 强制释放
    xnc::InputMsg key;
    key.sub_id = 11;
    key.seq = 3;
    key.type = xnc::kInputKey;
    key.scan = 0x1D;
    key.down = 1;
    CHECK("rt5-key-send", a.SendRaw(xnc::kMsgInput, xnc::EncodeInputMsg(key)));
    CHECK("rt5-key-held",
          WaitUntil([&im] { return im.HeldKeys() == 1; }, 2000));
    CHECK("rt5-detach", a.SendDetach(11));
    CHECK("rt5-release-all",
          WaitUntil([&rec] {
            return rec.CountKey(KEYEVENTF_SCANCODE | KEYEVENTF_KEYUP) >= 1;
          }, 2000));
    CHECK("rt5-released-state", im.HeldKeys() == 0 && im.HeldButtons() == 0);
    CHECK("rt5-cursor-stopped-after-last-detach", !cm.running());
    rt.Shutdown();
    g_input_rec = nullptr;
  }
  // ---- M2-Slice1 Task 1:DesktopWatch 状态机(纯转移函数,表驱动)----
  {
    CHECK("dw-state-name", std::strcmp(xnc::DesktopStateName(xnc::DesktopState::kDefault), "Default") == 0 &&
                              std::strcmp(xnc::DesktopStateName(xnc::DesktopState::kTransition), "Transition") == 0 &&
                              std::strcmp(xnc::DesktopStateName(xnc::DesktopState::kWinlogon), "Winlogon") == 0);
    CHECK("dw-is-default", xnc::IsDefaultDesktopName("Default") &&
                               xnc::IsDefaultDesktopName("default") &&
                               !xnc::IsDefaultDesktopName("Winlogon") &&
                               !xnc::IsDefaultDesktopName(""));
    // 表驱动:观察序列 (t, name) → 期望事件 (at, name, from, to, reason)
    struct Ev {
      uint64_t at;
      const char* name;
      xnc::DesktopState from, to;
      const char* reason;
    };
    struct Case {
      const char* name;
      uint64_t timeout;
      std::vector<std::pair<uint64_t, const char*>> obs;
      std::vector<Ev> want;
    };
    const std::vector<Case> cases = {
        // 1 全 Default:无事件
        {"stay-default", 2000,
         {{0, "Default"}, {500, "Default"}, {5000, "Default"}}, {}},
        // 2 UAC 型:Default→Winlogon 稳态;TRANSITION 立即,2s 超时升 WINLOGON,回 Default
        {"uac-enter-leave", 2000,
         {{0, "Default"}, {500, "Winlogon"}, {1000, "Winlogon"},
          {1500, "Winlogon"}, {2000, "Winlogon"}, {2500, "Winlogon"},
          {3000, "Default"}},
         {{500, "Winlogon", xnc::DesktopState::kDefault, xnc::DesktopState::kTransition, "name_change"},
          {2500, "Winlogon", xnc::DesktopState::kTransition, xnc::DesktopState::kWinlogon, "transition_timeout"},
          {3000, "Default", xnc::DesktopState::kWinlogon, xnc::DesktopState::kDefault, "back_to_default"}}},
        // 3 短毛刺(<2s):TRANSITION→DEFAULT,绝不进 WINLOGON
        {"blip-under-timeout", 2000,
         {{0, "Default"}, {500, "Winlogon"}, {1000, "Default"}, {5000, "Default"}},
         {{500, "Winlogon", xnc::DesktopState::kDefault, xnc::DesktopState::kTransition, "name_change"},
          {1000, "Default", xnc::DesktopState::kTransition, xnc::DesktopState::kDefault, "back_to_default"}}},
        // 4 超时边界:elapsed 1999 不升,2000 升
        {"timeout-boundary", 2000,
         {{0, "Default"}, {1000, "Winlogon"}, {2999, "Winlogon"}, {3000, "Winlogon"}},
         {{1000, "Winlogon", xnc::DesktopState::kDefault, xnc::DesktopState::kTransition, "name_change"},
          {3000, "Winlogon", xnc::DesktopState::kTransition, xnc::DesktopState::kWinlogon, "transition_timeout"}}},
        // 5 非默认期间换名:计时器重启(TRANSITION→TRANSITION 也发事件)
        {"name-change-restarts-timer", 2000,
         {{0, "Default"}, {1000, "Winlogon"}, {2500, "OtherDesk"},
          {3500, "OtherDesk"}, {4499, "OtherDesk"}, {4500, "OtherDesk"}},
         {{1000, "Winlogon", xnc::DesktopState::kDefault, xnc::DesktopState::kTransition, "name_change"},
          {2500, "OtherDesk", xnc::DesktopState::kTransition, xnc::DesktopState::kTransition, "name_change"},
          {4500, "OtherDesk", xnc::DesktopState::kTransition, xnc::DesktopState::kWinlogon, "transition_timeout"}}},
        // 6 WINLOGON 期间换名:退回 TRANSITION 重新计时
        {"winlogon-new-name-demotes", 2000,
         {{0, "Default"}, {500, "Winlogon"}, {2500, "Winlogon"},
          {3000, "Screen-0"}, {5000, "Screen-0"}},
         {{500, "Winlogon", xnc::DesktopState::kDefault, xnc::DesktopState::kTransition, "name_change"},
          {2500, "Winlogon", xnc::DesktopState::kTransition, xnc::DesktopState::kWinlogon, "transition_timeout"},
          {3000, "Screen-0", xnc::DesktopState::kWinlogon, xnc::DesktopState::kTransition, "name_change"},
          {5000, "Screen-0", xnc::DesktopState::kTransition, xnc::DesktopState::kWinlogon, "transition_timeout"}}},
        // 7 轮询失败合成名 (open_failed) 同样是状态机输入(证据:安全桌面可能拒绝打开)
        {"open-failed-is-input", 2000,
         {{0, "Default"}, {500, "(open_failed)"}, {2500, "(open_failed)"},
          {3000, "Default"}},
         {{500, "(open_failed)", xnc::DesktopState::kDefault, xnc::DesktopState::kTransition, "name_change"},
          {2500, "(open_failed)", xnc::DesktopState::kTransition, xnc::DesktopState::kWinlogon, "transition_timeout"},
          {3000, "Default", xnc::DesktopState::kWinlogon, xnc::DesktopState::kDefault, "back_to_default"}}},
        // 8 自定义超时(300ms)+ 小写 default 等价回退
        {"custom-timeout-lowercase", 300,
         {{0, "Default"}, {500, "Winlogon"}, {799, "Winlogon"},
          {800, "Winlogon"}, {900, "default"}},
         {{500, "Winlogon", xnc::DesktopState::kDefault, xnc::DesktopState::kTransition, "name_change"},
          {800, "Winlogon", xnc::DesktopState::kTransition, xnc::DesktopState::kWinlogon, "transition_timeout"},
          {900, "default", xnc::DesktopState::kWinlogon, xnc::DesktopState::kDefault, "back_to_default"}}},
    };
    for (const Case& c : cases) {
      const std::string tag = std::string("dw-") + c.name;
      xnc::DesktopMachineState st;
      std::vector<xnc::DesktopTransition> got;
      for (const auto& o : c.obs) {
        xnc::DesktopTransition ev;
        if (xnc::DesktopStep(&st, o.second, o.first, c.timeout, &ev))
          got.push_back(ev);
      }
      CHECK((tag + "-count").c_str(), got.size() == c.want.size());
      const size_t n = got.size() < c.want.size() ? got.size() : c.want.size();
      for (size_t i = 0; i < n; ++i) {
        const bool eq = got[i].at_ms == c.want[i].at &&
                        std::strcmp(got[i].name, c.want[i].name) == 0 &&
                        got[i].from == c.want[i].from && got[i].to == c.want[i].to &&
                        std::strcmp(got[i].reason, c.want[i].reason) == 0;
        CHECK((tag + "-ev" + std::to_string(i)).c_str(), eq);
      }
    }
    // 空/空指针观察:忽略,状态不动
    {
      xnc::DesktopMachineState st;
      xnc::DesktopTransition ev;
      CHECK("dw-empty-name-ignored", !xnc::DesktopStep(&st, "", 100, 2000, &ev));
      CHECK("dw-null-name-ignored", !xnc::DesktopStep(&st, nullptr, 100, 2000, &ev));
      CHECK("dw-null-state-ignored", !xnc::DesktopStep(nullptr, "Default", 100, 2000, &ev));
      CHECK("dw-state-untouched", st.state == xnc::DesktopState::kDefault);
    }
  }
  // ---- M2-Slice1 Task 1:DesktopWatch 线程版(fake seams:句柄配对 + 事件序列)----
  {
    // 脚本:t=500 Default;t=1000..3000 Winlogon(TRANSITION@1000,WINLOGON@3000,
    // 3000-1000=2000);t=3500+ Default(回退)。队列耗尽后重复最后样本(稳态)。
    std::vector<xnc::DesktopTransition> fired;
    ResetWatchSeams({"Default", "Winlogon", "Winlogon", "Winlogon", "Winlogon",
                     "Winlogon", "Default", "Default"});
    xnc::DesktopWatch w(TestWatchOpts([&](const xnc::DesktopTransition& t) {
      std::lock_guard<std::mutex> lk(g_dw_ev_mu);
      fired.push_back(t);
    }));
    CHECK("dw-watch-start", w.Start());
    CHECK("dw-watch-running", w.running());
    const bool done = WaitUntil([&fired] {
      std::lock_guard<std::mutex> lk(g_dw_ev_mu);
      return fired.size() >= 3;
    }, 5000);
    w.Stop();
    CHECK("dw-watch-stopped", !w.running());
    CHECK("dw-watch-events", done && fired.size() == 3);
    {
      std::lock_guard<std::mutex> lk(g_dw_ev_mu);
      if (fired.size() == 3) {
        CHECK("dw-watch-ev0",
              fired[0].from == xnc::DesktopState::kDefault &&
                  fired[0].to == xnc::DesktopState::kTransition &&
                  std::strcmp(fired[0].reason, "name_change") == 0 &&
                  std::strcmp(fired[0].name, "Winlogon") == 0 &&
                  fired[0].at_ms == 1000);
        CHECK("dw-watch-ev1",
              fired[1].from == xnc::DesktopState::kTransition &&
                  fired[1].to == xnc::DesktopState::kWinlogon &&
                  std::strcmp(fired[1].reason, "transition_timeout") == 0 &&
                  fired[1].at_ms == 3000);  // 3000-1000 = 2000 = timeout
        CHECK("dw-watch-ev2",
              fired[2].from == xnc::DesktopState::kWinlogon &&
                  fired[2].to == xnc::DesktopState::kDefault &&
                  std::strcmp(fired[2].reason, "back_to_default") == 0);
      }
    }
    CHECK("dw-watch-final-state", w.Snapshot().state == xnc::DesktopState::kDefault);
    char nbuf[xnc::kDesktopNameMax];
    w.CurrentName(nbuf, sizeof(nbuf));
    CHECK("dw-watch-current-name", std::strcmp(nbuf, "Default") == 0);
    // 每次 open 都配对 close,且句柄哨兵正确(无泄漏/无双重 close)
    CHECK("dw-watch-handle-pairing",
          g_dw_opens == g_dw_closes && g_dw_bad_closes == 0 && g_dw_opens >= 7);
    CHECK("dw-watch-polls", w.polls() >= 7);
    CHECK("dw-watch-no-failures", w.poll_failures() == 0);
    w.Stop();  // idempotent
    CHECK("dw-watch-stop-idempotent", !w.running());
  }
  {
    // 轮询失败路径:OpenInputDesktop 拒绝 → 合成名 (open_failed) 进入状态机;
    // close 永不被调(null 句柄),失败计数可见
    std::vector<xnc::DesktopTransition> fired;
    ResetWatchSeams({"Default", "Winlogon"});
    g_dw_open_fail = true;
    xnc::DesktopWatch w(TestWatchOpts([&](const xnc::DesktopTransition& t) {
      std::lock_guard<std::mutex> lk(g_dw_ev_mu);
      fired.push_back(t);
    }));
    CHECK("dw-fail-start", w.Start());
    const bool done = WaitUntil([&fired] {
      std::lock_guard<std::mutex> lk(g_dw_ev_mu);
      return fired.size() >= 2;
    }, 5000);
    w.Stop();
    CHECK("dw-fail-events",
          done && fired.size() == 2 &&
              std::strcmp(fired[0].name, xnc::kDesktopOpenFailedName) == 0 &&
              fired[1].to == xnc::DesktopState::kWinlogon);
    CHECK("dw-fail-counted", w.polls() >= 2 && w.poll_failures() == w.polls());
    CHECK("dw-fail-no-close", g_dw_closes == 0 && g_dw_bad_closes == 0);
    CHECK("dw-fail-state", w.Snapshot().state == xnc::DesktopState::kWinlogon);
  }
  // ---- M2-Slice1 Task 2:CaptureReset 合并/去抖(纯逻辑,假时钟表驱动) ----
  {
    xnc::CaptureReset::Opts o;
    o.clock_ms = &CrUnitClock;
    o.desktop_fn = &CrUnitGate;
    // 去抖合并:t=1000 首请求,t=1050 第二请求 → t=1099 不可取,t=1100 可取
    // (一次 reset,merged=1);取后再取 = false
    {
      xnc::CaptureReset r(o);
      r.RequestReset("access_lost");
      g_cr_unit_now = 1050;
      r.RequestReset("access_lost");
      char reason[xnc::kResetReasonMax] = {0};
      g_cr_unit_now = 1099;
      CHECK("cr-window-not-elapsed", !r.TakeReset(reason, sizeof(reason)));
      g_cr_unit_now = 1100;
      CHECK("cr-window-elapsed", r.TakeReset(reason, sizeof(reason)) &&
                                     std::strcmp(reason, "access_lost") == 0);
      CHECK("cr-one-reset-for-two-requests",
            r.executed() == 1 && r.merged() == 1 && r.requests() == 2);
      CHECK("cr-consumed-empty", !r.TakeReset(reason, sizeof(reason)));
    }
    // 优先级覆盖:去抖窗口内 access_lost → desktop_switch(高优先级胜出);
    // 反向 desktop_switch → resolution 不降级
    {
      g_cr_unit_now = 1000;
      xnc::CaptureReset r(o);
      r.RequestReset("access_lost");
      g_cr_unit_now = 1050;
      r.RequestReset("desktop_switch");
      g_cr_unit_now = 1100;
      char reason[xnc::kResetReasonMax] = {0};
      CHECK("cr-priority-upgrade", r.TakeReset(reason, sizeof(reason)) &&
                                      std::strcmp(reason, "desktop_switch") == 0);
    }
    {
      g_cr_unit_now = 1000;
      xnc::CaptureReset r(o);
      r.RequestReset("desktop_switch");
      g_cr_unit_now = 1050;
      r.RequestReset("resolution");
      g_cr_unit_now = 1100;
      char reason[xnc::kResetReasonMax] = {0};
      CHECK("cr-priority-no-downgrade", r.TakeReset(reason, sizeof(reason)) &&
                                            std::strcmp(reason, "desktop_switch") == 0);
    }
    // 消费后新窗口:新请求重新去抖,executed 累计 2
    {
      g_cr_unit_now = 1000;
      xnc::CaptureReset r(o);
      r.RequestReset("resolution");
      g_cr_unit_now = 1100;
      char reason[xnc::kResetReasonMax] = {0};
      CHECK("cr-first-taken", r.TakeReset(reason, sizeof(reason)));
      r.RequestReset("access_lost");
      g_cr_unit_now = 1150;
      CHECK("cr-new-window-waits", !r.TakeReset(reason, sizeof(reason)));
      g_cr_unit_now = 1200;
      CHECK("cr-new-window-elapsed", r.TakeReset(reason, sizeof(reason)));
      CHECK("cr-executed-total", r.executed() == 2);
      char last[xnc::kResetReasonMax] = {0};
      r.LastReason(last, sizeof(last));
      CHECK("cr-last-reason", std::strcmp(last, "access_lost") == 0);
    }
    // reason 边界:null → "?";超长截断 23;门状态透传
    {
      g_cr_unit_now = 1000;
      xnc::CaptureReset r(o);
      r.RequestReset(nullptr);
      g_cr_unit_now = 1100;
      char reason[64] = {0};
      CHECK("cr-null-reason-default", r.TakeReset(reason, sizeof(reason)) &&
                                         std::strcmp(reason, "?") == 0);
      char long_reason[40];
      std::memset(long_reason, 'y', sizeof(long_reason) - 1);
      long_reason[sizeof(long_reason) - 1] = '\0';
      r.RequestReset(long_reason);
      g_cr_unit_now = 1200;
      char out[64] = {0};
      CHECK("cr-long-reason-taken", r.TakeReset(out, sizeof(out)));
      CHECK("cr-long-reason-bounded", std::strlen(out) == xnc::kResetReasonMax - 1);
      g_cr_gate.store(xnc::ResetDesktop::kNonDefault);
      CHECK("cr-gate-nondefault", r.Desktop() == xnc::ResetDesktop::kNonDefault);
      g_cr_gate.store(xnc::ResetDesktop::kDefault);
      CHECK("cr-gate-default", r.Desktop() == xnc::ResetDesktop::kDefault);
    }
    // 无时钟 = 立即可取(遗留行为族;文档化)
    {
      xnc::CaptureReset r;  // clock_ms == nullptr
      r.RequestReset("change_backend");
      char reason[xnc::kResetReasonMax] = {0};
      CHECK("cr-no-clock-immediate", r.TakeReset(reason, sizeof(reason)) &&
                                        std::strcmp(reason, "change_backend") == 0);
    }
  }
  // ---- M2-Slice2 Task 1:gate 稳定门/健康分回满/GDI away 提示/风暴退避 ----
  { // 纯逻辑:gate 稳定步进 + 2s 判定表
    CHECK("gate-step-anchor", xnc::GateDefaultSinceStep(true, 0, 7000) == 7000);
    CHECK("gate-step-hold", xnc::GateDefaultSinceStep(true, 7000, 8000) == 7000);
    CHECK("gate-step-clear", xnc::GateDefaultSinceStep(false, 7000, 8000) == 0);
    CHECK("gate-stable-no", !xnc::GateStableFor(7000, 8999, 2000));
    CHECK("gate-stable-yes", xnc::GateStableFor(7000, 9000, 2000));
    CHECK("gate-stable-unset", !xnc::GateStableFor(0, 9000, 2000));
  }
  { // Ladder:create-fail −40 仅在 gate DEFAULT 连续 ≥2s 记;重建成功 → 100
    FakeDxgiFactory dxgi;  // frames 充足,Rebuild 三败一成(away/early/stable/成功)
    dxgi.rebuild_fails = 3;
    FakeGdiFactory gdi;
    g_ld_dxgi = &dxgi;
    g_ld_gdi = &gdi;
    xnc::CaptureReset::Opts co;
    co.clock_ms = &CrUnitClock;
    co.desktop_fn = &CrUnitGate;
    xnc::CaptureReset reset(co);
    xnc::LadderOpts lo;
    lo.make_dxgi = &LdMakeDxgi;
    lo.make_gdi = &LdMakeGdi;
    lo.reset = &reset;
    lo.clock_ms = &CrUnitClock;
    lo.probe_interval_ms = 0;  // 无探测线程:纯表驱动
    g_cr_gate.store(xnc::ResetDesktop::kNonDefault);
    g_cr_unit_now = 10000;
    xnc::LadderCapture ladder(lo);
    std::string err;
    xnc::FrameBlob blob;
    CHECK("lcf-init", ladder.Init(&err) &&
                         ladder.active() == xnc::BackendKind::kDxgi);
    // away 期 Rebuild 失败:不记分(期望中的安全桌面拒绝尾部)
    CHECK("lcf-away-fail", !ladder.Rebuild(&err));
    CHECK("lcf-away-unscored", ladder.dxgi_health() == 100);
    // 回到 DEFAULT 但未满 2s:仍不记分
    g_cr_gate.store(xnc::ResetDesktop::kDefault);
    CHECK("lcf-acquire-anchor", ladder.Acquire(blob, &err));
    g_cr_unit_now = 11000;
    CHECK("lcf-acquire-hold", ladder.Acquire(blob, &err));
    CHECK("lcf-early-fail", !ladder.Rebuild(&err));
    CHECK("lcf-early-unscored", ladder.dxgi_health() == 100);
    // ≥2s 稳定:记 −40
    g_cr_unit_now = 12001;
    CHECK("lcf-acquire-stable", ladder.Acquire(blob, &err));
    CHECK("lcf-stable-fail", !ladder.Rebuild(&err));
    CHECK("lcf-stable-scored", ladder.dxgi_health() == 60);
    // CaptureReset 成功(rebuild_fails 已耗尽)→ 健康分回满 100
    CHECK("lcf-recover-ok", ladder.Rebuild(&err));
    CHECK("lcf-health-restored", ladder.dxgi_health() == 100);
  }
  { // Ladder:GDI 在役 + watch 非默认 → gdi_stale_secure_desktop 每 5s 一条
    FakeDxgiFactory dxgi;
    FakeGdiFactory gdi;
    g_ld_dxgi = &dxgi;
    g_ld_gdi = &gdi;
    xnc::CaptureReset::Opts co;
    co.clock_ms = &CrUnitClock;
    co.desktop_fn = &CrUnitGate;
    xnc::CaptureReset reset(co);
    xnc::LadderOpts lo;
    lo.make_dxgi = &LdMakeDxgi;
    lo.make_gdi = &LdMakeGdi;
    lo.reset = &reset;
    lo.clock_ms = &CrUnitClock;
    lo.probe_interval_ms = 0;
    lo.force_gdi = true;
    g_cr_gate.store(xnc::ResetDesktop::kNonDefault);
    g_cr_unit_now = 20000;
    xnc::LadderCapture ladder(lo);
    std::string err;
    xnc::FrameBlob blob;
    CHECK("lgd-init", ladder.Init(&err) &&
                         ladder.active() == xnc::BackendKind::kGdi);
    CHECK("lgd-first-notice", ladder.Acquire(blob, &err) &&
                                 ladder.gdi_away_notices() == 1);
    for (int i = 0; i < 5; ++i) ladder.Acquire(blob, &err);  // 同一时刻:节流
    CHECK("lgd-throttled", ladder.gdi_away_notices() == 1);
    g_cr_unit_now = 25000;  // 满 5s:再一条
    ladder.Acquire(blob, &err);
    CHECK("lgd-second-notice", ladder.gdi_away_notices() == 2);
    g_cr_gate.store(xnc::ResetDesktop::kDefault);  // 回默认:不再发
    g_cr_unit_now = 40000;
    ladder.Acquire(blob, &err);
    ladder.Acquire(blob, &err);
    CHECK("lgd-silent-when-default", ladder.gdi_away_notices() == 2);
  }
  { // 风暴退避序列(注入时钟表驱动):同 reason ≥3/10s → 1s/2s/4s…封顶 10s
    xnc::ResetStormTracker st;
    const uint64_t t = 0;
    CHECK("storm-quiescent", st.BackoffMs("access_lost", t) == 0);
    st.RecordExecuted("access_lost", t);
    st.RecordExecuted("access_lost", t + 1000);
    st.RecordExecuted("access_lost", t + 2000);
    uint32_t cnt = 99;
    CHECK("storm-4th-1s",
          st.BackoffMs("access_lost", t + 3000, &cnt) == 1000 && cnt == 3);
    st.RecordExecuted("access_lost", t + 4000);
    CHECK("storm-5th-2s", st.BackoffMs("access_lost", t + 5000) == 2000);
    st.RecordExecuted("access_lost", t + 7000);
    CHECK("storm-6th-4s", st.BackoffMs("access_lost", t + 8000) == 4000);
    // 窗口滑出:10s 后不再风暴
    st.RecordExecuted("access_lost", t + 30000);
    CHECK("storm-window-expired", st.BackoffMs("access_lost", t + 31000) == 0);
    // 异 reason: streak 重置
    st.RecordExecuted("desktop_switch", t + 32000);
    CHECK("storm-other-reason", st.BackoffMs("access_lost", t + 33000) == 0);
    // 密集注入:封顶 10s
    xnc::ResetStormTracker st2;
    for (int i = 0; i < 8; ++i) st2.RecordExecuted("access_lost", t + i * 100);
    CHECK("storm-capped-10s", st2.BackoffMs("access_lost", t + 1000) == 10000);
    st2.NoteStorm();
    st2.NoteStorm();
    CHECK("storm-counted", st2.storm_resets() == 2);
  }
  // ---- M2-Slice1 Task 2:管线 reset 编排(真 MF 编码器 + ResetCapture) ----
  const uint32_t kRsW = 64, kRsH = 48, kRsFps = 15, kRsBitrate = 500000;
  { // 场景 A:desktop switch 挂起/恢复 —— gate 翻非默认 + access_lost →
    // STATE recovering → 等 desktop 回来 → 重建 → capture_rebuilt → 帧恢复;
    // 期间几十次 err_access_lost 请求全部并入一次 reset
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(kRsW, kRsH, kRsFps, kRsBitrate, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: rsA-init err=%s\n", err.c_str());
    CHECK("rsA-init", init_ok);
    if (init_ok) {
      ResetCapture cap(kRsW, kRsH, 60);
      g_cr_gate.store(xnc::ResetDesktop::kDefault);
      xnc::CaptureReset::Opts co;
      co.clock_ms = &CrRealClock;
      co.desktop_fn = &CrUnitGate;
      xnc::CaptureReset reset(co);
      RecordingSink sink;
      xnc::PipelineOpts po;
      po.duration_s = 8;
      po.fps = kRsFps;
      po.target_bitrate_bps = kRsBitrate;
      po.reset = &reset;
      xnc::PipelineResult res;
      std::thread pipe_th([&] { res = xnc::Pipeline::Run(cap, enc, sink, po); });
      Sleep(800);  // healthy frames first
      const uint32_t before = cap.yielded();
      g_cr_gate.store(xnc::ResetDesktop::kNonDefault);  // secure desktop up
      cap.LoseAccess();
      Sleep(800);                                       // suspended, waiting
      // Task 6 (T2 deferred list): while the gate is away the reset sequence
      // must sit in its wait-desktop phase - ZERO rebuild attempts against
      // the secure desktop (T1 live evidence: re-duplication there is refused
      // 0x80070005 even as SYSTEM, so any attempt is wasted churn).
      CHECK("rsA-no-rebuild-while-away", cap.RebuildCount() == 0);
      g_cr_gate.store(xnc::ResetDesktop::kDefault);     // desktop returned
      cap.MoreFrames(120);
      pipe_th.join();
      CHECK("rsA-ok", res.ok);
      CHECK("rsA-frames-before-loss", before >= 5);
      CHECK("rsA-recovering-state", sink.Saw("recovering") && sink.SawRecoverable("recovering"));
      CHECK("rsA-rebuilt-state", sink.Saw("capture_rebuilt"));
      CHECK("rsA-no-fatal", !sink.Saw("capture_fatal") && !sink.Saw("capture_failed"));
      CHECK("rsA-single-reset", res.resets == 1 && reset.executed() == 1);
      CHECK("rsA-requests-merged", reset.requests() >= 3 && reset.merged() >= 2);
      CHECK("rsA-reason", std::strcmp(res.last_reset_reason, "desktop_switch") == 0);
      CHECK("rsA-single-rebuild", cap.RebuildCount() == 1);
      CHECK("rsA-frames-resumed", res.counters.captured > before + 5);
      CHECK("rsA-rebuild-idr", sink.keys >= 2);
      std::printf("SELFTEST NOTE: rsA captured=%llu keys=%llu resets=%u requests=%u merged=%u\n",
                  (unsigned long long)res.counters.captured, (unsigned long long)sink.keys,
                  res.resets, reset.requests(), reset.merged());
    }
  }
  { // 场景 B:ACCESS_LOST 且 watch=DEFAULT → 后端内部重建(err_rebuilt)
    // 不走统一 reset(resets==0, FrameCache.rebuilds==1, 无 recovering 态)
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(kRsW, kRsH, kRsFps, kRsBitrate, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: rsB-init err=%s\n", err.c_str());
    CHECK("rsB-init", init_ok);
    if (init_ok) {
      ScriptedCapture cap(kRsW, kRsH, 40, 10);  // Acquire#10 → err_rebuilt
      g_cr_gate.store(xnc::ResetDesktop::kDefault);
      xnc::CaptureReset::Opts co;
      co.clock_ms = &CrRealClock;
      co.desktop_fn = &CrUnitGate;
      xnc::CaptureReset reset(co);
      RecordingSink sink;
      xnc::PipelineOpts po;
      po.duration_s = 4;
      po.fps = kRsFps;
      po.target_bitrate_bps = kRsBitrate;
      po.reset = &reset;
      const xnc::PipelineResult res = xnc::Pipeline::Run(cap, enc, sink, po);
      CHECK("rsB-ok", res.ok);
      CHECK("rsB-internal-rebuild", res.counters.rebuilds == 1);
      CHECK("rsB-no-unified-reset", res.resets == 0 && reset.executed() == 0);
      CHECK("rsB-no-recovering", !sink.Saw("recovering"));
      CHECK("rsB-rebuilt-state", sink.Saw("capture_rebuilt"));
      CHECK("rsB-rebuild-idr", sink.keys >= 1);
    }
  }
  { // 场景 C:分辨率变化 → reset("resolution") → 编码器按新尺寸重 Init →
    // DISPLAY_CHANGED(w=96,h=64,reason=resolution)+ capture_rebuilt + gen++
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(kRsW, kRsH, kRsFps, kRsBitrate, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: rsC-init err=%s\n", err.c_str());
    CHECK("rsC-init", init_ok);
    if (init_ok) {
      ResetCapture cap(kRsW, kRsH, 60);
      g_cr_gate.store(xnc::ResetDesktop::kDefault);
      xnc::CaptureReset::Opts co;
      co.clock_ms = &CrRealClock;
      co.desktop_fn = &CrUnitGate;
      xnc::CaptureReset reset(co);
      RecordingSink sink;
      xnc::PipelineOpts po;
      po.duration_s = 8;
      po.fps = kRsFps;
      po.target_bitrate_bps = kRsBitrate;
      po.reset = &reset;
      xnc::PipelineResult res;
      std::thread pipe_th([&] { res = xnc::Pipeline::Run(cap, enc, sink, po); });
      Sleep(800);
      cap.SetResolution(96, 64);  // next frame arrives at the new size
      cap.MoreFrames(120);
      pipe_th.join();
      CHECK("rsC-ok", res.ok);
      CHECK("rsC-single-reset", res.resets == 1);
      CHECK("rsC-reason", std::strcmp(res.last_reset_reason, "resolution") == 0);
      CHECK("rsC-display-event", sink.displays.size() == 1 &&
                                     sink.displays[0].w == 96 && sink.displays[0].h == 64 &&
                                     sink.displays[0].reason == "resolution");
      CHECK("rsC-display-after-rebuilt",
            sink.Saw("capture_rebuilt") && sink.Saw("recovering"));
      CHECK("rsC-new-dims", res.width == 96 && res.height == 64);
      CHECK("rsC-frames-after", res.counters.captured >= 20);
      CHECK("rsC-idr-after-reset", sink.keys >= 2);
      // M1 Task 5 (ruling 1a): sink-level identity contract on the delivered
      // AUs - this scenario spans a resolution reset (capture AND codec epoch
      // advance), so the replay also covers the re-baseline across epochs.
      CHECK("rsC-delivered-identity-monotonic", DeliveredIdentitiesValid(sink.ids));
      std::printf("SELFTEST NOTE: rsC captured=%llu keys=%llu resets=%u w=%u h=%u\n",
                  (unsigned long long)res.counters.captured, (unsigned long long)sink.keys,
                  res.resets, res.width, res.height);
    }
  }
  { // 场景 D:重建连续失败(3 次)→ STATE capture_failed(可恢复,1s 档
    // 重试)→ 第 6 次成功 → capture_rebuilt → 帧恢复
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(kRsW, kRsH, kRsFps, kRsBitrate, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: rsD-init err=%s\n", err.c_str());
    CHECK("rsD-init", init_ok);
    if (init_ok) {
      ResetCapture cap(kRsW, kRsH, 30);
      cap.FailRebuilds(5);  // 3 次进入 hard 档,第 6 次成功
      g_cr_gate.store(xnc::ResetDesktop::kDefault);
      xnc::CaptureReset::Opts co;
      co.clock_ms = &CrRealClock;
      co.desktop_fn = &CrUnitGate;
      xnc::CaptureReset reset(co);
      RecordingSink sink;
      xnc::PipelineOpts po;
      po.duration_s = 10;
      po.fps = kRsFps;
      po.target_bitrate_bps = kRsBitrate;
      po.reset = &reset;
      xnc::PipelineResult res;
      std::thread pipe_th([&] { res = xnc::Pipeline::Run(cap, enc, sink, po); });
      Sleep(500);
      cap.LoseAccess();
      cap.MoreFrames(90);
      pipe_th.join();
      CHECK("rsD-ok", res.ok);
      CHECK("rsD-failed-state", sink.Saw("capture_failed") && sink.SawRecoverable("capture_failed"));
      CHECK("rsD-recovered-state", sink.Saw("capture_rebuilt"));
      CHECK("rsD-single-reset", res.resets == 1);
      CHECK("rsD-rebuild-attempts", cap.RebuildCount() == 6);
      CHECK("rsD-no-fatal", !sink.Saw("capture_fatal"));
      std::printf("SELFTEST NOTE: rsD rebuilds=%u states=%zu\n", cap.RebuildCount(),
                  sink.states.size());
    }
  }
  { // 场景 rt6(rt pipe 回环,slot 4):挂起期订阅者加入 → 恢复后拿到 IDR;
    // 分辨率变化 → 0x010A DISPLAY_CHANGED(gen=3, w=96, h=64, reason=resolution)
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(kRsW, kRsH, kRsFps, kRsBitrate, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: rt6-init err=%s\n", err.c_str());
    CHECK("rt6-init", init_ok);
    if (init_ok) {
      xnc::RtServer rt;
      const xnc::RtServer::Opts ro = rt_opts(4);
      CHECK("rt6-start", rt.Start(ro, kRsW, kRsH));
      ResetCapture cap(kRsW, kRsH, 60);
      g_cr_gate.store(xnc::ResetDesktop::kDefault);
      xnc::CaptureReset::Opts co;
      co.clock_ms = &CrRealClock;
      co.desktop_fn = &CrUnitGate;
      xnc::CaptureReset reset(co);
      xnc::PipelineOpts po;
      po.duration_s = 12;
      po.fps = kRsFps;
      po.target_bitrate_bps = kRsBitrate;
      po.reset = &reset;
      xnc::PipelineResult res;
      std::thread pipe_th([&] { res = xnc::Pipeline::Run(cap, enc, rt, po); });
      RtTestClient a;
      CHECK("rt6-a-connect", a.Connect(ro.pipe_name.c_str(), kRtSecret, sizeof(kRtSecret)));
      CHECK("rt6-a-attach", a.Attach(7));
      a.Pump(2500, [&a] { return a.keys_ >= 1; });  // initial warm-up IDR
      CHECK("rt6-a-first-key", a.keys_ >= 1);
      // 挂起:secure desktop 上来
      g_cr_gate.store(xnc::ResetDesktop::kNonDefault);
      cap.LoseAccess();
      Sleep(300);
      // 恢复期订阅者 B 加入(join-while-recovering)
      RtTestClient b;
      CHECK("rt6-b-connect", b.Connect(ro.pipe_name.c_str(), kRtSecret, sizeof(kRtSecret)));
      CHECK("rt6-b-attach", b.Attach(9));
      CHECK("rt6-b-hello-gen-1", b.hello_ok_ && b.hello_.gen == 1);
      Sleep(600);
      g_cr_gate.store(xnc::ResetDesktop::kDefault);
      cap.MoreFrames(240);
      const bool recovered = WaitUntil([&a, &b] {
        a.Pump(60);
        b.Pump(60);
        return a.keys_ >= 2 && b.keys_ >= 1;
      }, 6000);
      // 分辨率变化 → 0x010A
      cap.SetResolution(96, 64);
      const bool display_seen = WaitUntil([&a, &b] {
        a.Pump(60);
        b.Pump(60);
        return !a.displays_.empty() && !b.displays_.empty();
      }, 6000);
      pipe_th.join();
      a.Pump(400);
      b.Pump(400);
      rt.Shutdown();
      CHECK("rt6-recovered", recovered);
      CHECK("rt6-recovering-state", a.SawState("recovering"));
      CHECK("rt6-join-during-recovery-gets-idr", b.keys_ >= 1 && b.frames_ >= 1);
      CHECK("rt6-display-seen", display_seen);
      CHECK("rt6-display-payload",
            !a.displays_.empty() && a.displays_[0].gen == 3 && a.displays_[0].w == 96 &&
                a.displays_[0].h == 64 &&
                std::strcmp(a.displays_[0].reason, "resolution") == 0);
      CHECK("rt6-display-both-subs",
            !b.displays_.empty() && b.displays_[0].gen == 3 && b.displays_[0].w == 96);
      CHECK("rt6-pipeline-ok", res.ok);
      CHECK("rt6-two-resets", res.resets == 2);
      const xnc::RtServer::Stats st = rt.stats();
      CHECK("rt6-two-attaches", st.attaches == 2);
      std::printf("SELFTEST NOTE: rt6 a_keys=%llu b_keys=%llu resets=%u gen=%u\n",
                  (unsigned long long)a.keys_, (unsigned long long)b.keys_, res.resets,
                  a.displays_.empty() ? 0 : a.displays_[0].gen);
    }
  }
  // ---- M2-Slice1 Task 3:GDI 采样 CRC 变化检测(纯函数,合成行) ----
  {
    // 64x16,rows=8 → 采样行中点 = 1,3,5,...,15(偶数行 0,2,4.. 不采样)
    constexpr uint32_t kW = 64, kH = 16;
    std::vector<uint8_t> a((size_t)kW * kH * 4, 0);
    for (size_t i = 0; i + 3 < a.size(); i += 4) a[i + 3] = 0xFF;
    // 行中点表:h=16 rows=8 → 1..15 奇数;h=1 → 0;单调不减
    bool rows_ok = true;
    for (uint32_t i = 0; i < 8; ++i)
      rows_ok = rows_ok && xnc::GdiSampledRowY(kH, i, 8) == 1 + 2 * i;
    CHECK("gdi-crc-row-y-midpoints", rows_ok);
    CHECK("gdi-crc-row-y-clamp", xnc::GdiSampledRowY(1, 0, 8) == 0 &&
                                    xnc::GdiSampledRowY(0, 0, 8) == 0 &&
                                    xnc::GdiSampledRowY(5, 0, 0) == 0);
    // 同图同 CRC;采样行上 1 字节变化 → 检出
    const uint64_t crc0 = xnc::GdiSampledCrc(a.data(), kW, kH);
    CHECK("gdi-crc-identical",
          xnc::GdiSampledCrc(a.data(), kW, kH) == crc0 && crc0 != 0);
    std::vector<uint8_t> b = a;
    b[(size_t)3 * kW * 4 + 8] = 0xAB;  // row 3 = sampled
    CHECK("gdi-crc-sampled-row-change", xnc::GdiSampledCrc(b.data(), kW, kH) != crc0);
    // 未采样行(row 2)的变化被漏掉 —— 记录在案的近似(文档化限制)
    std::vector<uint8_t> c = a;
    c[(size_t)2 * kW * 4 + 8] = 0xCD;  // row 2 = between sampled rows 1/3
    CHECK("gdi-crc-between-rows-miss-documented",
          xnc::GdiSampledCrc(c.data(), kW, kH) == crc0);
    // 尺寸混入:同像素流不同 w → 不同 CRC(模式变化不可能与同像素碰撞)
    CHECK("gdi-crc-dims-mixed-in",
          xnc::GdiSampledCrc(a.data(), 32, 32) != xnc::GdiSampledCrc(a.data(), 64, 16));
    // 退化输入:0 值安全
    CHECK("gdi-crc-degenerate-safe",
          xnc::GdiSampledCrc(nullptr, 64, 16) == 0 &&
              xnc::GdiSampledCrc(a.data(), 0, 16) == 0 &&
              xnc::GdiSampledCrc(a.data(), 64, 0) == 0 &&
              xnc::GdiSampledCrc(a.data(), 64, 16, 0) == 0);
    // FNV 链式:update(d1)+update(d2) == 一次哈希 d1||d2
    const uint8_t d1[4] = {1, 2, 3, 4}, d2[3] = {9, 8, 7};
    uint8_t both[7];
    for (int i = 0; i < 4; ++i) both[i] = d1[i];
    for (int i = 0; i < 3; ++i) both[4 + i] = d2[i];
    CHECK("gdi-crc-fnv-chaining",
          xnc::Fnv1a64Update(xnc::Fnv1a64(d1, 4), d2, 3) == xnc::Fnv1a64(both, 7));
    // 采样密度提高 → 之前漏掉的 row 2 变化被检出(rows=16 → 每行都采样)
    CHECK("gdi-crc-more-rows-catches",
          xnc::GdiSampledCrc(c.data(), kW, kH, 16) != crc0);
  }
  // ---- M2-Slice1 Task 3:DXGI 健康分决策表(spec §7.6 映射;纯表驱动) ----
  {
    CHECK("hs-delta-frame", xnc::DxgiHealthDelta(xnc::DxgiHealthEvent::kFrame) == 0);
    CHECK("hs-delta-timeout", xnc::DxgiHealthDelta(xnc::DxgiHealthEvent::kTimeout) == 0);
    CHECK("hs-delta-access-lost",
          xnc::DxgiHealthDelta(xnc::DxgiHealthEvent::kAccessLost) == 0);
    CHECK("hs-delta-create-fail",
          xnc::DxgiHealthDelta(xnc::DxgiHealthEvent::kCreateFail) == -40);
    CHECK("hs-delta-gpu-removed",
          xnc::DxgiHealthDelta(xnc::DxgiHealthEvent::kGpuRemoved) == -50);
    CHECK("hs-delta-no-useful",
          xnc::DxgiHealthDelta(xnc::DxgiHealthEvent::kNoUsefulFrame) == -10);
    CHECK("hs-apply-clamp-zero",
          xnc::ApplyDxgiHealthEvent(20, xnc::DxgiHealthEvent::kGpuRemoved) == 0);
    CHECK("hs-apply-exact",
          xnc::ApplyDxgiHealthEvent(100, xnc::DxgiHealthEvent::kCreateFail) == 60 &&
              xnc::ApplyDxgiHealthEvent(60, xnc::DxgiHealthEvent::kNoUsefulFrame) == 50);
    CHECK("hs-apply-no-recovery",
          xnc::ApplyDxgiHealthEvent(50, xnc::DxgiHealthEvent::kFrame) == 50 &&
              xnc::ApplyDxgiHealthEvent(50, xnc::DxgiHealthEvent::kTimeout) == 50 &&
              xnc::ApplyDxgiHealthEvent(50, xnc::DxgiHealthEvent::kAccessLost) == 50);
    // acquire 结果分类表
    CHECK("hs-classify-ok",
          xnc::ClassifyAcquireOutcome(true, nullptr) == xnc::DxgiHealthEvent::kFrame);
    CHECK("hs-classify-timeout",
          xnc::ClassifyAcquireOutcome(false, "err_timeout") ==
              xnc::DxgiHealthEvent::kTimeout);
    CHECK("hs-classify-rebuilt-is-frame",
          xnc::ClassifyAcquireOutcome(false, "err_rebuilt") ==
              xnc::DxgiHealthEvent::kFrame);
    CHECK("hs-classify-access-lost",
          xnc::ClassifyAcquireOutcome(false, "err_access_lost") ==
              xnc::DxgiHealthEvent::kAccessLost);
    CHECK("hs-classify-fatal-family",
          xnc::ClassifyAcquireOutcome(false, "AcquireNextFrame: hr=0x80004005") ==
              xnc::DxgiHealthEvent::kNoUsefulFrame);
    CHECK("hs-classify-null-empty",
          xnc::ClassifyAcquireOutcome(false, nullptr) ==
                  xnc::DxgiHealthEvent::kNoUsefulFrame &&
              xnc::ClassifyAcquireOutcome(false, "") ==
                  xnc::DxgiHealthEvent::kNoUsefulFrame);
    // 梯子决策表:<60 降级(59 降,60 留);GDI 仅 probe 成功才回升
    CHECK("hs-decision-stay-healthy",
          xnc::LadderDecision(xnc::BackendKind::kDxgi, 100, 60, false) ==
              xnc::LadderAction::kStay);
    CHECK("hs-decision-downgrade-59",
          xnc::LadderDecision(xnc::BackendKind::kDxgi, 59, 60, false) ==
              xnc::LadderAction::kDowngrade);
    CHECK("hs-decision-stay-60-boundary",
          xnc::LadderDecision(xnc::BackendKind::kDxgi, 60, 60, false) ==
              xnc::LadderAction::kStay);
    CHECK("hs-decision-threshold-configurable",
          xnc::LadderDecision(xnc::BackendKind::kDxgi, 49, 50, false) ==
              xnc::LadderAction::kDowngrade);
    CHECK("hs-decision-gdi-no-probe-stays",
          xnc::LadderDecision(xnc::BackendKind::kGdi, 0, 60, false) ==
              xnc::LadderAction::kStay);
    CHECK("hs-decision-gdi-probe-upgrades",
          xnc::LadderDecision(xnc::BackendKind::kGdi, 0, 60, true) ==
              xnc::LadderAction::kUpgrade);
    CHECK("hs-backend-names",
          xnc::BackendKindName(xnc::BackendKind::kDxgi) != nullptr &&
              std::strcmp(xnc::BackendKindName(xnc::BackendKind::kDxgi), "dxgi") == 0 &&
              std::strcmp(xnc::BackendKindName(xnc::BackendKind::kGdi), "gdi") == 0);
    // env 诊断钩子解析
    int32_t fh = -1;
    CHECK("hs-env-force-health-valid",
          xnc::ParseForceHealthEnv("50", &fh) && fh == 50 &&
              xnc::ParseForceHealthEnv("0", &fh) && fh == 0 &&
              xnc::ParseForceHealthEnv("100", &fh) && fh == 100 &&
              xnc::ParseForceHealthEnv("007", &fh) && fh == 7);
    CHECK("hs-env-force-health-invalid",
          !xnc::ParseForceHealthEnv("101", &fh) && !xnc::ParseForceHealthEnv("-1", &fh) &&
              !xnc::ParseForceHealthEnv("abc", &fh) && !xnc::ParseForceHealthEnv("", &fh) &&
              !xnc::ParseForceHealthEnv("5x", &fh) && !xnc::ParseForceHealthEnv(nullptr, &fh));
    CHECK("hs-env-force-backend",
          xnc::ParseForceBackendEnv("gdi") && xnc::ParseForceBackendEnv("GDI") &&
              !xnc::ParseForceBackendEnv("dxgi") &&
              !xnc::ParseForceBackendEnv("bogus") && !xnc::ParseForceBackendEnv("") &&
              !xnc::ParseForceBackendEnv(nullptr));
  }
  // ---- M2-Slice1 Task 3:CLI --backend(参数矩阵) ----
  {
    CHECK("args-backend-default-dxgi",
          Parse({L"--console-diag", L"--out", L"t.h264"}).opt.backend ==
              xnc::DiagBackend::kDxgi);
    CHECK("args-backend-gdi",
          Parse({L"--console-diag", L"--out", L"t.h264", L"--backend", L"gdi"}).ok &&
              Parse({L"--console-diag", L"--out", L"t.h264", L"--backend", L"gdi"})
                      .opt.backend == xnc::DiagBackend::kGdi);
    CHECK("args-backend-dxgi-explicit",
          Parse({L"--console-diag", L"--out", L"t.h264", L"--backend", L"dxgi"}).ok);
    CHECK("args-backend-invalid", !Parse({L"--console-diag", L"--out", L"t",
                                          L"--backend", L"wgc"})
                                         .ok);
    CHECK("args-backend-missing-value",
          !Parse({L"--console-diag", L"--out", L"t", L"--backend"}).ok);
  }
  // ---- M2-Slice1 Task 3:梯子运行时(fake 后端;无管线直驱) ----
  {
    // 初始:DXGI,健康 100
    {
      FakeDxgiFactory dxgi;
      FakeGdiFactory gdi;
      g_ld_dxgi = &dxgi;
      g_ld_gdi = &gdi;
      xnc::LadderOpts lo;
      lo.make_dxgi = &LdMakeDxgi;
      lo.make_gdi = &LdMakeGdi;
      lo.probe_interval_ms = 0;  // no probe in this unit block
      xnc::LadderCapture ladder(lo);
      std::string err;
      CHECK("ld-init-dxgi-default", ladder.Init(&err) &&
                                        ladder.active() == xnc::BackendKind::kDxgi &&
                                        ladder.dxgi_health() == 100);
      xnc::FrameBlob blob;
      CHECK("ld-init-dxgi-yields-frames",
            ladder.Acquire(blob, &err) && blob.w == dxgi.w && ladder.Width() == dxgi.w);
      CHECK("ld-init-gdi-not-created", gdi.created.load() == 0);
    }
    // force_gdi:直接从 GDI 起,DXGI 工厂不被调用
    {
      FakeDxgiFactory dxgi;
      FakeGdiFactory gdi;
      g_ld_dxgi = &dxgi;
      g_ld_gdi = &gdi;
      xnc::LadderOpts lo;
      lo.make_dxgi = &LdMakeDxgi;
      lo.make_gdi = &LdMakeGdi;
      lo.force_gdi = true;
      lo.probe_interval_ms = 0;
      xnc::LadderCapture ladder(lo);
      std::string err;
      CHECK("ld-init-force-gdi", ladder.Init(&err) &&
                                     ladder.active() == xnc::BackendKind::kGdi);
      xnc::FrameBlob blob;
      CHECK("ld-init-force-gdi-frames", ladder.Acquire(blob, &err) && blob.w == gdi.w);
      CHECK("ld-init-force-gdi-no-dxgi", dxgi.created.load() == 0);
    }
    // session-0 类拒绝(E_ACCESSDENIED 语义)不得静默落到 GDI(会采到错误桌面)
    {
      FakeDxgiFactory dxgi;
      dxgi.fail_after = 0;  // first creation fails, access-denied-shaped err
      dxgi.fail_err = "EnumOutputs: hr=0x80070005";
      FakeGdiFactory gdi;
      g_ld_dxgi = &dxgi;
      g_ld_gdi = &gdi;
      xnc::LadderOpts lo;
      lo.make_dxgi = &LdMakeDxgi;
      lo.make_gdi = &LdMakeGdi;
      lo.probe_interval_ms = 0;
      xnc::LadderCapture ladder(lo);
      std::string err;
      CHECK("ld-init-session0-denied-fails", !ladder.Init(&err));
      CHECK("ld-init-session0-err-preserved", err.find("0x80070005") != std::string::npos);
      CHECK("ld-init-session0-no-gdi-fallback", gdi.created.load() == 0);
    }
    // 真 DXGI 创建失败(非拒绝)→ GDI 兜底
    {
      FakeDxgiFactory dxgi;
      dxgi.fail_after = 0;
      dxgi.fail_err = "D3D11CreateDevice: hr=0x80004005";
      FakeGdiFactory gdi;
      LadderStateLog log;
      g_ld_dxgi = &dxgi;
      g_ld_gdi = &gdi;
      xnc::LadderOpts lo;
      lo.make_dxgi = &LdMakeDxgi;
      lo.make_gdi = &LdMakeGdi;
      lo.on_switch = &LadderLogThunk;
      lo.on_switch_ctx = &log;
      lo.probe_interval_ms = 0;
      xnc::LadderCapture ladder(lo);
      std::string err;
      CHECK("ld-init-dxgi-fail-gdi-fallback",
            ladder.Init(&err) && ladder.active() == xnc::BackendKind::kGdi);
      CHECK("ld-init-gdi-fallback-state", log.Has("gdi", "dxgi_init_failed"));
      xnc::FrameBlob blob;
      CHECK("ld-init-gdi-fallback-frames", ladder.Acquire(blob, &err));
    }
    // 降级(无协调器):硬错 → 健康分跌穿 → 原地换 GDI(err_rebuilt),帧恢复
    {
      FakeDxgiFactory dxgi;
      dxgi.frames = 1;
      dxgi.hard_errors = 100;
      FakeGdiFactory gdi;
      LadderStateLog log;
      g_ld_dxgi = &dxgi;
      g_ld_gdi = &gdi;
      xnc::LadderOpts lo;
      lo.make_dxgi = &LdMakeDxgi;
      lo.make_gdi = &LdMakeGdi;
      lo.on_switch = &LadderLogThunk;
      lo.on_switch_ctx = &log;
      lo.probe_interval_ms = 0;  // coordinator absent: inline swap path
      xnc::LadderCapture ladder(lo);
      std::string err;
      CHECK("ld-down-nocoord-init", ladder.Init(&err));
      xnc::FrameBlob blob;
      CHECK("ld-down-nocoord-first-frame", ladder.Acquire(blob, &err));
      // 之后每个硬错 −10:90,80,70,60,50 → 50 <60 → 原地切换
      bool saw_rebuilt = false;
      int gdi_frames = 0;
      for (int i = 0; i < 20; ++i) {
        std::string e;
        if (ladder.Acquire(blob, &e)) {
          if (ladder.active() == xnc::BackendKind::kGdi) ++gdi_frames;
        } else if (e == "err_rebuilt") {
          saw_rebuilt = true;
        }
      }
      CHECK("ld-down-nocoord-switched",
            ladder.active() == xnc::BackendKind::kGdi && ladder.switches() == 1);
      CHECK("ld-down-nocoord-err-rebuilt", saw_rebuilt);
      CHECK("ld-down-nocoord-gdi-frames", gdi_frames >= 5);
      CHECK("ld-down-nocoord-state", log.Has("gdi", "health"));
      CHECK("ld-down-nocoord-health-recorded", ladder.dxgi_health() == 50);
    }
    // 降级(有协调器):健康分跌穿 → RequestReset(change_backend);Rebuild 换 GDI
    {
      FakeDxgiFactory dxgi;
      dxgi.frames = 0;  // every acquire a hard error from the start
      dxgi.hard_errors = 100;
      FakeGdiFactory gdi;
      LadderStateLog log;
      g_ld_dxgi = &dxgi;
      g_ld_gdi = &gdi;
      xnc::CaptureReset reset;  // no clock: immediate take
      xnc::LadderOpts lo;
      lo.make_dxgi = &LdMakeDxgi;
      lo.make_gdi = &LdMakeGdi;
      lo.on_switch = &LadderLogThunk;
      lo.on_switch_ctx = &log;
      lo.reset = &reset;
      lo.probe_interval_ms = 0;
      xnc::LadderCapture ladder(lo);
      std::string err;
      CHECK("ld-down-coord-init", ladder.Init(&err));
      xnc::FrameBlob blob;
      std::string e;
      // 首个硬错即被换算为 err_access_lost 交给 T2(fatal 家族被梯子收容)
      CHECK("ld-down-coord-first-error-contained",
            !ladder.Acquire(blob, &e) && e == "err_access_lost");
      // 健康分随硬错继续跌:100-10×k;跌穿后梯子请求 change_backend reset
      for (int i = 0; i < 8 && ladder.dxgi_health() >= 60; ++i) ladder.Acquire(blob, &e);
      CHECK("ld-down-coord-health-below", ladder.dxgi_health() < 60);
      CHECK("ld-down-coord-reset-requested",
            reset.requests() >= 1 && reset.executed() == 0);
      char reason[xnc::kResetReasonMax] = {0};
      CHECK("ld-down-coord-reset-reason",
            reset.TakeReset(reason, sizeof(reason)) &&
                std::strcmp(reason, "change_backend") == 0);
      std::string rerr;
      CHECK("ld-down-coord-rebuild-swaps",
            ladder.Rebuild(&rerr) && ladder.active() == xnc::BackendKind::kGdi);
      CHECK("ld-down-coord-state", log.Has("gdi", "health"));
      CHECK("ld-down-coord-gdi-frames", ladder.Acquire(blob, &e) && e.empty());
    }
    // Rebuild 委托失败(桌面 DEFAULT)→ kCreateFail −40;门非默认(安全桌面)不扣
    {
      FakeDxgiFactory dxgi;
      dxgi.rebuild_fails = 3;  // 3 次 Rebuild 失败后成功
      FakeGdiFactory gdi;
      g_ld_dxgi = &dxgi;
      g_ld_gdi = &gdi;
      g_cr_gate.store(xnc::ResetDesktop::kDefault);
      xnc::CaptureReset reset;
      xnc::LadderOpts lo;
      lo.make_dxgi = &LdMakeDxgi;
      lo.make_gdi = &LdMakeGdi;
      lo.reset = &reset;
      lo.force_health = 100;  // 屏蔽降级分支,专测计分
      lo.probe_interval_ms = 0;
      lo.clock_ms = &CrUnitClock;  // M2-S2 T1:计分需 gate 稳定 ≥2s,表驱动
      g_cr_unit_now = 30000;
      xnc::LadderCapture ladder(lo);
      std::string err;
      CHECK("ld-score-init", ladder.Init(&err) && ladder.dxgi_health() == 100);
      xnc::FrameBlob blob;
      ladder.Acquire(blob, &err);  // frame: stays 100;gate 稳定锚点 t=30000
      std::string rerr;
      g_cr_unit_now = 32001;  // 稳定 ≥2s:计分
      CHECK("ld-score-rebuild-fail-1", !ladder.Rebuild(&rerr) && ladder.dxgi_health() == 60);
      g_cr_unit_now = 34100;
      CHECK("ld-score-rebuild-fail-2", !ladder.Rebuild(&rerr) && ladder.dxgi_health() == 20);
      // 20 < 60 → 第 3 次 Rebuild 变成换 GDI(不再是委托)
      CHECK("ld-score-below-threshold-swaps",
            ladder.Rebuild(&rerr) && ladder.active() == xnc::BackendKind::kGdi);
      // 门非默认时 Rebuild 失败不扣分:重建一个梯子验证
      FakeDxgiFactory dxgi2;
      dxgi2.rebuild_fails = 1;
      g_ld_dxgi = &dxgi2;
      xnc::CaptureReset::Opts co2;
      co2.desktop_fn = &CrUnitGate;  // gate-observable coordinator
      xnc::CaptureReset reset2(co2);
      xnc::LadderOpts lo2;
      lo2.make_dxgi = &LdMakeDxgi;
      lo2.make_gdi = &LdMakeGdi;
      lo2.reset = &reset2;
      lo2.force_health = 100;
      lo2.probe_interval_ms = 0;
      xnc::LadderCapture ladder2(lo2);
      CHECK("ld-score-init-2", ladder2.Init(&err) && ladder2.dxgi_health() == 100);
      g_cr_gate.store(xnc::ResetDesktop::kNonDefault);  // secure desktop up
      CHECK("ld-score-gate-blocked-no-penalty",
            !ladder2.Rebuild(&rerr) && ladder2.dxgi_health() == 100);
      g_cr_gate.store(xnc::ResetDesktop::kDefault);
    }
    // XNC_FORCE_DXGI_HEALTH=50:首个 acquire 后即降级请求
    {
      FakeDxgiFactory dxgi;
      FakeGdiFactory gdi;
      g_ld_dxgi = &dxgi;
      g_ld_gdi = &gdi;
      xnc::CaptureReset reset;
      xnc::LadderOpts lo;
      lo.make_dxgi = &LdMakeDxgi;
      lo.make_gdi = &LdMakeGdi;
      lo.reset = &reset;
      lo.force_health = 50;
      lo.probe_interval_ms = 0;
      xnc::LadderCapture ladder(lo);
      std::string err;
      CHECK("ld-force-health-init", ladder.Init(&err) && ladder.dxgi_health() == 50);
      xnc::FrameBlob blob;
      std::string e;
      CHECK("ld-force-health-frame-keeps-50", ladder.Acquire(blob, &e) &&
                                                   ladder.dxgi_health() == 50);
      CHECK("ld-force-health-reset-requested", reset.requests() == 1);
      char reason[xnc::kResetReasonMax] = {0};
      reset.TakeReset(reason, sizeof(reason));
      CHECK("ld-force-health-reason", std::strcmp(reason, "change_backend") == 0);
    }
    // probe 回升:force_gdi + 协调器 + 可用 DXGI → probe 成功 → reset(change_backend)
    // → Rebuild 换回 DXGI
    {
      FakeDxgiFactory dxgi;
      FakeGdiFactory gdi;
      LadderStateLog log;
      g_ld_dxgi = &dxgi;
      g_ld_gdi = &gdi;
      xnc::CaptureReset reset;
      xnc::LadderOpts lo;
      lo.make_dxgi = &LdMakeDxgi;
      lo.make_gdi = &LdMakeGdi;
      lo.on_switch = &LadderLogThunk;
      lo.on_switch_ctx = &log;
      lo.reset = &reset;
      lo.force_gdi = true;
      lo.probe_interval_ms = 80;  // fast probe for the test
      xnc::LadderCapture ladder(lo);
      std::string err;
      CHECK("ld-up-init-gdi", ladder.Init(&err) && ladder.active() == xnc::BackendKind::kGdi);
      xnc::FrameBlob blob;
      std::string e;
      CHECK("ld-up-gdi-frames-first", ladder.Acquire(blob, &e));
      bool probed = false;
      for (int i = 0; i < 100 && !probed; ++i) {  // ≤ ~2s wall
        Sleep(20);
        probed = ladder.probe_ok() && reset.requests() >= 1;
      }
      CHECK("ld-up-probe-ok-reset-requested", probed);
      char reason[xnc::kResetReasonMax] = {0};
      CHECK("ld-up-reason", reset.TakeReset(reason, sizeof(reason)) &&
                                std::strcmp(reason, "change_backend") == 0);
      std::string rerr;
      CHECK("ld-up-rebuild-swaps-dxgi",
            ladder.Rebuild(&rerr) && ladder.active() == xnc::BackendKind::kDxgi &&
                ladder.dxgi_health() == 100 && !ladder.probe_ok());
      CHECK("ld-up-state", log.Has("dxgi", "probe"));
      CHECK("ld-up-dxgi-frames", ladder.Acquire(blob, &e) && e.empty());
    }
    // probe 成功后创建再失败(两次创建之间 DXGI 又坏)→ 不搁浅 reset:留在 GDI,
    // Rebuild 仍成功(GDI 重建),probe_ok 清零待下次 probe
    {
      FakeDxgiFactory dxgi;
      dxgi.fail_after = 1;  // creation #1 (probe) ok, #2 (swap) fails
      FakeGdiFactory gdi;
      g_ld_dxgi = &dxgi;
      g_ld_gdi = &gdi;
      xnc::CaptureReset reset;
      xnc::LadderOpts lo;
      lo.make_dxgi = &LdMakeDxgi;
      lo.make_gdi = &LdMakeGdi;
      lo.reset = &reset;
      lo.force_gdi = true;
      lo.probe_interval_ms = 80;
      xnc::LadderCapture ladder(lo);
      std::string err;
      CHECK("ld-up2-init-gdi", ladder.Init(&err) && ladder.active() == xnc::BackendKind::kGdi);
      bool probed = false;
      for (int i = 0; i < 100 && !probed; ++i) {
        Sleep(20);
        probed = ladder.probe_ok();
      }
      CHECK("ld-up2-probe-ok", probed);
      char reason[xnc::kResetReasonMax] = {0};
      reset.TakeReset(reason, sizeof(reason));
      std::string rerr;
      CHECK("ld-up2-swap-fail-stays-gdi",
            ladder.Rebuild(&rerr) && ladder.active() == xnc::BackendKind::kGdi &&
                !ladder.probe_ok());
      xnc::FrameBlob blob;
      std::string e;
      CHECK("ld-up2-gdi-frames-still", ladder.Acquire(blob, &e) && e.empty());
    }
    g_ld_dxgi = nullptr;
    g_ld_gdi = nullptr;
  }
  // ---- M2-Slice1 Task 3:梯子 × 管线(真 MF 编码器;rsE 降级 / rsF 回升) ----
  const uint32_t kLdW = 64, kLdH = 48, kLdFps = 15, kLdBitrate = 500000;
  { // rsE:DXGI 帧后硬错 → 健康分跌穿 → GDI,流不断,STATE 序列完整
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(kLdW, kLdH, kLdFps, kLdBitrate, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: rsE-init err=%s\n", err.c_str());
    CHECK("rsE-init", init_ok);
    if (init_ok) {
      FakeDxgiFactory dxgi;  // 12 帧后永久硬错(分辨率同 64x48 → 无 encoder re-init)
      dxgi.w = kLdW;
      dxgi.h = kLdH;
      dxgi.frames = 12;
      dxgi.hard_errors = 1000000;
      FakeGdiFactory gdi;
      gdi.w = kLdW;
      gdi.h = kLdH;
      LadderStateLog log;
      g_ld_dxgi = &dxgi;
      g_ld_gdi = &gdi;
      g_cr_gate.store(xnc::ResetDesktop::kDefault);
      xnc::CaptureReset::Opts co;
      co.clock_ms = &CrRealClock;
      co.desktop_fn = &CrUnitGate;
      xnc::CaptureReset reset(co);
      xnc::LadderOpts lo;
      lo.make_dxgi = &LdMakeDxgi;
      lo.make_gdi = &LdMakeGdi;
      lo.on_switch = &LadderLogThunk;
      lo.on_switch_ctx = &log;
      lo.reset = &reset;
      lo.probe_interval_ms = 0;  // rsF covers the probe; here: stay degraded
      xnc::LadderCapture ladder(lo);
      CHECK("rsE-ladder-init", ladder.Init(&err));
      ladder.SetStateSink(nullptr);  // diag mode family: log-only
      RecordingSink sink;
      xnc::PipelineOpts po;
      po.duration_s = 8;
      po.fps = kLdFps;
      po.target_bitrate_bps = kLdBitrate;
      po.reset = &reset;
      xnc::PipelineResult res;
      std::thread pipe_th([&] { res = xnc::Pipeline::Run(ladder, enc, sink, po); });
      const bool switched = WaitUntil([&ladder] {
        return ladder.active() == xnc::BackendKind::kGdi;
      }, 6000);
      pipe_th.join();
      CHECK("rsE-switched-to-gdi", switched && ladder.switches() == 1);
      CHECK("rsE-pipeline-ok", res.ok);
      CHECK("rsE-frames-continue", sink.aus >= 20 && res.counters.captured > 12);
      CHECK("rsE-state-sequence", sink.Saw("recovering") && sink.Saw("capture_rebuilt") &&
                                     !sink.Saw("capture_fatal"));
      CHECK("rsE-backend-state", log.Has("gdi", "health"));
      CHECK("rsE-resets", res.resets >= 1);
      std::printf("SELFTEST NOTE: rsE switches=%u resets=%u aus=%llu captured=%llu\n",
                  ladder.switches(), res.resets,
                  (unsigned long long)sink.aus,
                  (unsigned long long)res.counters.captured);
    }
  }
  { // rsF:GDI 起(force_gdi)→ probe(80ms)成功 → 回升 DXGI,STATE backend_changed
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(kLdW, kLdH, kLdFps, kLdBitrate, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: rsF-init err=%s\n", err.c_str());
    CHECK("rsF-init", init_ok);
    if (init_ok) {
      FakeDxgiFactory dxgi;
      dxgi.w = kLdW;
      dxgi.h = kLdH;
      FakeGdiFactory gdi;
      gdi.w = kLdW;
      gdi.h = kLdH;
      LadderStateLog log;
      g_ld_dxgi = &dxgi;
      g_ld_gdi = &gdi;
      g_cr_gate.store(xnc::ResetDesktop::kDefault);
      xnc::CaptureReset::Opts co;
      co.clock_ms = &CrRealClock;
      co.desktop_fn = &CrUnitGate;
      xnc::CaptureReset reset(co);
      xnc::LadderOpts lo;
      lo.make_dxgi = &LdMakeDxgi;
      lo.make_gdi = &LdMakeGdi;
      lo.on_switch = &LadderLogThunk;
      lo.on_switch_ctx = &log;
      lo.reset = &reset;
      lo.force_gdi = true;
      lo.probe_interval_ms = 80;
      xnc::LadderCapture ladder(lo);
      CHECK("rsF-ladder-init-gdi", ladder.Init(&err) &&
                                       ladder.active() == xnc::BackendKind::kGdi);
      RecordingSink sink;
      ladder.SetStateSink(&sink);  // rt mode family: backend_changed STATE
      xnc::PipelineOpts po;
      po.duration_s = 8;
      po.fps = kLdFps;
      po.target_bitrate_bps = kLdBitrate;
      po.reset = &reset;
      xnc::PipelineResult res;
      std::thread pipe_th([&] { res = xnc::Pipeline::Run(ladder, enc, sink, po); });
      const bool upgraded = WaitUntil([&ladder, &sink] {
        return ladder.active() == xnc::BackendKind::kDxgi && sink.Saw("backend_changed");
      }, 6000);
      pipe_th.join();
      CHECK("rsF-upgraded-to-dxgi", upgraded && ladder.switches() == 1);
      CHECK("rsF-pipeline-ok", res.ok);
      CHECK("rsF-frames-continue", sink.aus >= 20);
      CHECK("rsF-backend-changed-state", sink.Saw("backend_changed") &&
                                             sink.SawRecoverable("backend_changed"));
      CHECK("rsF-ladder-state", log.Has("dxgi", "probe"));
      CHECK("rsF-two-idrs", sink.keys >= 2);  // 初始 + 回升 rebuild IDR
      CHECK("rsF-health-restored", ladder.dxgi_health() == 100);
      std::printf("SELFTEST NOTE: rsF switches=%u resets=%u aus=%llu keys=%llu\n",
                  ladder.switches(), res.resets,
                  (unsigned long long)sink.aus, (unsigned long long)sink.keys);
    }
    g_ld_dxgi = nullptr;
    g_ld_gdi = nullptr;
  }
  { // M2-Slice3 Task 3: --jpeg-single arg parsing + box-filter downscale
    // + WIC encode round trip on a synthetic frame.
    auto js = Parse({L"--jpeg-single", L"snap.jpg"});
    CHECK("js-mode-ok", js.ok && js.opt.jpeg_single && js.opt.jpeg_path == L"snap.jpg");
    CHECK("js-default-no-clamp", js.ok && js.opt.max_width == 0);
    auto jsm = Parse({L"--jpeg-single", L"s.jpg", L"--max-w", L"1280"});
    CHECK("js-maxw-ok", jsm.ok && jsm.opt.max_width == 1280);
    CHECK("js-maxw-bad", !Parse({L"--jpeg-single", L"s.jpg", L"--max-w", L"x"}).ok);
    CHECK("js-maxw-zero-ok", Parse({L"--jpeg-single", L"s.jpg", L"--max-w", L"0"}).ok);
    CHECK("js-no-path", !Parse({L"--jpeg-single"}).ok);
    CHECK("js-exclusive-with-diag", !Parse({L"--jpeg-single", L"s.jpg", L"--console-diag", L"--out", L"t"}).ok);
    // feat/rt-scale (hw-encode task 2 Part B): --max-w also applies to the
    // rt and diag modes (downscale before encode); rejected elsewhere.
    CHECK("maxw-diag-ok",
          Parse({L"--console-diag", L"--out", L"t", L"--max-w", L"10"}).ok &&
              Parse({L"--console-diag", L"--out", L"t", L"--max-w", L"10"}).opt.max_width == 10);
    CHECK("maxw-rt-ok",
          Parse({L"--console-rt", L"--secret-stdin", L"--max-w", L"1920"}).ok &&
              Parse({L"--console-rt", L"--secret-stdin", L"--max-w", L"1920"}).opt.max_width == 1920);
    CHECK("maxw-rejected-elsewhere", !Parse({L"--selftest", L"--max-w", L"5"}).ok);

    // feat/arch-clean: encode bitrate follows the (scaled) encode width.
    // 表驱动:≤1280→1.5M,≤1920→2.3M,≤2560→3.2M,更大→4.2M;h 目前不影响。
    {
      struct BitrateCase { uint32_t w, h, want; };
      const BitrateCase bitrate_cases[] = {
          {1, 1, 1500000}, {1280, 720, 1500000},        // ≤1280 → 1.5M
          {1281, 720, 2300000}, {1920, 1080, 2300000},  // ≤1920 → 2.3M
          {1921, 1080, 3200000}, {2560, 1440, 3200000}, // ≤2560 → 3.2M
          {2561, 1440, 4200000}, {3840, 2160, 4200000}, // 更大 → 4.2M
          {0, 0, 1500000},                              // 退化输入落入最低档
      };
      for (const BitrateCase& c : bitrate_cases)
        CHECK("bitrate-for-dims", xnc::BitrateForDims(c.w, c.h) == c.want);
    }

    // Box filter: 4x2 gradient -> 2x1 (each dst cell covers a 2x2 block).
    const uint32_t sw = 4, sh = 2;
    std::vector<uint8_t> src((size_t)sw * sh * 4);
    for (uint32_t i = 0; i < sw * sh; i++) {
      src[i * 4 + 0] = 0x10;  // B
      src[i * 4 + 1] = 0x20;  // G
      src[i * 4 + 2] = 0x30;  // R
      src[i * 4 + 3] = 0xFF;  // A
    }
    src[0 * 4 + 0] = 0x00; src[(sw + 1) * 4 + 0] = 0x40;  // mixed B corners
    std::vector<uint8_t> dst;
    uint32_t ow = 0, oh = 0;
    CHECK("ds-call", xnc::DownscaleBgra(src.data(), sw, sh, 2, &dst, &ow, &oh));
    CHECK("ds-dims", ow == 2 && oh == 1);
    CHECK("ds-size", dst.size() == (size_t)2 * 1 * 4);
    // nh = ceil(2*2/4) = 1: one dst row covers both src rows; the first dst
    // pixel covers the 2x2 block x=0..2,y=0..2: B = (0+0x10+0x10+0x40)/4 = 0x18.
    CHECK("ds-box-avg", dst[0] == 0x18 && dst[1] == 0x20 && dst[2] == 0x30 && dst[3] == 0xFF);
    // pipeline-decouple exactness regression: the optimized box filter must
    // be byte-identical to the original naive per-pixel-division algorithm
    // (the multiply-high + correction trick must not drift by even 1).
    {
      auto naive = [](const std::vector<uint8_t>& src, uint32_t w, uint32_t h,
                      uint32_t max_w, std::vector<uint8_t>* out, uint32_t* ow,
                      uint32_t* oh) {
        const uint32_t nw = max_w;
        const uint32_t nh = (uint32_t)(((uint64_t)h * nw + w - 1) / w);
        out->assign((size_t)nw * nh * 4, 0);
        for (uint32_t dy = 0; dy < nh; dy++) {
          const uint32_t y0 = (uint32_t)(((uint64_t)h * dy) / nh);
          const uint32_t y1 = (uint32_t)(((uint64_t)h * (dy + 1)) / nh);
          for (uint32_t dx = 0; dx < nw; dx++) {
            const uint32_t x0 = (uint32_t)(((uint64_t)w * dx) / nw);
            const uint32_t x1 = (uint32_t)(((uint64_t)w * (dx + 1)) / nw);
            uint64_t b = 0, g = 0, r = 0, a = 0;
            uint32_t n = 0;
            for (uint32_t y = y0; y < y1 && y < h; y++) {
              const uint8_t* row = src.data() + (size_t)y * w * 4 + (size_t)x0 * 4;
              for (uint32_t x = x0; x < x1 && x < w; x++, row += 4) {
                b += row[0]; g += row[1]; r += row[2]; a += row[3]; n++;
              }
            }
            uint8_t* d = out->data() + ((size_t)dy * nw + dx) * 4;
            d[0] = (uint8_t)(b / n); d[1] = (uint8_t)(g / n);
            d[2] = (uint8_t)(r / n); d[3] = (uint8_t)(a / n);
          }
        }
        *ow = nw; *oh = nh;
      };
      // Deterministic content + awkward ratios (non-integer spans exercise
      // the correction path), incl. a pathological tiny max_w (huge boxes).
      const std::pair<uint32_t, uint32_t> sizes[] = {
          {2880, 1800}, {1919, 1079}, {640, 480}, {4000, 3000}, {1024, 768}};
      const uint32_t maxws[] = {1920, 1000, 333, 3, 1};
      bool exact = true;
      for (size_t s = 0; s < sizeof(sizes) / sizeof(sizes[0]) && exact; ++s) {
        const uint32_t w = sizes[s].first, h = sizes[s].second;
        std::vector<uint8_t> src((size_t)w * h * 4);
        uint32_t st = 0x1234ABCDu;
        for (size_t i = 0; i < src.size(); i += 4) {
          st = st * 1664525u + 1013904223u;
          src[i] = (uint8_t)(st >> 24);
          src[i + 1] = (uint8_t)(st >> 16);
          src[i + 2] = (uint8_t)(st >> 8);
          src[i + 3] = 0xFF;
        }
        for (uint32_t mw : maxws) {
          if (mw >= w) continue;
          std::vector<uint8_t> fast, ref;
          uint32_t fw = 0, fh = 0, rw = 0, rh = 0;
          xnc::DownscaleBgra(src.data(), w, h, mw, &fast, &fw, &fh);
          naive(src, w, h, mw, &ref, &rw, &rh);
          if (fw != rw || fh != rh || fast != ref) exact = false;
        }
      }
      CHECK("ds-exact-vs-naive", exact);
    }
    // Identity pass-through when w <= max_w (and when max_w == 0).
    std::vector<uint8_t> id;
    CHECK("ds-identity", xnc::DownscaleBgra(src.data(), sw, sh, sw, &id, &ow, &oh) &&
          ow == sw && oh == sh && id == src);
    CHECK("ds-identity-zero-clamp", xnc::DownscaleBgra(src.data(), sw, sh, 0, &id, &ow, &oh) && id == src);

    // feat/rt-scale (hw-encode task 2 Part B): ScaledDims pure helper +
    // ScaledCapture wrapper over a synthetic capture (2880x1800 -> 1920x1200).
    uint32_t sdw = 0, sdh = 0;
    CHECK("sd-2880x1800", xnc::ScaledDims(2880, 1800, 1920, &sdw, &sdh) &&
          sdw == 1920 && sdh == 1200);
    CHECK("sd-3440x1440", xnc::ScaledDims(3440, 1440, 1920, &sdw, &sdh) &&
          sdw == 1920 && sdh == 804);
    CHECK("sd-identity", xnc::ScaledDims(1280, 720, 1920, &sdw, &sdh) &&
          sdw == 1280 && sdh == 720);
    CHECK("sd-zero-clamp", xnc::ScaledDims(2880, 1800, 0, &sdw, &sdh) &&
          sdw == 2880 && sdh == 1800);
    CHECK("sd-even-snap-h", xnc::ScaledDims(1920, 1081, 1920, &sdw, &sdh) &&
          sdw == 1920 && sdh == 1080);
    CHECK("sd-even-snap-w", xnc::ScaledDims(1365, 768, 1920, &sdw, &sdh) &&
          sdw == 1364 && sdh == 768);
    CHECK("sd-reject-zero", !xnc::ScaledDims(0, 100, 1920, &sdw, &sdh));
    {
      xnc::ScaledCapture sc(std::make_unique<ScriptedCapture>(2880, 1800, 2), 1920);
      CHECK("sc-dims", sc.Width() == 1920 && sc.Height() == 1200);
      xnc::FrameBlob b;
      std::string serr;
      CHECK("sc-acquire1", sc.Acquire(b, &serr));
      CHECK("sc-blob-dims", b.w == 1920 && b.h == 1200 &&
            b.bgra.size() == (size_t)1920 * 1200 * 4);
      CHECK("sc-acquire2", sc.Acquire(b, &serr));
      CHECK("sc-blob-dims2", b.w == 1920 && b.h == 1200);
      CHECK("sc-timeout-passthrough", !sc.Acquire(b, &serr) && serr == "err_timeout");
      // identity wrapper: <= max_w hands the inner frame through untouched
      xnc::ScaledCapture sci(std::make_unique<ScriptedCapture>(800, 600, 1), 1920);
      CHECK("sci-dims", sci.Width() == 800 && sci.Height() == 600);
      CHECK("sci-acquire", sci.Acquire(b, &serr) && b.w == 800 && b.h == 600 &&
            b.bgra.size() == (size_t)800 * 600 * 4);
      // gpu-readback: NV12 inner (the DXGI GPU path) passes through ScaledCapture
      // untouched - no CPU downscale, dims/pixfmt/gpu_scale preserved, and the
      // wrapped capture's scaled dims are the wrapper's dims (identity scale).
      {
        xnc::ScaledCapture scn(std::make_unique<Nv12ScriptedCapture>(1920, 1200, 2), 1920);
        CHECK("scn-dims", scn.Width() == 1920 && scn.Height() == 1200);
        xnc::FrameBlob bn;
        CHECK("scn-acquire1", scn.Acquire(bn, &serr));
        CHECK("scn-nv12-pixfmt", bn.pixfmt == xnc::Pixfmt::kNv12);
        CHECK("scn-nv12-dims", bn.w == 1920 && bn.h == 1200 &&
              bn.bgra.size() == xnc::Nv12Bytes(1920, 1200));
        CHECK("scn-gpu-scale-preserved", bn.gpu_scale_us == 2000);  // diag stamp survives
        CHECK("scn-acquire2", scn.Acquire(bn, &serr));
        CHECK("scn-nv12-again", bn.pixfmt == xnc::Pixfmt::kNv12 && bn.w == 1920 &&
              bn.gpu_scale_us == 2007);
        // 小屏 NV12(无需缩放)同样直通
        xnc::ScaledCapture scni(std::make_unique<Nv12ScriptedCapture>(800, 600, 1), 1920);
        CHECK("scni-acquire", scni.Acquire(bn, &serr) && bn.pixfmt == xnc::Pixfmt::kNv12 &&
              bn.w == 800 && bn.h == 600 && bn.bgra.size() == xnc::Nv12Bytes(800, 600));
        CHECK("scni-timeout-passthrough", !scni.Acquire(bn, &serr) && serr == "err_timeout");
      }
    }

    // WIC round trip: synthetic 64x48 BGRA -> JPEG -> JFIF magic + SOF dims.
    const uint32_t jw = 64, jh = 48;
    std::vector<uint8_t> frame((size_t)jw * jh * 4);
    for (uint32_t y = 0; y < jh; y++)
      for (uint32_t x = 0; x < jw; x++) {
        uint8_t* p = &frame[((size_t)y * jw + x) * 4];
        p[0] = (uint8_t)(x * 4); p[1] = (uint8_t)(y * 5);
        p[2] = (uint8_t)((x + y) * 2); p[3] = 0xFF;
      }
    std::vector<uint8_t> jpeg;
    std::string jerr;
    CHECK("wic-encode-ok", xnc::WicEncodeJpeg(frame.data(), jw, jh, 0.85f, &jpeg, &jerr));
    if (jpeg.size() >= 4) {
      CHECK("wic-jfif-magic", jpeg[0] == 0xFF && jpeg[1] == 0xD8 && jpeg[2] == 0xFF);
    } else {
      CHECK("wic-jfif-magic", false);
    }
    // Parseable size: scan markers for SOF0/SOF2 and read the big-endian
    // height/width fields (offsets +5..+8 inside the SOF segment).
    uint32_t pw = 0, ph = 0;
    size_t i = 2;
    while (i + 8 < jpeg.size()) {
      if (jpeg[i] != 0xFF) { i++; continue; }
      const uint8_t m = jpeg[i + 1];
      if (m == 0xC0 || m == 0xC2) {
        ph = ((uint32_t)jpeg[i + 5] << 8) | jpeg[i + 6];
        pw = ((uint32_t)jpeg[i + 7] << 8) | jpeg[i + 8];
        break;
      }
      if (m == 0xD8 || (m >= 0xD0 && m <= 0xD9)) { i += 2; continue; }
      const size_t seg = ((size_t)jpeg[i + 2] << 8) | jpeg[i + 3];
      if (seg < 2) break;
      i += 2 + seg;
    }
    CHECK("wic-sof-found", pw != 0 && ph != 0);
    CHECK("wic-dims", pw == jw && ph == jh);
    CHECK("wic-nonempty", jpeg.size() > 500);
  }

  // ---- M2-Slice3 Task 5:多显示器枚举 + 切换 ----
  {
    // 纯枚举表构造(fake outputs 经 seam):去重、GDI 序稳定索引、primary。
    std::vector<xnc::RawDisplayOutput> raws = {
        {11, true, 0, 0, 1920, 1080, true},      // adapter 序:primary 在前
        {22, true, -2560, 0, 2560, 1440, false}, // GDI 序第一位
        {11, true, 0, 0, 1920, 1080, true},      // 重复 monitor 11(跨适配器)
        {33, true, 1920, 0, 1280, 1024, false},  // GDI 未列 → 表尾
        {44, false, 0, 0, 1, 1, false},          // 未挂桌面:跳过
    };
    const std::vector<uint64_t> gdi = {22, 11};
    const std::vector<xnc::DisplayInfo> table = xnc::BuildDisplayTable(raws, gdi);
    CHECK("disp-count", table.size() == 3);
    CHECK("disp-gdi-order", table.size() == 3 && table[0].idx == 0 &&
                              table[0].monitor_id == 22 && table[1].idx == 1 &&
                              table[1].monitor_id == 11);
    CHECK("disp-tail", table.size() == 3 && table[2].monitor_id == 33 &&
                         table[2].idx == 2);
    CHECK("disp-geom", table.size() == 3 && table[0].origin_x == -2560 &&
                         table[0].w == 2560 && table[0].h == 1440);
    CHECK("disp-primary",
          table.size() == 3 && table[1].primary == 1 && table[0].primary == 0);

    // Task 6 ⑤:期望选择解析——过期选择(idx 越界,显示器拔掉/表收缩)
    // 回落 primary 而非拒绝;auto=primary;显式有效选择保持。
    bool stale = true;
    CHECK("sel-explicit", xnc::ResolveDisplayIndex(2, table, &stale) == 2 && !stale);
    CHECK("sel-auto-primary",
          xnc::ResolveDisplayIndex(xnc::kDisplaySelectAuto, table, &stale) == 1 &&
          !stale);
    CHECK("sel-stale-falls-back-primary",
          xnc::ResolveDisplayIndex(9, table, &stale) == 1 && stale);
    const std::vector<xnc::DisplayInfo> noprimary = {
        {0, 0, 0, 640, 480, 0, 1}, {1, 640, 0, 640, 480, 0, 2}};
    CHECK("sel-stale-falls-back-zero",
          xnc::ResolveDisplayIndex(5, noprimary, &stale) == 0 && stale);
    CHECK("sel-empty-table",
          xnc::ResolveDisplayIndex(1, {}, &stale) == 0 && !stale);

    // HOST_HELLO displays 块黄金字节(兼容扩展:legacy 20B 前缀不变)。
    xnc::HostHelloPayload hh;
    hh.gen = 7; hh.w = 1920; hh.h = 1080; hh.fps = 30; hh.max_subs = 4;
    hh.displays = table;
    const std::vector<uint8_t> wire = xnc::EncodeHostHello(hh);
    CHECK("hh-size", wire.size() == 24 + 3 * 21);
    const uint8_t want_head[24] = {7, 0, 0, 0, 0x80, 7, 0, 0, 0x38, 4, 0, 0,
                                   30, 0, 0, 0, 4, 0, 0, 0, 3, 0, 0, 0};
    CHECK("hh-head-golden", std::memcmp(wire.data(), want_head, 24) == 0);
    // entry 0(idx=0, origin=-2560=0xFFFFF600, 2560x1440, 非 primary)
    CHECK("hh-entry0-origin", wire[28] == 0x00 && wire[29] == 0xF6 &&
                                wire[30] == 0xFF && wire[31] == 0xFF);
    CHECK("hh-entry0-dims", wire[36] == 0x00 && wire[37] == 0x0A &&
                              wire[40] == 0xA0 && wire[41] == 0x05);
    CHECK("hh-entry0-notprimary", wire[44] == 0);
    // entry 1(idx=1, primary=1)
    const size_t e1 = 24 + 21;
    CHECK("hh-entry1", wire[e1] == 1 && wire[e1 + 20] == 1);
    // entry 2(idx=2, origin.x=1920=0x780)
    const size_t e2 = 24 + 42;
    CHECK("hh-entry2", wire[e2] == 2 && wire[e2 + 4] == 0x80 && wire[e2 + 5] == 0x07);
    // round trip
    xnc::HostHelloPayload back{};
    xnc::Frame hf{0, xnc::kMsgHostHello, 0, wire};
    CHECK("hh-roundtrip", xnc::DecodeHostHello(hf, &back) && back.gen == 7 &&
                            back.w == 1920 && back.h == 1080 &&
                            back.displays.size() == 3 &&
                            back.displays[0].origin_x == -2560 &&
                            back.displays[1].primary == 1 &&
                            back.displays[2].idx == 2);
    // legacy 20B 载荷仍可解码(displays 空)
    std::vector<uint8_t> legacy(20, 0);
    xnc::Frame lf{0, xnc::kMsgHostHello, 0, legacy};
    xnc::HostHelloPayload lback{};
    CHECK("hh-legacy20", xnc::DecodeHostHello(lf, &lback) && lback.displays.empty());
    // 截断 displays 块拒绝
    std::vector<uint8_t> trunc(wire.begin(), wire.begin() + 26);
    xnc::Frame tf{0, xnc::kMsgHostHello, 0, trunc};
    CHECK("hh-truncated-rejected", !xnc::DecodeHostHello(tf, &lback));

    // 0x0128 codec
    CHECK("sw-enc", xnc::EncodeSwitchDisplay(99).size() == 4 &&
                      xnc::EncodeSwitchDisplay(99)[0] == 99);
    uint32_t sidx = 0;
    xnc::Frame sf{0, xnc::kMsgSwitchDisplay, 3, xnc::EncodeSwitchDisplay(2)};
    CHECK("sw-dec", xnc::DecodeSwitchDisplay(sf, &sidx) && sidx == 2);
    xnc::Frame bad{0, xnc::kMsgSwitchDisplay, 3, {1, 2, 3}};
    CHECK("sw-dec-badsize", !xnc::DecodeSwitchDisplay(bad, &sidx));

    // 0x0129 codec (M3 Task 3): [u32 kbps][u32 fps][u32 max_w],12B 定长。
    xnc::VideoConfigPayload vc{2300, 30, 1920};
    const std::vector<uint8_t> vcwire = xnc::EncodeSetVideoConfig(vc);
    CHECK("vc-enc", vcwire.size() == 12 && vcwire[0] == 0xFC && vcwire[4] == 30 &&
                      vcwire[8] == 0x80 && vcwire[9] == 0x07);
    xnc::VideoConfigPayload vback{};
    xnc::Frame vf{0, xnc::kMsgSetVideoConfig, 5, vcwire};
    CHECK("vc-dec", xnc::DecodeSetVideoConfig(vf, &vback) &&
                      vback.bitrate_kbps == 2300 && vback.fps == 30 &&
                      vback.max_w == 1920);
    xnc::Frame vbad{0, xnc::kMsgSetVideoConfig, 5, {1, 2, 3}};
    CHECK("vc-dec-badsize", !xnc::DecodeSetVideoConfig(vbad, &vback));

    // HOST_HELLO v2 capabilities 扩展(M3 Task 3):v2 形状 + 尾随 caps u32;
    // 无 handler 的 v2 形状照旧(caps=0),legacy 不受影响;两种形状互不
    // 误读(caps ∈ {0,1} != 2,位移 8B 后 legacy 校验必败)。
    xnc::HostHelloPayload chh;
    chh.gen = 4; chh.w = 2560; chh.h = 1440; chh.fps = 30; chh.max_subs = 4;
    const std::vector<uint8_t> capswire =
        xnc::EncodeHostHelloV2Caps(chh, xnc::kHostCapSetVideoConfig);
    CHECK("hhv2c-size", capswire.size() == 24 + 4 + 4);
    CHECK("hhv2c-tail", capswire.size() == 32 &&
                          xnc::rt_detail::GetU32(capswire.data() + 24) == 2 &&
                          xnc::rt_detail::GetU32(capswire.data() + 28) ==
                              xnc::kHostCapSetVideoConfig);
    xnc::HostHelloPayload cback{};
    uint32_t mp = 0, caps = 0;
    xnc::Frame cf{0, xnc::kMsgHostHello, 0, capswire};
    CHECK("hhv2c-dec", xnc::DecodeHostHelloV2(cf, &cback, &mp, &caps) &&
                         cback.w == 2560 && mp == 2 &&
                         caps == xnc::kHostCapSetVideoConfig);
    // 纯 v2 形状(无 caps 字段)仍可解,caps 报 0。
    const std::vector<uint8_t> plainv2 = xnc::EncodeHostHelloV2(chh);
    xnc::Frame pf{0, xnc::kMsgHostHello, 0, plainv2};
    CHECK("hhv2-plain-dec", xnc::DecodeHostHelloV2(pf, &cback, &mp, &caps) &&
                              mp == 2 && caps == 0);
  }
  { // rt 场景 ⑧(M2-S3 Task 5):HOST_HELLO 带 displays;0x0128 非法 idx →
    // STATE{invalid_display}(可恢复,无 reset);合法 idx → 受理 + reset 请求
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(kRtW, kRtH, kRtFps, kRtBitrate, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: rt8-init err=%s\n", err.c_str());
    CHECK("rt8-init", init_ok);
    if (init_ok) {
      xnc::RtServer rt;
      xnc::RtServer::Opts ro = rt_opts(5);
      // 静态存储:displays_fn/switch_display_fn 是无捕获 fn 指针
      static std::vector<xnc::DisplayInfo> fake_table;
      static uint32_t switched_idx = 0xFFFFFFFFu;
      fake_table.clear();
      switched_idx = 0xFFFFFFFFu;
      xnc::DisplayInfo d0; d0.idx = 0; d0.w = kRtW; d0.h = kRtH; d0.primary = 1;
      xnc::DisplayInfo d1; d1.idx = 1; d1.origin_x = 64; d1.w = 32; d1.h = 24;
      fake_table.push_back(d0); fake_table.push_back(d1);
      ro.displays_fn = [](void*) { return fake_table; };
      ro.switch_display_fn = [](void*, uint32_t idx) {
        if (idx >= fake_table.size()) return false;
        switched_idx = idx;
        return true;
      };
      xnc::CaptureReset reset;  // clock=null → TakeReset 立即可取(无消费方)
      ro.reset = &reset;
      CHECK("rt8-start", rt.Start(ro, kRtW, kRtH));
      ScriptedCapture cap(kRtW, kRtH, 3);
      xnc::PipelineOpts po;
      po.duration_s = 2;
      po.fps = kRtFps;
      po.target_bitrate_bps = kRtBitrate;
      xnc::PipelineResult res;
      std::thread pipe_th([&] { res = xnc::Pipeline::Run(cap, enc, rt, po); });
      RtTestClient a;
      CHECK("rt8-connect", a.Connect(ro.pipe_name.c_str(), kRtSecret, sizeof(kRtSecret)));
      CHECK("rt8-attach-hello", a.Attach(9));
      CHECK("rt8-hello-displays", a.hello_ok_ && a.hello_.displays.size() == 2 &&
                                   a.hello_.displays[1].origin_x == 64);
      // 非法 idx(表大小 2)→ STATE{invalid_display} + FlagError 应答,无 reset
      CHECK("rt8-send-invalid", a.SendRaw(xnc::kMsgSwitchDisplay,
                                          xnc::EncodeSwitchDisplay(99)));
      // 合法 idx=1 → FlagResponse 应答 + reset 请求记账
      CHECK("rt8-send-valid", a.SendRaw(xnc::kMsgSwitchDisplay,
                                        xnc::EncodeSwitchDisplay(1)));
      bool saw_invalid_state = false, saw_ok_resp = false, saw_err_resp = false;
      const ULONGLONG dl = GetTickCount64() + 2500;
      while (GetTickCount64() < dl &&
             !(saw_invalid_state && saw_ok_resp && saw_err_resp)) {
        xnc::Frame f;
        if (!a.ReadFrameT(f, 200)) break;
        a.CountFrame(f);
        if (f.message_type == xnc::kMsgState) {
          xnc::StateEventPayload st;
          if (xnc::DecodeStateEvent(f, &st) && std::strcmp(st.code, "invalid_display") == 0)
            saw_invalid_state = st.recoverable != 0;
        } else if (f.message_type == xnc::kMsgSwitchDisplay) {
          if (f.flags & xnc::kFlagError) saw_err_resp = true;
          if (f.flags & xnc::kFlagResponse) saw_ok_resp = true;
        }
      }
      pipe_th.join();
      rt.Shutdown();
      CHECK("rt8-invalid-state", saw_invalid_state);
      CHECK("rt8-invalid-error-resp", saw_err_resp);
      CHECK("rt8-valid-ok-resp", saw_ok_resp);
      CHECK("rt8-switch-fn-idx", switched_idx == 1);
      CHECK("rt8-reset-requested", reset.requests() == 1);
      const xnc::RtServer::Stats st = rt.stats();
      CHECK("rt8-stats", st.switch_accepted == 1 && st.switch_invalid == 1);
      CHECK("rt8-pipeline-ok", res.ok);
      std::printf("SELFTEST NOTE: rt8 switch_accepted=%llu switch_invalid=%llu reset_reqs=%u\n",
                  (unsigned long long)st.switch_accepted,
                  (unsigned long long)st.switch_invalid, reset.requests());
    }
  }
  { // rt 场景 ⑨(M1 Task 2):XNC_DESKTOP_PIPELINE_V2 路径端到端 - opts
    // 直设 pipeline_v2=true(不依赖进程环境),服务器发扩展 HOST_HELLO
    // (media_protocol=2)+ 0x0205 v2 帧;断言客户端收到合法 v2 帧且身份正确。
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(kRtW, kRtH, kRtFps, kRtBitrate, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: rt9-init err=%s\n", err.c_str());
    CHECK("rt9-init", init_ok);
    if (init_ok) {
      xnc::RtServer rt;
      xnc::RtServer::Opts ro = rt_opts(8);
      ro.pipeline_v2 = true;  // M1 Task 2: v2 media wire (opts, not env)
      CHECK("rt9-start", rt.Start(ro, kRtW, kRtH));
      ScriptedCapture cap(kRtW, kRtH, 3);
      xnc::PipelineOpts po;
      po.duration_s = 3;
      po.fps = kRtFps;
      po.target_bitrate_bps = kRtBitrate;
      xnc::PipelineResult res;
      std::thread pipe_th([&] { res = xnc::Pipeline::Run(cap, enc, rt, po); });
      RtTestClient a;
      CHECK("rt9-connect", a.Connect(ro.pipe_name.c_str(), kRtSecret, sizeof(kRtSecret)));
      CHECK("rt9-attach-hello", a.Attach(10));
      CHECK("rt9-no-frame-before-hello", !a.frame_before_hello_);
      CHECK("rt9-hello-v2",
            a.hello_ok_ && a.media_protocol_ == xnc::kMediaProtocolV2 &&
                a.hello_.w == kRtW && a.hello_.h == kRtH && a.hello_.gen == 1);
      a.Pump(2800, [&a] { return a.keys_ >= 1; });
      pipe_th.join();
      a.Pump(700);
      rt.Shutdown();
      CHECK("rt9-first-key", a.keys_ >= 1);
      CHECK("rt9-v2-frames-received", a.v2_frames_ >= 1);
      CHECK("rt9-frames-received", a.frames_ >= 1);
      // Identity correctness (pipeline.cpp assigns epochs=1 on a fresh run,
      // content/seq from 1, timestamps from the real clock).
      CHECK("rt9-key-identity",
            a.first_key_id_set_ && a.first_key_id_.capture_epoch == 1 &&
                a.first_key_id_.codec_epoch == 1 &&
                a.first_key_id_.content_id >= 1 &&
                a.first_key_id_.encode_seq >= 1 &&
                a.first_key_id_.source_mono_us >= 1 &&
                a.first_key_id_.present_mono_us >= 1);
      CHECK("rt9-key-dims", a.first_key_w_ == kRtW && a.first_key_h_ == kRtH);
      CHECK("rt9-key-payload-shaped", StreamStartsWithKeyframe(a.last_key_payload_));
      // M1 Task 5 (ruling 1a): every 0x0205 AU the wire delivered replays
      // clean through a fresh ledger (in-epoch strict monotonicity) and
      // carries source_mono_us <= present_mono_us - end-to-end, encoder to
      // decoded subscriber frame.
      CHECK("rt9-delivered-identity-monotonic", DeliveredIdentitiesValid(a.v2_ids_));
      CHECK("rt9-stream-end-state", a.saw_stream_end_);
      CHECK("rt9-pipeline-ok", res.ok);
      const xnc::RtServer::Stats st = rt.stats();
      CHECK("rt9-stats", st.aus_emitted >= 1 && st.frames_enqueued >= 1);
      std::printf("SELFTEST NOTE: rt9 v2_frames=%llu keys=%llu id=[cap=%llu codec=%llu content=%llu seq=%llu src=%llu present=%llu]\n",
                  (unsigned long long)a.v2_frames_, (unsigned long long)a.keys_,
                  (unsigned long long)a.first_key_id_.capture_epoch,
                  (unsigned long long)a.first_key_id_.codec_epoch,
                  (unsigned long long)a.first_key_id_.content_id,
                  (unsigned long long)a.first_key_id_.encode_seq,
                  (unsigned long long)a.first_key_id_.source_mono_us,
                  (unsigned long long)a.first_key_id_.present_mono_us);
    }
  }
  { // rt 场景 ⑩(M1 Task 4):v2 队列溢出 —— 卡死订阅者 → 溢出 → 清空队列 +
    // WAIT_IDR + 合并 IDR(reason=queue_overflow);健康订阅者照常收到第二个
    // IDR;本场景无 epoch 变化 → 无 0x020B。噪声帧(320x240)保证溢出确定
    // 触发(同 rt3;64x48 彩条 AU 太小打不满管道缓冲)。
    const uint32_t nw = 320, nh = 240, nfps = 15, nbitrate = 2300000;
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(nw, nh, nfps, nbitrate, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: rt10-init err=%s\n", err.c_str());
    CHECK("rt10-init", init_ok);
    if (init_ok) {
      xnc::RtServer rt;
      xnc::RtServer::Opts ro = rt_opts(9);
      ro.fps = nfps;
      ro.bitrate_bps = nbitrate;
      ro.pipeline_v2 = true;  // M1 Task 4: WAIT_IDR/0x020B 是 v2 模式行为
      CHECK("rt10-start", rt.Start(ro, nw, nh));
      NoisyCapture cap(nw, nh, 130);  // ~8.7s 连续噪声帧
      xnc::PipelineOpts po;
      po.duration_s = 9;
      po.fps = nfps;
      po.target_bitrate_bps = nbitrate;
      xnc::PipelineResult res;
      std::thread pipe_th([&] { res = xnc::Pipeline::Run(cap, enc, rt, po); });
      RtTestClient a;  // 健康订阅者
      CHECK("rt10-a-connect", a.Connect(ro.pipe_name.c_str(), kRtSecret, sizeof(kRtSecret)));
      CHECK("rt10-a-attach", a.Attach(7));
      RtTestClient b;  // 卡死订阅者:attach 后一个字节都不读
      CHECK("rt10-b-connect", b.Connect(ro.pipe_name.c_str(), kRtSecret, sizeof(kRtSecret)));
      CHECK("rt10-b-attach", b.Attach(9));
      a.Pump(8500, [&a] { return a.keys_ >= 2; });
      pipe_th.join();
      a.Pump(500);
      rt.Shutdown();
      CHECK("rt10-a-two-keys", a.keys_ >= 2);  // 溢出合并的 IDR 广播给了健康订阅者
      CHECK("rt10-pipeline-ok", res.ok);
      const xnc::RtServer::Stats st = rt.stats();
      CHECK("rt10-overflow-drops", st.frames_dropped_overflow >= 1);
      CHECK("rt10-overflow-merged-idr", st.idr_queue_overflow >= 1);
      // 无 epoch 变化:WAIT_IDR 来自溢出,不产生任何 0x020B。
      CHECK("rt10-no-discontinuity",
            st.stream_discontinuities == 0 && a.discontinuities_ == 0);
      std::printf("SELFTEST NOTE: rt10 a_keys=%llu drop_of=%llu drop_nk=%llu overflow_idr=%llu disc=%llu\n",
                  (unsigned long long)a.keys_,
                  (unsigned long long)st.frames_dropped_overflow,
                  (unsigned long long)st.frames_dropped_needkey,
                  (unsigned long long)st.idr_queue_overflow,
                  (unsigned long long)st.stream_discontinuities);
    }
  }
  { // rt 场景 ⑪(M1 Task 4):v2 epoch 前进(capture 重建)→ 每订阅者 0x020B
    // STREAM_DISCONTINUITY(新 epoch 对 + 固定 reason)+ 清空 + WAIT_IDR;随后
    // 新 epoch 的 IDR 恢复 LIVE。断流后首帧必须是该 epoch 的 IDR,重建后
    // 旧 epoch 的 AU(编码器前瞻尾)一律被抑制 —— 重建信号之后再无
    // epoch-1 帧上线(rebuild floor 挡尾),最后一个 key 是 epoch 2。
    xnc::MfSoftEncoder enc;
    std::string err;
    const bool init_ok = enc.Init(kRtW, kRtH, kRtFps, kRtBitrate, &err);
    if (!init_ok) std::printf("SELFTEST NOTE: rt11-init err=%s\n", err.c_str());
    CHECK("rt11-init", init_ok);
    if (init_ok) {
      xnc::RtServer rt;
      xnc::RtServer::Opts ro = rt_opts(10);
      ro.pipeline_v2 = true;
      CHECK("rt11-start", rt.Start(ro, kRtW, kRtH));
      // 第 40 帧 err_rebuilt → capture_epoch 2;重建晚于首个 AU 的出现
      // (编码器 warm-up ~12+ 帧),保证服务端已有扇出历史 → rebuild floor
      // 生效;之后 20 帧持续流入,保证重建强制的 IDR 在运行期内浮现。
      ScriptedCapture cap(kRtW, kRtH, 60, 40);
      xnc::PipelineOpts po;
      po.duration_s = 6;
      po.fps = kRtFps;
      po.target_bitrate_bps = kRtBitrate;
      xnc::PipelineResult res;
      std::thread pipe_th([&] { res = xnc::Pipeline::Run(cap, enc, rt, po); });
      RtTestClient a;
      CHECK("rt11-connect", a.Connect(ro.pipe_name.c_str(), kRtSecret, sizeof(kRtSecret)));
      CHECK("rt11-attach-hello", a.Attach(11));
      a.Pump(5800, [&a] { return a.discontinuities_ >= 1 && a.keys_ >= 1; });
      pipe_th.join();
      a.Pump(2000);  // 排空在途帧(含可能的第二把 epoch-2 IDR)
      rt.Shutdown();
      CHECK("rt11-first-key", a.keys_ >= 1);
      CHECK("rt11-discontinuity", a.discontinuities_ >= 1);
      // 一次重建 = 一次 epoch 前进 = 恰一条 0x020B,携带新 epoch 对。
      CHECK("rt11-disc-epochs",
            a.discontinuities_ == 1 && a.disc_capture_epoch_ == 2 &&
                a.disc_codec_epoch_ == 1);
      CHECK("rt11-disc-then-idr", a.disc_idr_ok_ && !a.disc_delta_seen_);
      // 旧 epoch 的一切 AU 被抑制(确定性契约):重建信号之后再无 epoch-1
      // 帧上线,最后一个 key 也是 epoch 2。(不断言「首个 key 即 epoch 2」:
      // 若流水线首个 AU 先于重建上线,订阅者按 join-from-IDR 合法收到
      // epoch-1 的 IDR —— 那是编码器预热与重建的时序竞态,非契约。)
      CHECK("rt11-last-key-epoch",
            a.last_key_id_set_ && a.last_key_id_.capture_epoch == 2);
      CHECK("rt11-old-epoch-suppressed", a.old_epoch_after_rebuilt_ == 0);
      // M1 Task 5 (ruling 1a): the delivered sequence spans the epoch-1 →
      // epoch-2 advance (capture rebuild); the fresh-ledger replay proves the
      // re-baseline held end-to-end and no in-epoch regression slipped out.
      CHECK("rt11-delivered-identity-monotonic", DeliveredIdentitiesValid(a.v2_ids_));
      CHECK("rt11-pipeline-ok", res.ok);
      const xnc::RtServer::Stats st = rt.stats();
      CHECK("rt11-stats-disc", st.stream_discontinuities >= 1);
      std::printf("SELFTEST NOTE: rt11 keys=%llu frames=%llu v2=%llu disc=%llu disc_ep=[%llu,%llu] disc_idr=%d disc_delta=%d old_epoch=%llu stats_disc=%llu enq=%llu keyenq=%llu dropnk=%llu streamend=%d\n",
                  (unsigned long long)a.keys_, (unsigned long long)a.frames_,
                  (unsigned long long)a.v2_frames_, (unsigned long long)a.discontinuities_,
                  (unsigned long long)a.disc_capture_epoch_,
                  (unsigned long long)a.disc_codec_epoch_,
                  a.disc_idr_ok_ ? 1 : 0, a.disc_delta_seen_ ? 1 : 0,
                  (unsigned long long)a.old_epoch_after_rebuilt_,
                  (unsigned long long)st.stream_discontinuities,
                  (unsigned long long)st.frames_enqueued,
                  (unsigned long long)st.keys_enqueued,
                  (unsigned long long)st.frames_dropped_needkey,
                  a.saw_stream_end_ ? 1 : 0);
    }
  }
  { // rt 场景 ⑫(M3 Task 3):SET_VIDEO_CONFIG 0x0129 —— v2 wire + 已接 applier
    // 的 host:扩展 HOST_HELLO 广告 kHostCapSetVideoConfig;合法 payload →
    // FlagResponse(受理,applier 收到 bps 换算值);坏 payload →
    // FlagResponse|FlagError + 记账;未接 applier 的 host 不广告能力且
    // 受理失败。无需管线(0x0129 是 opts 层管控面)。
    xnc::RtServer rt;
    xnc::RtServer::Opts ro = rt_opts(11);
    ro.pipeline_v2 = true;  // 能力广告只走 v2 扩展 HOST_HELLO
    // 静态记账(场景 ⑧ 同款):applier 收到的参数。
    static uint32_t got_bps = 0, got_fps = 0, got_maxw = 0;
    got_bps = got_fps = got_maxw = 0;
    ro.set_video_config_fn = [](void*, uint32_t bps, uint32_t fps, uint32_t mw) {
      got_bps = bps; got_fps = fps; got_maxw = mw;
      return true;
    };
    CHECK("rt12-start", rt.Start(ro, kRtW, kRtH));
    RtTestClient a;
    CHECK("rt12-connect", a.Connect(ro.pipe_name.c_str(), kRtSecret, sizeof(kRtSecret)));
    CHECK("rt12-attach", a.Attach(12));
    CHECK("rt12-hello-caps", a.hello_ok_ && a.media_protocol_ == 2 &&
                               a.caps_ == xnc::kHostCapSetVideoConfig);
    // 合法 0x0129:1500kbps → 1.5Mbps;FlagResponse 无错误。
    CHECK("rt12-send", a.SendRaw(xnc::kMsgSetVideoConfig,
                                 xnc::EncodeSetVideoConfig({1500, 20, 1280})));
    // 坏 payload(4B)→ FlagResponse|FlagError。
    CHECK("rt12-send-bad", a.SendRaw(xnc::kMsgSetVideoConfig, {1, 2, 3, 4}));
    bool ok_resp = false, err_resp = false;
    const ULONGLONG dl = GetTickCount64() + 2500;
    while (GetTickCount64() < dl && !(ok_resp && err_resp)) {
      xnc::Frame f;
      if (!a.ReadFrameT(f, 200)) break;
      a.CountFrame(f);
      if (f.message_type == xnc::kMsgSetVideoConfig) {
        if ((f.flags & xnc::kFlagError) != 0) err_resp = true;
        if ((f.flags & xnc::kFlagResponse) != 0 && (f.flags & xnc::kFlagError) == 0)
          ok_resp = true;
      }
    }
    rt.Shutdown();
    CHECK("rt12-ok-resp", ok_resp);
    CHECK("rt12-bad-resp", err_resp);
    CHECK("rt12-applier", got_bps == 1500000 && got_fps == 20 && got_maxw == 1280);
    const xnc::RtServer::Stats st = rt.stats();
    CHECK("rt12-stats", st.video_configs == 1 && st.video_config_rejected == 1);

    // 对照:未接 applier(v1 wire + 无 handler)→ 无能力广告 + 受理失败。
    xnc::RtServer rt2;
    xnc::RtServer::Opts ro2 = rt_opts(12);
    CHECK("rt12b-start", rt2.Start(ro2, kRtW, kRtH));
    RtTestClient b;
    CHECK("rt12b-connect", b.Connect(ro2.pipe_name.c_str(), kRtSecret, sizeof(kRtSecret)));
    CHECK("rt12b-attach", b.Attach(13));
    CHECK("rt12b-no-caps", b.hello_ok_ && b.caps_ == 0);
    CHECK("rt12b-send", b.SendRaw(xnc::kMsgSetVideoConfig,
                                  xnc::EncodeSetVideoConfig({1500, 20, 1280})));
    bool err2 = false;
    const ULONGLONG dl2 = GetTickCount64() + 2500;
    while (GetTickCount64() < dl2 && !err2) {
      xnc::Frame f;
      if (!b.ReadFrameT(f, 200)) break;
      b.CountFrame(f);
      if (f.message_type == xnc::kMsgSetVideoConfig &&
          (f.flags & xnc::kFlagError) != 0)
        err2 = true;
    }
    rt2.Shutdown();
    CHECK("rt12b-err-resp", err2);
    CHECK("rt12b-stats", rt2.stats().video_config_rejected == 1);
    std::printf("SELFTEST NOTE: rt12 ok=%d err=%d bps=%u fps=%u max_w=%u stats=[%llu,%llu]\n",
                ok_resp ? 1 : 0, err_resp ? 1 : 0, got_bps, got_fps, got_maxw,
                (unsigned long long)st.video_configs,
                (unsigned long long)st.video_config_rejected);
  }
  // ---- M2 Task 4: MediaPipelineV2 (depth-one GPU media pipeline). These
  // scenarios run ONLY with --desktop-pipeline-v2 (ruling 1: the CLI flag
  // selects MediaPipelineV2 + the v2 wire together; the plain selftest
  // keeps the M0-pinned behavior untouched).
  if (desktop_pipeline_v2) {
    // Poll-wait helper (bounded; fakes never block inside AcquireSurface).
    auto wait_for = [](auto pred, DWORD timeout_ms) {
      const ULONGLONG deadline = GetTickCount64() + timeout_ms;
      while (GetTickCount64() < deadline) {
        if (pred()) return true;
        Sleep(5);
      }
      return pred();
    };

    // ---- shared fakes (the DeviceSurfaceCapture shape + a script) ----

    // v2q content discriminator: solid white with a moving 4px black
    // stripe (content still changes every frame). After the BGRA->NV12
    // studio-range conversion its luma is ~235 almost everywhere, vs ~16
    // in the bars pattern's BLACK bar ([w/2, 5w/8)) - a margin no driver
    // rounding can bridge, so a readback of a converted slot tells the
    // GDI rung's live pixels from frozen DXGI pixels apart.
    class BrightSolid {
     public:
      BrightSolid(uint32_t w, uint32_t h)
          : bgra_((size_t)w * h * 4, 0xFF), w_(w), h_(h) {}
      const uint8_t* Frame(uint32_t i) {
        std::fill(bgra_.begin(), bgra_.end(), 0xFF);
        static const uint8_t kBlack[4] = {0, 0, 0, 0xFF};
        const uint32_t sw = 4;
        const uint32_t x0 = (i * 9) % (w_ - sw);
        for (uint32_t y = 0; y < h_; ++y)
          for (uint32_t x = x0; x < x0 + sw; ++x)
            std::memcpy(bgra_.data() + ((size_t)y * w_ + x) * 4, kBlack, 4);
        return bgra_.data();
      }

     private:
      std::vector<uint8_t> bgra_;
      uint32_t w_, h_;
    };

    // Device-backed scripted ICaptureSurface: per-scripted-frame draws the
    // synthetic bars frame i into a DEFAULT BGRA texture via
    // UpdateSubresource (the GDI upload shape) and CopyFrom's it into the
    // caller-owned LatestSurface with the GIVEN identity (capture.h
    // contract). Optional injected kAccessLost entries drive the reset
    // path. Also implements ICapture (Width/Height/Rebuild) for the
    // pipeline's reset sequence.
    class ScriptedDeviceCapture final : public xnc::ICapture,
                                         public xnc::ICaptureSurface {
     public:
      enum class Step : uint8_t { kFrame, kAccessLost };
      ScriptedDeviceCapture(ID3D11Device* dev, ID3D11DeviceContext* ctx,
                            std::vector<Step> script, uint32_t w, uint32_t h,
                            bool use_bright = false)
          : dev_(dev), ctx_(ctx), script_(std::move(script)), w_(w), h_(h),
            bars_(w, h), bright_(w, h), use_bright_(use_bright) {
        D3D11_TEXTURE2D_DESC td{};
        td.Width = w;
        td.Height = h;
        td.MipLevels = 1;
        td.ArraySize = 1;
        td.SampleDesc.Count = 1;
        td.Format = DXGI_FORMAT_B8G8R8A8_UNORM;
        td.Usage = D3D11_USAGE_DEFAULT;
        tex_ = nullptr;
        dev_->CreateTexture2D(&td, nullptr, &tex_);
      }
      ~ScriptedDeviceCapture() override {
        if (tex_) tex_->Release();
      }
      ScriptedDeviceCapture(const ScriptedDeviceCapture&) = delete;
      ScriptedDeviceCapture& operator=(const ScriptedDeviceCapture&) = delete;

      bool Acquire(xnc::FrameBlob&, std::string* err = nullptr,
                   uint32_t = 0) override {
        if (err) *err = "err_timeout";  // CPU path unused under MediaPipelineV2
        return false;  // (probe semantics: err_timeout = duplication healthy)
      }
      uint32_t Width() const override { return w_; }
      uint32_t Height() const override { return h_; }
      uint32_t RebuildCount() const override { return rebuilds_; }
      bool Rebuild(std::string* err) override {
        ++rebuilds_;
        // Task 5: the first rebuild_fail_first calls fail (a dead DXGI
        // rung for the GDI-fallback scenarios); 0 = always succeed (the
        // Task 4 default).
        if (rebuild_fail_first != 0 && rebuilds_ <= rebuild_fail_first) {
          if (err) *err = "err_rebuild_failed (scripted)";
          return false;
        }
        return true;
      }

      xnc::CaptureStatus AcquireSurface(xnc::LatestSurface& latest,
                                        uint32_t timeout_ms,
                                        xnc::FrameIdentity* id,
                                        std::string* err) override {
        (void)timeout_ms;
        if (err) err->clear();
        if (next_ < script_.size()) {
          const Step st = script_[next_++];
          if (st == Step::kAccessLost) {
            if (err) *err = "err_access_lost";
            if (id) *id = last_id_;
            return xnc::CaptureStatus::kAccessLost;
          }
          // Distinct content per script index (stripe moves).
          const uint32_t fi = static_cast<uint32_t>(next_ - 1);
          const uint8_t* px = use_bright_ ? bright_.Frame(fi)
                                          : bars_.Frame(fi);
          ctx_->UpdateSubresource(tex_, 0, nullptr, px, w_ * 4, 0);
          if (use_bright_) SyncTexture(tex_);  // upload resident (v2q)
          if (latest.width() != w_ || latest.height() != h_) {
            std::string ierr;
            if (!latest.Init(dev_, w_, h_, &ierr)) {
              if (err) *err = "latest init: " + ierr;
              return xnc::CaptureStatus::kFatal;
            }
          }
          xnc::FrameIdentity stamp = id != nullptr ? *id : xnc::FrameIdentity{};
          stamp.source_mono_us = xnc::NowMonoUs();
          stamp.encode_seq = 0;
          stamp.present_mono_us = 0;
          std::string cerr_;
          if (!latest.CopyFrom(ctx_, tex_, stamp, &cerr_)) {
            if (err) *err = "latest copy: " + cerr_;
            return xnc::CaptureStatus::kFatal;
          }
          if (use_bright_) {  // the surface copy EXECUTED before kFrame
            xnc::FrameIdentity dsid;
            ID3D11Texture2D* dtex = nullptr;
            if (latest.Snapshot(&dsid, &dtex) && dtex != nullptr) {
              SyncTexture(dtex);
              dtex->Release();
            }
          }
          last_id_ = stamp;
          ++frames_;
          if (id) *id = stamp;
          return xnc::CaptureStatus::kFrame;
        }
        if (err) *err = "err_timeout";  // static screen from here on
        if (id) *id = last_id_;
        return xnc::CaptureStatus::kNoChange;
      }

      size_t frames() const { return frames_.load(); }
      uint32_t rebuilds() const { return rebuilds_; }
      // Task 5 backend-fallback knob (see Rebuild).
      uint32_t rebuild_fail_first = 0;

      // v2q determinism: staging-Map a texture so its most recent write on
      // this device's context has EXECUTED (Map drains the queue). The
      // two-device swap scenario needs the fake's writes visible before
      // AcquireSurface returns: the converter's video-engine
      // VideoProcessorBlt on a BRAND-NEW device otherwise races the first
      // read of the surface (measured: the first post-swap conversion read
      // the zero-initialized surface; frame 2 onward is correct). The
      // production backends keep their adjacent copy chains (the M2 gate
      // validated real output end to end); the fake owns making its OWN
      // timing deterministic.
      void SyncTexture(ID3D11Texture2D* t) {
        if (t == nullptr) return;
        D3D11_TEXTURE2D_DESC ud{};
        t->GetDesc(&ud);
        D3D11_TEXTURE2D_DESC sd = ud;
        sd.Usage = D3D11_USAGE_STAGING;
        sd.BindFlags = 0;
        sd.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
        sd.MiscFlags = 0;
        Microsoft::WRL::ComPtr<ID3D11Texture2D> stage;
        if (FAILED(dev_->CreateTexture2D(&sd, nullptr, &stage))) return;
        ctx_->CopyResource(stage.Get(), t);
        D3D11_MAPPED_SUBRESOURCE m{};
        if (SUCCEEDED(ctx_->Map(stage.Get(), 0, D3D11_MAP_READ, 0, &m)))
          ctx_->Unmap(stage.Get(), 0);
      }

     private:
      ID3D11Device* dev_;
      ID3D11DeviceContext* ctx_;
      ID3D11Texture2D* tex_ = nullptr;
      std::vector<Step> script_;
      uint32_t w_, h_;
      SyntheticBars bars_;
      BrightSolid bright_;      // v2q: the GDI rung's bright content
      bool use_bright_ = false;
      size_t next_ = 0;
      std::atomic<size_t> frames_{0};
      uint32_t rebuilds_ = 0;
      xnc::FrameIdentity last_id_{};
    };

    // Recording IEncoderSession factory target: log survives session
    // deletion (the pipeline owns/deletes the sessions it creates).
    // hold=true models the GPU shape (leases + outputs gated until `open`
    // - the busy-slot coalescing test); hold=false models the CPU shape
    // (lease completed at consumption, outputs ready immediately).
    // Fix round 1 knobs (v2g, non-hold only): delay queues outputs behind
    // `delay` further submissions; swap_pairs emits each eligible pair as
    // [k+1, k] (the measured software-MFT emission reorder); swallow_seq
    // never emits that seq at all (a never-filling gap for the
    // reorder-before-publish window's skip path).
    struct SessionLog {
      struct Rec {
        xnc::FrameIdentity id{};
        bool force = false;
      };
      std::mutex mu;
      std::vector<Rec> subs;    // every submission, in order (never erased)
      std::deque<Rec> fifo;     // pending outputs (hold mode / delay queue)
      std::deque<Rec> ready;    // ready outputs (non-hold mode)
      std::atomic<size_t> submits{0};
      size_t completes = 0, double_completes = 0;
      std::atomic<size_t> outputs{0};
      size_t shutdowns = 0, shutdown_completed = 0;
      bool hold = false;
      std::atomic<bool> open{false};
      uint32_t reconf_bitrate = 0, reconf_fps = 0;
      bool reconf_ok = true;
      uint32_t delay = 0;                 // v2g: emit after N more submits
      bool swap_pairs = false;            // v2g: emit pairs [k+1, k]
      uint64_t swallow_seq = 0;           // v2g: never emit this seq
      std::atomic<size_t> hw_faults{0};   // v2m: HwFaultSession contract breaks
    };
    class LogSession final : public xnc::IEncoderSession {
     public:
      LogSession(SessionLog* log, xnc::Nv12SurfacePool* pool)
          : log_(log), pool_(pool) {}
      xnc::SubmitResult Submit(const xnc::FrameIdentity& id,
                               xnc::SurfaceLease&& lease,
                               bool force_idr) override {
        if (!lease.Submit(id.encode_seq)) {
          lease.Release();
          return xnc::SubmitResult::kRejected;
        }
        SessionLog::Rec r;
        r.id = id;
        r.force = force_idr;
        bool complete_now = false;
        {
          std::lock_guard<std::mutex> lk(log_->mu);
          log_->subs.push_back(r);
          ++log_->submits;
          if (log_->hold) {
            log_->fifo.push_back(r);
          } else if (id.encode_seq == log_->swallow_seq) {
            // Counted as submitted + lease completed, but its output is
            // never emitted: a permanent gap for the reorder window.
            complete_now = true;
          } else {
            log_->fifo.push_back(r);
            while (log_->fifo.size() > log_->delay) {
              if (log_->swap_pairs && log_->fifo.size() >= 2) {
                const SessionLog::Rec a = log_->fifo.front();
                log_->fifo.pop_front();
                const SessionLog::Rec b = log_->fifo.front();
                log_->fifo.pop_front();
                log_->ready.push_back(b);  // k+1 first, then k
                log_->ready.push_back(a);
              } else {
                log_->ready.push_back(log_->fifo.front());
                log_->fifo.pop_front();
              }
            }
            complete_now = true;
          }
        }
        if (complete_now) Complete(r.id.encode_seq);
        return xnc::SubmitResult::kOk;
      }
      bool TakeOutput(xnc::EncoderOutput* out, uint32_t) override {
        if (out == nullptr) return false;
        SessionLog::Rec r;
        bool have = false, complete = false;
        {
          std::lock_guard<std::mutex> lk(log_->mu);
          if (log_->hold) {
            if (log_->open && !log_->fifo.empty()) {
              r = log_->fifo.front();
              log_->fifo.pop_front();
              complete = true;
              have = true;
            }
          } else if (!log_->ready.empty()) {
            r = log_->ready.front();
            log_->ready.pop_front();
            have = true;
          }
          if (have) ++log_->outputs;
        }
        if (!have) return false;
        if (complete) Complete(r.id.encode_seq);
        out->id = r.id;
        out->submit_id = r.id.encode_seq;
        out->key = true;  // the fake's single NALU is an IDR (0x65)
        out->au = {0, 0, 0, 1, 0x65};
        return true;
      }
      bool Reconfigure(uint32_t bitrate, uint32_t fps) override {
        std::lock_guard<std::mutex> lk(log_->mu);
        log_->reconf_bitrate = bitrate;
        log_->reconf_fps = fps;
        return log_->reconf_ok;
      }
      void Shutdown(xnc::ShutdownMode) override {
        std::vector<uint64_t> sids;
        {
          std::lock_guard<std::mutex> lk(log_->mu);
          ++log_->shutdowns;
          while (!log_->fifo.empty()) {
            sids.push_back(log_->fifo.front().id.encode_seq);
            log_->fifo.pop_front();
            ++log_->shutdown_completed;
          }
        }
        for (const uint64_t sid : sids) Complete(sid);  // held leases (GPU shape)
      }

     private:
      void Complete(uint64_t sid) {
        if (!pool_->Complete(sid)) {
          std::lock_guard<std::mutex> lk(log_->mu);
          ++log_->double_completes;
        } else {
          std::lock_guard<std::mutex> lk(log_->mu);
          ++log_->completes;
        }
      }
      SessionLog* log_;
      xnc::Nv12SurfacePool* pool_;
    };

    // Task 5 (v2m): the HARDWARE-rung fake for the injected-contract-
    // failure tests. Honors the session contract for ok_submits
    // submissions, then breaks it (kIdentityFault, lease released) - the
    // shape of a hardware encoder MFT that stops pairing outputs 1:1. The
    // pipeline must treat that as a hardware CONTRACT FAILURE: strike the
    // process-lifetime fallback lock and route the recovery through the
    // unified reset - never an immediate fatal.
    class HwFaultSession final : public xnc::IEncoderSession {
     public:
      HwFaultSession(SessionLog* log, xnc::Nv12SurfacePool* pool,
                     size_t ok_submits)
          : log_(log), pool_(pool), ok_submits_(ok_submits) {}
      xnc::SubmitResult Submit(const xnc::FrameIdentity& id,
                               xnc::SurfaceLease&& lease,
                               bool force_idr) override {
        if (subs_ >= ok_submits_) {
          lease.Release();
          log_->hw_faults.fetch_add(1);
          return xnc::SubmitResult::kIdentityFault;
        }
        ++subs_;
        if (!lease.Submit(id.encode_seq)) {
          lease.Release();
          return xnc::SubmitResult::kRejected;
        }
        SessionLog::Rec r;
        r.id = id;
        r.force = force_idr;
        bool fresh = false;
        {
          std::lock_guard<std::mutex> lk(log_->mu);
          log_->subs.push_back(r);
          ++log_->submits;
          if (id.encode_seq != log_->swallow_seq) {
            log_->ready.push_back(r);
            fresh = true;
          }
        }
        if (fresh) Complete(id.encode_seq);
        return xnc::SubmitResult::kOk;
      }
      bool TakeOutput(xnc::EncoderOutput* out, uint32_t) override {
        if (out == nullptr) return false;
        SessionLog::Rec r;
        {
          std::lock_guard<std::mutex> lk(log_->mu);
          if (log_->ready.empty()) return false;
          r = log_->ready.front();
          log_->ready.pop_front();
          ++log_->outputs;
        }
        out->id = r.id;
        out->submit_id = r.id.encode_seq;
        out->key = true;
        out->au = {0, 0, 0, 1, 0x65};
        return true;
      }
      bool Reconfigure(uint32_t, uint32_t) override { return true; }
      void Shutdown(xnc::ShutdownMode) override {}

     private:
      void Complete(uint64_t sid) {
        if (pool_->Complete(sid)) {
          std::lock_guard<std::mutex> lk(log_->mu);
          ++log_->completes;
        }
      }
      SessionLog* log_;
      xnc::Nv12SurfacePool* pool_;
      size_t ok_submits_;
      size_t subs_ = 0;
    };

    // v2q (final-review fix 2026-08): the two-device swap probe session.
    // Records, per submission, WHICH D3D device the leased NV12 slot lives
    // on (the pool/converter device the pipeline adopted from the surface
    // snapshot) plus the max luma over a fixed sample grid of the
    // CONVERTED slot - a test-only staging readback proving the submitted
    // pixels are the live GDI content, not the stale device's frozen frame.
    // (The readback lives in this FACTORY session only; the production
    // pipeline's no-readback contract is untouched - res.cpu_readbacks
    // stays 0 for factory rungs.)
    class SwapProbeSession final : public xnc::IEncoderSession {
     public:
      struct Rec {
        uint64_t encode_seq = 0;
        uint64_t content_id = 0;
        ID3D11Device* dev = nullptr;  // raw identity; scenario-owned devices
                                      // outlive pipe.Stop()
        uint32_t bright_pts = 0;      // sampled NV12 points with luma >= 200
        bool sampled = false;         // the staging readback actually ran
        uint8_t row_profile[8] = {0}; // middle-row luma samples (the NOTE)
      };
      SwapProbeSession(std::vector<Rec>* recs, std::mutex* mu,
                       xnc::Nv12SurfacePool* pool)
          : recs_(recs), mu_(mu), pool_(pool) {}
      xnc::SubmitResult Submit(const xnc::FrameIdentity& id,
                               xnc::SurfaceLease&& lease,
                               bool /*force_idr*/) override {
        Rec r;
        r.encode_seq = id.encode_seq;
        r.content_id = id.content_id;
        ID3D11Texture2D* tex = lease.texture();
        if (tex != nullptr) {
          Microsoft::WRL::ComPtr<ID3D11Device> d;
          tex->GetDevice(d.GetAddressOf());
          r.dev = d.Get();
          r.sampled = SampleBrightPoints(tex, d.Get(), &r.bright_pts,
                                         r.row_profile);
        }
        if (!lease.Submit(id.encode_seq)) {
          lease.Release();
          return xnc::SubmitResult::kRejected;
        }
        {
          std::lock_guard<std::mutex> lk(*mu_);
          recs_->push_back(r);
          ready_.push_back(id);
        }
        return xnc::SubmitResult::kOk;
      }
      bool TakeOutput(xnc::EncoderOutput* out, uint32_t) override {
        xnc::FrameIdentity id{};
        {
          std::lock_guard<std::mutex> lk(*mu_);
          if (ready_.empty()) return false;
          id = ready_.front();
          ready_.pop_front();
        }
        pool_->Complete(id.encode_seq);
        if (out == nullptr) return true;
        out->id = id;
        out->submit_id = id.encode_seq;
        out->key = true;  // single IDR NALU, the LogSession shape
        out->au = {0, 0, 0, 1, 0x65};
        return true;
      }
      bool Reconfigure(uint32_t, uint32_t) override { return true; }
      void Shutdown(xnc::ShutdownMode) override {}

     private:
      // Staging-readback luma probe: 12 sample points (4 x positions in the
      // bars pattern's BLACK bar [w/2, 5w/8) x 3 heights), counting points
      // with NV12 luma >= 200 (studio bright). BrightSolid content -> >= 11
      // of 12 (the 4px moving stripe can cover at most ONE point - the
      // points are 13 px apart at 320 wide); bars content -> <= 1 (only a
      // stripe-covered point can be bright; the black bar itself is ~16).
      // Frozen DXGI pixels on the GDI device would read <= 1 forever.
      // Returns false when any D3D step failed (a skipped sample, never a
      // "dark" verdict - a transient staging failure must not read as
      // frozen content).
      static bool SampleBrightPoints(ID3D11Texture2D* tex, ID3D11Device* dev,
                                     uint32_t* out,
                                     uint8_t* row_profile = nullptr) {
        *out = 0;
        D3D11_TEXTURE2D_DESC td{};
        tex->GetDesc(&td);
        if (td.Format != DXGI_FORMAT_NV12) return false;
        D3D11_TEXTURE2D_DESC sd = td;
        sd.Usage = D3D11_USAGE_STAGING;
        sd.BindFlags = 0;
        sd.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
        sd.MiscFlags = 0;
        Microsoft::WRL::ComPtr<ID3D11Texture2D> stage;
        if (FAILED(dev->CreateTexture2D(&sd, nullptr, &stage))) return false;
        Microsoft::WRL::ComPtr<ID3D11DeviceContext> ctx;
        dev->GetImmediateContext(ctx.GetAddressOf());
        ctx->CopyResource(stage.Get(), tex);
        D3D11_MAPPED_SUBRESOURCE m{};
        if (FAILED(ctx->Map(stage.Get(), 0, D3D11_MAP_READ, 0, &m)))
          return false;
        static const double kXs[4] = {0.52, 0.56, 0.60, 0.615};
        static const double kYs[3] = {0.35, 0.50, 0.65};
        uint32_t bright = 0;
        const uint8_t* y_plane = static_cast<const uint8_t*>(m.pData);
        for (double fy : kYs)
          for (double fx : kXs) {
            const UINT x = static_cast<UINT>(fx * td.Width) % td.Width;
            const UINT y = static_cast<UINT>(fy * td.Height) % td.Height;
            if (y_plane[(size_t)y * m.RowPitch + x] >= 200) ++bright;
          }
        // Middle-row luma samples (8 evenly spread x positions) for the
        // scenario NOTE - the human-readable content fingerprint (bars
        // [81 145 41 235 16 ...] vs bright [235 ...]).
        if (row_profile != nullptr && td.Width >= 8) {
          const UINT y0 = td.Height / 2;
          for (UINT i = 0; i < 8; ++i) {
            const UINT xx = (td.Width / 8) * i + td.Width / 16;
            row_profile[i] = y_plane[(size_t)y0 * m.RowPitch + xx];
          }
        }
        ctx->Unmap(stage.Get(), 0);
        *out = bright;
        return true;
      }
      std::vector<Rec>* recs_;
      std::mutex* mu_;
      xnc::Nv12SurfacePool* pool_;
      std::deque<xnc::FrameIdentity> ready_;  // guarded by *mu_
    };

    // Recording AuSink: immutable AUs + state codes + display changes.
    class V2RecordingSink final : public xnc::AuSink {
     public:
      const char* OnAu(const xnc::EncodedAU& au) override {
        std::lock_guard<std::mutex> lk(mu);
        aus.push_back(au);
        return nullptr;
      }
      const char* PendingIdrReason() override {
        std::lock_guard<std::mutex> lk(mu);
        return pending.empty() ? nullptr : pending.c_str();
      }
      void ConsumePendingIdr(const char* reason) override {
        std::lock_guard<std::mutex> lk(mu);
        consumed.emplace_back(reason != nullptr ? reason : "?");
      }
      void OnState(const char* code, bool recoverable) override {
        std::lock_guard<std::mutex> lk(mu);
        states.emplace_back(code != nullptr ? code : "?", recoverable);
      }
      void OnDisplayChanged(uint32_t w, uint32_t h, const char*) override {
        std::lock_guard<std::mutex> lk(mu);
        ++display_changes;
        last_w = w;
        last_h = h;
      }
      bool HasState(const char* code) const {
        std::lock_guard<std::mutex> lk(mu);
        for (const auto& s : states)
          if (s.first == code) return true;
        return false;
      }
      std::vector<xnc::EncodedAU> CopyAus() const {
        std::lock_guard<std::mutex> lk(mu);
        return aus;
      }
      std::vector<xnc::FrameIdentity> CopyIds() const {
        std::lock_guard<std::mutex> lk(mu);
        std::vector<xnc::FrameIdentity> ids;
        ids.reserve(aus.size());
        for (const auto& a : aus) ids.push_back(a.id);
        return ids;
      }
      mutable std::mutex mu;
      std::vector<xnc::EncodedAU> aus;
      std::vector<std::pair<std::string, bool>> states;
      std::vector<std::string> consumed;
      std::string pending;  // set by tests wanting the merged-IDR poll
      uint32_t display_changes = 0, last_w = 0, last_h = 0;
    };

    // Shared device (hardware -> WARP; the Task 1/3 selftest pattern).
    UINT v2_flags =
        D3D11_CREATE_DEVICE_VIDEO_SUPPORT | D3D11_CREATE_DEVICE_BGRA_SUPPORT;
    Microsoft::WRL::ComPtr<ID3D11Device> v2_dev;
    Microsoft::WRL::ComPtr<ID3D11DeviceContext> v2_ctx;
    D3D_FEATURE_LEVEL v2_fl{};
    const char* v2_via = "hardware";
    auto v2_try = [&v2_dev, &v2_ctx, &v2_fl](UINT f, D3D_DRIVER_TYPE dt) {
      v2_dev.Reset();
      v2_ctx.Reset();
      return D3D11CreateDevice(nullptr, dt, nullptr, f, nullptr, 0,
                               D3D11_SDK_VERSION, &v2_dev, &v2_fl, &v2_ctx);
    };
    HRESULT v2_hr = v2_try(v2_flags, D3D_DRIVER_TYPE_HARDWARE);
    if (FAILED(v2_hr)) {
      v2_via = "warp";
      v2_hr = v2_try(v2_flags, D3D_DRIVER_TYPE_WARP);
    }
    CHECK("v2-device", SUCCEEDED(v2_hr));
    if (SUCCEEDED(v2_hr)) {
      std::printf("SELFTEST NOTE: v2 scenarios device driver=%s\n", v2_via);

      // ---- (A) the mailbox itself: depth-one coalescing + sticky-once ----
      {
        xnc::MediaMailbox mb;
        CHECK("v2a-empty", !mb.HasContent() && !mb.idr_armed() &&
                               !mb.reset_pending());
        // Coalescing: publish 1, 2, 3 while "slots are busy" - only 3 left.
        mb.PublishContent(xnc::FrameIdentity{1, 1, 1, 0, 10, 0});
        mb.PublishContent(xnc::FrameIdentity{1, 1, 2, 0, 20, 0});
        mb.PublishContent(xnc::FrameIdentity{1, 1, 3, 0, 30, 0});
        xnc::FrameIdentity got{};
        CHECK("v2a-coalesce-latest",
              mb.TakeContent(&got) && got.content_id == 3 &&
                  got.source_mono_us == 30);
        CHECK("v2a-coalesce-drained", !mb.TakeContent(&got) && !mb.HasContent());
        // Sticky IDR: arm during the wait, consumed exactly once by the
        // next submission.
        char reason[32] = {0};
        mb.ArmIdr("coalesce-test");
        CHECK("v2a-idr-armed", mb.idr_armed());
        CHECK("v2a-idr-sticky-across-takes",
              mb.TakeContent(&got) == false && mb.idr_armed());
        CHECK("v2a-idr-take-once",
              mb.TakeIdr(reason, sizeof(reason)) &&
                  std::strcmp(reason, "coalesce-test") == 0);
        CHECK("v2a-idr-take-twice-fails", !mb.TakeIdr(reason, sizeof(reason)));
        // Reconfigure: depth one, latest wins.
        mb.RequestReconfigure(1500000, 24);
        mb.RequestReconfigure(900000, 15);
        uint32_t rb = 0, rf = 0;
        CHECK("v2a-reconf-latest",
              mb.TakeReconfigure(&rb, &rf) && rb == 900000 && rf == 15);
        CHECK("v2a-reconf-drained", !mb.TakeReconfigure(&rb, &rf));
        // Reset: depth one.
        mb.RequestReset("desktop_switch");
        char rr[32] = {0};
        CHECK("v2a-reset-take",
              mb.TakeReset(rr, sizeof(rr)) &&
                  std::strcmp(rr, "desktop_switch") == 0);
        CHECK("v2a-reset-drained", !mb.TakeReset(rr, sizeof(rr)));
      }

      // ---- (B) the brief's binding test at the REAL loop: publish
      // contentIds 1,2,3 while all three NV12 slots are busy; release one
      // slot and assert ONLY contentId 3 is submitted; an IDR armed during
      // the wait applies exactly once to that submission. ----
      {
        const uint32_t w = 320, h = 240, frames = 6;
        std::vector<ScriptedDeviceCapture::Step> script(
            frames, ScriptedDeviceCapture::Step::kFrame);
        ScriptedDeviceCapture cap(v2_dev.Get(), v2_ctx.Get(), script, w, h);
        SessionLog log;
        log.hold = true;  // leases + outputs held until the gate opens
        V2RecordingSink sink;
        xnc::MediaPipelineV2::Config cfg;
        cfg.cap = &cap;
        cfg.surf = &cap;
        cfg.sink = &sink;
        cfg.fps = 60;  // spf 16ms: submissions outpace nothing (slots gate)
        cfg.duration_s = 30;
        cfg.session_factory = [](void* ctx, xnc::Nv12SurfacePool* pool) ->
            xnc::IEncoderSession* {
              return new LogSession(static_cast<SessionLog*>(ctx), pool);
        };
        cfg.session_ctx = &log;
        xnc::MediaPipelineV2 pipe;
        CHECK("v2b-start", pipe.Start(cfg));
        if (pipe.running()) {
          // Wait until the script is fully captured and 3 submissions
          // exhausted the pool.
          CHECK("v2b-busy-reached",
                wait_for([&] { return log.submits >= 3 && cap.frames() == frames; },
                         5000));
          Sleep(150);  // settle: nothing more may be submitted while busy
          CHECK("v2b-slots-bound-at-three", log.submits == 3);
          // Arm the sticky IDR during the wait.
          pipe.RequestIdr("coalesce-test");
          Sleep(100);
          CHECK("v2b-idr-held-while-busy", log.submits == 3);
          // Release ONE slot (the gate): the loop's output collection
          // completes one lease, freeing one slot.
          log.open.store(true);
          CHECK("v2b-next-submit-arrives",
                wait_for([&] { return log.submits >= 4; }, 5000));
          const std::vector<SessionLog::Rec> subs = [&] {
            std::lock_guard<std::mutex> lk(log.mu);
            return log.subs;
          }();
          CHECK("v2b-submission-count", subs.size() == 4);
          // The binding assertion: contentIds 1 and 2 (script frames 4/5)
          // were coalesced away - only frame 6 (contentId 6) submitted.
          bool saw_4 = false, saw_5 = false;
          for (const auto& s : subs) {
            if (s.id.content_id == 4) saw_4 = true;
            if (s.id.content_id == 5) saw_5 = true;
          }
          CHECK("v2b-only-latest-content",
                !saw_4 && !saw_5 && subs[3].id.content_id == 6);
          // Sticky-once: the base IDR (submission 0) + the armed IDR
          // (submission 3) and nothing else.
          size_t forces = 0;
          for (const auto& s : subs)
            if (s.force) ++forces;
          CHECK("v2b-sticky-idr-once",
                forces == 2 && subs[0].force && subs[3].force &&
                    !subs[1].force && !subs[2].force);
          // present_mono_us stamped at submission, source at capture.
          CHECK("v2b-identity-stamps",
                subs[3].id.present_mono_us != 0 &&
                    subs[3].id.source_mono_us != 0 &&
                    subs[3].id.encode_seq > subs[2].id.encode_seq);
          // The released output published through the sink as an IDR AU.
          CHECK("v2b-au-published",
                wait_for([&] { return sink.CopyAus().size() >= 1; }, 5000));
          const auto aus_b = sink.CopyAus();
          CHECK("v2b-au-key", !aus_b.empty() && (aus_b[0].flags &
                                                xnc::AuFlags::kAuFlagKey) != 0);
          const xnc::MediaPipelineV2::Result res = pipe.Stop();
          CHECK("v2b-ok", res.ok);
          CHECK("v2b-captured", res.captured == frames);
          CHECK("v2b-leases-released",
                [&] {
                  std::lock_guard<std::mutex> lk(log.mu);
                  return log.completes == log.submits &&
                         log.double_completes == 0;
                }());
          // Task 6: the run recorded every stage histogram into the final
          // summary (sidecar block) - keys present, percentiles monotone -
          // and the factory session performed no CPU readbacks (the
          // readback counter only moves on the internal software rung).
          CHECK("v2b-stages-json",
                res.stages_json.find("gpu_copy_us") != std::string::npos &&
                    res.stages_json.find("gpu_convert_us") !=
                        std::string::npos &&
                    res.stages_json.find("mft_submit_to_output_us") !=
                        std::string::npos &&
                    res.stages_json.find("inflight_slots") !=
                        std::string::npos &&
                    res.stages_json.find("queue_age_us") != std::string::npos &&
                    res.stages_json.find("capture_to_au_us") !=
                        std::string::npos);
          CHECK("v2b-no-cpu-readback", res.cpu_readbacks == 0);
          // Fix round 1: the durable block carries the semantics + the
          // serving rung's label (the factory session ran the "factory"
          // rung here).
          CHECK("v2b-stages-labeled",
                res.stages_json.find("\"stage_semantics\": ") !=
                    std::string::npos &&
                    res.stages_json.find("\"encoder_backend\": \"factory\"") !=
                        std::string::npos);
          std::printf("SELFTEST NOTE: v2b submits=%llu outputs=%llu aus=%llu\n",
                      (unsigned long long)log.submits,
                      (unsigned long long)log.outputs,
                      (unsigned long long)aus_b.size());
        }
      }

      // ---- (D) the reset path: an injected kAccessLost mid-run executes
      // the full reset sequence (state events, epoch advance, new base
      // IDR of the new epoch, monotonic identity throughout). ----
      {
        const uint32_t w = 320, h = 240;
        std::vector<ScriptedDeviceCapture::Step> script;
        for (int i = 0; i < 8; ++i) script.push_back(ScriptedDeviceCapture::Step::kFrame);
        script.push_back(ScriptedDeviceCapture::Step::kAccessLost);
        for (int i = 0; i < 8; ++i) script.push_back(ScriptedDeviceCapture::Step::kFrame);
        ScriptedDeviceCapture cap(v2_dev.Get(), v2_ctx.Get(), script, w, h);
        SessionLog log;
        log.hold = false;
        V2RecordingSink sink;
        xnc::MediaPipelineV2::Config cfg;
        cfg.cap = &cap;
        cfg.surf = &cap;
        cfg.sink = &sink;
        cfg.fps = 60;
        cfg.duration_s = 30;
        cfg.session_factory = [](void* ctx, xnc::Nv12SurfacePool* pool) ->
            xnc::IEncoderSession* {
              return new LogSession(static_cast<SessionLog*>(ctx), pool);
        };
        cfg.session_ctx = &log;
        xnc::MediaPipelineV2 pipe;
        CHECK("v2d-start", pipe.Start(cfg));
        if (pipe.running()) {
          CHECK("v2d-epoch2-published",
                wait_for([&] {
                  for (const auto& a : sink.CopyAus())
                    if (a.id.capture_epoch == 2) return true;
                  return false;
                }, 8000));
          const xnc::MediaPipelineV2::Result res = pipe.Stop();
          CHECK("v2d-ok", res.ok);
          CHECK("v2d-resets", res.resets == 1);
          CHECK("v2d-rebuild-called", cap.rebuilds() == 1);
          CHECK("v2d-states", sink.HasState("recovering") &&
                                  sink.HasState("capture_rebuilt") &&
                                  sink.HasState("stream_end"));
          const auto aus_d = sink.CopyAus();
          bool saw_e1 = false, saw_e2 = false, e2_first_key = true,
               e2_seen = false;
          for (const auto& a : aus_d) {
            if (a.id.capture_epoch == 1) saw_e1 = true;
            if (a.id.capture_epoch == 2) {
              if (!e2_seen) e2_first_key = (a.flags & xnc::AuFlags::kAuFlagKey) != 0;
              e2_seen = true;
              saw_e2 = true;
            }
          }
          CHECK("v2d-epoch-advance", saw_e1 && saw_e2);
          CHECK("v2d-new-epoch-idr-first", e2_seen && e2_first_key);
          CHECK("v2d-identity-monotonic",
                DeliveredIdentitiesValid(sink.CopyIds()));
          // The test stops as soon as the epoch-2 AUs surface, so only the
          // pre-reset frames plus the first post-reset frames ran.
          CHECK("v2d-captured", res.captured >= 10 && res.captured <= 16);
          std::printf("SELFTEST NOTE: v2d aus=%llu resets=%u rebuilds=%u\n",
                      (unsigned long long)aus_d.size(), res.resets,
                      cap.rebuilds());
        }
      }

      // ---- (C) end-to-end on the REAL internal ladder: real
      // VideoProcessor BGRA->NV12 (+ --max-w scaling) + real encoder
      // session (hardware-first, CPU fallback - under RDP the documented
      // rung is software), decoded-pixel determinism across a pristine
      // second run. ----
      // Fix round 1: the v2 wire's delivery contract is STRICT delivery-
      // order monotonicity (what the Go client's frameLedger enforces on
      // every 0x0205 frame). The pipeline's reorder-before-publish window
      // restores it despite the software rung's reordered emissions, so
      // the strict DeliveredIdentitiesValid replay is asserted everywhere
      // below (there is exactly ONE definition of the contract).
      auto run_v2_e2e = [&](ScriptedDeviceCapture& cap, V2RecordingSink& sink,
                            uint32_t max_w, uint32_t fps, uint32_t duration) ->
          xnc::MediaPipelineV2::Result {
        xnc::MediaPipelineV2::Config cfg;
        cfg.cap = &cap;
        cfg.surf = &cap;
        cfg.sink = &sink;
        cfg.fps = fps;
        cfg.duration_s = duration;
        cfg.max_width = max_w;
        xnc::MediaPipelineV2 pipe;
        if (!pipe.Start(cfg)) {
          xnc::MediaPipelineV2::Result bad;
          bad.ok = false;
          bad.err = "start failed: " + pipe.start_error();
          return bad;
        }
        while (pipe.running()) Sleep(100);
        return pipe.Stop();
      };
      {
        const uint32_t w = 640, h = 480, frames = 48;
        std::vector<ScriptedDeviceCapture::Step> script(
            frames, ScriptedDeviceCapture::Step::kFrame);
        ScriptedDeviceCapture cap1(v2_dev.Get(), v2_ctx.Get(), script, w, h);
        V2RecordingSink sink1;
        const xnc::MediaPipelineV2::Result r1 =
            run_v2_e2e(cap1, sink1, 320, 30, 5);
        if (!r1.ok)
          std::printf("SELFTEST NOTE: v2c run1 err=%s\n", r1.err.c_str());
        CHECK("v2c-ok", r1.ok);
        CHECK("v2c-dims", r1.width == 320 && r1.height == 240);
        CHECK("v2c-aus", r1.aus_written >= 8);
        CHECK("v2c-keys", r1.keyframes >= 1);
        const auto ids_c = sink1.CopyIds();
        CHECK("v2c-identity-monotonic", DeliveredIdentitiesValid(ids_c));
        std::printf("SELFTEST NOTE: v2c delivery strictly monotonic n=%zu "
                    "(reorder window: gap_skips=%llu late_drops=%llu)\n",
                    ids_c.size(),
                    (unsigned long long)r1.reorder_gap_skips,
                    (unsigned long long)r1.reorder_late_drops);
        const auto aus_c = sink1.CopyAus();
        // Stream contract: 4-byte start codes everywhere; the first key AU
        // is SPS/PPS-prefixed (7/8 before the IDR 5).
        bool shaped_ok = !aus_c.empty(), first_key_ok = false, saw_key = false;
        for (const auto& a : aus_c) {
          const std::vector<uint8_t>& p = *a.annexb;
          if (p.size() < 4 || p[0] || p[1] || p[2] || p[3] != 1) shaped_ok = false;
          if (!saw_key && (a.flags & xnc::AuFlags::kAuFlagKey) != 0) {
            saw_key = true;
            first_key_ok = xnc::NalHasType(p.data(), p.size(), 7) &&
                           xnc::NalHasType(p.data(), p.size(), 8) &&
                           xnc::NalHasType(p.data(), p.size(), 5);
          }
        }
        CHECK("v2c-shaped-4byte-startcodes", shaped_ok);
        CHECK("v2c-first-key-spspps-idr", saw_key && first_key_ok);
        std::printf("SELFTEST NOTE: v2c rung=%s friendly=\"%s\" w=%u h=%u "
                    "aus=%llu keys=%llu captured=%llu encoded=%llu feeds=%llu\n",
                    r1.encoder_backend, r1.encoder_friendly.c_str(), r1.width,
                    r1.height, (unsigned long long)r1.aus_written,
                    (unsigned long long)r1.keyframes,
                    (unsigned long long)r1.captured,
                    (unsigned long long)r1.encoded,
                    (unsigned long long)r1.warmup_feeds);
        // Pristine second run: the first IDR AU decodes to the SAME luma
        // hash (deterministic pixels through the whole V2 chain).
        ScriptedDeviceCapture cap2(v2_dev.Get(), v2_ctx.Get(), script, w, h);
        V2RecordingSink sink2;
        const xnc::MediaPipelineV2::Result r2 =
            run_v2_e2e(cap2, sink2, 320, 30, 5);
        CHECK("v2c-run2-ok", r2.ok);
        const auto aus_2 = sink2.CopyAus();
        const auto first_key = [](const std::vector<xnc::EncodedAU>& aus) ->
            std::shared_ptr<const std::vector<uint8_t>> {
          for (const auto& a : aus)
            if ((a.flags & xnc::AuFlags::kAuFlagKey) != 0) return a.annexb;
          return nullptr;
        };
        auto fk1 = first_key(aus_c);
        auto fk2 = first_key(aus_2);
        CHECK("v2c-run2-first-key", fk1 != nullptr && fk2 != nullptr);
        if (fk1 != nullptr && fk2 != nullptr) {
          uint64_t hash1 = 0, hash2 = 0;
          std::string derr1, derr2;
          const bool dec1 = xnc::DecodeAnnexBToLumaHash(*fk1, &hash1, &derr1);
          const bool dec2 = xnc::DecodeAnnexBToLumaHash(*fk2, &hash2, &derr2);
          if (!dec1 || !dec2)
            std::printf("SELFTEST NOTE: v2c decode err1=%s err2=%s\n",
                        derr1.c_str(), derr2.c_str());
          CHECK("v2c-decode-first-idr", dec1 && dec2);
          CHECK("v2c-decode-deterministic", hash1 == hash2 && hash1 != 0);
          std::printf("SELFTEST NOTE: v2c first-IDR luma hash=%016llx\n",
                      (unsigned long long)hash1);
        }
      }

      // ---- (E) the GDI + CPU fallback rung end-to-end (ruling 5): the
      // real GdiCapture (BitBlt -> CPU upload -> LatestSurface) feeding
      // the real internal session ladder. ----
      {
        std::string gerr;
        std::unique_ptr<xnc::ICapture> gcap = xnc::TryCreateGdiCapture(&gerr);
        if (gcap == nullptr) {
          std::printf("SELFTEST NOTE: v2e GDI capture unavailable err=\"%s\" "
                      "- scenario SKIPPED (the CPU-shape coverage is v2c)\n",
                      gerr.c_str());
          CHECK("v2e-skip-reason", !gerr.empty());
        } else {
          auto* gsurf = dynamic_cast<xnc::ICaptureSurface*>(gcap.get());
          CHECK("v2e-surface-iface", gsurf != nullptr);
          if (gsurf != nullptr) {
            V2RecordingSink sink;
            xnc::MediaPipelineV2::Config cfg;
            cfg.cap = gcap.get();
            cfg.surf = gsurf;
            cfg.sink = &sink;
            cfg.fps = 60;  // submissions outpace the GDI BitBlt cadence so
                           // the 2s warm-up wall bound fits enough inputs
            cfg.duration_s = 10;
            xnc::MediaPipelineV2 pipe;
            CHECK("v2e-start", pipe.Start(cfg));
            if (pipe.running()) {
              while (pipe.running()) Sleep(100);
              const xnc::MediaPipelineV2::Result res = pipe.Stop();
              if (!res.ok)
                std::printf("SELFTEST NOTE: v2e err=%s\n", res.err.c_str());
              CHECK("v2e-ok", res.ok);
              CHECK("v2e-captured", res.captured >= 1);
              CHECK("v2e-encoded", res.encoded >= 1);
              if (res.keyframes == 0)
                std::printf("SELFTEST NOTE: v2e static screen starved the "
                            "2s warm-up bound (encoded=%llu, no IDR; the "
                            "pixel proof is v2c's decoded determinism)\n",
                            (unsigned long long)res.encoded);
              CHECK("v2e-keys", res.keyframes >= 1 || res.encoded >= 10);
              CHECK("v2e-identity-monotonic",
                    DeliveredIdentitiesValid(sink.CopyIds()));
              std::printf("SELFTEST NOTE: v2e rung=%s friendly=\"%s\" "
                          "w=%u h=%u aus=%llu keys=%llu feeds=%llu\n",
                          res.encoder_backend, res.encoder_friendly.c_str(),
                          res.width, res.height,
                          (unsigned long long)res.aus_written,
                          (unsigned long long)res.keyframes,
                          (unsigned long long)res.warmup_feeds);
            }
          }
        }
      }

      // ---- (F) identical publication to the v2 wire on the REAL software
      // rung (fix round 1: no fake session anymore): MediaPipelineV2
      // driving the REAL RtServer (HOST_HELLO media_protocol=2 + validated
      // 0x0205 frames) over a real pipe with a fake viewer, asserting the
      // STRICT delivery-order monotonicity the Go client's frameLedger
      // enforces - through PushAuV2, at the real encoder's reordered
      // emission order. ----
      {
        const uint32_t w = 64, h = 48, frames = 90;
        std::vector<ScriptedDeviceCapture::Step> script(
            frames, ScriptedDeviceCapture::Step::kFrame);
        ScriptedDeviceCapture cap(v2_dev.Get(), v2_ctx.Get(), script, w, h);
        xnc::RtServer rt;
        xnc::RtServer::Opts ro;
        ro.pipe_name = RtPipeNameOf(15);  // the name table has 16 slots
        ro.secret = kRtSecret;
        ro.secret_len = sizeof(kRtSecret);
        ro.max_subs = 4;
        ro.fps = 15;
        ro.bitrate_bps = 500000;
        ro.sddl_override = L"D:P(A;;GA;;;WD)";  // TEST-ONLY permissive DACL
        ro.pipeline_v2 = true;  // ruling 1: v2 pipeline + v2 wire together
        CHECK("v2f-rt-start", rt.Start(ro, w, h));
        xnc::MediaPipelineV2::Config cfg;
        cfg.cap = &cap;
        cfg.surf = &cap;
        cfg.sink = &rt;
        cfg.fps = 15;
        cfg.duration_s = 60;  // safety bound; the test Stops the pipe
        xnc::MediaPipelineV2 pipe;
        CHECK("v2f-start", pipe.Start(cfg));
        if (pipe.running()) {
          RtTestClient a;
          CHECK("v2f-connect",
                a.Connect(ro.pipe_name.c_str(), kRtSecret, sizeof(kRtSecret)));
          CHECK("v2f-attach-hello", a.Attach(12));
          a.Pump(9000, [&a] { return a.keys_ >= 1 && a.v2_frames_ >= 20; });
          const xnc::MediaPipelineV2::Result res = pipe.Stop();
          a.Pump(1500);  // drain in-flight frames + stream_end
          rt.Shutdown();
          CHECK("v2f-hello-v2", a.hello_ok_ &&
                                   a.media_protocol_ == xnc::kMediaProtocolV2);
          CHECK("v2f-v2-frames", a.v2_frames_ >= 1);
          CHECK("v2f-keys", a.keys_ >= 1);
          CHECK("v2f-stream-end", a.saw_stream_end_);
          CHECK("v2f-identity-monotonic", DeliveredIdentitiesValid(a.v2_ids_));
          CHECK("v2f-pipeline-ok", res.ok);
          std::printf("SELFTEST NOTE: v2f REAL rung=%s frames=%llu v2=%llu "
                      "keys=%llu aus=%llu gap_skips=%llu late_drops=%llu\n",
                      res.encoder_backend,
                      (unsigned long long)a.frames_,
                      (unsigned long long)a.v2_frames_,
                      (unsigned long long)a.keys_,
                      (unsigned long long)res.aus_written,
                      (unsigned long long)res.reorder_gap_skips,
                      (unsigned long long)res.reorder_late_drops);
        }
      }

      // ---- (G) the reorder-before-publish window, deterministically: a
      // fake session emits swapped pairs ([k+1, k] - the measured software
      // -MFT emission shape) and swallows seq 3 entirely (a never-filling
      // gap). The wire must stay STRICTLY monotonic: swapped pairs
      // re-sequenced, the permanent gap skipped + counted + recovered by
      // a sticky IDR that forces a later submission. ----
      {
        const uint32_t w = 320, h = 240, frames = 120;
        std::vector<ScriptedDeviceCapture::Step> script(
            frames, ScriptedDeviceCapture::Step::kFrame);
        ScriptedDeviceCapture cap(v2_dev.Get(), v2_ctx.Get(), script, w, h);
        SessionLog log;
        log.hold = false;
        log.delay = 1;        // emit pairs two submissions apart
        log.swap_pairs = true;  // emission order [k+1, k]
        log.swallow_seq = 3;    // never emitted: the permanent gap
        V2RecordingSink sink;
        xnc::MediaPipelineV2::Config cfg;
        cfg.cap = &cap;
        cfg.surf = &cap;
        cfg.sink = &sink;
        cfg.fps = 60;
        cfg.duration_s = 60;  // safety bound
        cfg.session_factory = [](void* ctx, xnc::Nv12SurfacePool* pool) ->
            xnc::IEncoderSession* {
              return new LogSession(static_cast<SessionLog*>(ctx), pool);
        };
        cfg.session_ctx = &log;
        xnc::MediaPipelineV2 pipe;
        CHECK("v2g-start", pipe.Start(cfg));
        if (pipe.running()) {
          // Wait until the gap was skipped, the post-gap run published AND
          // the recovery IDR actually forced a later submission (the flush
          // fires on the window cap or the hold timeout; the sticky is
          // consumed by the NEXT submission after it).
          CHECK("v2g-gap-skipped",
                wait_for([&] {
                  const auto ids = sink.CopyIds();
                  if (ids.size() < 8 || ids.back().encode_seq < 10)
                    return false;
                  std::lock_guard<std::mutex> lk(log.mu);
                  for (const auto& s : log.subs)
                    if (s.force && s.id.encode_seq > 3) return true;
                  return false;
                }, 8000));
          const xnc::MediaPipelineV2::Result res = pipe.Stop();
          CHECK("v2g-ok", res.ok);
          const auto ids = sink.CopyIds();
          // Strict delivery-order monotonicity - the one wire contract.
          CHECK("v2g-identity-monotonic", DeliveredIdentitiesValid(ids));
          // The swallowed seq never reached the wire...
          bool saw_3 = false;
          for (const auto& id : ids)
            if (id.encode_seq == 3) saw_3 = true;
          CHECK("v2g-gap-not-on-wire", !saw_3);
          // ...but everything around it did, in order.
          CHECK("v2g-published-past-gap", ids.size() >= 8 &&
                                              ids.front().encode_seq == 1 &&
                                              ids.back().encode_seq >= 10);
          CHECK("v2g-gap-accounted", res.reorder_gap_skips >= 1);
          // The gap incident armed a sticky IDR that forced a LATER
          // submission (the recovery contract).
          std::vector<SessionLog::Rec> subs;
          {
            std::lock_guard<std::mutex> lk(log.mu);
            subs = log.subs;
          }
          bool forced_after_gap = false;
          for (const auto& s : subs)
            if (s.force && s.id.encode_seq > 3) forced_after_gap = true;
          CHECK("v2g-gap-idr-forced", forced_after_gap);
          std::printf("SELFTEST NOTE: v2g aus=%llu gap_skips=%llu "
                      "late_drops=%llu forced_after_gap=%d\n",
                      (unsigned long long)ids.size(),
                      (unsigned long long)res.reorder_gap_skips,
                      (unsigned long long)res.reorder_late_drops,
                      forced_after_gap ? 1 : 0);
        }
      }

      // ---- (H) finding 2 end-to-end: ServeV2 with --max-w - the
      // HOST_HELLO carries the SCALED stream dims, the encoder runs at
      // them, and the REAL software rung's delivery stays strictly
      // monotonic through the full Serve path. ----
      {
        const uint32_t w = 640, h = 480, frames = 90;
        std::vector<ScriptedDeviceCapture::Step> script(
            frames, ScriptedDeviceCapture::Step::kFrame);
        ScriptedDeviceCapture cap(v2_dev.Get(), v2_ctx.Get(), script, w, h);
        xnc::RtServer server;
        xnc::RtServer::Opts ro;
        ro.pipe_name = RtPipeNameOf(14);  // the name table has 16 slots
        ro.secret = kRtSecret;
        ro.secret_len = sizeof(kRtSecret);
        ro.max_subs = 4;
        ro.fps = 15;
        ro.bitrate_bps = 500000;
        ro.sddl_override = L"D:P(A;;GA;;;WD)";  // TEST-ONLY permissive DACL
        ro.pipeline_v2 = true;
        int rc = 1;
        std::thread serve_th([&] { rc = server.ServeV2(cap, cap, ro, 320); });
        RtTestClient a;
        CHECK("v2h-connect",
              a.Connect(ro.pipe_name.c_str(), kRtSecret, sizeof(kRtSecret)));
        CHECK("v2h-attach-hello", a.Attach(13));
        // HOST_HELLO must carry the SCALED dims (320x240), not the
        // capture's native 640x480.
        CHECK("v2h-hello-scaled-dims",
              a.hello_ok_ && a.hello_.w == 320 && a.hello_.h == 240);
        a.Pump(9000, [&a] { return a.keys_ >= 1 && a.v2_frames_ >= 8; });
        server.RequestStop();
        serve_th.join();
        a.Pump(1500);  // drain + stream_end
        server.Shutdown();
        CHECK("v2h-v2-frames", a.v2_frames_ >= 1);
        CHECK("v2h-frame-dims-scaled",
              a.first_key_w_ == 320 && a.first_key_h_ == 240);
        CHECK("v2h-keys", a.keys_ >= 1);
        CHECK("v2h-identity-monotonic", DeliveredIdentitiesValid(a.v2_ids_));
        CHECK("v2h-serve-ok", rc == 0);
        std::printf("SELFTEST NOTE: v2h hello=%ux%u first_key=%ux%u "
                    "frames=%llu v2=%llu keys=%llu rc=%d\n",
                    a.hello_.w, a.hello_.h, a.first_key_w_, a.first_key_h_,
                    (unsigned long long)a.frames_,
                    (unsigned long long)a.v2_frames_,
                    (unsigned long long)a.keys_, rc);
      }

      // ---- (I) M2 Task 5 pure decision logic (device-free): the severity
      // order, the mailbox's severity coalescing, the storm-backoff curve,
      // the 3-strike hardware lock, the reset-phase vocabulary and the DXGI
      // probe rule shared with the ladder. ----
      {
        // Ruling 2 severity: device-removed > desktop/display change >
        // access-lost > encoder (unknown lowest).
        CHECK("v2i-severity-order",
              xnc::ResetSeverity(xnc::kResetReasonDeviceRemoved) >
                  xnc::ResetSeverity(xnc::kResetReasonDesktopSwitch) &&
              xnc::ResetSeverity(xnc::kResetReasonDesktopSwitch) >
                  xnc::ResetSeverity(xnc::kResetReasonResolution) &&
              xnc::ResetSeverity(xnc::kResetReasonSwitch) ==
                  xnc::ResetSeverity(xnc::kResetReasonResolution) &&
              xnc::ResetSeverity(xnc::kResetReasonResolution) >
                  xnc::ResetSeverity(xnc::kResetReasonAccessLost) &&
              xnc::ResetSeverity(xnc::kResetReasonAccessLost) >
                  xnc::ResetSeverity(xnc::kResetReasonEncoder) &&
              xnc::ResetSeverity("manual") == 0 &&
              xnc::ResetSeverity(nullptr) == 0);
        // Mailbox coalescing: a pending reason is superseded only by a
        // HIGHER severity (ties: latest wins) - same-cycle reasons merge
        // into ONE reset carrying the winner.
        xnc::MediaMailbox mb;
        char rr[32] = {0};
        mb.RequestReset(xnc::kResetReasonAccessLost);
        mb.RequestReset(xnc::kResetReasonEncoder);  // lower: displaced? no
        CHECK("v2i-mailbox-keep-higher",
              mb.TakeReset(rr, sizeof(rr)) &&
                  std::strcmp(rr, xnc::kResetReasonAccessLost) == 0);
        mb.RequestReset(xnc::kResetReasonAccessLost);
        mb.RequestReset(xnc::kResetReasonDesktopSwitch);  // higher: wins
        CHECK("v2i-mailbox-raise",
              mb.TakeReset(rr, sizeof(rr)) &&
                  std::strcmp(rr, xnc::kResetReasonDesktopSwitch) == 0);
        mb.RequestReset(xnc::kResetReasonResolution);
        mb.RequestReset(xnc::kResetReasonDeviceRemoved);  // top severity
        mb.RequestReset(xnc::kResetReasonDesktopSwitch);  // lower: ignored
        CHECK("v2i-mailbox-top",
              mb.TakeReset(rr, sizeof(rr)) &&
                  std::strcmp(rr, xnc::kResetReasonDeviceRemoved) == 0);
        CHECK("v2i-mailbox-drained", !mb.TakeReset(rr, sizeof(rr)));
        // Storm backoff (ruling 2): first reset for a reason is free,
        // repeats space out base doubling, capped.
        CHECK("v2i-storm-backoff",
              xnc::ResetStormBackoffMs(500, 4000, 0) == 0 &&
              xnc::ResetStormBackoffMs(500, 4000, 1) == 0 &&
              xnc::ResetStormBackoffMs(500, 4000, 2) == 500 &&
              xnc::ResetStormBackoffMs(500, 4000, 3) == 1000 &&
              xnc::ResetStormBackoffMs(500, 4000, 4) == 2000 &&
              xnc::ResetStormBackoffMs(500, 4000, 5) == 4000 &&
              xnc::ResetStormBackoffMs(500, 4000, 9) == 4000);
        // The 3-strike hardware lock (ruling 3): injectable, trips at
        // three contract failures, stays tripped (process lifetime).
        xnc::EncoderFallbackLock lock;
        CHECK("v2i-lock-open", !lock.SoftwareLocked());
        CHECK("v2i-lock-two-strikes",
              !lock.NoteHwFailure() && !lock.NoteHwFailure() &&
                  !lock.SoftwareLocked() && lock.failures() == 2);
        CHECK("v2i-lock-trips-on-third",
              lock.NoteHwFailure() && lock.SoftwareLocked());
        CHECK("v2i-lock-stays",
              lock.NoteHwFailure() && lock.SoftwareLocked());
        // The reset-phase vocabulary: the spec's exact sequence order.
        CHECK("v2i-phase-order",
            xnc::ResetPhase::kDiscontinuity < xnc::ResetPhase::kStopSubmissions &&
            xnc::ResetPhase::kStopSubmissions < xnc::ResetPhase::kRetireLeases &&
            xnc::ResetPhase::kRetireLeases < xnc::ResetPhase::kRebuild &&
            xnc::ResetPhase::kRebuild < xnc::ResetPhase::kBase &&
            xnc::ResetPhase::kBase < xnc::ResetPhase::kConfig &&
            xnc::ResetPhase::kConfig < xnc::ResetPhase::kIdr &&
            xnc::ResetPhase::kIdr < xnc::ResetPhase::kRunning);
        // The DXGI probe rule (backend_ladder.h, shared with the ladder's
        // own probe loop): frame/timeout/rebuilt all prove the duplication
        // is functional; anything else fails the probe.
        CHECK("v2i-probe-rule",
              xnc::DxgiProbeOutcomeHealthy(true, "") &&
              xnc::DxgiProbeOutcomeHealthy(false, "err_timeout") &&
              xnc::DxgiProbeOutcomeHealthy(false, "err_rebuilt") &&
              !xnc::DxgiProbeOutcomeHealthy(false, "err_access_lost") &&
              !xnc::DxgiProbeOutcomeHealthy(false, nullptr));
      }

      // ---- (J) the unified reset sequence at the real loop (ruling 2):
      // phases in spec order discontinuity -> stop submissions -> retire
      // leases -> rebuild -> base -> config -> IDR -> running, ONE epoch
      // increment per executed reset. ----
      {
        const uint32_t w = 320, h = 240;
        std::vector<ScriptedDeviceCapture::Step> script;
        for (int i = 0; i < 8; ++i)
          script.push_back(ScriptedDeviceCapture::Step::kFrame);
        script.push_back(ScriptedDeviceCapture::Step::kAccessLost);
        for (int i = 0; i < 8; ++i)
          script.push_back(ScriptedDeviceCapture::Step::kFrame);
        ScriptedDeviceCapture cap(v2_dev.Get(), v2_ctx.Get(), script, w, h);
        SessionLog log;
        V2RecordingSink sink;
        xnc::EncoderFallbackLock lock;
        xnc::MediaPipelineV2::Config cfg;
        cfg.cap = &cap;
        cfg.surf = &cap;
        cfg.sink = &sink;
        cfg.fps = 60;
        cfg.duration_s = 30;
        cfg.reset_backoff_base_ms = 20;
        cfg.encoder_lock = &lock;
        cfg.session_factory = [](void* ctx, xnc::Nv12SurfacePool* pool) ->
            xnc::IEncoderSession* {
              return new LogSession(static_cast<SessionLog*>(ctx), pool);
            };
        cfg.session_ctx = &log;
        xnc::MediaPipelineV2 pipe;
        CHECK("v2j-start", pipe.Start(cfg));
        if (pipe.running()) {
          CHECK("v2j-epoch2-published",
                wait_for([&] {
                  for (const auto& a : sink.CopyAus())
                    if (a.id.capture_epoch == 2) return true;
                  return false;
                }, 8000));
          const xnc::MediaPipelineV2::Result res = pipe.Stop();
          CHECK("v2j-ok", res.ok);
          CHECK("v2j-one-executed-reset", res.resets == 1);
          // The binding sequence assertion: the eight phases, in order.
          const std::vector<xnc::ResetPhase> want = {
              xnc::ResetPhase::kDiscontinuity, xnc::ResetPhase::kStopSubmissions,
              xnc::ResetPhase::kRetireLeases, xnc::ResetPhase::kRebuild,
              xnc::ResetPhase::kBase, xnc::ResetPhase::kConfig,
              xnc::ResetPhase::kIdr, xnc::ResetPhase::kRunning};
          CHECK("v2j-phase-sequence", res.reset_phases == want);
          // One epoch increment per executed reset: exactly epochs 1->2.
          bool saw_e1 = false, saw_e2 = false, epoch_violation = false;
          for (const auto& id : sink.CopyIds()) {
            if (id.capture_epoch == 1) saw_e1 = true;
            if (id.capture_epoch == 2) saw_e2 = true;
            if (id.capture_epoch < 1 || id.capture_epoch > 2)
              epoch_violation = true;
          }
          CHECK("v2j-one-epoch-per-reset",
                saw_e1 && saw_e2 && !epoch_violation);
          // The first reset for a reason carries no storm backoff.
          CHECK("v2j-storm-first-free",
                res.reset_storm_ms.size() == 1 && res.reset_storm_ms[0] == 0);
          CHECK("v2j-identity-monotonic",
                DeliveredIdentitiesValid(sink.CopyIds()));
          // No hardware rung ran (the sw-only factory shape): no strikes.
          CHECK("v2j-no-hw-strikes",
                res.hw_contract_failures == 0 && !res.encoder_software_locked);
        }
      }

      // ---- (K) rejection of outputs from retired epochs: outputs parked
      // in the reorder window when a reset fires belong to the RETIRED
      // generation - dropped (counted), never published after the reset. ----
      {
        const uint32_t w = 320, h = 240;
        std::vector<ScriptedDeviceCapture::Step> script;
        for (int i = 0; i < 12; ++i)
          script.push_back(ScriptedDeviceCapture::Step::kFrame);
        script.push_back(ScriptedDeviceCapture::Step::kAccessLost);
        for (int i = 0; i < 12; ++i)
          script.push_back(ScriptedDeviceCapture::Step::kFrame);
        ScriptedDeviceCapture cap(v2_dev.Get(), v2_ctx.Get(), script, w, h);
        SessionLog log;
        log.swallow_seq = 3;  // seq 3 never emitted: 4+ park in the window
        V2RecordingSink sink;
        xnc::MediaPipelineV2::Config cfg;
        cfg.cap = &cap;
        cfg.surf = &cap;
        cfg.sink = &sink;
        cfg.fps = 60;
        cfg.duration_s = 30;
        cfg.reset_backoff_base_ms = 20;
        cfg.session_factory = [](void* ctx, xnc::Nv12SurfacePool* pool) ->
            xnc::IEncoderSession* {
              return new LogSession(static_cast<SessionLog*>(ctx), pool);
            };
        cfg.session_ctx = &log;
        xnc::MediaPipelineV2 pipe;
        CHECK("v2k-start", pipe.Start(cfg));
        if (pipe.running()) {
          CHECK("v2k-epoch2-published",
                wait_for([&] {
                  for (const auto& a : sink.CopyAus())
                    if (a.id.capture_epoch == 2) return true;
                  return false;
                }, 8000));
          const xnc::MediaPipelineV2::Result res = pipe.Stop();
          CHECK("v2k-ok", res.ok);
          CHECK("v2k-one-reset", res.resets == 1);
          // The parked epoch-1 outputs (seq 4+, parked behind the
          // swallowed 3) were DROPPED at the discontinuity phase.
          CHECK("v2k-retired-dropped", res.epoch_retired_drops >= 1);
          // Wire contract: no epoch-1 AU at seq >= 3, and no epoch-1 AU
          // delivered after the first epoch-2 AU.
          bool saw_e2 = false, violation = false;
          for (const auto& id : sink.CopyIds()) {
            if (id.capture_epoch == 2) saw_e2 = true;
            if (id.capture_epoch == 1 && saw_e2) violation = true;
            if (id.capture_epoch == 1 && id.encode_seq >= 3) violation = true;
          }
          CHECK("v2k-no-retired-on-wire", saw_e2 && !violation);
          CHECK("v2k-identity-monotonic",
                DeliveredIdentitiesValid(sink.CopyIds()));
          std::printf("SELFTEST NOTE: v2k retired_drops=%llu aus=%llu "
                      "gap_skips=%llu late_drops=%llu\n",
                      (unsigned long long)res.epoch_retired_drops,
                      (unsigned long long)res.aus_written,
                      (unsigned long long)res.reorder_gap_skips,
                      (unsigned long long)res.reorder_late_drops);
        }
      }

      // ---- (L) exponential backoff for repeated IDENTICAL reasons: three
      // sequential access_lost resets record backoffs 0, base, 2*base. ----
      {
        const uint32_t w = 320, h = 240;
        std::vector<ScriptedDeviceCapture::Step> script;
        for (int r = 0; r < 3; ++r) {
          for (int i = 0; i < 6; ++i)
            script.push_back(ScriptedDeviceCapture::Step::kFrame);
          script.push_back(ScriptedDeviceCapture::Step::kAccessLost);
        }
        for (int i = 0; i < 8; ++i)
          script.push_back(ScriptedDeviceCapture::Step::kFrame);
        ScriptedDeviceCapture cap(v2_dev.Get(), v2_ctx.Get(), script, w, h);
        SessionLog log;
        V2RecordingSink sink;
        xnc::MediaPipelineV2::Config cfg;
        cfg.cap = &cap;
        cfg.surf = &cap;
        cfg.sink = &sink;
        cfg.fps = 60;
        cfg.duration_s = 30;
        cfg.reset_backoff_base_ms = 25;  // storm: 0, 25, 50 ms
        cfg.session_factory = [](void* ctx, xnc::Nv12SurfacePool* pool) ->
            xnc::IEncoderSession* {
              return new LogSession(static_cast<SessionLog*>(ctx), pool);
            };
        cfg.session_ctx = &log;
        xnc::MediaPipelineV2 pipe;
        CHECK("v2l-start", pipe.Start(cfg));
        if (pipe.running()) {
          CHECK("v2l-epoch4-published",
                wait_for([&] {
                  for (const auto& a : sink.CopyAus())
                    if (a.id.capture_epoch == 4) return true;
                  return false;
                }, 8000));
          const xnc::MediaPipelineV2::Result res = pipe.Stop();
          CHECK("v2l-ok", res.ok);
          CHECK("v2l-three-resets", res.resets == 3);
          CHECK("v2l-storm-backoffs",
                res.reset_storm_ms.size() == 3 &&
                    res.reset_storm_ms[0] == 0 &&
                    res.reset_storm_ms[1] == 25 &&
                    res.reset_storm_ms[2] == 50);
          // One epoch per executed reset: three resets -> epoch 4 on top.
          CHECK("v2l-phase-count", res.reset_phases.size() == 24);
          uint64_t max_epoch = 0;
          for (const auto& id : sink.CopyIds())
            if (id.capture_epoch > max_epoch) max_epoch = id.capture_epoch;
          CHECK("v2l-epoch-per-reset", max_epoch == 4);
          CHECK("v2l-identity-monotonic",
                DeliveredIdentitiesValid(sink.CopyIds()));
          std::printf("SELFTEST NOTE: v2l resets=%u storm=%u/%u/%u\n",
                      res.resets, res.reset_storm_ms.size() > 0
                          ? res.reset_storm_ms[0] : 0,
                      res.reset_storm_ms.size() > 1
                          ? res.reset_storm_ms[1] : 0,
                      res.reset_storm_ms.size() > 2
                          ? res.reset_storm_ms[2] : 0);
        }
      }

      // ---- (M) mid-run hardware-encoder CONTRACT failures (ruling 3 +
      // ruling 5): each fault strikes the lock and routes recovery through
      // the unified reset (never an immediate fatal); after THREE strikes
      // the process locks to software and the stream survives on it. ----
      {
        const uint32_t w = 320, h = 240, frames = 90;
        std::vector<ScriptedDeviceCapture::Step> script(
            frames, ScriptedDeviceCapture::Step::kFrame);
        ScriptedDeviceCapture cap(v2_dev.Get(), v2_ctx.Get(), script, w, h);
        SessionLog log;
        V2RecordingSink sink;
        xnc::EncoderFallbackLock lock;
        struct HwFake {
          SessionLog* log;
          std::atomic<int> creates{0};
        } hw{&log};
        xnc::MediaPipelineV2::Config cfg;
        cfg.cap = &cap;
        cfg.surf = &cap;
        cfg.sink = &sink;
        cfg.fps = 60;
        cfg.duration_s = 30;
        cfg.reset_backoff_base_ms = 20;
        cfg.encoder_lock = &lock;
        cfg.hw_session_factory = [](void* ctx, xnc::Nv12SurfacePool* pool) ->
            xnc::IEncoderSession* {
              auto* f = static_cast<HwFake*>(ctx);
              f->creates.fetch_add(1);
              return new HwFaultSession(f->log, pool, 2);  // 2 ok, then fault
            };
        cfg.hw_session_ctx = &hw;
        cfg.session_factory = [](void* ctx, xnc::Nv12SurfacePool* pool) ->
            xnc::IEncoderSession* {
              return new LogSession(static_cast<SessionLog*>(ctx), pool);
            };
        cfg.session_ctx = &log;
        xnc::MediaPipelineV2 pipe;
        CHECK("v2m-start", pipe.Start(cfg));
        if (pipe.running()) {
          // fault(1) -> reset -> hw(2) -> fault(2) -> reset -> hw(3) ->
          // fault(3) LOCKS -> reset -> software rung -> epoch 4 serves.
          CHECK("v2m-epoch4-published",
                wait_for([&] {
                  for (const auto& a : sink.CopyAus())
                    if (a.id.capture_epoch == 4) return true;
                  return false;
                }, 8000));
          const xnc::MediaPipelineV2::Result res = pipe.Stop();
          CHECK("v2m-ok-not-fatal", res.ok);
          CHECK("v2m-three-resets", res.resets == 3);
          CHECK("v2m-three-hw-attempts",
                hw.creates.load() == 3 && log.hw_faults.load() == 3);
          CHECK("v2m-locked",
                lock.SoftwareLocked() && res.encoder_software_locked &&
                    res.hw_contract_failures == 3);
          CHECK("v2m-states-loud",
                sink.HasState("encoder_hw_strike") &&
                    sink.HasState("encoder_software_locked") &&
                    sink.HasState("capture_rebuilt"));
          // Ruling 5's backoff: the three encoder resets spaced 0/20/40.
          CHECK("v2m-reinit-backoff",
                res.reset_storm_ms.size() == 3 && res.reset_storm_ms[0] == 0 &&
                    res.reset_storm_ms[1] == 20 && res.reset_storm_ms[2] == 40);
          // The new (software) generation starts from an IDR.
          bool e4_first_key = false, e4_seen = false;
          for (const auto& a : sink.CopyAus()) {
            if (a.id.capture_epoch == 4) {
              if (!e4_seen)
                e4_first_key = (a.flags & xnc::AuFlags::kAuFlagKey) != 0;
              e4_seen = true;
            }
          }
          CHECK("v2m-epoch4-idr-first", e4_seen && e4_first_key);
          CHECK("v2m-identity-monotonic",
                DeliveredIdentitiesValid(sink.CopyIds()));
          std::printf("SELFTEST NOTE: v2m hw_faults=%llu creates=%d "
                      "resets=%u strikes=%u\n",
                      (unsigned long long)log.hw_faults.load(),
                      hw.creates.load(), res.resets,
                      res.hw_contract_failures);
        }
      }

      // ---- (N) hardware INIT failures strike the same lock (the RDP
      // shape: the hardware rung never completes its probe), and the lock
      // is PROCESS-lifetime: a second pipeline never re-attempts hardware. ----
      {
        const uint32_t w = 320, h = 240;
        std::vector<ScriptedDeviceCapture::Step> script;
        for (int i = 0; i < 6; ++i)
          script.push_back(ScriptedDeviceCapture::Step::kFrame);
        script.push_back(ScriptedDeviceCapture::Step::kAccessLost);
        for (int i = 0; i < 6; ++i)
          script.push_back(ScriptedDeviceCapture::Step::kFrame);
        script.push_back(ScriptedDeviceCapture::Step::kAccessLost);
        for (int i = 0; i < 10; ++i)
          script.push_back(ScriptedDeviceCapture::Step::kFrame);
        ScriptedDeviceCapture cap(v2_dev.Get(), v2_ctx.Get(), script, w, h);
        SessionLog log;
        V2RecordingSink sink;
        xnc::EncoderFallbackLock lock;
        struct HwInitFake {
          std::atomic<int> creates{0};
        } hw;
        auto hw_factory = [](void* ctx, xnc::Nv12SurfacePool*) ->
            xnc::IEncoderSession* {
          static_cast<HwInitFake*>(ctx)->creates.fetch_add(1);
          return nullptr;  // hardware init always fails (injected)
        };
        xnc::MediaPipelineV2::Config cfg;
        cfg.cap = &cap;
        cfg.surf = &cap;
        cfg.sink = &sink;
        cfg.fps = 60;
        cfg.duration_s = 30;
        cfg.reset_backoff_base_ms = 20;
        cfg.encoder_lock = &lock;
        cfg.hw_session_factory = hw_factory;
        cfg.hw_session_ctx = &hw;
        cfg.session_factory = [](void* ctx, xnc::Nv12SurfacePool* pool) ->
            xnc::IEncoderSession* {
              return new LogSession(static_cast<SessionLog*>(ctx), pool);
            };
        cfg.session_ctx = &log;
        xnc::MediaPipelineV2 pipe;
        CHECK("v2n-start", pipe.Start(cfg));
        if (pipe.running()) {
          // init strike(1) at the first InitStream; the two injected
          // access-lost resets re-init twice more: strikes 2 and 3 LOCK.
          CHECK("v2n-epoch3-published",
                wait_for([&] {
                  for (const auto& a : sink.CopyAus())
                    if (a.id.capture_epoch == 3) return true;
                  return false;
                }, 8000));
          const xnc::MediaPipelineV2::Result res = pipe.Stop();
          CHECK("v2n-ok", res.ok);
          CHECK("v2n-three-hw-attempts-then-locked",
                hw.creates.load() == 3 && lock.SoftwareLocked());
          CHECK("v2n-states-loud",
                sink.HasState("encoder_hw_strike") &&
                    sink.HasState("encoder_software_locked"));
          // PROCESS-lifetime: a second pipeline sharing the lock never
          // touches the hardware factory again.
          std::vector<ScriptedDeviceCapture::Step> script2(
              12, ScriptedDeviceCapture::Step::kFrame);
          ScriptedDeviceCapture cap2(v2_dev.Get(), v2_ctx.Get(), script2, w,
                                     h);
          SessionLog log2;
          V2RecordingSink sink2;
          xnc::MediaPipelineV2::Config cfg2 = cfg;
          cfg2.cap = &cap2;
          cfg2.surf = &cap2;
          cfg2.sink = &sink2;
          cfg2.session_ctx = &log2;
          xnc::MediaPipelineV2 pipe2;
          CHECK("v2n-pipe2-start", pipe2.Start(cfg2));
          if (pipe2.running()) {
            CHECK("v2n-pipe2-published",
                  wait_for([&] { return sink2.CopyAus().size() >= 2; }, 8000));
            const xnc::MediaPipelineV2::Result res2 = pipe2.Stop();
            CHECK("v2n-pipe2-ok", res2.ok);
            CHECK("v2n-pipe2-no-hw-retry", hw.creates.load() == 3);
            CHECK("v2n-pipe2-locked-snapshot",
                  res2.encoder_software_locked &&
                      res2.hw_contract_failures == 0);
          }
          std::printf("SELFTEST NOTE: v2n hw_creates=%d resets=%u "
                      "locked=%d\n",
                      hw.creates.load(), res.resets,
                      lock.SoftwareLocked() ? 1 : 0);
        }
      }

      // ---- (O) DXGI failure falls to GDI THROUGH the reset sequence
      // (ruling 3): a DXGI rung whose rebuild keeps failing is swapped for
      // GDI inside the same executed reset; the stream resumes on GDI. ----
      {
        const uint32_t w = 320, h = 240;
        // The initial (caller-owned) DXGI fake: frames, one access-lost,
        // frames; its Rebuild always fails (dead duplication).
        std::vector<ScriptedDeviceCapture::Step> dxgi_script;
        for (int i = 0; i < 6; ++i)
          dxgi_script.push_back(ScriptedDeviceCapture::Step::kFrame);
        dxgi_script.push_back(ScriptedDeviceCapture::Step::kAccessLost);
        for (int i = 0; i < 6; ++i)
          dxgi_script.push_back(ScriptedDeviceCapture::Step::kFrame);
        ScriptedDeviceCapture cap(v2_dev.Get(), v2_ctx.Get(), dxgi_script, w,
                                  h);
        cap.rebuild_fail_first = 1000;  // DXGI can never be rebuilt
        SessionLog log;
        V2RecordingSink sink;
        // The factory context: everything a capture-less factory lambda
        // needs (spec + create counters for both rungs).
        struct BackendSpec {
          ID3D11Device* dev;
          ID3D11DeviceContext* ctx;
          std::vector<ScriptedDeviceCapture::Step> gdi;
          uint32_t w, h;
          uint32_t dxgi_rebuild_fail_first;
          std::atomic<int> dxgi_creates{0}, gdi_creates{0};
        } spec{v2_dev.Get(), v2_ctx.Get(),
               std::vector<ScriptedDeviceCapture::Step>(
                   24, ScriptedDeviceCapture::Step::kFrame),
               w, h, 1000, {}, {}};
        xnc::MediaPipelineV2::Config cfg;
        cfg.cap = &cap;
        cfg.surf = &cap;
        cfg.sink = &sink;
        cfg.fps = 60;
        cfg.duration_s = 30;
        cfg.reset_backoff_base_ms = 20;
        cfg.initial_backend = xnc::MediaBackend::kDxgi;
        cfg.make_backend = [](void* c, xnc::MediaBackend kind,
                              std::string*) ->
            std::unique_ptr<xnc::ICapture> {
          auto* s = static_cast<BackendSpec*>(c);
          if (kind == xnc::MediaBackend::kGdi) {
            s->gdi_creates.fetch_add(1);
            return std::make_unique<ScriptedDeviceCapture>(s->dev, s->ctx,
                                                           s->gdi, s->w, s->h);
          }
          s->dxgi_creates.fetch_add(1);
          auto made = std::make_unique<ScriptedDeviceCapture>(s->dev, s->ctx,
              std::vector<ScriptedDeviceCapture::Step>(
                  8, ScriptedDeviceCapture::Step::kFrame), s->w, s->h);
          made->rebuild_fail_first = s->dxgi_rebuild_fail_first;
          return made;
        };
        cfg.backend_ctx = &spec;
        cfg.session_factory = [](void* ctx, xnc::Nv12SurfacePool* pool) ->
            xnc::IEncoderSession* {
              return new LogSession(static_cast<SessionLog*>(ctx), pool);
            };
        cfg.session_ctx = &log;
        xnc::MediaPipelineV2 pipe;
        CHECK("v2o-start", pipe.Start(cfg));
        if (pipe.running()) {
          // access_lost -> reset: DXGI rebuild fails the hard-fail streak
          // -> the SAME reset swaps to GDI and completes there.
          CHECK("v2o-epoch2-published",
                wait_for([&] {
                  for (const auto& a : sink.CopyAus())
                    if (a.id.capture_epoch == 2) return true;
                  return false;
                }, 8000));
          const xnc::MediaPipelineV2::Result res = pipe.Stop();
          CHECK("v2o-ok", res.ok);
          CHECK("v2o-one-reset", res.resets == 1);
          CHECK("v2o-swapped-to-gdi",
                res.backend_swaps == 1 &&
                    res.backend_at_stop == xnc::MediaBackend::kGdi &&
                    spec.gdi_creates.load() == 1);
          // No DXGI probe ran during the short scenario (30 s cadence), so
          // the factory never created a DXGI rung.
          CHECK("v2o-no-dxgi-factory", spec.dxgi_creates.load() == 0);
          CHECK("v2o-backend-state", sink.HasState("backend_changed"));
          CHECK("v2o-phase-sequence", res.reset_phases.size() == 8);
          CHECK("v2o-identity-monotonic",
                DeliveredIdentitiesValid(sink.CopyIds()));
          std::printf("SELFTEST NOTE: v2o gdi_creates=%d resets=%u "
                      "retired_drops=%llu\n",
                      spec.gdi_creates.load(), res.resets,
                      (unsigned long long)res.epoch_retired_drops);
        }
      }

      // ---- (P) the return path (ruling 3): while serving on GDI, a DXGI
      // probe every dxgi_reprobe_ms (30 s in production) re-tries DXGI and
      // RETURNS through the same reset sequence (backend_changed). ----
      {
        const uint32_t w = 320, h = 240;
        // The initial (caller-owned) GDI fake: a few frames, then a
        // static screen (script exhausted -> kNoChange forever).
        std::vector<ScriptedDeviceCapture::Step> gdi_script(
            6, ScriptedDeviceCapture::Step::kFrame);
        ScriptedDeviceCapture cap(v2_dev.Get(), v2_ctx.Get(), gdi_script, w,
                                  h);
        SessionLog log;
        V2RecordingSink sink;
        struct BackendSpec {
          ID3D11Device* dev;
          ID3D11DeviceContext* ctx;
          std::vector<ScriptedDeviceCapture::Step> dxgi;
          uint32_t w, h;
          std::atomic<int> dxgi_creates{0}, gdi_creates{0};
        } spec{v2_dev.Get(), v2_ctx.Get(),
               std::vector<ScriptedDeviceCapture::Step>(
                   24, ScriptedDeviceCapture::Step::kFrame),
               w, h, {}, {}};
        xnc::MediaPipelineV2::Config cfg;
        cfg.cap = &cap;
        cfg.surf = &cap;
        cfg.sink = &sink;
        cfg.fps = 60;
        cfg.duration_s = 30;
        cfg.reset_backoff_base_ms = 20;
        cfg.initial_backend = xnc::MediaBackend::kGdi;
        cfg.dxgi_reprobe_ms = 250;  // fast re-probe for the scenario
        cfg.make_backend = [](void* c, xnc::MediaBackend kind,
                              std::string*) ->
            std::unique_ptr<xnc::ICapture> {
          auto* s = static_cast<BackendSpec*>(c);
          if (kind == xnc::MediaBackend::kGdi) {
            s->gdi_creates.fetch_add(1);
            return std::make_unique<ScriptedDeviceCapture>(s->dev, s->ctx,
                std::vector<ScriptedDeviceCapture::Step>(
                    6, ScriptedDeviceCapture::Step::kFrame), s->w, s->h);
          }
          s->dxgi_creates.fetch_add(1);
          return std::make_unique<ScriptedDeviceCapture>(s->dev, s->ctx,
                                                         s->dxgi, s->w, s->h);
        };
        cfg.backend_ctx = &spec;
        cfg.session_factory = [](void* ctx, xnc::Nv12SurfacePool* pool) ->
            xnc::IEncoderSession* {
              return new LogSession(static_cast<SessionLog*>(ctx), pool);
            };
        cfg.session_ctx = &log;
        xnc::MediaPipelineV2 pipe;
        CHECK("v2p-start", pipe.Start(cfg));
        if (pipe.running()) {
          // ~250 ms: probe -> healthy -> change_backend reset -> swap to
          // DXGI -> the DXGI fake's frames publish as epoch 2.
          CHECK("v2p-epoch2-published",
                wait_for([&] {
                  for (const auto& a : sink.CopyAus())
                    if (a.id.capture_epoch == 2) return true;
                  return false;
                }, 8000));
          const xnc::MediaPipelineV2::Result res = pipe.Stop();
          CHECK("v2p-ok", res.ok);
          CHECK("v2p-returned-to-dxgi",
                res.backend_at_stop == xnc::MediaBackend::kDxgi &&
                    res.backend_swaps == 1 &&
                    res.dxgi_probes_ok >= 1);
          // Probe (throwaway) + swap (fresh) both asked the factory for a
          // DXGI rung; the GDI rung was never factory-created.
          CHECK("v2p-probe-plus-swap",
                spec.dxgi_creates.load() == 2 && spec.gdi_creates.load() == 0);
          CHECK("v2p-backend-state", sink.HasState("backend_changed"));
          CHECK("v2p-reset-reason-change-backend",
                std::strcmp(res.last_reset_reason,
                            xnc::kResetReasonChangeBackend) == 0);
          CHECK("v2p-phase-sequence", res.reset_phases.size() == 8);
          CHECK("v2p-identity-monotonic",
                DeliveredIdentitiesValid(sink.CopyIds()));
          std::printf("SELFTEST NOTE: v2p probes_ok=%u creates_dxgi=%d "
                      "resets=%u\n",
                      res.dxgi_probes_ok, spec.dxgi_creates.load(),
                      res.resets);
        }
      }

      // ---- (Q) final-review fix 2026-08 (CRITICAL): the DXGI->GDI backend
      // swap on TWO DISTINCT D3D devices at EQUAL dims. The production bug:
      // the shared LatestSurface kept the dead DXGI device's texture, the
      // GDI rung's CopyFrom paired two devices (API-invalid, silently
      // undefined in retail), the pipeline adopted the STALE device from
      // the surface snapshot and served frozen DXGI pixels with healthy,
      // ever-advancing identities. Every earlier swap scenario shared the
      // suite's single device, so the fakes could not see the failure.
      // The fix (LatestSurface::Reset at reset phase 3) must hand the
      // surface, pool, converter and session to the GDI device and serve
      // ITS live content. ----
      {
        const uint32_t w = 320, h = 240;
        // Device B: a SECOND D3D instance - WARP first (fully detached from
        // the suite device), else a second HARDWARE instance (some machines
        // refuse WARP with the video flag, 0x887A0004). Either way it is a
        // DISTINCT ID3D11Device with its own immediate context, which is
        // what makes the cross-device pairing of the bug observable at all.
        Microsoft::WRL::ComPtr<ID3D11Device> gdi_dev;
        Microsoft::WRL::ComPtr<ID3D11DeviceContext> gdi_ctx;
        D3D_FEATURE_LEVEL gdi_fl{};
        auto gdi_try = [&gdi_dev, &gdi_ctx, &gdi_fl](D3D_DRIVER_TYPE dt,
                                                     UINT f) {
          gdi_dev.Reset();
          gdi_ctx.Reset();
          return D3D11CreateDevice(nullptr, dt, nullptr, f, nullptr, 0,
                                   D3D11_SDK_VERSION, &gdi_dev, &gdi_fl,
                                   &gdi_ctx);
        };
        HRESULT gdi_hr = gdi_try(D3D_DRIVER_TYPE_WARP, v2_flags);
        const char* gdi_via = "warp";
        if (FAILED(gdi_hr)) {
          gdi_via = "hardware";
          gdi_hr = gdi_try(D3D_DRIVER_TYPE_HARDWARE, v2_flags);
        }
        CHECK("v2q-second-device", SUCCEEDED(gdi_hr));
        if (gdi_dev) {
          // DXGI rung (device A): frames, one access-lost, frames; its
          // Rebuild never succeeds (a dead duplication).
          std::vector<ScriptedDeviceCapture::Step> dxgi_script;
          for (int i = 0; i < 6; ++i)
            dxgi_script.push_back(ScriptedDeviceCapture::Step::kFrame);
          dxgi_script.push_back(ScriptedDeviceCapture::Step::kAccessLost);
          for (int i = 0; i < 6; ++i)
            dxgi_script.push_back(ScriptedDeviceCapture::Step::kFrame);
          ScriptedDeviceCapture cap(v2_dev.Get(), v2_ctx.Get(), dxgi_script,
                                    w, h);
          cap.rebuild_fail_first = 1000;
          // The factory context: the GDI rung is created on DEVICE B at the
          // SAME dims with BRIGHT content (luma ~235 where the DXGI bars
          // stay black ~16).
          struct BackendSpec {
            ID3D11Device* dev;
            ID3D11DeviceContext* ctx;
            uint32_t w, h;
            std::atomic<int> gdi_creates{0};
          } spec{gdi_dev.Get(), gdi_ctx.Get(), w, h, {}};
          std::vector<SwapProbeSession::Rec> recs;
          std::mutex recs_mu;
          struct ProbeCtx {
            std::vector<SwapProbeSession::Rec>* recs;
            std::mutex* mu;
          } probe_ctx{&recs, &recs_mu};
          V2RecordingSink sink;
          xnc::MediaPipelineV2::Config cfg;
          cfg.cap = &cap;
          cfg.surf = &cap;
          cfg.sink = &sink;
          cfg.fps = 60;
          cfg.duration_s = 30;
          cfg.reset_backoff_base_ms = 20;
          cfg.initial_backend = xnc::MediaBackend::kDxgi;
          cfg.make_backend = [](void* c, xnc::MediaBackend kind,
                                std::string*) ->
              std::unique_ptr<xnc::ICapture> {
            auto* s = static_cast<BackendSpec*>(c);
            if (kind != xnc::MediaBackend::kGdi) return nullptr;
            s->gdi_creates.fetch_add(1);
            return std::make_unique<ScriptedDeviceCapture>(
                s->dev, s->ctx,
                std::vector<ScriptedDeviceCapture::Step>(
                    60, ScriptedDeviceCapture::Step::kFrame),
                s->w, s->h, /*use_bright=*/true);
          };
          cfg.backend_ctx = &spec;
          cfg.session_factory = [](void* c, xnc::Nv12SurfacePool* pool) ->
              xnc::IEncoderSession* {
            auto* p = static_cast<ProbeCtx*>(c);
            return new SwapProbeSession(p->recs, p->mu, pool);
          };
          cfg.session_ctx = &probe_ctx;
          xnc::MediaPipelineV2 pipe;
          CHECK("v2q-start", pipe.Start(cfg));
          if (pipe.running()) {
            // access_lost -> reset: the dead DXGI rebuild trips the swap in
            // the SAME reset; wait until the GDI rung drove >= 8 BRIGHT,
            // sampled, on-device-B submissions - i.e. the stream is
            // provably serving the GDI device's live content (not frozen
            // DXGI pixels) before the scenario stops anything.
            CHECK("v2q-gdi-submissions",
                  wait_for([&] {
                    std::lock_guard<std::mutex> lk(recs_mu);
                    size_t bright_b = 0;
                    for (const auto& r : recs)
                      if (r.dev == gdi_dev.Get() && r.sampled &&
                          r.bright_pts >= 8)
                        ++bright_b;
                    return bright_b >= 8;
                  }, 8000));
            const xnc::MediaPipelineV2::Result res = pipe.Stop();
            CHECK("v2q-ok", res.ok);
            CHECK("v2q-swapped-to-gdi",
                  res.backend_swaps == 1 &&
                      res.backend_at_stop == xnc::MediaBackend::kGdi &&
                      spec.gdi_creates.load() == 1);
            std::vector<SwapProbeSession::Rec> snap;
            {
              std::lock_guard<std::mutex> lk(recs_mu);
              snap = recs;
            }
            CHECK("v2q-submissions-flowed", snap.size() >= 10);
            // THE device handoff: submissions before the swap ran on the
            // DXGI device, every submission after it on the GDI device -
            // the pool/converter/session followed the surface's device,
            // never the stale DXGI one (under the bug the post-swap
            // submissions kept coming from device A).
            bool saw_gdi = false, clean = true;
            size_t on_a = 0, on_b = 0;
            for (const auto& r : snap) {
              if (r.dev == gdi_dev.Get()) {
                saw_gdi = true;
                ++on_b;
              } else {
                if (saw_gdi) clean = false;
                ++on_a;
              }
            }
            CHECK("v2q-device-handoff", saw_gdi && clean && on_a >= 4);
            // THE content proof: every SAMPLED post-swap converted slot
            // carries the GDI rung's BRIGHT pixels (>= 8 of 12 points) in
            // the region the DXGI bars keep black - frozen DXGI pixels
            // would read <= 1 bright point forever while the identities
            // kept advancing (the P0 "not recovered, frozen" class this
            // scenario exists to kill). A staging sample that failed to
            // run is excluded, never counted dark.
            size_t bright = 0, dark_after_swap = 0;
            for (const auto& r : snap) {
              if (r.dev != gdi_dev.Get() || !r.sampled) continue;
              if (r.bright_pts >= 8) ++bright; else ++dark_after_swap;
            }
            CHECK("v2q-serves-gdi-content", bright >= 8 && dark_after_swap == 0);
            // Discriminator sanity: the DXGI-era submissions really were
            // the bars (<= 1 bright point; the bright/dark split
            // discriminates on THIS converter, not vacuously).
            size_t dark_before = 0;
            for (const auto& r : snap)
              if (r.dev == v2_dev.Get() && r.sampled && r.bright_pts <= 1)
                ++dark_before;
            CHECK("v2q-dxgi-content-was-bars", dark_before >= 4);
            // Identities advanced with the real GDI content changes:
            // strictly increasing content_ids across the post-swap
            // submissions (each a distinct moving-stripe frame).
            uint64_t last_cid = 0;
            bool cids_advance = true;
            for (const auto& r : snap)
              if (r.dev == gdi_dev.Get()) {
                if (last_cid != 0 && r.content_id <= last_cid)
                  cids_advance = false;
                last_cid = r.content_id;
              }
            CHECK("v2q-content-ids-advance", cids_advance && last_cid > 6);
            CHECK("v2q-identity-monotonic",
                  DeliveredIdentitiesValid(sink.CopyIds()));
            std::printf("SELFTEST NOTE: v2q dev_b=%s on_a=%zu on_b=%zu "
                        "bright=%zu dark_after=%zu dark_before=%zu "
                        "last_cid=%llu pts=[",
                        gdi_via, on_a, on_b, bright, dark_after_swap,
                        dark_before,
                        static_cast<unsigned long long>(last_cid));
            for (size_t i = 0; i < snap.size(); ++i)
              std::printf("%s%u:%u[%u %u %u %u %u %u %u %u]", i ? " " : "",
                          snap[i].dev == gdi_dev.Get() ? 1 : 0,
                          snap[i].bright_pts, snap[i].row_profile[0],
                          snap[i].row_profile[1], snap[i].row_profile[2],
                          snap[i].row_profile[3], snap[i].row_profile[4],
                          snap[i].row_profile[5], snap[i].row_profile[6],
                          snap[i].row_profile[7]);
            std::printf("] resets=%u captured=%llu encoded=%llu\n",
                        res.resets,
                        static_cast<unsigned long long>(res.captured),
                        static_cast<unsigned long long>(res.encoded));
          }
        }
      }

      // ---- (R) final-review fix 2026-08 (IMPORTANT): --encoder software
      // must reach the V2 session selection in rt mode. Pipeline half: the
      // pin keeps CreateSession off the hardware rung ENTIRELY (no probe,
      // no strikes) while the software rung serves (the unpinned hardware
      // attempts are v2m/v2n's creates>=1 assertions). ----
      {
        const uint32_t w = 320, h = 240;
        std::vector<ScriptedDeviceCapture::Step> script(
            12, ScriptedDeviceCapture::Step::kFrame);
        ScriptedDeviceCapture cap(v2_dev.Get(), v2_ctx.Get(), script, w, h);
        SessionLog log;
        V2RecordingSink sink;
        xnc::EncoderFallbackLock lock;
        struct HwProbe {
          std::atomic<int> creates{0};
        } hw;
        xnc::MediaPipelineV2::Config cfg;
        cfg.cap = &cap;
        cfg.surf = &cap;
        cfg.sink = &sink;
        cfg.fps = 60;
        cfg.duration_s = 30;
        cfg.encoder_lock = &lock;
        cfg.force_software_encoder = true;  // THE PIN (ServeV2 threads it)
        cfg.hw_session_factory = [](void* c, xnc::Nv12SurfacePool*) ->
            xnc::IEncoderSession* {
          static_cast<HwProbe*>(c)->creates.fetch_add(1);
          return nullptr;  // a would-be hardware rung
        };
        cfg.hw_session_ctx = &hw;
        cfg.session_factory = [](void* ctx, xnc::Nv12SurfacePool* pool) ->
            xnc::IEncoderSession* {
          return new LogSession(static_cast<SessionLog*>(ctx), pool);
        };
        cfg.session_ctx = &log;
        xnc::MediaPipelineV2 pipe;
        CHECK("v2r-start", pipe.Start(cfg));
        if (pipe.running()) {
          CHECK("v2r-published",
                wait_for([&] { return sink.CopyAus().size() >= 2; }, 8000));
          const xnc::MediaPipelineV2::Result res = pipe.Stop();
          CHECK("v2r-ok", res.ok);
          CHECK("v2r-hw-never-probed", hw.creates.load() == 0);
          CHECK("v2r-no-strikes",
                !lock.SoftwareLocked() && res.hw_contract_failures == 0);
          CHECK("v2r-software-rung-served",
                std::strcmp(res.encoder_backend, "factory") == 0 &&
                    res.aus_written >= 2);
          CHECK("v2r-no-cpu-readback", res.cpu_readbacks == 0);
          CHECK("v2r-identity-monotonic",
                DeliveredIdentitiesValid(sink.CopyIds()));
          std::printf("SELFTEST NOTE: v2r hw_creates=%d backend=%s aus=%llu\n",
                      hw.creates.load(), res.encoder_backend,
                      static_cast<unsigned long long>(res.aus_written));
        }
      }

      // ---- (R, rt half) ServeV2 with the pin through the REAL production
      // ladder in rt mode: the hardware rung is suppressed (no
      // encoder_hw_strike reaches the subscriber on a box where it would
      // fail), the software rung serves, exit 0. ----
      {
        const uint32_t w = 320, h = 240;
        std::vector<ScriptedDeviceCapture::Step> script(
            30, ScriptedDeviceCapture::Step::kFrame);
        ScriptedDeviceCapture cap(v2_dev.Get(), v2_ctx.Get(), script, w, h);
        xnc::RtServer server;
        xnc::RtServer::Opts ro;
        ro.pipe_name = RtPipeNameOf(13);
        ro.secret = kRtSecret;
        ro.secret_len = sizeof(kRtSecret);
        ro.max_subs = 4;
        ro.fps = 15;
        ro.bitrate_bps = 500000;
        ro.sddl_override = L"D:P(A;;GA;;;WD)";  // TEST-ONLY permissive DACL
        ro.pipeline_v2 = true;
        int rc = 1;
        std::thread serve_th([&] {
          rc = server.ServeV2(cap, cap, ro, 0, xnc::MediaBackend::kDxgi,
                              /*force_software=*/true);
        });
        RtTestClient a;
        CHECK("v2r-rt-connect",
              a.Connect(ro.pipe_name.c_str(), kRtSecret, sizeof(kRtSecret)));
        CHECK("v2r-rt-attach", a.Attach(13));
        a.Pump(6000, [&a] { return a.keys_ >= 1 && a.v2_frames_ >= 4; });
        server.RequestStop();
        serve_th.join();
        a.Pump(1500);  // drain + stream_end
        server.Shutdown();
        CHECK("v2r-rt-frames", a.v2_frames_ >= 1 && a.keys_ >= 1);
        CHECK("v2r-rt-no-hw-strike", !a.SawState("encoder_hw_strike"));
        CHECK("v2r-rt-serve-ok", rc == 0);
        std::printf("SELFTEST NOTE: v2r-rt frames=%llu keys=%llu rc=%d\n",
                    static_cast<unsigned long long>(a.v2_frames_),
                    static_cast<unsigned long long>(a.keys_), rc);
      }
    }
  }
  if (fails == 0) std::printf("selftest ok\n");
  return fails == 0 ? 0 : 1;
}
