// media_pipeline_v2.cpp - the M2 Task 4 single GPU loop (see
// media_pipeline_v2.h). Structure:
//
//   LoopThread (the ONE media GPU thread - it is also the capture thread,
//   which is how capture.h's and gpu_surface.h's single-thread contracts
//   are one and the same under this pipeline):
//     1. collect encoder outputs (publish AUs through the identity ledger);
//     2. poll the unified CaptureReset coordinator + the sink's merged IDR
//        request into the mailbox;
//     3. consume mailbox commands (reset > reconfigure);
//     4. capture: ICaptureSurface::AcquireSurface into the LatestSurface
//        (speculative identity assigned here, committed on kFrame; the
//        kFrame identity is PUBLISHED into the mailbox's depth-one content
//        cell - coalescing happens right there);
//     5. FPS gate, then (if a pool slot is free AND the mailbox has
//        content) VideoProcessorBlt BGRA->NV12 into the leased slot and
//        Submit with the sticky-IDR flag consumed exactly once;
//     6. per-second diag beat.
//
// No frame FIFO anywhere: when all three NV12 slots are busy the content
// cell keeps replacing itself; when a slot frees only the LATEST identity
// is converted and submitted (the Task 4 binding test pins this).
//
// The D3D11 device is adopted from the capture backend: the backend Inits
// the caller-owned LatestSurface on ITS device (EnsureLatestSurface /
// EnsureGpuUpload), so the pool, the VideoProcessor and the encoder session
// are all created on the device the surface's texture reports - one
// device, one thread, no cross-device copies.
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>  // Sleep, GetTickCount64
#include <d3d11.h>
#include <mmsystem.h>  // timeBeginPeriod/timeEndPeriod (1 ms pacing)

#include <cstdint>
#include <cstdio>
#include <deque>
#include <memory>
#include <string>
#include <vector>
#include <wrl/client.h>  // ComPtr (device/context/video views)

#include "../common/log.h"
#include "backend_ladder.h"  // DxgiProbeOutcomeHealthy (shared probe rule)
#include "dxgi_capture.h"    // GpuScaledDims, NowMonoUs

#include "media_pipeline_v2.h"

namespace xnc {

MediaFileSink::MediaFileSink(FILE* f) : f_(f) {}
MediaFileSink::~MediaFileSink() = default;
const char* MediaFileSink::OnAu(const EncodedAU& au) {
  if (au.annexb == nullptr || au.annexb->empty()) return nullptr;
  const uint8_t* p = au.annexb->data();
  const size_t len = au.annexb->size();
  if (std::fwrite(p, 1, len, f_) != len) {
    XNC_LOG_ERROR("write_out_failed");
    return "fwrite out failed";
  }
  return nullptr;
}

namespace {

uint64_t NowMs() { return GetTickCount64(); }

// 1 ms timer resolution for the pacing Sleeps (pipeline.cpp pattern).
class TimePeriodGuard {
 public:
  TimePeriodGuard() { timeBeginPeriod(1); }
  ~TimePeriodGuard() { timeEndPeriod(1); }
};

constexpr DWORD kIdleSleepMs = 15;
// Warm-up wall bound (spec 7.4; mirrors pipeline.cpp).
constexpr uint64_t kWarmupWallBoundMs = 2000ull;
// 2026-08-28 QSV static-idle park: the idle flush's trigger and budget.
// kFlushAfterSpf: input starvation threshold in spf units before a flush
// feed fires (2 = one missed submission slot; the healthy paced path never
// qualifies because last_submit_ms advances every slot).
// kFlushFeedBound: feeds per content episode - the QSV emit depth is ~5,
// so 8 covers the tail with margin; a new captured frame resets the
// episode (AcquireOnce kFrame branch).
constexpr uint64_t kFlushAfterSpf = 2;
constexpr uint32_t kFlushFeedBound = 8;
// ON by default (the 2026-08-28 labs-xiaoxin measurement: with the flush,
// the hardware rung's semi-static dwell collapsed from a uniform ~586 ms
// to ~114 ms p50 / ~119 ms p95 at native 2880x1800 with zero unrecovered
// freezes; without it, a fully static desktop parks the stream's tail
// inside the encoder for the whole gap - the RERUN-2 P0: 4 unrecovered
// freezes, capture->AU p95 ~59 s). XNC_QSV_IDLE_FLUSH=0 disables (A/B);
// read once per process (the QsvDiagVerbose pattern, mf_gpu_encoder.cpp).
bool IdleFlushEnabled() {
  static const bool v = [] {
    char buf[8]{};
    const DWORD n = GetEnvironmentVariableA("XNC_QSV_IDLE_FLUSH", buf,
                                            sizeof(buf));
    return !(n > 0 && buf[0] == '0');
  }();
  return v;
}
// Reset retry cadence (M2-S1 T2 shape). The soft/hard rebuild backoff is
// Config::reset_backoff_base_ms / 2x that (500/1000 ms production; M2
// Task 5 made it injectable for the deterministic scenarios); the 500 here
// is the fallback when a config passes 0 explicitly.
constexpr uint32_t kResetPollMs = 100;
constexpr uint32_t kResetBackoffMs = 500;
constexpr uint32_t kResetHardFailStreak = 3;
// Pipeline-initiated IDR throttle (spec 7.5).
constexpr uint64_t kIdrMinIntervalMs = 500;
// M2 Task 5: the storm-backoff cap (base is Config::reset_backoff_base_ms,
// default 500 ms - 0, 500, 1000, 2000, 4000, 4000...), and the
// dead-end bound: resets that keep "completing" without a single captured
// frame between them are a rebuild that cannot serve - fatal after
// kResetHardFailStreak+1 of them (never an infinite loop).
constexpr uint32_t kResetStormCapMs = 4000;
// Reorder-before-publish window bounds (fix round 1): the software rung
// emits AUs reordered relative to submissions, and the v2 wire's delivery
// contract is strictly increasing encode_seq per epoch pair (the Go
// client's frameLedger tears down on the first regression). Out-of-order
// outputs are PARKED until the gap fills; a gap that exceeds 2x the
// ~17-frame lookahead (the WarmupFeedBound convention) or outlives
// kReorderHoldMs (static screen: no further outputs to fill it) is
// skipped and a sticky IDR resynchronizes decoders.
constexpr size_t kReorderWindowMax = 34;
constexpr uint64_t kReorderHoldMs = 500;

std::string HrErr(const char* step, HRESULT hr) {
  char buf[128];
  _snprintf_s(buf, sizeof(buf), _TRUNCATE, "%s: hr=0x%08lX", step,
              static_cast<unsigned long>(hr));
  return std::string(buf);
}

// Best-effort device-removed detection from the converter's HrErr strings
// (DXGI_ERROR_DEVICE_REMOVED 0x887A0005 / DEVICE_HUNG 0x887A0001 /
// DEVICE_RESET 0x887A0007): a dead device routes into the unified reset
// with the TOP severity reason instead of a fatal - the rebuild recreates
// the device (or falls to GDI when it cannot).
bool LooksDeviceRemoved(const std::string& err) {
  return err.find("hr=0x887A0005") != std::string::npos ||
         err.find("hr=0x887A0001") != std::string::npos ||
         err.find("hr=0x887A0007") != std::string::npos;
}

// ---- BGRA(LatestSurface) -> NV12(pool slot) converter ----
//
// One VideoProcessor over the surface's device: the enumerator carries the
// capture dims as input and the stream dims as output (the --max-w scale
// rides the same Blt), the color rule is Nv12ColorForSize(out_h) (BT.709
// for >= 720, else BT.601 - the same rule the encoder sessions negotiate
// their input matrix by, so converter and encoder agree per 8.4). One
// persistent output view per pool slot; the input view is rebuilt whenever
// the surface's texture object changes (backend re-Init). Single-thread
// (the media loop) like everything it touches.
class Nv12Converter {
 public:
  bool Init(ID3D11Device* dev, uint32_t src_w, uint32_t src_h, uint32_t out_w,
            uint32_t out_h, Nv12SurfacePool* pool, std::string* err) {
    Reset();
    if (dev == nullptr || pool == nullptr) {
      if (err) *err = "converter init: null dev/pool";
      return false;
    }
    dev_ = dev;
    dev->GetImmediateContext(ctx_.GetAddressOf());
    if (FAILED(dev_.As(&vid_dev_)) || FAILED(ctx_.As(&vid_ctx_))) {
      if (err) *err = "converter init: QI video device/context failed";
      return false;
    }
    D3D11_VIDEO_PROCESSOR_CONTENT_DESC vd{};
    vd.InputFrameFormat = D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE;
    vd.InputFrameRate = {0, 0};
    vd.InputWidth = src_w;
    vd.InputHeight = src_h;
    vd.OutputFrameRate = {0, 0};
    vd.OutputWidth = out_w;
    vd.OutputHeight = out_h;
    HRESULT hr = vid_dev_->CreateVideoProcessorEnumerator(&vd, vpe_.GetAddressOf());
    if (FAILED(hr)) {
      if (err) *err = HrErr("CreateVideoProcessorEnumerator", hr);
      return false;
    }
    UINT in_sup = 0, out_sup = 0;
    vpe_->CheckVideoProcessorFormat(DXGI_FORMAT_B8G8R8A8_UNORM, &in_sup);
    vpe_->CheckVideoProcessorFormat(DXGI_FORMAT_NV12, &out_sup);
    if (!in_sup || !out_sup) {
      if (err) *err = "video processor does not support bgra->nv12";
      return false;
    }
    hr = vid_dev_->CreateVideoProcessor(vpe_.Get(), 0, vp_.GetAddressOf());
    if (FAILED(hr)) {
      if (err) *err = HrErr("CreateVideoProcessor", hr);
      return false;
    }
    // Color spaces: input is the desktop's full-range RGB; output is
    // limited-range YCbCr with the Nv12ColorForSize matrix (8.4 rule).
    D3D11_VIDEO_PROCESSOR_COLOR_SPACE in_cs{};
    in_cs.Usage = 1;  // RGB full range (dxgi_capture.cpp convention)
    vid_ctx_->VideoProcessorSetStreamColorSpace(vp_.Get(), 0, &in_cs);
    const Nv12ColorConfig rule = Nv12ColorForSize(out_h);
    D3D11_VIDEO_PROCESSOR_COLOR_SPACE out_cs{};
    out_cs.Usage = 3;         // YCbCr studio range
    out_cs.RGB_Range = 1;     // full-range RGB input
    out_cs.YCbCr_Matrix = rule.bt709 ? 1 : 0;  // 1 = BT.709, 0 = BT.601
    out_cs.Nominal_Range = D3D11_VIDEO_PROCESSOR_NOMINAL_RANGE_16_235;
    vid_ctx_->VideoProcessorSetOutputColorSpace(vp_.Get(), &out_cs);
    // One persistent output view per pool slot (Blt takes a view). All
    // three leases are HELD while the views are created (a released slot
    // would just be re-acquired - Acquire returns the first FREE slot), so
    // the three acquires yield the three distinct slots; each view is
    // keyed by the LEASE's index, never an assumed one.
    SurfaceLease* leases[Nv12SurfacePool::kSlotCount] = {};
    for (size_t i = 0; i < Nv12SurfacePool::kSlotCount; ++i) {
      leases[i] = pool->Acquire();
      if (leases[i] == nullptr) {
        for (size_t j = 0; j < i; ++j) leases[j]->Release();
        if (err) *err = "converter init: pool exhausted during view setup";
        return false;
      }
    }
    for (size_t i = 0; i < Nv12SurfacePool::kSlotCount; ++i) {
      D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC ovd{};
      ovd.ViewDimension = D3D11_VPOV_DIMENSION_TEXTURE2D;
      ovd.Texture2D.MipSlice = 0;
      hr = vid_dev_->CreateVideoProcessorOutputView(
          leases[i]->texture(), vpe_.Get(), &ovd,
          out_views_[leases[i]->index()].GetAddressOf());
      if (FAILED(hr)) {
        if (err) *err = HrErr("CreateVideoProcessorOutputView", hr);
        for (auto* l : leases) l->Release();
        return false;
      }
    }
    for (auto* l : leases) l->Release();
    src_w_ = src_w;
    src_h_ = src_h;
    out_w_ = out_w;
    out_h_ = out_h;
    return true;
  }

