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
#include "dxgi_capture.h"  // GpuScaledDims, NowMonoUs

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
// Reset retry cadence (M2-S1 T2 shape).
constexpr uint32_t kResetPollMs = 100;
constexpr uint32_t kResetBackoffMs = 500;
constexpr uint32_t kResetHardBackoffMs = 1000;
constexpr uint32_t kResetHardFailStreak = 3;
// Pipeline-initiated IDR throttle (spec 7.5).
constexpr uint64_t kIdrMinIntervalMs = 500;

std::string HrErr(const char* step, HRESULT hr) {
  char buf[128];
  _snprintf_s(buf, sizeof(buf), _TRUNCATE, "%s: hr=0x%08lX", step,
              static_cast<unsigned long>(hr));
  return std::string(buf);
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

}  // namespace

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
                               // only order-fair gate)
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
  std::vector<uint8_t> shaped;
  uint64_t t0 = 0;
  uint64_t duration_ms = 0;
  uint32_t next_beat_s = 1;
  uint32_t stream_w = 0, stream_h = 0;
  uint32_t src_w = 0, src_h = 0;

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
      // 2. external requests into the mailbox.
      PollExternal();
      // 3. commands.
      char rr[32];
      if (im_.mbox.TakeReset(rr, sizeof(rr))) {
        if (RunReset(rr)) break;
        continue;
      }
      uint32_t rb = 0, rf = 0;
      if (im_.mbox.TakeReconfigure(&rb, &rf)) ApplyReconfigure(rb, rf);
      PollIdrRequest();
      // 4. capture.
      if (!AcquireOnce()) break;
      // 5. submission (FPS gate + slot gate + mailbox content).
      if (im_.stream_inited) TrySubmit();
      // 6. beat.
      Beat();
    }
    // Drain: collect whatever is ready, then tear the session down (its
    // own drain validates the tail identity mapping and completes leases).
    if (im_.stream_inited) {
      CollectOutputs();
      if (im_.session) im_.session->Shutdown(ShutdownMode::kDrain);
      im_.session.reset();
      im_.pool.FreeRetired();
      if (im_.pool.LiveLeases() != 0)
        XNC_LOG_ERROR("media_v2_teardown live_leases=%zu (expected 0)",
                      im_.pool.LiveLeases());
    }
    im_.res.rebuilds =
        im_.cfg.cap != nullptr ? im_.cfg.cap->RebuildCount() : 0;
    im_.sink().OnState("stream_end", im_.res.ok);
    const MediaPipelineV2::Result& r = im_.res;
    XNC_LOG_INFO("media_v2_stop elapsed=%llums captured=%llu encoded=%llu keyframes=%llu timeouts=%llu warmup_feeds=%llu resets=%u rebuilds=%u w=%u h=%u aus=%llu bytes=%llu backend=%s ok=%d",
                 static_cast<unsigned long long>(NowMs() - im_.t0),
                 static_cast<unsigned long long>(r.captured),
                 static_cast<unsigned long long>(r.encoded),
                 static_cast<unsigned long long>(r.keyframes),
                 static_cast<unsigned long long>(r.timeouts),
                 static_cast<unsigned long long>(r.warmup_feeds), r.resets,
                 r.rebuilds, r.width, r.height,
                 static_cast<unsigned long long>(r.aus_written),
                 static_cast<unsigned long long>(r.bytes_written),
                 r.encoder_backend, r.ok ? 1 : 0);
    im_.running.store(false);
  }

 private:
  // ---- output collection + publication ----
  void CollectOutputs() {
    if (!im_.session) return;
    EncoderOutput out;
    while (im_.session->TakeOutput(&out, 0)) {
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
        continue;
      }
      if (is_idr && im_.sps_pps.empty() && !im_.sps_pps_missing_logged) {
        im_.sps_pps_missing_logged = true;
        XNC_LOG_INFO("media_v2_spspps_absent (backend IDR carries none)");
      }
      // Publication guard (ruling 3's never-a-regression-on-the-wire): the
      // AU's identity must be one of OUR submissions and must never
      // publish twice. (The M1 FrameIdentityLedger itself runs at the
      // SUBMISSION boundary in M0's exact spot - see TrySubmit - because
      // the software rung's AUs surface reordered; a publication-order
      // ledger would false-fatal on that documented MFT shape.)
      if (out.id.encode_seq == 0 ||
          out.id.encode_seq > im_.next_encode_seq ||
          im_.AlreadyPublished(out.id.encode_seq)) {
        im_.Fatal("encoder_identity_mismatch");
        XNC_LOG_ERROR("encoder_identity_mismatch source=publish seq=%llu next=%llu",
                      static_cast<unsigned long long>(out.id.encode_seq),
                      static_cast<unsigned long long>(im_.next_encode_seq));
        return;
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
      if (const char* err = im_.sink().OnAu(eau)) {
        im_.Fatal(err);
        XNC_LOG_ERROR("sink_onau_failed err=\"%s\"", err);
        return;
      }
      if (is_idr) {
        im_.res.keyframes++;
        im_.have_key = true;
        if (im_.idr_in_flight) {
          XNC_LOG_INFO("idr_delivered seq=%llu",
                       static_cast<unsigned long long>(out.id.encode_seq));
          im_.idr_in_flight = false;
        }
      }
    }
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
    if (fps != 0) {
      im_.spf_ms = 1000u / fps;
      im_.warmup_feed_bound = WarmupFeedBound(fps);
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
    const CaptureStatus st =
        im_.cfg.surf->AcquireSurface(im_.latest, im_.spf_ms, &spec, &aerr);
    switch (st) {
      case CaptureStatus::kFrame: {
        im_.next_content_id = spec.content_id;  // committed
        im_.res.captured++;
        if (!im_.stream_inited) {
          std::string ierr;
          if (!InitStream(&ierr)) {
            im_.Fatal("media_v2_stream_init: " + ierr);
            return false;
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
        im_.Fatal(aerr.empty() ? "acquire failed" : aerr);
        im_.sink().OnState("capture_fatal", false);
        return false;
    }
  }

  // Idle re-feed (spec 7.4/7.5; the M0 timeout-path semantics): on a
  // static screen, re-publish the surface's current identity so the
  // encoder's lookahead fills and the (possibly forced) IDR emerges.
  // Bounded by WarmupFeedBound and the 2 s wall clock; paced to spf.
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
    const bool conv_ok = im_.conv.Convert(tex, *lease, &cerr_);
    tex->Release();
    if (!conv_ok) {
      lease->Release();
      im_.Fatal("convert: " + cerr_);
      return;
    }
    const SubmitResult r = im_.session->Submit(id, std::move(*lease), force);
    if (r != SubmitResult::kOk) {
      im_.Fatal("encoder submit rejected (result=" + std::to_string(static_cast<int>(r)) + ")");
      return;
    }
    im_.res.encoded++;
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
    if (im_.cfg.max_width > 0) {
      uint32_t sw = 0, sh = 0;
      if (!GpuScaledDims(im_.src_w, im_.src_h, Rotate::kNone,
                         im_.cfg.max_width, &sw, &sh) ||
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
    if (!im_.cfg.force_software_encoder) {
      auto gpu = std::make_unique<MfGpuEncoder>();
      std::string gerr;
      if (gpu->Init(im_.dev.Get(), &im_.pool, w, h, im_.cfg.fps,
                    im_.cfg.bitrate_bps, &gerr)) {
        im_.res.encoder_backend = gpu->BackendName();
        im_.res.encoder_friendly = gpu->FriendlyName();
        im_.session = std::move(gpu);
        return true;
      }
      XNC_LOG_INFO("media_v2_gpu_session_unavailable err=\"%s\" (software rung)",
                   gerr.c_str());
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

  // ---- reset (the M2-S1 T2 sequence on the single loop thread) ----
  // Returns true when the RUN must end.
  bool RunReset(const char* reason) {
    const uint64_t ts = NowMs();
    XNC_LOG_INFO("capture_reset_start reason=%s", reason);
    im_.sink().OnState("recovering", true);
    // Drop pending pre-reset content; tear the session (completes every
    // outstanding lease); the surface content dies with the rebuild.
    FrameIdentity drop;
    while (im_.mbox.TakeContent(&drop)) {
    }
    if (im_.session) {
      im_.session->Shutdown(ShutdownMode::kImmediate);
      im_.session.reset();
    }
    im_.pool.FreeRetired();
    im_.latest.Invalidate();
    // Wait out a secure desktop (immediate rebuilds are futile there).
    while (im_.cfg.reset != nullptr &&
           im_.cfg.reset->Desktop() == ResetDesktop::kNonDefault) {
      if (im_.Abort()) return true;
      Sleep(kResetPollMs);
    }
    // Rebuild with retry/backoff.
    uint32_t streak = 0;
    bool failed_state = false;
    uint32_t old_w = im_.cfg.cap != nullptr ? im_.cfg.cap->Width() : 0;
    uint32_t old_h = im_.cfg.cap != nullptr ? im_.cfg.cap->Height() : 0;
    for (;;) {
      if (im_.Abort()) return true;
      std::string rerr;
      if (im_.cfg.cap == nullptr || im_.cfg.cap->Rebuild(&rerr)) break;
      ++streak;
      XNC_LOG_ERROR("capture_reset_rebuild_failed streak=%u err=\"%s\"", streak,
                    rerr.c_str());
      if (streak >= kResetHardFailStreak && !failed_state) {
        failed_state = true;
        im_.sink().OnState("capture_failed", true);  // still retrying
      }
      const uint32_t backoff =
          failed_state ? kResetHardBackoffMs : kResetBackoffMs;
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
    // New generation: epochs advance, the next kFrame re-inits the stream
    // (new device/dims unknown until then) and the sticky rebuild IDR
    // rides its first submission.
    im_.capture_epoch++;
    im_.codec_epoch++;
    im_.stream_inited = false;
    im_.warmup_started_ms = 0;
    im_.warmup_gen_feeds = 0;
    im_.warmup_phase_logged = false;
    im_.mbox.ArmIdr("rebuild");
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

  void Beat() {
    const uint32_t elapsed_s = static_cast<uint32_t>((NowMs() - im_.t0) / 1000);
    if (elapsed_s < im_.next_beat_s) return;
    im_.next_beat_s = elapsed_s + 1;
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
  impl_->res = Result{};
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