  // src: the LatestSurface's BGRA texture (Snapshot AddRef'd by the caller
  // for the duration of the call); lease: the destination NV12 slot.
  bool Convert(ID3D11Texture2D* src, const SurfaceLease& lease,
               std::string* err) {
    if (src == nullptr || !lease) {
      if (err) *err = "convert: null src/lease";
      return false;
    }
    if (src != in_tex_) {  // surface re-Init'ed (backend rebuild)
      in_view_.Reset();
      D3D11_VIDEO_PROCESSOR_INPUT_VIEW_DESC ivd{};
      ivd.FourCC = 0;  // use the texture's format (BGRA)
      ivd.ViewDimension = D3D11_VPIV_DIMENSION_TEXTURE2D;
      ivd.Texture2D.MipSlice = 0;
      ivd.Texture2D.ArraySlice = 0;
      const HRESULT hr = vid_dev_->CreateVideoProcessorInputView(
          src, vpe_.Get(), &ivd, in_view_.GetAddressOf());
      if (FAILED(hr)) {
        if (err) *err = HrErr("CreateVideoProcessorInputView", hr);
        return false;
      }
      in_tex_ = src;
    }
    D3D11_VIDEO_PROCESSOR_STREAM st{};
    st.Enable = TRUE;
    st.pInputSurface = in_view_.Get();
    const HRESULT hr = vid_ctx_->VideoProcessorBlt(
        vp_.Get(), out_views_[lease.index()].Get(), 0, 1, &st);
    if (FAILED(hr)) {
      if (err) *err = HrErr("VideoProcessorBlt", hr);
      return false;
    }
    return true;
  }

  void Reset() {
    in_view_.Reset();
    in_tex_ = nullptr;
    for (auto& v : out_views_) v.Reset();
    vp_.Reset();
    vpe_.Reset();
    vid_ctx_.Reset();
    vid_dev_.Reset();
    ctx_.Reset();
    dev_.Reset();
    src_w_ = src_h_ = out_w_ = out_h_ = 0;
  }

 private:
  Microsoft::WRL::ComPtr<ID3D11Device> dev_;
  Microsoft::WRL::ComPtr<ID3D11DeviceContext> ctx_;
  Microsoft::WRL::ComPtr<ID3D11VideoDevice> vid_dev_;
  Microsoft::WRL::ComPtr<ID3D11VideoContext> vid_ctx_;
  Microsoft::WRL::ComPtr<ID3D11VideoProcessorEnumerator> vpe_;
  Microsoft::WRL::ComPtr<ID3D11VideoProcessor> vp_;
  Microsoft::WRL::ComPtr<ID3D11VideoProcessorInputView> in_view_;
  ID3D11Texture2D* in_tex_ = nullptr;
  Microsoft::WRL::ComPtr<ID3D11VideoProcessorOutputView>
      out_views_[Nv12SurfacePool::kSlotCount];
  uint32_t src_w_ = 0, src_h_ = 0, out_w_ = 0, out_h_ = 0;
};

// ---- M2 Task 6: stage histograms ----
//
// Each measured stage keeps TWO bounded rings (pipeline.h StageHistogram):
// `total` (the whole run, keep-newest) and `win` (reset at every 10 s
// emission). Add is one store + index bump per ring - no per-sample
// allocation anywhere; percentiles sort a reused scratch once per emission.
//
// Semantics (all wall-clock as observed by the media loop thread; the D3D
// commands below are ASYNC enqueues on the single immediate context, so on
// hardware these are enqueue-cost upper bounds - on WARP they approximate
// execution because WARP executes inline):
//   gpu_copy_us            - the AcquireSurface call that returned kFrame:
//                            the AcquireNextFrame compositor wait + the
//                            CopyResource enqueue + ReleaseFrame.
//   gpu_convert_us         - the VideoProcessorBlt BGRA->NV12 call
//                            (Nv12Converter::Convert), successful calls only.
//   mft_submit_to_output_us- per output: its submission stamp
//                            (present_mono_us) -> the loop observing the
//                            output via TakeOutput (encoder lookahead +
//                            encode + poll cadence).
//   inflight_slots         - pool live leases observed at each submission
//                            attempt, AFTER Acquire's retire sweep (3 =
//                            saturated: the coalescing path).
//   queue_age_us           - the latest-content wait: the content's capture
//                            stamp (source_mono_us) -> its submission stamp
//                            (present_mono_us) through the depth-one mailbox.
//   capture_to_au_us       - the plan's capture->AU gate observation: the
//                            AU's source_mono_us -> publication (sink OnAu).
// Zero-sample stages are omitted from every output, never fake-zero.
struct StagePair {
  StagePair(const char* name, size_t total_cap, size_t win_cap)
      : total(name, total_cap), win(name, win_cap) {}
  void Add(uint64_t v) {
    total.Add(v);
    win.Add(v);
  }
  StageHistogram total;
  StageHistogram win;
};

// 60 s at 60 fps = 3600 samples: the totals cover the whole standard diag
// run (longer runs keep the most recent kStageTotalCap); a 10 s window is
// <= 600 samples at 60 fps, so kStageWinCap never wraps mid-window.
constexpr size_t kStageTotalCap = 4096;
constexpr size_t kStageWinCap = 1024;

struct StageHists {
  StagePair gpu_copy{"gpu_copy_us", kStageTotalCap, kStageWinCap};
  StagePair gpu_convert{"gpu_convert_us", kStageTotalCap, kStageWinCap};
  StagePair mft_submit_to_output{"mft_submit_to_output_us", kStageTotalCap,
                                 kStageWinCap};
  StagePair inflight_slots{"inflight_slots", kStageTotalCap, kStageWinCap};
  StagePair queue_age_us{"queue_age_us", kStageTotalCap, kStageWinCap};
  StagePair capture_to_au_us{"capture_to_au_us", kStageTotalCap,
                             kStageWinCap};
};

}  // namespace

// The process-wide fallback lock (Config::encoder_lock == null path).
// Function-local static: process lifetime, thread-safe initialization.
EncoderFallbackLock* ProcessEncoderLock() {
  static EncoderFallbackLock lock;
  return &lock;
}

// ---- MediaPipelineV2 ----

struct MediaPipelineV2::Impl {
  Config cfg;
  MediaMailbox mbox;
  LatestSurface latest;                    // pipeline-owned (the "caller"
                                           // of AcquireSurface)
  Nv12SurfacePool pool;
  Nv12Converter conv;
  std::unique_ptr<IEncoderSession> session;
  Microsoft::WRL::ComPtr<ID3D11Device> dev;  // adopted from the surface

  // stream state (media loop thread only)
  bool stream_inited = false;
  bool ever_inited = false;
  uint64_t capture_epoch = 1, codec_epoch = 1;
  uint64_t next_content_id = 0;
  uint64_t next_encode_seq = 0;
  FrameIdentityLedger ledger;  // submission-site monotonicity gate (M1
                               // type; see the TrySubmit note - outputs
                               // surface REORDERED on the software rung,
                               // so the M0-exact submission site is the
                               // only order-fair gate). DELIVERY-order
                               // monotonicity is restored downstream by
                               // the reorder-before-publish window.
  std::deque<uint64_t> published_seqs;  // bounded duplicate guard at
                                        // publication (belt to the
                                        // session's 1:1 mapping)
  bool AlreadyPublished(uint64_t seq) const {
    for (const uint64_t s : published_seqs)
      if (s == seq) return true;
    return false;
  }
  void NotePublished(uint64_t seq) {
    published_seqs.push_back(seq);
    if (published_seqs.size() > OutputIdentityTracker::kMaxTracked)
      published_seqs.pop_front();
  }
  // Reorder-before-publish window (fix round 1): emission order becomes
  // delivery order ONLY through here. publish_next_seq is the next
  // encode_seq that may go on the wire; everything above it parks until
  // the gap fills (or is skipped, counted, and IDR-recovered).
  std::vector<EncoderOutput> reorder_window_;  // kept sorted by encode_seq
  uint64_t publish_next_seq = 1;
  uint64_t window_park_ms = 0;      // first park time (0 = window empty)
  bool reorder_gap_idr_armed = false;  // one sticky IDR per gap incident
  std::vector<uint8_t> sps_pps;  // harvested from the session's key AUs
  bool sps_pps_missing_logged = false;
  bool have_key = false;
  bool idr_in_flight = false;   // armed subscriber IDR until the key AU
  uint64_t od_armed_ms = 0;     // on-demand window anchor
  uint32_t od_feeds = 0;
  uint64_t last_initiated_idr_ms = 0;
  uint64_t warmup_started_ms = 0;
  uint32_t warmup_gen_feeds = 0;
  bool warmup_phase_logged = false;
  uint64_t last_submit_ms = 0;
  uint64_t submit_t0 = 0, submit_count = 0;
  uint32_t spf_ms = 33;
  uint32_t warmup_feed_bound = 34;
  // 2026-08-28 QSV static-idle park (Defect 2): the hardware rung's
  // structural ~5-input emit depth parks the LAST submissions of a content
  // burst inside the MFT until further inputs arrive - on a static desktop
  // that is tens of seconds (the RERUN-2 P0 burst/stall: 4 unrecovered
  // freezes, capture->AU p95 ~59 s at native 2880x1800 while the SAME
  // binary at the SAME resolution paces at 65-110 ms under moving
  // content). The idle flush re-feeds the current surface (the spec
  // 7.4/7.5 feed mechanism, extended past warmup/IDR) when outputs are
  // owed and input has starved, bounded per content episode.
  uint32_t flush_feeds = 0;    // feeds spent on the current episode
  uint64_t flush_episodes = 0; // episodes started (diagnostics)
  // M3 Task 3 (SET_VIDEO_CONFIG): live max_w (mirrors cfg.max_width at
  // Start; SetMaxWidth stores from any thread, InitStream loads on the media
  // loop - the atomic is the whole synchronization).
  std::atomic<uint32_t> max_width{0};
  std::vector<uint8_t> shaped;
  uint64_t t0 = 0;
  uint64_t duration_ms = 0;
  uint32_t next_beat_s = 1;
  uint32_t stream_w = 0, stream_h = 0;
  uint32_t src_w = 0, src_h = 0;

  // ---- M2 Task 5: unified reset + fallback state (media loop thread) ----
  EncoderFallbackLock* enc_lock = nullptr;  // resolved at Start
  bool session_is_hw = false;   // the live session is the hardware rung
  bool hw_init_failed = false;  // the last CreateSession's hw rung failed
  // Backend fallback (Config::make_backend): the pipeline-owned swap-in
  // backend (cfg.cap/cfg.surf point into it after a swap) + which rung is
  // serving + the DXGI re-probe schedule while on GDI.
  std::unique_ptr<ICapture> owned_backend;
  MediaBackend backend = MediaBackend::kDxgi;
  uint64_t next_dxgi_probe_ms = 0;
  bool dxgi_probe_ok = false;  // probe blessed DXGI; Rebuild swaps up
  // Reset-storm accounting: consecutive executed resets carrying the
  // IDENTICAL reason space out exponentially (ResetStormBackoffMs).
  char last_exec_reason[32] = {0};
  uint32_t storm_streak = 0;
  // Recovery bounds: consecutive stream re-init failures (software/infra
  // rung; hardware attempts are bounded by the 3-strike lock instead) and
  // consecutive executed resets with no captured frame in between (a
  // rebuild that "succeeds" but never yields frames is a dead end - loud
  // fatal, never an infinite loop).
  uint32_t reinit_fail_streak = 0;
  uint32_t no_frame_resets = 0;

  // ---- M2 Task 6: stage histograms + the 10 s emission cadence ----
  StageHists stages;
  uint32_t next_hist_beat_s = 10;
  bool stages_semantics_logged = false;  // one-time preamble per run

  Result res;
  std::atomic<bool> running{false};
  std::atomic<bool> stop_now{false};
  std::thread th;
  std::string start_err;

  AuSink& sink() { return *cfg.sink; }

  bool Abort() const {
    return stop_now.load() ||
           (cfg.stop != nullptr && cfg.stop->load()) ||
           (duration_ms != 0 && NowMs() - t0 >= duration_ms) || !res.ok;
  }

  void Fatal(std::string msg) {
    res.ok = false;
    res.err = std::move(msg);
    XNC_LOG_ERROR("media_v2_fatal err=\"%s\"", res.err.c_str());
  }
};

namespace {

// The loop body, one file-static helper over the impl (keeps Impl lean).
class Loop {
 public:
  explicit Loop(MediaPipelineV2::Impl& im) : im_(im) {}

  void Run() {
    TimePeriodGuard tpg;
    im_.t0 = NowMs();
    im_.duration_ms =
        im_.cfg.duration_s != 0
            ? static_cast<uint64_t>(im_.cfg.duration_s) * 1000ull
            : 0ull;
    im_.spf_ms = 1000u / (im_.cfg.fps ? im_.cfg.fps : 30u);
    im_.warmup_feed_bound = WarmupFeedBound(im_.cfg.fps);
    XNC_LOG_INFO("media_v2_start fps=%u bitrate=%u max_w=%u duration_s=%u",
                 im_.cfg.fps, im_.cfg.bitrate_bps, im_.cfg.max_width,
                 im_.cfg.duration_s);
    for (;;) {
      if (im_.Abort()) break;
      // 1. outputs first (frees slots, publishes AUs).
      CollectOutputs();
      if (!im_.res.ok) break;
      // 2. external requests into the mailbox + the DXGI re-probe while
      // the fallback configuration serves on GDI (ruling 3).
      PollExternal();
      MaybeProbeBackend();
      // 3. commands.
      char rr[32];
      if (im_.mbox.TakeReset(rr, sizeof(rr))) {
        if (RunReset(rr)) break;
        continue;
      }
      // A reconfigure request is consumed only with a LIVE session, so it
      // SURVIVES a reset and applies to the new session at re-init (the
      // reset's config phase).
      uint32_t rb = 0, rf = 0;
      if (im_.session != nullptr &&
          im_.mbox.TakeReconfigure(&rb, &rf))
        ApplyReconfigure(rb, rf);
      PollIdrRequest();
      // 4. capture.
      if (!AcquireOnce()) break;
      // 5. submission (FPS gate + slot gate + mailbox content).
      if (im_.stream_inited) TrySubmit();
      // 6. beat.
      Beat();
    }
    // Drain: collect whatever is ready, flush the reorder window in seq
    // order (skipping any never-emitted tail gap), then tear the session
    // down (its own drain validates the tail identity mapping and
    // completes leases).
    if (im_.stream_inited) {
      CollectOutputs();
      FlushReorderWindow(true);
      if (im_.session) im_.session->Shutdown(ShutdownMode::kDrain);
      im_.session.reset();
      im_.pool.FreeRetired();
      if (im_.pool.LiveLeases() != 0)
        XNC_LOG_ERROR("media_v2_teardown live_leases=%zu (expected 0)",
                      im_.pool.LiveLeases());
    }
    im_.res.rebuilds =
        im_.cfg.cap != nullptr ? im_.cfg.cap->RebuildCount() : 0;
    im_.res.backend_at_stop = im_.backend;
    im_.res.encoder_software_locked =
        im_.enc_lock != nullptr ? im_.enc_lock->SoftwareLocked() : false;
    EmitStages(true);  // Task 6: final summary (log line + sidecar block)
    im_.sink().OnState("stream_end", im_.res.ok);
    const MediaPipelineV2::Result& r = im_.res;
    XNC_LOG_INFO("media_v2_stop elapsed=%llums captured=%llu encoded=%llu keyframes=%llu timeouts=%llu warmup_feeds=%llu resets=%u rebuilds=%u w=%u h=%u aus=%llu bytes=%llu reorder_gap_skips=%llu reorder_late_drops=%llu backend=%s cpu_readbacks=%llu ok=%d",
                 static_cast<unsigned long long>(NowMs() - im_.t0),
                 static_cast<unsigned long long>(r.captured),
                 static_cast<unsigned long long>(r.encoded),
                 static_cast<unsigned long long>(r.keyframes),
                 static_cast<unsigned long long>(r.timeouts),
                 static_cast<unsigned long long>(r.warmup_feeds), r.resets,
                 r.rebuilds, r.width, r.height,
                 static_cast<unsigned long long>(r.aus_written),
                 static_cast<unsigned long long>(r.bytes_written),
                 static_cast<unsigned long long>(r.reorder_gap_skips),
                 static_cast<unsigned long long>(r.reorder_late_drops),
                 r.encoder_backend,
                 static_cast<unsigned long long>(r.cpu_readbacks),
                 r.ok ? 1 : 0);
    im_.running.store(false);
  }

 private:
  // ---- output collection + publication ----
  //
  // Delivery-order monotonicity (fix round 1): the v2 wire contract is a
  // strictly increasing encode_seq per epoch pair in DELIVERY order (the
  // Go client's frameLedger tears down on the first same-epoch
  // seq <= last). The software rung emits AUs reordered relative to
  // submissions (measured, M1 Task 3 + v2c), so CollectOutputs runs a
  // bounded reorder-before-publish window: in-order outputs publish
  // immediately (true pairing preserved for the common case); an output
  // whose seq skips parks until the gap fills; a gap that exceeds the
  // window cap or outlives kReorderHoldMs is skipped (counted) and a
  // sticky IDR ("reorder_gap") resynchronizes decoders.
  void CollectOutputs() {
    if (!im_.session) return;
    EncoderOutput out;
    while (im_.session->TakeOutput(&out, 0)) {
      // Task 6: submit -> output-availability for THIS output (its
      // submission stamp -> the moment the loop observes it here).
      const uint64_t now_us = NowMonoUs();
      if (out.id.present_mono_us != 0 && now_us > out.id.present_mono_us)
        im_.stages.mft_submit_to_output.Add(now_us -
                                            out.id.present_mono_us);
      if (!AcceptForPublication(std::move(out))) return;  // fatal
    }
    // A gap that stopped filling (static screen: no further outputs):
    // bound the hold, then skip the gap and continue in order.
    if (!im_.reorder_window_.empty() && im_.window_park_ms != 0 &&
        NowMs() - im_.window_park_ms > kReorderHoldMs)
      FlushReorderWindow(true);
  }

  // Routes one collected output through the reorder window. False = fatal.
  bool AcceptForPublication(EncoderOutput out) {
    // M2 Task 5: outputs from RETIRED epochs are rejected here (counted,
    // logged, never fatal): a reset's discontinuity phase retired the
    // generation this output belonged to - publishing it would put an old
    // generation on the wire behind the new one's 0x020B (and re-stamp it
    // with the new generation's stream dims).
    if (out.id.capture_epoch < im_.capture_epoch) {
      im_.res.epoch_retired_drops++;
      XNC_LOG_INFO("media_v2_epoch_retired_drop seq=%llu epoch=%llu cur=%llu",
                   static_cast<unsigned long long>(out.id.encode_seq),
                   static_cast<unsigned long long>(out.id.capture_epoch),
                   static_cast<unsigned long long>(im_.capture_epoch));
      return true;
    }
    if (out.id.encode_seq == 0 || out.id.encode_seq > im_.next_encode_seq) {
      im_.Fatal("encoder_identity_mismatch");
      XNC_LOG_ERROR("encoder_identity_mismatch source=publish seq=%llu next=%llu",
                    static_cast<unsigned long long>(out.id.encode_seq),
                    static_cast<unsigned long long>(im_.next_encode_seq));
      return false;
    }
    if (out.id.encode_seq < im_.publish_next_seq) {
      // Straggler of an already-published or gap-skipped seq: publishing
      // it would regress the wire - drop it, count it.
      im_.res.reorder_late_drops++;
      XNC_LOG_INFO("media_v2_reorder_late_drop seq=%llu",
                   static_cast<unsigned long long>(out.id.encode_seq));
      return true;
    }
    if (out.id.encode_seq == im_.publish_next_seq) {
      if (!PublishNow(out)) return false;
      ++im_.publish_next_seq;
      // Drain the contiguous run that the arrival just unblocked.
      while (!im_.reorder_window_.empty() &&
             im_.reorder_window_.front().id.encode_seq ==
                 im_.publish_next_seq) {
        EncoderOutput next = std::move(im_.reorder_window_.front());
        im_.reorder_window_.erase(im_.reorder_window_.begin());
        if (!PublishNow(next)) return false;
        ++im_.publish_next_seq;
      }
      if (im_.reorder_window_.empty()) im_.window_park_ms = 0;
      return true;
    }
    ParkInWindow(std::move(out));
    if (im_.reorder_window_.size() >= kReorderWindowMax)
      FlushReorderWindow(true);
    return true;
  }

  void ParkInWindow(EncoderOutput out) {
    if (im_.window_park_ms == 0) im_.window_park_ms = NowMs();
    size_t at = 0;
    while (at < im_.reorder_window_.size() &&
           im_.reorder_window_[at].id.encode_seq < out.id.encode_seq)
      ++at;
    im_.reorder_window_.insert(im_.reorder_window_.begin() + at,
                               std::move(out));
  }

  // Publishes everything parked, in seq order. gap_skip=true (window cap
  // or hold timeout hit): the never-emitted seqs are counted as skipped
  // and one sticky IDR is armed so decoders resynchronize on the next
  // submission. Publication stays strictly increasing either way.
  void FlushReorderWindow(bool gap_skip) {
    if (im_.reorder_window_.empty()) {
      im_.window_park_ms = 0;
      return;
    }
    if (gap_skip) {
      uint64_t expect = im_.publish_next_seq;
      uint64_t skipped = 0;
      for (const auto& held : im_.reorder_window_) {
        if (held.id.encode_seq > expect)
          skipped += held.id.encode_seq - expect;
        expect = held.id.encode_seq + 1;
      }
      if (skipped != 0) {
        im_.res.reorder_gap_skips += skipped;
        if (!im_.reorder_gap_idr_armed) {
          im_.reorder_gap_idr_armed = true;
          im_.mbox.ArmIdr("reorder_gap");
        }
        XNC_LOG_INFO("media_v2_reorder_gap skipped=%llu next_on_wire=%llu (sticky IDR armed)",
                     static_cast<unsigned long long>(skipped),
                     static_cast<unsigned long long>(
                         im_.reorder_window_.front().id.encode_seq));
      }
    }
    while (!im_.reorder_window_.empty()) {
      EncoderOutput next = std::move(im_.reorder_window_.front());
      im_.reorder_window_.erase(im_.reorder_window_.begin());
      const uint64_t seq = next.id.encode_seq;
      if (!PublishNow(next)) return;  // fatal; window state is moot then
      if (seq >= im_.publish_next_seq) im_.publish_next_seq = seq + 1;
    }
    im_.window_park_ms = 0;
  }

  // The publish step itself (shape + guards + sink). False = fatal.
  bool PublishNow(const EncoderOutput& out) {
    const bool is_idr = NalHasType(out.au.data(), out.au.size(), 5);
    if (is_idr && im_.sps_pps.empty()) {
      // Harvest the session's own parameter sets for the IDR prefix
      // (the sessions do not expose MfSoftEncoder::SpsPps()).
      const uint8_t types[2] = {7, 8};
      NalExtractTypes(out.au.data(), out.au.size(), types, 2, &im_.sps_pps);
    }
    ShapeAu(out.au.data(), out.au.size(), is_idr, im_.sps_pps, &im_.shaped);
    if (im_.shaped.empty()) {
      XNC_LOG_INFO("media_v2_empty_au seq=%llu",
                   static_cast<unsigned long long>(out.id.encode_seq));
      return true;
    }
    if (is_idr && im_.sps_pps.empty() && !im_.sps_pps_missing_logged) {
      im_.sps_pps_missing_logged = true;
      XNC_LOG_INFO("media_v2_spspps_absent (backend IDR carries none)");
    }
    // Publication guard: the identity must be one of OUR submissions and
    // must never publish twice. (The M1 FrameIdentityLedger itself runs at
    // the SUBMISSION boundary in M0's exact spot - see TrySubmit - because
    // the software rung's AUs surface reordered; delivery-order
    // monotonicity is the reorder window's job above.)
    if (out.id.encode_seq > im_.next_encode_seq ||
        im_.AlreadyPublished(out.id.encode_seq)) {
      im_.Fatal("encoder_identity_mismatch");
      XNC_LOG_ERROR("encoder_identity_mismatch source=publish seq=%llu next=%llu",
                    static_cast<unsigned long long>(out.id.encode_seq),
                    static_cast<unsigned long long>(im_.next_encode_seq));
      return false;
    }
    im_.NotePublished(out.id.encode_seq);
    EncodedAU eau;
    eau.id = out.id;
    eau.width = im_.stream_w;
    eau.height = im_.stream_h;
    eau.flags = is_idr ? AuFlags::kAuFlagKey : AuFlags::kAuFlagNone;
    eau.annexb = std::make_shared<const std::vector<uint8_t>>(im_.shaped);
    im_.res.aus_written++;
    im_.res.bytes_written += im_.shaped.size();
    // Task 6: the capture->AU observation - the AU's pixel-capture stamp to
    // the shaped AU being complete (one step short of the sink's own I/O).
    {
      const uint64_t now_us = NowMonoUs();
      if (out.id.source_mono_us != 0 && now_us > out.id.source_mono_us)
        im_.stages.capture_to_au_us.Add(now_us - out.id.source_mono_us);
    }
    if (const char* err = im_.sink().OnAu(eau)) {
      im_.Fatal(err);
      XNC_LOG_ERROR("sink_onau_failed err=\"%s\"", err);
      return false;
    }
    if (is_idr) {
      im_.res.keyframes++;
      im_.have_key = true;
      im_.reorder_gap_idr_armed = false;  // a new incident may re-arm
      if (im_.idr_in_flight) {
        XNC_LOG_INFO("idr_delivered seq=%llu",
                     static_cast<unsigned long long>(out.id.encode_seq));
        im_.idr_in_flight = false;
      }
    }
    return true;
  }

  // ---- external request polling ----
  void PollExternal() {
    if (im_.cfg.reset == nullptr) return;
    char r[kResetReasonMax];
    while (im_.cfg.reset->TakeReset(r, sizeof(r))) im_.mbox.RequestReset(r);
  }

  // Subscriber-merged IDR request (spec 7.5; the M0 encode-thread poll,
  // moved into the loop): arm at most once per 500 ms, only after the
  // stream's first key AU and never while one is in flight.
  void PollIdrRequest() {
    const char* pending = im_.sink().PendingIdrReason();
    if (pending == nullptr) return;
    if (!im_.have_key || im_.idr_in_flight ||
        NowMs() - im_.last_initiated_idr_ms < kIdrMinIntervalMs)
      return;
    im_.last_initiated_idr_ms = NowMs();
    im_.mbox.ArmIdr(pending);
    im_.idr_in_flight = true;
    im_.od_armed_ms = NowMs();
    im_.od_feeds = 0;
    im_.sink().ConsumePendingIdr(pending);
    XNC_LOG_INFO("idr_request reason=%s min_interval_ms=%llu", pending,
                 static_cast<unsigned long long>(kIdrMinIntervalMs));
  }

  void ApplyReconfigure(uint32_t bitrate, uint32_t fps) {
    if (im_.session == nullptr) return;
    const bool ok = im_.session->Reconfigure(bitrate, fps);
    XNC_LOG_INFO("media_v2_reconfigure bitrate=%u fps=%u ok=%d", bitrate, fps,
                 ok ? 1 : 0);
    // M3 Task 3: re-key the config the NEXT reset re-init uses (CreateSession
    // reads cfg.bitrate_bps/cfg.fps) so a hot SET_VIDEO_CONFIG is not
    // silently reverted by a rebuild. cfg is Impl state touched only on this
    // (media loop) thread.
    if (bitrate != 0) im_.cfg.bitrate_bps = bitrate;
    if (fps != 0) {
      im_.cfg.fps = fps;
      im_.spf_ms = 1000u / fps;
      im_.warmup_feed_bound = WarmupFeedBound(fps);
      // Hot-fps fix (M4 congestion repro, 2026-08-28): TrySubmit's FPS gate
      // paces on the ABSOLUTE deadline submit_t0 + (submit_count+1)*spf_ms.
      // A hot spf change (30fps -> 5fps) re-scales the whole accumulated
      // count by the NEW spf - 171 submits x 200ms = +34s - jumping the
      // next deadline tens of seconds into the future. The loop then sleeps
      // inside the gate (15ms slices) and stops serving outputs, resets and
      // diag beats until wall time catches up (observed as a ~20s media-
      // loop stall right after the fps ladder bottomed out; stack captured
      // in a live dump: SleepEx <- Loop::TrySubmit <- Loop::Run). Rebase
      // the gate on the reconfigure: anti-drift pacing restarts from now,
      // the relative by_last term (last_submit_ms + spf) keeps the local
      // no-burst guarantee.
      im_.submit_t0 = NowMs();
      im_.submit_count = 0;
    }
  }

  // ---- capture ----
  // Returns false when the RUN must end.
  bool AcquireOnce() {
    FrameIdentity spec{};
    spec.capture_epoch = im_.capture_epoch;
    spec.codec_epoch = im_.codec_epoch;
    spec.content_id = im_.next_content_id + 1;  // speculative (capture.h)
    std::string aerr;
    // Task 6: the capture stage's wall cost, measured around the WHOLE
    // AcquireSurface call (see StageHists: compositor wait + copy enqueue).
    const uint64_t acq_t0 = NowMonoUs();
    const CaptureStatus st =
        im_.cfg.surf->AcquireSurface(im_.latest, im_.spf_ms, &spec, &aerr);
    if (st == CaptureStatus::kFrame)
      im_.stages.gpu_copy.Add(NowMonoUs() - acq_t0);
    switch (st) {
      case CaptureStatus::kFrame: {
        im_.next_content_id = spec.content_id;  // committed
        im_.res.captured++;
        im_.no_frame_resets = 0;  // a frame flowed: the last reset served
        if (!im_.stream_inited) {
          std::string ierr;
          if (!InitStream(&ierr)) {
            // M2 Task 5 (ruling 5): a failed re-init goes back through
            // the reset sequence with backoff (bounded retries, then
            // fatal) instead of the immediate fatal - and stays loud.
            return HandleInitFailure(ierr);
          }
        } else {
          // Device/dims drift (backend re-Init'ed the surface on a new
          // device or at new dims): route through the reset.
          FrameIdentity sid;
          ID3D11Texture2D* tex = nullptr;
          bool drift = im_.latest.width() != im_.src_w ||
                       im_.latest.height() != im_.src_h;
          if (!drift && im_.latest.Snapshot(&sid, &tex) && tex != nullptr) {
            Microsoft::WRL::ComPtr<ID3D11Device> d;
            tex->GetDevice(d.GetAddressOf());  // void; null check below
            if (d.Get() != im_.dev.Get()) drift = true;
            tex->Release();
          }
          if (drift) {
            im_.mbox.RequestReset(kResetReasonResolution);
            Sleep(kIdleSleepMs);
            return true;
          }
        }
        im_.mbox.PublishContent(spec);  // depth-one cell (coalescing)
        im_.flush_feeds = 0;  // new content: a fresh park-flush episode
        if (im_.warmup_started_ms == 0) im_.warmup_started_ms = NowMs();
        return true;
      }
      case CaptureStatus::kNoChange:
        im_.res.timeouts++;
        IdleFeed();
        return true;
      case CaptureStatus::kRetry:
        return true;  // backend healed in place; call again
      case CaptureStatus::kAccessLost: {
        const bool away = im_.cfg.reset != nullptr &&
                          im_.cfg.reset->Desktop() == ResetDesktop::kNonDefault;
        im_.mbox.RequestReset(away ? kResetReasonDesktopSwitch
                                   : kResetReasonAccessLost);
        Sleep(kIdleSleepMs);  // debounce window (repeated errors merge)
        return true;
      }
      case CaptureStatus::kFatal:
      default:
        // M2 Task 5: with a backend-fallback configuration (ruling 3), a
        // hard capture error routes into the unified reset under the TOP
        // severity reason - the rebuild loop falls to GDI when DXGI
        // cannot be rebuilt. Without fallback factories the Task 4
        // contract stands: fatal, loud.
        if (im_.cfg.make_backend != nullptr) {
          im_.mbox.RequestReset(kResetReasonDeviceRemoved);
          im_.sink().OnState("capture_failed", true);
          XNC_LOG_ERROR("capture_fatal_reset err=\"%s\" (backend fallback armed)",
                        aerr.c_str());
          Sleep(kIdleSleepMs);  // debounce window (repeated errors merge)
          return true;
        }
        im_.Fatal(aerr.empty() ? "acquire failed" : aerr);
        im_.sink().OnState("capture_fatal", false);
        return false;
    }
  }

  // Idle re-feed (spec 7.4/7.5; the M0 timeout-path semantics): on a
  // static screen, re-publish the surface's current identity so the
  // encoder's lookahead fills and the (possibly forced) IDR emerges.
  // Bounded by WarmupFeedBound and the 2 s wall clock; paced to spf.
  // 2026-08-28: extended with the static-idle PARK flush - the encoder
  // rungs park the tail of a content burst inside their emit depth (QSV
  // ~5 inputs, the software MFT ~17) and only emit when MORE inputs
  // arrive; without a flush a static desktop freezes the stream for the
  // whole gap (the RERUN-2 P0). XNC_QSV_IDLE_FLUSH=1 arms it (A/B gate).
  void IdleFeed() {
    if (im_.last_submit_ms != 0 &&
        NowMs() - im_.last_submit_ms < im_.spf_ms)
      return;
    const uint64_t elapsed = im_.warmup_started_ms != 0
                                 ? NowMs() - im_.warmup_started_ms
                                 : 0;
    const bool warmup_ok = !im_.have_key &&
                           im_.warmup_gen_feeds < im_.warmup_feed_bound &&
                           elapsed < kWarmupWallBoundMs;
    const bool ondemand_ok =
        im_.have_key && im_.idr_in_flight &&
        im_.od_feeds < im_.warmup_feed_bound &&
        (im_.od_armed_ms == 0 ||
         NowMs() - im_.od_armed_ms < kWarmupWallBoundMs);
    if (warmup_ok || ondemand_ok) {
      FrameIdentity sid;
      ID3D11Texture2D* tex = nullptr;
      if (im_.latest.Snapshot(&sid, &tex)) {
        if (tex != nullptr) tex->Release();
        im_.mbox.PublishContent(sid);  // same content, new seq at submit
        if (!im_.have_key) {
          ++im_.warmup_gen_feeds;
          ++im_.res.warmup_feeds;
        } else {
          ++im_.od_feeds;
          ++im_.res.warmup_feeds;
        }
        return;
      }
    }
    // Static-idle park flush: outputs are owed (submissions the rung has
    // not emitted yet) and input has starved for > kFlushAfterSpf slots ->
    // re-feed the SAME surface so the parked tail emits within ~depth*spf
    // instead of at the next desktop change. Bounded per content episode
    // (reset when a real frame arrives); no-op on a healthy paced feed
    // (last_submit_ms advances every slot, so the starvation predicate
    // never qualifies) and on the software rung (outputs surface at
    // submit, owed stays ~0).
    if (IdleFlushEnabled() && im_.have_key && !im_.idr_in_flight &&
        im_.last_submit_ms != 0 &&
        NowMs() - im_.last_submit_ms > kFlushAfterSpf * im_.spf_ms &&
        im_.flush_feeds < kFlushFeedBound &&
        im_.res.encoded > im_.res.aus_written) {
      FrameIdentity sid;
      ID3D11Texture2D* tex = nullptr;
      if (im_.latest.Snapshot(&sid, &tex)) {
        if (tex != nullptr) tex->Release();
        if (im_.flush_feeds == 0) {
          ++im_.flush_episodes;
          XNC_LOG_INFO("idle_flush_begin owed=%llu starved_ms=%llu",
                       static_cast<unsigned long long>(im_.res.encoded -
                                                       im_.res.aus_written),
                       static_cast<unsigned long long>(NowMs() -
                                                       im_.last_submit_ms));
        }
        ++im_.flush_feeds;
        im_.mbox.PublishContent(sid);  // same content, new seq at submit
        return;
      }
    }
    PhaseOutcomeLogs();
  }

  void PhaseOutcomeLogs() {
    const bool have_base = im_.next_content_id != 0;
    if (!im_.warmup_phase_logged && have_base) {
      if (im_.have_key) {
        im_.warmup_phase_logged = true;
        XNC_LOG_INFO("warmup_done feeds=%u keyframes=%llu",
                     im_.warmup_gen_feeds,
                     static_cast<unsigned long long>(im_.res.keyframes));
      } else if (im_.warmup_gen_feeds >= im_.warmup_feed_bound ||
                 (im_.warmup_started_ms != 0 &&
                  NowMs() - im_.warmup_started_ms >= kWarmupWallBoundMs)) {
        im_.warmup_phase_logged = true;
        XNC_LOG_INFO("warmup_exhausted feeds=%u bound=%u",
                     im_.warmup_gen_feeds, im_.warmup_feed_bound);
      }
    }
    if (im_.idr_in_flight && have_base &&
        im_.od_feeds >= im_.warmup_feed_bound) {
      XNC_LOG_INFO("idr_feed_exhausted feeds=%u bound=%u", im_.od_feeds,
                   im_.warmup_feed_bound);
      im_.idr_in_flight = false;  // surfaces with the next real frame batch
    }
  }

  // ---- submission ----
  void TrySubmit() {
    // FPS gate: absolute-index deadline (anti-drift) floored by a relative
    // spf since the last submission (no back-to-back bursts after a static
    // gap). The wait is sliced so outputs still get collected.
    if (im_.submit_t0 == 0) im_.submit_t0 = NowMs();
    for (;;) {
      const uint64_t by_index =
          im_.submit_t0 + (im_.submit_count + 1) * im_.spf_ms;
      const uint64_t by_last =
          im_.last_submit_ms != 0 ? im_.last_submit_ms + im_.spf_ms : 0;
      uint64_t deadline = by_index;
      if (by_last != 0 && by_last > deadline) deadline = by_last;
      const uint64_t now = NowMs();
      if (now >= deadline) break;
      uint64_t remain = deadline - now;
      if (remain > kIdleSleepMs) remain = kIdleSleepMs;
      Sleep(static_cast<DWORD>(remain));
      CollectOutputs();
      if (!im_.res.ok || im_.Abort()) return;
    }
    // Slot gate FIRST: acquiring a lease before taking the mailbox content
    // guarantees a taken identity is always submitted (the coalescing
    // contract only drops content that was REPLACED, never taken content).
    SurfaceLease* lease = im_.pool.Acquire();
    // Task 6: in-flight occupancy observed at the submission attempt
    // (Acquire swept retired slots first; 3 = saturated = coalescing path).
    im_.stages.inflight_slots.Add(
        static_cast<uint64_t>(im_.pool.LiveLeases()));
    if (lease == nullptr) return;  // all three slots busy: mailbox keeps
                                   // the latest; nothing is lost
    FrameIdentity mid;
    if (!im_.mbox.TakeContent(&mid)) {
      lease->Release();
      return;
    }
    FrameIdentity sid;
    ID3D11Texture2D* tex = nullptr;
    if (!im_.latest.Snapshot(&sid, &tex) || tex == nullptr) {
      lease->Release();
      im_.Fatal("latest snapshot failed");
      return;
    }
    if (sid.content_id != mid.content_id ||
        sid.capture_epoch != mid.capture_epoch) {
      tex->Release();
      lease->Release();
      im_.Fatal("content pair mismatch (surface vs mailbox)");
      return;
    }
    FrameIdentity id = mid;
    id.encode_seq = ++im_.next_encode_seq;
    id.present_mono_us = NowMonoUs();  // stamped at submission (ruling 3)
    // Task 6: queue age - the latest-content wait: this content's capture
    // stamp to its submission stamp (depth-one mailbox + slot/FPS gates).
    if (mid.source_mono_us != 0 && id.present_mono_us > mid.source_mono_us)
      im_.stages.queue_age_us.Add(id.present_mono_us - mid.source_mono_us);
    // Ruling 3: the identity runs through the M1 FrameIdentityLedger at
    // the submission boundary - the M0 pipeline's exact site (pipeline.cpp
    // accepts at the input boundary). Every AU that can reach the wire was
    // accepted here in strictly monotonic submission order; CollectOutputs
    // additionally guards publication against unknown/duplicated seqs.
    if (!im_.ledger.Accept(id)) {
      tex->Release();
      lease->Release();
      im_.Fatal("encoder_identity_monotonicity");
      XNC_LOG_ERROR("encoder_identity_monotonicity cap_epoch=%llu codec_epoch=%llu content=%llu seq=%llu",
                    static_cast<unsigned long long>(id.capture_epoch),
                    static_cast<unsigned long long>(id.codec_epoch),
                    static_cast<unsigned long long>(id.content_id),
                    static_cast<unsigned long long>(id.encode_seq));
      return;
    }
    char idr_reason[32];
    const bool force = im_.mbox.TakeIdr(idr_reason, sizeof(idr_reason));
    std::string cerr_;
    // Task 6: the conversion stage's wall cost (VideoProcessorBlt).
    const uint64_t conv_t0 = NowMonoUs();
    const bool conv_ok = im_.conv.Convert(tex, *lease, &cerr_);
    const uint64_t conv_us = NowMonoUs() - conv_t0;
    tex->Release();
    if (conv_ok) im_.stages.gpu_convert.Add(conv_us);
    if (!conv_ok) {
      lease->Release();
      if (LooksDeviceRemoved(cerr_)) {
        // Dead device: the unified reset rebuilds on a fresh device (or
        // falls to GDI) under the top-severity reason - not a fatal.
        im_.mbox.RequestReset(kResetReasonDeviceRemoved);
        XNC_LOG_ERROR("convert_device_removed err=\"%s\" (reset)",
                      cerr_.c_str());
        return;
      }
      im_.Fatal("convert: " + cerr_);
      return;
    }
    const SubmitResult r = im_.session->Submit(id, std::move(*lease), force);
    if (r != SubmitResult::kOk) {
      // M2 Task 5 (rulings 3+5): a HARDWARE session that breaks its
      // contract is one strike on the process-lifetime fallback lock, and
      // the recovery rides the unified reset (bounded: <= 3 hardware
      // attempts per process, then the software rung serves) - never an
      // immediate fatal. The software rung breaking the contract is the
      // M1 fatal guarantee, unchanged.
      if (im_.session_is_hw) {
        StrikeHw("submit");
        im_.mbox.RequestReset(kResetReasonEncoder);
        return;
      }
      im_.Fatal("encoder submit rejected (result=" + std::to_string(static_cast<int>(r)) + ")");
      return;
    }
    im_.res.encoded++;
    // Task 6: the INTERNAL software rung's Submit performs exactly one
    // staging Map (its designed fallback cost, mf_gpu_encoder.cpp); the
    // hardware rung and the test factories perform none, and the
    // capture/convert stages never do (read alongside encoder_backend).
    if (!im_.session_is_hw && im_.cfg.session_factory == nullptr)
      im_.res.cpu_readbacks++;
    im_.submit_count++;
    im_.last_submit_ms = NowMs();
    if (force)
      XNC_LOG_INFO("idr_submitted seq=%llu reason=%s",
                   static_cast<unsigned long long>(id.encode_seq), idr_reason);
    CollectOutputs();  // CPU rung surfaces outputs at Submit
  }

  // ---- stream init (first frame of a generation) ----
  bool InitStream(std::string* err) {
    FrameIdentity sid;
    ID3D11Texture2D* tex = nullptr;
    if (!im_.latest.Snapshot(&sid, &tex) || tex == nullptr) {
      if (err) *err = "no surface snapshot";
      return false;
    }
    Microsoft::WRL::ComPtr<ID3D11Device> sdev;
    tex->GetDevice(sdev.GetAddressOf());  // void; the AddRef'd out param
    tex->Release();
    if (sdev == nullptr) {
      if (err) *err = "surface texture has no device";
      return false;
    }
    im_.dev = sdev;
    im_.src_w = im_.latest.width();
    im_.src_h = im_.latest.height();
    uint32_t ow = im_.src_w, oh = im_.src_h;
    const uint32_t max_w = im_.max_width.load();  // M3 T3: live (SetMaxWidth)
    if (max_w > 0) {
      uint32_t sw = 0, sh = 0;
      if (!GpuScaledDims(im_.src_w, im_.src_h, Rotate::kNone,
                         max_w, &sw, &sh) ||
          (sw % 2) != 0 || (sh % 2) != 0) {
        if (err) *err = "scaled dims rejected (nv12 needs even)";
        return false;
      }
      ow = sw;
      oh = sh;
    }
    if ((ow % 2) != 0 || (oh % 2) != 0) {
      if (err) *err = "odd capture dims (nv12 needs even)";
      return false;
    }
    std::string ierr;
    if (!im_.pool.Init(im_.dev.Get(), ow, oh, &ierr)) {
      if (err) *err = "pool: " + ierr;
      return false;
    }
    if (!im_.conv.Init(im_.dev.Get(), im_.src_w, im_.src_h, ow, oh, &im_.pool,
                       &ierr)) {
      if (err) *err = "converter: " + ierr;
      return false;
    }
    if (!CreateSession(ow, oh, &ierr)) {
      if (err) *err = "session: " + ierr;
      return false;
    }
    // A NEW session means a NEW SPS/PPS cache.
    im_.sps_pps.clear();
    im_.sps_pps_missing_logged = false;
    im_.reinit_fail_streak = 0;  // re-init succeeded: the streak clears
    im_.stream_w = ow;
    im_.stream_h = oh;
    im_.res.width = ow;
    im_.res.height = oh;
    if (!im_.ever_inited && !im_.mbox.idr_armed()) {
      im_.mbox.ArmIdr("base");  // the stream starts from an IDR
    }
    im_.ever_inited = true;
    im_.stream_inited = true;
    XNC_LOG_INFO("media_v2_stream_init src=%ux%u out=%ux%u backend=%s friendly=\"%s\"",
                 im_.src_w, im_.src_h, ow, oh, im_.res.encoder_backend,
                 im_.res.encoder_friendly.c_str());
    return true;
  }

  bool CreateSession(uint32_t w, uint32_t h, std::string* err) {
    im_.session_is_hw = false;
    im_.hw_init_failed = false;
    // Hardware rung - skipped entirely once the process-lifetime lock
    // tripped (three contract failures) or on --encoder software. The
    // seam semantics: hw_session_factory IS the hardware rung (injected
    // contract failures); a plain session_factory REPLACES THE WHOLE
    // LADDER (the Task 4 shape - no hardware attempt, no strikes); with
    // neither factory the real MfGpuEncoder runs (production).
    const bool hw_allowed =
        !im_.cfg.force_software_encoder && im_.enc_lock != nullptr &&
        !im_.enc_lock->SoftwareLocked();
    if (!hw_allowed) {
      if (im_.enc_lock != nullptr && im_.enc_lock->SoftwareLocked())
        XNC_LOG_INFO("media_v2_gpu_session_skipped (software locked: %u strikes)",
                     im_.enc_lock->failures());
    } else if (im_.cfg.hw_session_factory != nullptr) {
      // The test seam for injected contract failures (ruling 3): a null
      // return models the hardware encoder failing its Init contract.
      IEncoderSession* s =
          im_.cfg.hw_session_factory(im_.cfg.hw_session_ctx, &im_.pool);
      if (s != nullptr) {
        im_.session.reset(s);
        im_.session_is_hw = true;
        im_.res.encoder_backend = "factory-hw";
        im_.res.encoder_friendly = "(test hardware session)";
        return true;
      }
      im_.hw_init_failed = true;
      StrikeHw("init");
      if (err) *err = "hardware factory returned null";
    } else if (im_.cfg.session_factory == nullptr) {
      auto gpu = std::make_unique<MfGpuEncoder>();
      std::string gerr;
      if (gpu->Init(im_.dev.Get(), &im_.pool, w, h, im_.cfg.fps,
                    im_.cfg.bitrate_bps, &gerr)) {
        im_.res.encoder_backend = gpu->BackendName();
        im_.res.encoder_friendly = gpu->FriendlyName();
        im_.session = std::move(gpu);
        im_.session_is_hw = true;
        return true;
      }
      im_.hw_init_failed = true;
      StrikeHw("init");
      XNC_LOG_INFO("media_v2_gpu_session_unavailable err=\"%s\" (software rung)",
                   gerr.c_str());
      if (err) *err = gerr;
    }
    // Software rung: the test seam, else the internal CPU encoder.
    if (im_.cfg.session_factory != nullptr) {
      IEncoderSession* s =
          im_.cfg.session_factory(im_.cfg.session_ctx, &im_.pool);
      if (s == nullptr) {
        if (err) *err = "factory returned null";
        return false;
      }
      im_.session.reset(s);
      im_.res.encoder_backend = "factory";
      im_.res.encoder_friendly = "(test factory session)";
      return true;
    }
    auto cpu = std::make_unique<MfCpuEncoder>();
    if (!cpu->Init(im_.dev.Get(), &im_.pool, w, h, im_.cfg.fps,
                   im_.cfg.bitrate_bps, err)) {
      return false;
    }
    im_.res.encoder_backend = cpu->BackendName();
    im_.res.encoder_friendly = cpu->FriendlyName();
    im_.session = std::move(cpu);
    return true;
  }

  // ---- M2 Task 5 helpers ----

  // One hardware-encoder CONTRACT FAILURE (Init probe or a live session
  // fault): strike the process-lifetime lock and surface it loudly.
  void StrikeHw(const char* what) {
    if (im_.enc_lock == nullptr) return;
    const bool locked_now = im_.enc_lock->NoteHwFailure();
    im_.res.hw_contract_failures = im_.enc_lock->failures();
    XNC_LOG_ERROR("media_v2_hw_contract_failure what=%s strikes=%u%s",
                  what, im_.res.hw_contract_failures,
                  locked_now ? " (SOFTWARE LOCKED)" : "");
    im_.sink().OnState("encoder_hw_strike", true);
    if (locked_now) im_.sink().OnState("encoder_software_locked", true);
  }

  // InitStream failed (ruling 5): go back through the reset sequence with
  // backoff - bounded retries, then fatal; loud throughout. Hardware-rung
  // failures are bounded by the 3-strike lock instead (after three the
  // software rung serves). Returns false only when the RUN must end.
  bool HandleInitFailure(const std::string& err) {
    const bool hw_rung = im_.hw_init_failed;
    if (!hw_rung) {
      ++im_.reinit_fail_streak;
      if (im_.reinit_fail_streak >= kResetHardFailStreak) {
        im_.sink().OnState("capture_fatal", false);
        im_.Fatal("media_v2_stream_init: " + err);
        return false;
      }
    }
    im_.sink().OnState("capture_failed", true);  // still retrying: loud
    XNC_LOG_ERROR("media_v2_stream_init_failed hw_rung=%d streak=%u err=\"%s\""
                  " (reset sequence retries)",
                  hw_rung ? 1 : 0, im_.reinit_fail_streak, err.c_str());
    im_.mbox.RequestReset(hw_rung || err.rfind("session: ", 0) == 0
                              ? kResetReasonEncoder
                              : kResetReasonDeviceRemoved);
    return true;
  }

  // Swaps the desktop backend during a reset (ruling 3). The pipeline
  // OWNS factory-created backends; the caller-owned initial backend
  // (cfg.cap at Start) is never freed here. False = the factory could not
  // produce a usable rung (keep serving on the current one). The shared
  // LatestSurface was already fully Reset by the reset's phase 3 (runs
  // before any swap): the new rung's first AcquireSurface re-Inits it on
  // the new backend's device - the swap never inherits the old device's
  // texture (final-review fix 2026-08).
  bool SwapBackend(MediaBackend kind, const char* reason) {
    if (im_.cfg.make_backend == nullptr) return false;
    std::string berr;
    std::unique_ptr<ICapture> made =
        im_.cfg.make_backend(im_.cfg.backend_ctx, kind, &berr);
    if (made == nullptr) {
      XNC_LOG_ERROR("backend_swap %s create failed err=\"%s\"",
                    MediaBackendName(kind), berr.c_str());
      return false;
    }
    ICaptureSurface* s = dynamic_cast<ICaptureSurface*>(made.get());
    if (s == nullptr) {
      XNC_LOG_ERROR("backend_swap %s not an ICaptureSurface",
                    MediaBackendName(kind));
      return false;
    }
    im_.owned_backend = std::move(made);  // frees the previous swap-in
    im_.cfg.cap = im_.owned_backend.get();
    im_.cfg.surf = s;
    im_.backend = kind;
    im_.res.backend_swaps++;
    XNC_LOG_INFO("media_v2_backend_swap backend=%s reason=%s swaps=%u",
                 MediaBackendName(kind), reason, im_.res.backend_swaps);
    im_.sink().OnState("backend_changed", true);
    return true;
  }

  // The DXGI re-probe while serving on GDI (ruling 3: every
  // dxgi_reprobe_ms - 30 s in production). Runs on the media loop thread
  // between iterations; a healthy probe arms the upgrade and requests a
  // change_backend reset, so the RETURN rides the same reset sequence as
  // every other rebuild. Throwaway probe: the probe capture is released
  // and the swap creates a fresh backend (the ladder's pattern).
  void MaybeProbeBackend() {
    if (im_.cfg.make_backend == nullptr ||
        im_.cfg.dxgi_reprobe_ms == 0 ||
        im_.backend != MediaBackend::kGdi || !im_.stream_inited ||
        im_.mbox.reset_pending())
      return;
    const uint64_t now = NowMs();
    if (im_.next_dxgi_probe_ms == 0)
      im_.next_dxgi_probe_ms = now + im_.cfg.dxgi_reprobe_ms;
    if (now < im_.next_dxgi_probe_ms) return;
    im_.next_dxgi_probe_ms = now + im_.cfg.dxgi_reprobe_ms;
    std::string perr;
    std::unique_ptr<ICapture> probe = im_.cfg.make_backend(
        im_.cfg.backend_ctx, MediaBackend::kDxgi, &perr);
    bool healthy = false;
    if (probe != nullptr) {
      FrameBlob blob;
      std::string aerr;
      healthy = DxgiProbeOutcomeHealthy(probe->Acquire(blob, &aerr),
                                        aerr.c_str());
    }
    if (healthy) {
      im_.res.dxgi_probes_ok++;
      im_.dxgi_probe_ok = true;
      XNC_LOG_INFO("media_v2_dxgi_probe ok=1 (change_backend reset)");
      im_.mbox.RequestReset(kResetReasonChangeBackend);
    } else {
      im_.res.dxgi_probes_failed++;
      XNC_LOG_INFO("media_v2_dxgi_probe ok=0 err=\"%s\"", perr.c_str());
    }
  }

  // Records one reset phase (Result::reset_phases) + the compact log the
  // field diagnosis reads. The phases of one executed reset are the spec
  // sequence, in order.
  void Phase(ResetPhase p) {
    im_.res.reset_phases.push_back(p);
    XNC_LOG_INFO("capture_reset_phase %s", ResetPhaseName(p));
  }

  // ---- reset (M2 Task 5: the unified, phased reset sequence on the
  // single loop thread) ----
  //
  // discontinuity (0x020B - the retired generation's pending outputs are
  // rejected from here; subscribers see the discontinuity with the new
  // epoch's first AU) -> stop submissions -> retire leases -> rebuild
  // (+ backend fallback swaps, ruling 3) -> base -> config -> IDR ->
  // running. ONE epoch increment per executed reset; repeated identical
  // reasons back off exponentially (ResetStormBackoffMs); every phase is
  // recorded (Result::reset_phases) so the order is assertable.
  // Returns true when the RUN must end.
  bool RunReset(const char* reason) {
    const uint64_t ts = NowMs();
    // Storm backoff: repeated identical reasons space out exponentially
    // (abortable, sliced - never an unbounded wait).
    const bool same = std::strcmp(reason, im_.last_exec_reason) == 0;
    im_.storm_streak = same ? im_.storm_streak + 1 : 1;
    CopyReason(im_.last_exec_reason, sizeof(im_.last_exec_reason), reason);
    const uint32_t storm_ms =
        ResetStormBackoffMs(im_.cfg.reset_backoff_base_ms, kResetStormCapMs,
                            im_.storm_streak);
    im_.res.reset_storm_ms.push_back(storm_ms);
    for (uint32_t slept = 0; slept < storm_ms; slept += kResetPollMs) {
      if (im_.Abort()) return true;
      Sleep(kResetPollMs);
    }
    // Dead-end bound: executed resets that never see a frame between them
    // are a rebuild that cannot serve - loud fatal, never an infinite
    // loop (kFrame clears the counter).
    if (++im_.no_frame_resets > kResetHardFailStreak) {
      im_.sink().OnState("capture_fatal", false);
      im_.Fatal("capture_reset_no_recovery: rebuilds complete but no frame");
      return true;
    }
    XNC_LOG_INFO("capture_reset_start reason=%s storm_streak=%u storm_ms=%u",
                 reason, im_.storm_streak, storm_ms);
    im_.sink().OnState("recovering", true);

    // Phase 1 - discontinuity: retire the current generation. Parked
    // reorder-window outputs of the retiring epoch are dropped now
    // (counted); AcceptForPublication additionally rejects any straggler
    // from an earlier epoch, so no retired-generation AU can follow its
    // successor on the wire.
    Phase(ResetPhase::kDiscontinuity);
    if (!im_.reorder_window_.empty()) {
      size_t retired = 0;
      for (auto it = im_.reorder_window_.begin();
           it != im_.reorder_window_.end();) {
        if (it->id.capture_epoch <= im_.capture_epoch) {
          ++retired;
          it = im_.reorder_window_.erase(it);
        } else {
          ++it;
        }
      }
      if (retired != 0) {
        im_.res.epoch_retired_drops += retired;
        if (im_.reorder_window_.empty()) im_.window_park_ms = 0;
        XNC_LOG_INFO("capture_reset_retired_outputs dropped=%zu", retired);
      }
    }

    // Phase 2 - stop submissions: drop pending pre-reset content and
    // close the submission gate (stream_inited=false stops TrySubmit; it
    // re-opens at the new base frame's InitStream).
    Phase(ResetPhase::kStopSubmissions);
    FrameIdentity drop;
    while (im_.mbox.TakeContent(&drop)) {
    }
    im_.stream_inited = false;

    // Phase 3 - retire leases: tear the session (its Shutdown completes
    // every outstanding lease), sweep the pool, RESET the surface - not
    // merely Invalidate (final-review fix 2026-08): the texture AND its
    // device pairing die with the rebuild. The next backend's first
    // AcquireSurface re-Inits the surface on ITS device, so a swap to GDI
    // at identical dims (a duplicated primary) can never keep serving the
    // dead DXGI device's frozen pixels through a cross-device CopyFrom.
    Phase(ResetPhase::kRetireLeases);
    if (im_.session) {
      im_.session->Shutdown(ShutdownMode::kImmediate);
      im_.session.reset();
    }
    im_.pool.RetireAll();
    im_.pool.FreeRetired();
    im_.latest.Reset();

    // Phase 4 - rebuild. Wait out a secure desktop first (immediate
    // rebuilds are futile there), then rebuild with retry/backoff; a DXGI
    // rung that cannot be rebuilt falls to GDI THROUGH THIS RESET, and a
    // probe-blessed DXGI returns from GDI here (ruling 3).
    Phase(ResetPhase::kRebuild);
    while (im_.cfg.reset != nullptr &&
           im_.cfg.reset->Desktop() == ResetDesktop::kNonDefault) {
      if (im_.Abort()) return true;
      Sleep(kResetPollMs);
    }
    uint32_t streak = 0;
    bool failed_state = false;
    uint32_t old_w = im_.cfg.cap != nullptr ? im_.cfg.cap->Width() : 0;
    uint32_t old_h = im_.cfg.cap != nullptr ? im_.cfg.cap->Height() : 0;
    const uint32_t backoff_base = im_.cfg.reset_backoff_base_ms != 0
                                       ? im_.cfg.reset_backoff_base_ms
                                       : kResetBackoffMs;
    const uint32_t backoff_hard = backoff_base * 2;
    for (;;) {
      if (im_.Abort()) return true;
      // Upgrade first: GDI serving + a probe-blessed DXGI -> swap up. A
      // fresh DXGI that breaks between probe and swap keeps GDI alive
      // (never strand the reset loop; the next probe re-arms).
      if (im_.backend == MediaBackend::kGdi && im_.dxgi_probe_ok) {
        im_.dxgi_probe_ok = false;
        if (SwapBackend(MediaBackend::kDxgi, "probe")) break;
      }
      std::string rerr;
      if (im_.cfg.cap == nullptr || im_.cfg.cap->Rebuild(&rerr)) break;
      ++streak;
      XNC_LOG_ERROR("capture_reset_rebuild_failed streak=%u err=\"%s\"", streak,
                    rerr.c_str());
      // DXGI failure falls to GDI through the same reset sequence (the
      // swap IS the rebuild's success; the reset completes on GDI).
      if (streak >= kResetHardFailStreak &&
          im_.backend == MediaBackend::kDxgi &&
          SwapBackend(MediaBackend::kGdi, "dxgi_rebuild_failed")) {
        break;
      }
      if (streak >= kResetHardFailStreak && !failed_state) {
        failed_state = true;
        im_.sink().OnState("capture_failed", true);  // still retrying
      }
      const uint32_t backoff = failed_state ? backoff_hard : backoff_base;
      for (uint32_t slept = 0; slept < backoff; slept += kResetPollMs) {
        if (im_.Abort()) return true;
        Sleep(kResetPollMs);
        while (im_.cfg.reset != nullptr &&
               im_.cfg.reset->Desktop() == ResetDesktop::kNonDefault) {
          if (im_.Abort()) return true;
          Sleep(kResetPollMs);
        }
      }
    }

    // Phase 5 - base: ONE epoch increment per executed reset (the rebuild
    // EVENT); the next kFrame re-inits the stream (new device/dims are
    // unknown until then) and becomes the new base frame (WAIT_BASE).
    Phase(ResetPhase::kBase);
    im_.capture_epoch++;
    im_.codec_epoch++;
    im_.warmup_started_ms = 0;
    im_.warmup_gen_feeds = 0;
    im_.warmup_phase_logged = false;
    // The rebuild is a content-episode boundary for the idle park flush
    // too (review IMPORTANT 2, 2026-08-28). Defense-in-depth rather than
    // load-bearing today: phase 3 below tears the SESSION down, so a
    // post-rebuild flush always requires the new base kFrame - which
    // already replenishes via AcquireOnce's kFrame branch (measured: the
    // v2t selftest pin passes with this line removed). Kept so the budget
    // is correct by construction at every episode boundary, independent
    // of the re-init path's shape.
    im_.flush_feeds = 0;

    // Phase 6 - config: a pending reconfigure request SURVIVES the reset
    // (the loop consumes it only with a live session) and applies to the
    // new session at re-init.
    Phase(ResetPhase::kConfig);

    // Phase 7 - IDR: the sticky rebuild IDR rides the new generation's
    // first submission.
    Phase(ResetPhase::kIdr);
    im_.mbox.ArmIdr("rebuild");

    // Phase 8 - running: the reset is accounted, subscribers hear it, and
    // a geometry change surfaces as DISPLAY_CHANGED.
    Phase(ResetPhase::kRunning);
    im_.res.resets++;
    CopyReason(im_.res.last_reset_reason, sizeof(im_.res.last_reset_reason),
               reason);
    im_.sink().OnState("capture_rebuilt", true);
    const uint32_t new_w = im_.cfg.cap != nullptr ? im_.cfg.cap->Width() : 0;
    const uint32_t new_h = im_.cfg.cap != nullptr ? im_.cfg.cap->Height() : 0;
    if (new_w != old_w || new_h != old_h)
      im_.sink().OnDisplayChanged(new_w, new_h, reason);
    XNC_LOG_INFO("capture_reset_done reason=%s w=%u h=%u elapsed_ms=%llu",
                 reason, new_w, new_h,
                 static_cast<unsigned long long>(NowMs() - ts));
    return false;
  }

  // ---- M2 Task 6: stage-histogram emission ----
  // Window lines every 10 s (per-window rings, reset right after each
  // emission); the final summary renders the whole-run rings into
  // Result::stages_json (for the stats.json sidecar) plus the
  // media_v2_stages_final log line. Zero-sample stages are omitted from
  // both outputs; an all-empty window emits nothing at all.
  void EmitStages(bool final_summary) {
    StageStat stats[6];
    size_t n = 0;
    const auto push = [&stats, &n](const StageHistogram& h) {
      if (n >= sizeof(stats) / sizeof(stats[0])) return;
      stats[n].key = h.name();
      stats[n].p = h.Summary();
      if (stats[n].p.has_samples) ++n;  // zero-sample: absent, not zero
    };
    if (final_summary) {
      push(im_.stages.gpu_copy.total);
      push(im_.stages.gpu_convert.total);
      push(im_.stages.mft_submit_to_output.total);
      push(im_.stages.inflight_slots.total);
      push(im_.stages.queue_age_us.total);
      push(im_.stages.capture_to_au_us.total);
    } else {
      push(im_.stages.gpu_copy.win);
      push(im_.stages.gpu_convert.win);
      push(im_.stages.mft_submit_to_output.win);
      push(im_.stages.inflight_slots.win);
      push(im_.stages.queue_age_us.win);
      push(im_.stages.capture_to_au_us.win);
    }
    const std::string line = FormatStageLog(stats, n);
    if (line.empty()) return;
    const uint32_t elapsed_s =
        static_cast<uint32_t>((NowMs() - im_.t0) / 1000);
    // Fix round 1: the semantics ride the EMITTED output, not just the
    // source comments - once per run, ahead of the first histogram line.
    if (!im_.stages_semantics_logged) {
      im_.stages_semantics_logged = true;
      XNC_LOG_INFO("media_v2_stages_semantics note=\"%s\"",
                   StageSemanticsNote());
    }
    XNC_LOG_INFO("media_v2_stages%s elapsed=%us %s",
                 final_summary ? "_final" : "", elapsed_s, line.c_str());
    if (final_summary) {
      im_.res.stages_json = FormatStagesJson(stats, n, im_.res.cpu_readbacks,
                                             im_.res.encoder_backend);
      return;
    }
    im_.stages.gpu_copy.win.Reset();
    im_.stages.gpu_convert.win.Reset();
    im_.stages.mft_submit_to_output.win.Reset();
    im_.stages.inflight_slots.win.Reset();
    im_.stages.queue_age_us.win.Reset();
    im_.stages.capture_to_au_us.win.Reset();
  }

  void Beat() {
    const uint32_t elapsed_s = static_cast<uint32_t>((NowMs() - im_.t0) / 1000);
    if (elapsed_s < im_.next_beat_s) return;
    im_.next_beat_s = elapsed_s + 1;
    // Task 6: the stage histograms ride the same beat, once per 10 s.
    if (elapsed_s >= im_.next_hist_beat_s) {
      im_.next_hist_beat_s = elapsed_s + 10;
      EmitStages(false);
    }
    const char* desktop = im_.cfg.desktop_name_fn != nullptr
                              ? im_.cfg.desktop_name_fn(im_.cfg.desktop_name_ctx)
                              : nullptr;
    if (desktop != nullptr && *desktop != '\0') {
      XNC_LOG_INFO("diag_media_v2 elapsed=%us captured=%llu encoded=%llu keyframes=%llu timeouts=%llu warmup_feeds=%llu resets=%u w=%u h=%u aus=%llu bytes=%llu desktop=%s",
                   elapsed_s, static_cast<unsigned long long>(im_.res.captured),
                   static_cast<unsigned long long>(im_.res.encoded),
                   static_cast<unsigned long long>(im_.res.keyframes),
                   static_cast<unsigned long long>(im_.res.timeouts),
                   static_cast<unsigned long long>(im_.res.warmup_feeds),
                   im_.res.resets, im_.res.width, im_.res.height,
                   static_cast<unsigned long long>(im_.res.aus_written),
                   static_cast<unsigned long long>(im_.res.bytes_written),
                   desktop);
    } else {
      XNC_LOG_INFO("diag_media_v2 elapsed=%us captured=%llu encoded=%llu keyframes=%llu timeouts=%llu warmup_feeds=%llu resets=%u w=%u h=%u aus=%llu bytes=%llu",
                   elapsed_s, static_cast<unsigned long long>(im_.res.captured),
                   static_cast<unsigned long long>(im_.res.encoded),
                   static_cast<unsigned long long>(im_.res.keyframes),
                   static_cast<unsigned long long>(im_.res.timeouts),
                   static_cast<unsigned long long>(im_.res.warmup_feeds),
                   im_.res.resets, im_.res.width, im_.res.height,
                   static_cast<unsigned long long>(im_.res.aus_written),
                   static_cast<unsigned long long>(im_.res.bytes_written));
    }
  }

  MediaPipelineV2::Impl& im_;
};

}  // namespace

MediaPipelineV2::MediaPipelineV2() : impl_(new Impl) {}

MediaPipelineV2::~MediaPipelineV2() {
  Stop();
  delete impl_;
}

bool MediaPipelineV2::Start(const Config& cfg) {
  if (impl_->running.load() || impl_->th.joinable()) {
    impl_->start_err = "already started";
    return false;
  }
  if (cfg.cap == nullptr || cfg.surf == nullptr || cfg.sink == nullptr) {
    impl_->start_err = "config requires cap/surf/sink";
    return false;
  }
  if (cfg.fps == 0) {
    impl_->start_err = "fps must be > 0";
    return false;
  }
  impl_->cfg = cfg;
  impl_->max_width.store(cfg.max_width);  // M3 T3: live mirror for SetMaxWidth
  impl_->res = Result{};
  impl_->enc_lock = cfg.encoder_lock != nullptr ? cfg.encoder_lock
                                                : ProcessEncoderLock();
  impl_->backend = cfg.initial_backend;
  impl_->owned_backend.reset();
  impl_->next_dxgi_probe_ms = 0;
  impl_->dxgi_probe_ok = false;
  impl_->last_exec_reason[0] = '\0';
  impl_->storm_streak = 0;
  impl_->reinit_fail_streak = 0;
  impl_->no_frame_resets = 0;
  impl_->session_is_hw = false;
  impl_->hw_init_failed = false;
  impl_->stop_now.store(false);
  impl_->running.store(true);
  auto* im = impl_;
  impl_->th = std::thread([im] { Loop(*im).Run(); });
  return true;
}

void MediaPipelineV2::RequestIdr(const char* reason) {
  impl_->mbox.ArmIdr(reason != nullptr && reason[0] != '\0' ? reason : "explicit");
}

void MediaPipelineV2::Reconfigure(uint32_t bitrate_bps, uint32_t fps) {
  impl_->mbox.RequestReconfigure(bitrate_bps, fps);
}

// M3 Task 3 (SET_VIDEO_CONFIG): live max_w. Any thread. A change requests a
// unified reset (reason=resolution) - the rebuild's InitStream re-derives
// the scaled dims from the new value, re-Init's pool/converter/session (new
// codec epoch; subscribers recover through the 0x020B/WAIT_IDR machine).
void MediaPipelineV2::SetMaxWidth(uint32_t max_w) {
  if (max_w == 0) return;
  if (impl_->max_width.exchange(max_w) == max_w) return;  // no change
  impl_->mbox.RequestReset(kResetReasonResolution);
  XNC_LOG_INFO("media_v2_set_max_w max_w=%u (reset reason=resolution)", max_w);
}

void MediaPipelineV2::Reset(const char* reason) {
  impl_->mbox.RequestReset(reason != nullptr && reason[0] != '\0' ? reason
                                                                  : "manual");
}

bool MediaPipelineV2::running() const { return impl_->running.load(); }

MediaPipelineV2::Result MediaPipelineV2::Stop() {
  if (impl_->th.joinable()) {
    impl_->stop_now.store(true);
    impl_->th.join();
  }
  return impl_->res;
}

const std::string& MediaPipelineV2::start_error() const {
  return impl_->start_err;
}

}  // namespace xnc
