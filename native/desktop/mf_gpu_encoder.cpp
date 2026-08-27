// mf_gpu_encoder.cpp - M2 Task 3: the D3D11-aware hardware session and the
// CPU adapter over MfSoftEncoder (see mf_gpu_encoder.h). COM/D3D plumbing
// follows the mf_encoder.cpp idioms (MTA apartment ownership, ComPtr,
// MFTEnumEx + ActivateObject, best-effort ICodecAPI shaping).
//
// The async-MFT protocol (measured on this machine's Intel QSV MFT, see
// header): a hardware encoder MFT is an ASYNC MFT locked until
// MF_TRANSFORM_ASYNC_UNLOCK is set on the MFT's own attribute store; every
// IMFTransform call before that returns MF_E_TRANSFORM_ASYNC_LOCKED
// (0xC00D6D77 - the error the CPU-input ladder kept hitting). After the
// unlock: MFT_MESSAGE_SET_D3D_MANAGER(imfdxgi_mgr), media types, codec
// properties and streaming messages all succeed. From then on the MFT
// drives the cadence through its IMFMediaEventGenerator: METransformNeedInput
// credits one ProcessInput, METransformHaveOutput credits one ProcessOutput.
// Those events only flow after MFStartup (sync MFTs - everything else in
// this binary - never needed it), so MfGpuEncoder::Init owns one
// MFStartup reference and releases it at Shutdown. Event waits are bounded
// low-frequency Sleep(2) polls (no hot spin, no blocking GetEvent - the
// C++ IMFMediaEventGenerator::GetEvent has no timeout flag).
//
// Startup probe (spec §8.3): every hardware candidate must encode >= 8
// synthetic inputs (alternating two contents, distinct ids, forced IDRs at
// positions 0 and 7) with the first output within two inputs or 100 ms, a
// 1:1 sample-time mapping, and structurally valid IDR NALs (SPS+PPS+type
// 5). Probe inputs are synthesized through a REAL D3D11 VideoProcessor
// (BGRA -> NV12 with the §8.4 color rule - the exact conversion shape the
// live path uses), into three round-robin NV12 textures (in-flight <= 3,
// spec §9). The pixel-signature half of §8.3.3 (decode + compare) is
// test-only by design (mf_decoder_probe must not link into production),
// so it lives in the selftest and runs against whichever session
// initializes. A candidate that passes the probe is dropped and a PRISTINE
// instance is re-activated + re-configured for the live stream, so no
// synthetic AU can ever reach the caller.
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>

#include <d3d11.h>
#include <d3d11_1.h>  // ID3D11VideoContext1 (color-space APIs)
#include <codecapi.h>
#include <mfapi.h>
#include <mferror.h>
#include <mftransform.h>
#include <strmif.h>
#include <wrl/client.h>

#include <cstdio>
#include <cstring>

#include "../common/log.h"
#include "mf_gpu_encoder.h"

namespace xnc {

using Microsoft::WRL::ComPtr;

namespace {

// Event-wait budgets (ms). Hardware encoders in low-latency mode raise
// NeedInput within milliseconds (measured ~6 ms cold); the budgets only
// bound a wedged candidate.
constexpr uint32_t kNeedInputWaitMs = 200;
constexpr uint32_t kProbeFirstOutputMs = 100;  // spec §8.3 item 2
constexpr uint32_t kProbeInputBound = 8;       // spec §8.3 item 1
constexpr uint32_t kDrainWaitMs = 500;

std::string HrStep(const char* step, HRESULT hr) {
  char buf[160];
  std::snprintf(buf, sizeof(buf), "%s: hr=0x%08X", step,
                static_cast<unsigned int>(hr));
  return std::string(buf);
}

bool CodecApiSetUi4(ICodecAPI* api, const GUID* prop, uint32_t value,
                    const char* name) {
  VARIANT v{};
  v.vt = VT_UI4;
  v.ulVal = value;
  const HRESULT hr = api->SetValue(prop, &v);
  if (FAILED(hr)) {
    XNC_LOG_INFO("gpu_codec_api_set_skip name=%s hr=0x%08x", name,
                 static_cast<unsigned int>(hr));
    return false;
  }
  return true;
}

std::string WideToNarrow(const std::wstring& w) {
  std::string out;
  for (wchar_t c : w) {
    if (c < 0x80) out.push_back(static_cast<char>(c));
    else out.push_back('?');
  }
  return out;
}

std::wstring ActivateFriendlyName(IMFActivate* act) {
  wchar_t* name = nullptr;
  UINT32 nlen = 0;
  std::wstring out;
  if (SUCCEEDED(act->GetAllocatedString(MFT_FRIENDLY_NAME_Attribute, &name,
                                         &nlen)) &&
      name != nullptr) {
    out.assign(name, nlen);
  }
  CoTaskMemFree(name);
  return out;
}

HRESULT MakeH264OutputType(uint32_t w, uint32_t h, uint32_t fps,
                           uint32_t bitrate, IMFMediaType** out) {
  const uint64_t frame_size = (static_cast<uint64_t>(w) << 32) | h;
  const uint64_t frame_rate = (static_cast<uint64_t>(fps) << 32) | 1;
  ComPtr<IMFMediaType> mt;
  HRESULT hr = MFCreateMediaType(mt.GetAddressOf());
  if (SUCCEEDED(hr)) hr = mt->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
  if (SUCCEEDED(hr)) hr = mt->SetGUID(MF_MT_SUBTYPE, MFVideoFormat_H264);
  if (SUCCEEDED(hr)) hr = mt->SetUINT64(MF_MT_FRAME_SIZE, frame_size);
  if (SUCCEEDED(hr))
    hr = mt->SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
  if (SUCCEEDED(hr)) hr = mt->SetUINT64(MF_MT_FRAME_RATE, frame_rate);
  if (SUCCEEDED(hr)) hr = mt->SetUINT32(MF_MT_AVG_BITRATE, bitrate);
  if (SUCCEEDED(hr)) *out = mt.Detach();
  return hr;
}

// NV12 input type with the §8.4 color attributes (the same set
// mf_encoder.cpp applies - Nv12ColorForSize; MF_MT_VIDEO_NOMINAL_RANGE is
// deliberately omitted, see the measured note there).
HRESULT MakeNv12InputType(uint32_t w, uint32_t h, uint32_t fps,
                          IMFMediaType** out) {
  const Nv12ColorConfig color = Nv12ColorForSize(h);
  const uint64_t frame_size = (static_cast<uint64_t>(w) << 32) | h;
  const uint64_t frame_rate = (static_cast<uint64_t>(fps) << 32) | 1;
  ComPtr<IMFMediaType> mt;
  HRESULT hr = MFCreateMediaType(mt.GetAddressOf());
  if (SUCCEEDED(hr)) hr = mt->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
  if (SUCCEEDED(hr)) hr = mt->SetGUID(MF_MT_SUBTYPE, MFVideoFormat_NV12);
  if (SUCCEEDED(hr)) hr = mt->SetUINT64(MF_MT_DEFAULT_STRIDE, w);
  if (SUCCEEDED(hr)) hr = mt->SetUINT64(MF_MT_FRAME_SIZE, frame_size);
  if (SUCCEEDED(hr))
    hr = mt->SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
  if (SUCCEEDED(hr)) hr = mt->SetUINT64(MF_MT_FRAME_RATE, frame_rate);
  if (SUCCEEDED(hr)) hr = mt->SetUINT32(MF_MT_YUV_MATRIX, color.matrix);
  if (SUCCEEDED(hr))
    hr = mt->SetUINT32(MF_MT_VIDEO_PRIMARIES, color.primaries);
  if (SUCCEEDED(hr))
    hr = mt->SetUINT32(MF_MT_TRANSFER_FUNCTION, color.transfer);
  if (SUCCEEDED(hr)) *out = mt.Detach();
  return hr;
}

// One output AU as pulled from an MFT, with its own 100ns sample time.
struct UnitAu {
  std::vector<uint8_t> au;
  int64_t time = 0;
  bool has_time = false;
  bool key = false;
};

// ---- one activated candidate (the live unit or a probe instance) ----
struct Unit {
  ComPtr<IMFTransform> mft;
  ComPtr<ICodecAPI> codec_api;
  ComPtr<IMFActivate> activate;  // owns the hardware session (ShutdownObject)
  ComPtr<IMFMediaEventGenerator> gen;
  bool is_async = false;
  bool provides_samples = false;  // MFT_OUTPUT_STREAM_PROVIDES_SAMPLES
  size_t out_buf_size = 0;
  int need_credits = 0;  // queued METransformNeedInput
  int have_credits = 0;  // queued METransformHaveOutput
};

// Drains queued MFT events for up to slice_ms (bounded Sleep(2) polls; no
// hot spin). Accounts NeedInput/HaveOutput credits; notes drain-complete.
void PumpEvents(Unit& u, uint32_t slice_ms, bool* drain_done = nullptr) {
  if (u.gen.Get() == nullptr) return;
  const ULONGLONG deadline = GetTickCount64() + slice_ms;
  for (;;) {
    ComPtr<IMFMediaEvent> ev;
    const HRESULT hr =
        u.gen->GetEvent(MF_EVENT_FLAG_NO_WAIT, ev.GetAddressOf());
    if (hr == MF_E_NO_EVENTS_AVAILABLE) {
      if (GetTickCount64() >= deadline) return;
      Sleep(2);
      continue;
    }
    if (FAILED(hr)) return;
    MediaEventType et = 0;
    ev->GetType(&et);
    if (et == METransformNeedInput) {
      ++u.need_credits;
    } else if (et == METransformHaveOutput) {
      ++u.have_credits;
    } else if (et == METransformDrainComplete) {
      if (drain_done != nullptr) *drain_done = true;
    }
    // METransformMarker / stream-state changes: ignored.
  }
}

enum class PullResult : uint8_t { kGot = 0, kNeedMore = 1, kError = 2 };

// One ProcessOutput. kGot appends to outs; kNeedMore = drained for now;
// kError sets *err. STREAM_CHANGE is consumed and reported as kGot without
// an AU (the caller's bounded loop retries).
PullResult PullOneOutput(Unit& u, std::vector<UnitAu>* outs,
                          std::string* err) {
  MFT_OUTPUT_DATA_BUFFER ob{};
  ComPtr<IMFSample> client_sample;  // holds the client-provided case
  if (!u.provides_samples) {
    ComPtr<IMFMediaBuffer> client_buffer;
    HRESULT hrb = MFCreateSample(client_sample.GetAddressOf());
    if (SUCCEEDED(hrb))
      hrb = MFCreateMemoryBuffer(static_cast<DWORD>(u.out_buf_size),
                                 client_buffer.GetAddressOf());
    if (SUCCEEDED(hrb)) hrb = client_sample->AddBuffer(client_buffer.Get());
    if (FAILED(hrb)) {
      if (err) *err = HrStep("output sample alloc", hrb);
      return PullResult::kError;
    }
    ob.pSample = client_sample.Get();
  }
  DWORD status = 0;
  HRESULT hr = u.mft->ProcessOutput(0, 1, &ob, &status);
  if (ob.pEvents) {
    ob.pEvents->Release();
    ob.pEvents = nullptr;
  }
  if (hr == MF_E_TRANSFORM_NEED_MORE_INPUT) return PullResult::kNeedMore;
  if (hr == MF_E_TRANSFORM_STREAM_CHANGE) {
    if (u.provides_samples && ob.pSample) ob.pSample->Release();
    return PullResult::kGot;  // consumed; caller loops (bounded)
  }
  if (FAILED(hr)) {
    if (err) *err = HrStep("ProcessOutput", hr);
    return PullResult::kError;
  }
  if (ob.pSample == nullptr) return PullResult::kGot;
  UnitAu out;
  LONGLONG t = 0;
  out.has_time = SUCCEEDED(ob.pSample->GetSampleTime(&t));
  out.time = static_cast<int64_t>(t);
  ComPtr<IMFMediaBuffer> buf;
  hr = ob.pSample->ConvertToContiguousBuffer(buf.GetAddressOf());
  if (SUCCEEDED(hr)) {
    BYTE* base = nullptr;
    DWORD cur = 0;
    hr = buf->Lock(&base, nullptr, &cur);
    if (SUCCEEDED(hr) && base) {
      out.au.assign(base, base + cur);
      buf->Unlock();
      out.key = NalHasType(out.au.data(), out.au.size(), 5);
    } else if (SUCCEEDED(hr)) {
      hr = E_FAIL;
    }
  }
  if (u.provides_samples) ob.pSample->Release();  // MFT-allocated
  if (FAILED(hr)) {
    if (err) *err = HrStep("output buffer", hr);
    return PullResult::kError;
  }
  outs->push_back(std::move(out));
  return PullResult::kGot;
}

// Collects every output the unit currently offers. wait_slice_ms bounds
// the async event wait; a sync MFT is swept once (it only produces on new
// input, so waiting is pointless there). false only on a collection error.
bool CollectUnitOutputs(Unit& u, uint32_t wait_slice_ms,
                        std::vector<UnitAu>* outs, std::string* err) {
  if (u.is_async) {
    PumpEvents(u, wait_slice_ms);
    int guard = 0;
    while (u.have_credits > 0 && guard++ < 64) {
      --u.have_credits;
      const PullResult r = PullOneOutput(u, outs, err);
      if (r == PullResult::kNeedMore) {
        ++u.have_credits;  // credit not consumable yet
        break;
      }
      if (r == PullResult::kError) return false;
      PumpEvents(u, 0);
    }
  } else {
    for (int guard = 0; guard < 64; ++guard) {
      const PullResult r = PullOneOutput(u, outs, err);
      if (r == PullResult::kError) return false;
      if (r == PullResult::kNeedMore) break;
    }
  }
  return true;
}

// Waits for a NeedInput credit (submit permission).
bool WaitForNeedInput(Unit& u, uint32_t budget_ms) {
  if (!u.is_async) return true;  // sync MFT: ProcessInput directly
  const ULONGLONG deadline = GetTickCount64() + budget_ms;
  for (;;) {
    PumpEvents(u, 2);
    if (u.need_credits > 0) return true;
    if (GetTickCount64() >= deadline) return false;
  }
}

// Full negotiation + streaming start on a fresh candidate instance.
bool ConfigureUnit(Unit& u, IMFDXGIDeviceManager* mgr, uint32_t w, uint32_t h,
                   uint32_t fps, uint32_t bitrate, std::string* err) {
  const uint64_t frame_size = (static_cast<uint64_t>(w) << 32) | h;
  const uint64_t frame_rate = (static_cast<uint64_t>(fps) << 32) | 1;

  // Async unlock FIRST: every other call returns MF_E_TRANSFORM_ASYNC_LOCKED
  // (0xC00D6D77) until this is set on the MFT's own attribute store.
  ComPtr<IMFAttributes> attrs;
  HRESULT hr = u.mft->GetAttributes(attrs.GetAddressOf());
  if (SUCCEEDED(hr)) {
    UINT32 async = 0;
    if (SUCCEEDED(attrs->GetUINT32(MF_TRANSFORM_ASYNC, &async)) && async) {
      u.is_async = true;
      hr = attrs->SetUINT32(MF_TRANSFORM_ASYNC_UNLOCK, TRUE);
      if (FAILED(hr)) {
        if (err) *err = HrStep("MF_TRANSFORM_ASYNC_UNLOCK", hr);
        return false;
      }
      UINT32 aware = 0;
      attrs->GetUINT32(MF_SA_D3D11_AWARE, &aware);
      // Informational only: the QSV MFT reports 0 here yet accepts
      // SET_D3D_MANAGER once unlocked (measured) - the manager attempt
      // below is the real gate.
      XNC_LOG_INFO("gpu_mft_async=1 d3d11_aware=%u", aware);
    }
  }
  if (u.is_async) {
    hr = u.mft.As(&u.gen);
    if (FAILED(hr)) {
      if (err) *err = HrStep("QI(IMFMediaEventGenerator)", hr);
      return false;
    }
  }

  // Device manager BEFORE media types (the D3D11-aware contract).
  hr = u.mft->ProcessMessage(MFT_MESSAGE_SET_D3D_MANAGER,
                             reinterpret_cast<UINT_PTR>(mgr));
  if (FAILED(hr)) {
    if (err) *err = HrStep("SET_D3D_MANAGER", hr);
    return false;
  }

  // Rate control BEFORE types (the mf_encoder.cpp lesson). LowDelayVBR
  // first, CBR fallback - measured: QSV accepts both, software only CBR.
  u.codec_api.Reset();
  u.mft->QueryInterface(IID_PPV_ARGS(u.codec_api.GetAddressOf()));
  if (u.codec_api.Get() != nullptr) {
    if (!CodecApiSetUi4(u.codec_api.Get(), &CODECAPI_AVEncCommonRateControlMode,
                        eAVEncCommonRateControlMode_LowDelayVBR,
                        "rate_control_low_delay")) {
      CodecApiSetUi4(u.codec_api.Get(), &CODECAPI_AVEncCommonRateControlMode,
                     eAVEncCommonRateControlMode_CBR, "rate_control_cbr");
    }
  }

  // Output type: the caller's H.264 type, then the MFT's enumerated H.264
  // types with dims/rate/bitrate overridden (hardware MFTs may not take
  // the caller's type verbatim - mf_encoder.cpp fallback shape).
  ComPtr<IMFMediaType> out_mt;
  hr = MakeH264OutputType(w, h, fps, bitrate, out_mt.GetAddressOf());
  if (SUCCEEDED(hr)) hr = u.mft->SetOutputType(0, out_mt.Get(), 0);
  if (FAILED(hr)) {
    for (DWORD i = 0;; ++i) {
      ComPtr<IMFMediaType> t;
      hr = u.mft->GetOutputAvailableType(0, i, t.GetAddressOf());
      if (FAILED(hr)) break;
      GUID sub{};
      if (FAILED(t->GetGUID(MF_MT_SUBTYPE, &sub)) || sub != MFVideoFormat_H264)
        continue;
      hr = t->SetUINT64(MF_MT_FRAME_SIZE, frame_size);
      if (SUCCEEDED(hr)) hr = t->SetUINT64(MF_MT_FRAME_RATE, frame_rate);
      if (SUCCEEDED(hr))
        hr = t->SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
      if (SUCCEEDED(hr)) hr = t->SetUINT32(MF_MT_AVG_BITRATE, bitrate);
      if (SUCCEEDED(hr)) hr = u.mft->SetOutputType(0, t.Get(), 0);
      if (SUCCEEDED(hr)) break;
    }
  }
  if (FAILED(hr)) {
    if (err) *err = HrStep("SetOutputType(hw)", hr);
    return false;
  }

  // Input type: NV12 + the §8.4 color attributes.
  ComPtr<IMFMediaType> in_mt;
  hr = MakeNv12InputType(w, h, fps, in_mt.GetAddressOf());
  if (SUCCEEDED(hr)) hr = u.mft->SetInputType(0, in_mt.Get(), 0);
  if (FAILED(hr)) {
    if (err) *err = HrStep("SetInputType(hw)", hr);
    return false;
  }

  // Low-latency shaping after types (accepted there; best-effort).
  if (u.codec_api.Get() != nullptr) {
    CodecApiSetUi4(u.codec_api.Get(), &CODECAPI_AVEncMPVGOPSize, fps * 10,
                   "gop_size");
    CodecApiSetUi4(u.codec_api.Get(), &CODECAPI_AVEncMPVDefaultBPictureCount,
                   0, "b_picture_count");
    VARIANT v{};  // AVLowLatencyMode is VT_BOOL
    v.vt = VT_BOOL;
    v.boolVal = VARIANT_TRUE;
    if (FAILED(u.codec_api->SetValue(&CODECAPI_AVLowLatencyMode, &v)))
      XNC_LOG_INFO("gpu_codec_api_set_skip name=low_latency hr=rejected");
  }

  MFT_OUTPUT_STREAM_INFO osi{};
  hr = u.mft->GetOutputStreamInfo(0, &osi);
  if (SUCCEEDED(hr)) {
    u.provides_samples =
        (osi.dwFlags & MFT_OUTPUT_STREAM_PROVIDES_SAMPLES) != 0;
    if (osi.cbSize > u.out_buf_size) u.out_buf_size = osi.cbSize;
  }
  if (u.out_buf_size == 0)
    u.out_buf_size = static_cast<size_t>(w) * h * 4 + 65536;

  hr = u.mft->ProcessMessage(MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0);
  if (SUCCEEDED(hr))
    hr = u.mft->ProcessMessage(MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0);
  if (FAILED(hr)) {
    if (err) *err = HrStep("ProcessMessage(start streaming)", hr);
    return false;
  }
  return true;
}

void ReleaseUnit(Unit& u) {
  if (u.mft.Get() != nullptr)
    u.mft->ProcessMessage(MFT_MESSAGE_NOTIFY_END_STREAMING, 0);
  if (u.activate.Get() != nullptr) {
    u.activate->ShutdownObject();
    u.activate.Reset();
  }
  u.gen.Reset();
  u.codec_api.Reset();
  u.mft.Reset();
  u.need_credits = u.have_credits = 0;
}

// Builds one DXGI-surface-backed input sample straight on `tex` (the
// pool's NV12 texture - no copy, no readback) and submits it.
bool UnitSubmitTexture(Unit& u, ID3D11Texture2D* tex, int64_t time_100ns,
                       int64_t dur_100ns, bool force_idr) {
  ComPtr<IMFSample> sample;
  ComPtr<IMFMediaBuffer> buffer;
  HRESULT hr = MFCreateSample(sample.GetAddressOf());
  if (SUCCEEDED(hr))
    hr = MFCreateDXGISurfaceBuffer(__uuidof(ID3D11Texture2D), tex, 0, FALSE,
                                   buffer.GetAddressOf());
  if (SUCCEEDED(hr)) hr = sample->AddBuffer(buffer.Get());
  if (SUCCEEDED(hr)) {
    sample->SetSampleTime(time_100ns);
    sample->SetSampleDuration(dur_100ns);
  }
  if (FAILED(hr)) return false;
  // One-shot force-key at SUBMISSION (E2 contract; consumed unconditionally).
  if (force_idr && u.codec_api.Get() != nullptr) {
    VARIANT v{};
    v.vt = VT_UI4;
    v.ulVal = 1;
    u.codec_api->SetValue(&CODECAPI_AVEncVideoForceKeyFrame, &v);
  }
  hr = u.mft->ProcessInput(0, sample.Get(), 0);
  return SUCCEEDED(hr);
}

// Shared post-collection mapping for BOTH sessions: consume by exact
// sample time, complete the matched lease (GPU rung holds leases until
// output), queue the AU. Any non-kOk verdict poisons the session and
// completes every outstanding lease exactly once (no leak under fault).
bool SessionCollectOutputs(OutputIdentityTracker& tracker,
                           Nv12SurfacePool* pool, std::vector<UnitAu>& aus,
                           std::vector<EncoderOutput>& queued,
                           bool* identity_fault, bool complete_leases,
                           EncoderSessionError* last_error) {
  for (auto& a : aus) {
    OutputIdentityRecord rec{};
    const OutputConsume v = tracker.Consume(a.has_time, a.time, &rec);
    if (v != OutputConsume::kOk) {
      *identity_fault = true;
      *last_error = EncoderSessionError::kEncoderIdentityMismatch;
      XNC_LOG_ERROR("encoder_identity_mismatch verdict=%d time=%lld "
                    "(releasing all outstanding leases)",
                    static_cast<int>(v), static_cast<long long>(a.time));
      if (complete_leases)
        for (const auto& p : tracker.PendingRecords())
          pool->Complete(p.submit_id);
      tracker.Clear();
      return false;
    }
    if (complete_leases) pool->Complete(rec.submit_id);
    EncoderOutput out;
    out.id = rec.id;
    out.submit_id = rec.submit_id;
    out.slot = rec.slot;
    out.sample_time = a.time;
    out.key = a.key;
    out.au = std::move(a.au);
    queued.push_back(std::move(out));
  }
  return true;
}

// ---- probe-side synthetic content (two distinct gradients, BGRA) ----
void DrawProbePattern(std::vector<uint8_t>* bgra, uint32_t w, uint32_t h,
                      int variant) {
  for (uint32_t y = 0; y < h; ++y) {
    for (uint32_t x = 0; x < w; ++x) {
      uint8_t* px = bgra->data() + (static_cast<size_t>(y) * w + x) * 4;
      if (variant == 0) {
        px[0] = static_cast<uint8_t>(x & 0xFF);
        px[1] = static_cast<uint8_t>(y & 0xFF);
        px[2] = static_cast<uint8_t>((x + y) & 0xFF);
      } else {
        px[0] = static_cast<uint8_t>(255 - (x & 0xFF));
        px[1] = static_cast<uint8_t>((y * 7) & 0xFF);
        px[2] = static_cast<uint8_t>((x ^ y) & 0xFF);
      }
      px[3] = 0xFF;
    }
  }
}

// The probe's BGRA -> NV12 conversion path: a real VideoProcessor with the
// §8.4 color rule (the conversion shape the live GPU loop will use).
struct ProbeConvert {
  ComPtr<ID3D11VideoDevice> vid_dev;
  ComPtr<ID3D11VideoContext> vid_ctx;
  ComPtr<ID3D11VideoProcessorEnumerator> vpe;
  ComPtr<ID3D11VideoProcessor> vp;
  ComPtr<ID3D11VideoProcessorOutputView> out_view[3];
  ComPtr<ID3D11Texture2D> nv12[3];

  bool Init(ID3D11Device* dev, ID3D11DeviceContext* ctx, uint32_t w,
            uint32_t h, std::string* err) {
    if (FAILED(dev->QueryInterface(IID_PPV_ARGS(vid_dev.GetAddressOf()))) ||
        FAILED(ctx->QueryInterface(IID_PPV_ARGS(vid_ctx.GetAddressOf())))) {
      if (err) *err = "QI(ID3D11VideoDevice/Context)";
      return false;
    }
    D3D11_VIDEO_PROCESSOR_CONTENT_DESC vd{};
    vd.InputFrameFormat = D3D11_VIDEO_FRAME_FORMAT_PROGRESSIVE;
    vd.InputFrameRate = {0, 0};
    vd.InputWidth = w;
    vd.InputHeight = h;
    vd.OutputFrameRate = {0, 0};
    vd.OutputWidth = w;
    vd.OutputHeight = h;
    HRESULT hr = vid_dev->CreateVideoProcessorEnumerator(&vd, &vpe);
    if (FAILED(hr)) {
      if (err) *err = HrStep("CreateVideoProcessorEnumerator", hr);
      return false;
    }
    UINT in_sup = 0, out_sup = 0;
    vpe->CheckVideoProcessorFormat(DXGI_FORMAT_B8G8R8A8_UNORM, &in_sup);
    vpe->CheckVideoProcessorFormat(DXGI_FORMAT_NV12, &out_sup);
    if (!in_sup || !out_sup) {
      if (err) *err = "video processor cannot convert bgra->nv12";
      return false;
    }
    hr = vid_dev->CreateVideoProcessor(vpe.Get(), 0, &vp);
    if (FAILED(hr)) {
      if (err) *err = HrStep("CreateVideoProcessor", hr);
      return false;
    }
    // §8.4 color spaces: full-range RGB in, limited-range YCbCr out with
    // the matrix selected by resolution (>=720p BT.709, else BT.601).
    // ID3D11VideoContext1's DXGI_COLOR_SPACE route when available, the
    // documented numeric D3D11_VIDEO_PROCESSOR_COLOR_SPACE otherwise
    // (dxgi_capture.cpp pattern).
    const Nv12ColorConfig color = Nv12ColorForSize(h);
    ComPtr<ID3D11VideoContext1> vc1;
    if (SUCCEEDED(vid_ctx.As(&vc1))) {
      vc1->VideoProcessorSetStreamColorSpace1(
          vp.Get(), 0, DXGI_COLOR_SPACE_RGB_FULL_G22_NONE_P709);
      vc1->VideoProcessorSetOutputColorSpace1(
          vp.Get(), color.bt709 ? DXGI_COLOR_SPACE_YCBCR_STUDIO_G22_LEFT_P709
                                : DXGI_COLOR_SPACE_YCBCR_STUDIO_G22_LEFT_P601);
    } else {
      D3D11_VIDEO_PROCESSOR_COLOR_SPACE in_cs{};
      in_cs.Usage = 1;  // RGB full range in (documented numeric value)
      vid_ctx->VideoProcessorSetStreamColorSpace(vp.Get(), 0, &in_cs);
      D3D11_VIDEO_PROCESSOR_COLOR_SPACE out_cs{};
      out_cs.Usage = 3;  // YCBCR studio (limited) range out
      out_cs.RGB_Range = 1;
      out_cs.YCbCr_Matrix = color.bt709 ? 1 : 0;
      out_cs.Nominal_Range = D3D11_VIDEO_PROCESSOR_NOMINAL_RANGE_16_235;
      vid_ctx->VideoProcessorSetOutputColorSpace(vp.Get(), &out_cs);
    }
    D3D11_TEXTURE2D_DESC od{};
    od.Width = w;
    od.Height = h;
    od.MipLevels = 1;
    od.ArraySize = 1;
    od.SampleDesc.Count = 1;
    od.Format = DXGI_FORMAT_NV12;
    od.Usage = D3D11_USAGE_DEFAULT;
    od.BindFlags = D3D11_BIND_RENDER_TARGET;
    D3D11_VIDEO_PROCESSOR_OUTPUT_VIEW_DESC ovd{};
    ovd.ViewDimension = D3D11_VPOV_DIMENSION_TEXTURE2D;
    ovd.Texture2D.MipSlice = 0;
    for (int i = 0; i < 3; ++i) {
      hr = dev->CreateTexture2D(&od, nullptr, &nv12[i]);
      if (FAILED(hr)) {
        if (err) *err = HrStep("CreateTexture2D(probe nv12)", hr);
        return false;
      }
      hr = vid_dev->CreateVideoProcessorOutputView(nv12[i].Get(), vpe.Get(),
                                                   &ovd, &out_view[i]);
      if (FAILED(hr)) {
        if (err) *err = HrStep("CreateVideoProcessorOutputView", hr);
        return false;
      }
    }
    return true;
  }

  bool Blt(ID3D11Texture2D* src, int slot) {
    D3D11_VIDEO_PROCESSOR_INPUT_VIEW_DESC ivd{};
    ivd.ViewDimension = D3D11_VPIV_DIMENSION_TEXTURE2D;
    ComPtr<ID3D11VideoProcessorInputView> in_view;
    if (FAILED(vid_dev->CreateVideoProcessorInputView(
            src, vpe.Get(), &ivd, &in_view)))
      return false;
    D3D11_VIDEO_PROCESSOR_STREAM vs{};
    vs.Enable = TRUE;
    vs.pInputSurface = in_view.Get();
    return SUCCEEDED(vid_ctx->VideoProcessorBlt(vp.Get(), out_view[slot].Get(),
                                                0, 1, &vs));
  }
};

// The §8.3 startup probe over one pristine activation of `act` (the enum's
// reference is left with the caller - RunStartupProbe takes its own).
bool RunStartupProbe(IMFDXGIDeviceManager* mgr, ID3D11Device* dev,
                     ID3D11DeviceContext* ctx, IMFActivate* act, uint32_t w,
                     uint32_t h, uint32_t fps, uint32_t bitrate,
                     const std::wstring& friendly, std::string* err) {
  Unit probe;
  HRESULT hr = act->ActivateObject(IID_PPV_ARGS(probe.mft.GetAddressOf()));
  if (FAILED(hr)) {
    if (err) *err = HrStep("ActivateObject(probe)", hr);
    return false;
  }
  probe.activate = act;  // ComPtr operator= AddRefs; ReleaseUnit balances
  if (!ConfigureUnit(probe, mgr, w, h, fps, bitrate, err)) {
    ReleaseUnit(probe);
    return false;
  }

  // Synthesize the 8 inputs through the real conversion path.
  ProbeConvert conv;
  if (!conv.Init(dev, ctx, w, h, err)) {
    ReleaseUnit(probe);
    return false;
  }
  D3D11_TEXTURE2D_DESC bd{};
  bd.Width = w;
  bd.Height = h;
  bd.MipLevels = 1;
  bd.ArraySize = 1;
  bd.SampleDesc.Count = 1;
  bd.Format = DXGI_FORMAT_B8G8R8A8_UNORM;
  bd.Usage = D3D11_USAGE_DEFAULT;
  ComPtr<ID3D11Texture2D> bgra_tex[2];
  std::vector<uint8_t> bgra(static_cast<size_t>(w) * h * 4);
  for (int v = 0; v < 2; ++v) {
    DrawProbePattern(&bgra, w, h, v);
    D3D11_SUBRESOURCE_DATA srd{bgra.data(), w * 4, 0};
    hr = dev->CreateTexture2D(&bd, &srd, &bgra_tex[v]);
    if (FAILED(hr)) {
      if (err) *err = HrStep("CreateTexture2D(probe bgra)", hr);
      ReleaseUnit(probe);
      return false;
    }
  }

  const int64_t frame_dur = static_cast<int64_t>(10000000 / (fps ? fps : 30));
  LARGE_INTEGER qpf{}, t0{};
  QueryPerformanceFrequency(&qpf);
  QueryPerformanceCounter(&t0);
  auto elapsed_ms = [&] {
    LARGE_INTEGER t1{};
    QueryPerformanceCounter(&t1);
    return static_cast<uint32_t>(
        (t1.QuadPart - t0.QuadPart) * 1000 / qpf.QuadPart);
  };

  OutputIdentityTracker map;  // the probe's own 1:1 mapping
  bool first_output_seen = false;
  uint32_t first_output_inputs = 0;  // inputs submitted when it appeared
  uint32_t first_output_ms = 0;
  size_t outputs = 0;
  std::string why;
  for (uint32_t i = 0; i < kProbeInputBound && why.empty(); ++i) {
    if (!WaitForNeedInput(probe, kNeedInputWaitMs)) {
      why = "probe: no METransformNeedInput within budget (input #" +
            std::to_string(i) + ")";
      break;
    }
    if (probe.is_async) --probe.need_credits;
    if (!conv.Blt(bgra_tex[i % 2].Get(), static_cast<int>(i % 3))) {
      why = "probe: VideoProcessorBlt failed";
      break;
    }
    const int64_t t = static_cast<int64_t>(i + 1) * frame_dur;
    OutputIdentityRecord rec{};
    rec.id = FrameIdentity{1, 1, 100 + i, i + 1, 0, 0};
    rec.submit_id = i + 1;
    rec.slot = i % 3;
    if (!map.Register(t, rec)) {
      why = "probe: sample-time registration not strictly increasing";
      break;
    }
    if (!UnitSubmitTexture(probe, conv.nv12[i % 3].Get(), t, frame_dur,
                           i == 0 || i + 1 == kProbeInputBound)) {
      why = "probe: ProcessInput rejected the DXGI sample";
      break;
    }
    std::vector<UnitAu> aus;
    CollectUnitOutputs(probe, 0, &aus, nullptr);
    for (auto& a : aus) {
      if (!first_output_seen) {
        first_output_seen = true;
        first_output_inputs = i + 1;
        first_output_ms = elapsed_ms();
      }
      ++outputs;
      OutputIdentityRecord got{};
      const OutputConsume v = map.Consume(a.has_time, a.time, &got);
      if (v != OutputConsume::kOk) {
        why = "probe: output sample time " +
              std::string(v == OutputConsume::kTimeMissing   ? "missing"
                          : v == OutputConsume::kTimeDuplicated ? "duplicated"
                                                                : "unknown") +
              " (1:1 mapping violated)";
        break;
      }
      if (a.key && !(NalHasType(a.au.data(), a.au.size(), 7) &&
                     NalHasType(a.au.data(), a.au.size(), 8))) {
        why = "probe: IDR AU lacks SPS/PPS NALs";
        break;
      }
    }
  }

  bool ok = why.empty();
  if (ok && !first_output_seen) {
    ok = false;
    why = "probe: no output within " + std::to_string(kProbeInputBound) +
          " inputs";
  }
  if (ok) {
    // First output within two inputs OR 100 ms (spec §8.3 item 2).
    if (first_output_inputs > 2 && first_output_ms > kProbeFirstOutputMs) {
      ok = false;
      why = "probe: first output at input #" +
            std::to_string(first_output_inputs) + "/" +
            std::to_string(first_output_ms) + "ms (bound: 2 inputs or " +
            std::to_string(kProbeFirstOutputMs) + "ms)";
    }
  }
  if (ok && outputs == 0) {
    ok = false;
    why = "probe: zero outputs";
  }
  if (ok && map.PendingCount() != 0) {
    // Not a rejection by itself (pipeline depth may exceed 8) - logged for
    // diagnosis; every emitted output still mapped 1:1.
    XNC_LOG_INFO("gpu_probe_pending_after_8 inputs=%u outputs=%zu pending=%zu",
                 kProbeInputBound, outputs, map.PendingCount());
  }
  XNC_LOG_INFO("gpu_probe friendly=\"%s\" ok=%d inputs=%u outputs=%zu "
               "first_out_input=%u first_out_ms=%u why=\"%s\"",
               WideToNarrow(friendly).c_str(), ok ? 1 : 0, kProbeInputBound,
               outputs, first_output_inputs, first_output_ms,
               (ok ? std::string() : why).c_str());
  if (err != nullptr && !ok) *err = why;
  ReleaseUnit(probe);
  return ok;
}

}  // namespace

// ---- MfGpuEncoder ----

struct MfGpuEncoder::Impl {
  ComPtr<ID3D11Device> dev;  // AddRef'd view of the caller's shared device
  ComPtr<ID3D11DeviceContext> ctx;
  ComPtr<IMFDXGIDeviceManager> mgr;
  UINT reset_token = 0;
  Unit unit;
  OutputIdentityTracker tracker;
  std::vector<EncoderOutput> queued;
  uint64_t last_seq = 0;
  int64_t last_time = 0;
  uint32_t w = 0, h = 0, fps = 30, bitrate = 0;
  bool mf_startup_owner = false;
  bool co_init_owner = false;
  bool identity_fault = false;
};

MfGpuEncoder::MfGpuEncoder() = default;
MfGpuEncoder::~MfGpuEncoder() { Shutdown(ShutdownMode::kImmediate); }

bool MfGpuEncoder::Init(ID3D11Device* dev, Nv12SurfacePool* pool, uint32_t w,
                        uint32_t h, uint32_t fps, uint32_t bitrate_bps,
                        std::string* err) {
  Shutdown(ShutdownMode::kImmediate);
  auto fail = [err](const std::string& msg) {
    if (err) *err = msg;
    return false;
  };
  if (dev == nullptr || pool == nullptr) return fail("gpu init: null dev/pool");
  if (w == 0 || h == 0 || (w % 2) != 0 || (h % 2) != 0)
    return fail("gpu init: dimensions must be non-zero even");
  if (fps == 0) return fail("gpu init: fps must be > 0");
  if (bitrate_bps == 0) bitrate_bps = 2000000;

  // Async MFTs only raise events on an initialized MF platform; the rest
  // of this binary runs sync MFTs and never needed MFStartup. S_OK/S_FALSE
  // both mean the platform is up under our reference.
  HRESULT hr = MFStartup(MF_VERSION, MFSTARTUP_LITE);
  const bool mf_owner = SUCCEEDED(hr);
  if (FAILED(hr))
    XNC_LOG_INFO("gpu_mfstartup_failed hr=0x%08x (continuing; sync-only)",
                 static_cast<unsigned int>(hr));

  hr = CoInitializeEx(nullptr, COINIT_MULTITHREADED);
  bool co_owner = false;
  if (hr == RPC_E_CHANGED_MODE) {
    XNC_LOG_INFO("gpu_com_init already_initialized_other_apartment");
  } else if (FAILED(hr)) {
    if (mf_owner) MFShutdown();
    return fail(HrStep("CoInitializeEx", hr));
  } else {
    co_owner = true;
  }

  impl_ = new Impl();
  impl_->mf_startup_owner = mf_owner;
  impl_->co_init_owner = co_owner;
  impl_->dev = dev;  // AddRef'd view (shared device, plan ruling 4)
  dev->GetImmediateContext(impl_->ctx.GetAddressOf());
  pool_ = pool;
  impl_->w = w;
  impl_->h = h;
  impl_->fps = fps;
  impl_->bitrate = bitrate_bps;

  hr = MFCreateDXGIDeviceManager(&impl_->reset_token,
                                 impl_->mgr.GetAddressOf());
  if (SUCCEEDED(hr))
    hr = impl_->mgr->ResetDevice(dev, impl_->reset_token);
  if (FAILED(hr)) {
    const std::string msg = HrStep("MFCreateDXGIDeviceManager/ResetDevice", hr);
    Shutdown(ShutdownMode::kImmediate);
    return fail(msg);
  }

  // Hardware ladder: first candidate that negotiates AND passes the §8.3
  // startup probe wins; a pristine instance of the winner is re-activated
  // for the live stream. acts[i] references: the winner's is Attach'ed to
  // the live unit (released at Shutdown); every other candidate's is
  // ShutdownObject'd + released once, below.
  MFT_REGISTER_TYPE_INFO in_ri{MFMediaType_Video, MFVideoFormat_NV12};
  MFT_REGISTER_TYPE_INFO out_ri{MFMediaType_Video, MFVideoFormat_H264};
  IMFActivate** acts = nullptr;
  UINT32 nacts = 0;
  hr = MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER,
                 MFT_ENUM_FLAG_HARDWARE | MFT_ENUM_FLAG_SORTANDFILTER, &in_ri,
                 &out_ri, &acts, &nacts);
  if (FAILED(hr)) {
    const std::string msg = HrStep("MFTEnumEx(hw)", hr);
    Shutdown(ShutdownMode::kImmediate);
    return fail(msg);
  }
  long winner = -1;
  std::string last_err = "no D3D11-aware hardware H.264 encoder enumerated";
  for (UINT32 i = 0; i < nacts; ++i) {
    const std::wstring friendly = ActivateFriendlyName(acts[i]);
    std::string perr;
    if (!RunStartupProbe(impl_->mgr.Get(), impl_->dev.Get(),
                         impl_->ctx.Get(), acts[i], w, h, fps, bitrate_bps,
                         friendly, &perr)) {
      last_err = perr.empty() ? "startup probe failed" : perr;
      continue;
    }
    // Pristine live instance of the probed winner.
    Unit live;
    hr = acts[i]->ActivateObject(IID_PPV_ARGS(live.mft.GetAddressOf()));
    if (FAILED(hr)) {
      last_err = HrStep("ActivateObject(live)", hr);
      continue;
    }
    live.activate.Attach(acts[i]);  // own the enum's reference from here
    std::string lerr;
    if (ConfigureUnit(live, impl_->mgr.Get(), w, h, fps, bitrate_bps, &lerr)) {
      impl_->unit = std::move(live);
      winner = static_cast<long>(i);
      friendly_name_ = WideToNarrow(friendly);
      break;
    }
    last_err = "live re-init: " + lerr;
    ReleaseUnit(live);  // releases the attached enum reference
  }
  if (acts != nullptr) {
    for (UINT32 j = 0; j < nacts; ++j) {
      if (static_cast<long>(j) == winner) continue;
      acts[j]->ShutdownObject();
      acts[j]->Release();
    }
    CoTaskMemFree(acts);
  }
  if (winner < 0) {
    const std::string msg =
        "no hardware H.264 encoder passed the D3D11-aware startup probe: " +
        last_err;
    Shutdown(ShutdownMode::kImmediate);
    return fail(msg);
  }
  XNC_LOG_INFO("gpu_encoder_init w=%u h=%u fps=%u bitrate=%u async=%d "
               "provides=%d friendly=\"%s\"",
               w, h, fps, bitrate_bps, impl_->unit.is_async ? 1 : 0,
               impl_->unit.provides_samples ? 1 : 0, friendly_name_.c_str());
  return true;
}

SubmitResult MfGpuEncoder::Submit(const FrameIdentity& id, SurfaceLease&& lease,
                                  bool force_idr) {
  last_error_ = EncoderSessionError::kNone;
  if (impl_ == nullptr || impl_->unit.mft.Get() == nullptr) {
    last_error_ = EncoderSessionError::kNotReady;
    lease.Release();  // CONVERTING -> FREE: never submitted, handed back
    return SubmitResult::kNotReady;
  }
  if (impl_->identity_fault) {
    last_error_ = EncoderSessionError::kEncoderIdentityMismatch;
    lease.Release();
    return SubmitResult::kIdentityFault;
  }
  if (!lease) {
    lease.Release();
    return SubmitResult::kRejected;
  }
  if (id.encode_seq <= impl_->last_seq) {
    XNC_LOG_ERROR("gpu_submit_seq_not_increasing seq=%llu last=%llu",
                  static_cast<unsigned long long>(id.encode_seq),
                  static_cast<unsigned long long>(impl_->last_seq));
    lease.Release();
    return SubmitResult::kIdentityFault;
  }

  const size_t slot = lease.index();
  if (!lease.Submit(id.encode_seq)) {
    lease.Release();
    last_error_ = EncoderSessionError::kNotReady;
    return SubmitResult::kNotReady;
  }

  // Strictly increasing 100ns time keyed to encodeSeq.
  const int64_t frame_dur =
      static_cast<int64_t>(10000000 / (impl_->fps ? impl_->fps : 30));
  int64_t t = static_cast<int64_t>(id.encode_seq) * frame_dur;
  if (impl_->last_time != 0 && t <= impl_->last_time) t = impl_->last_time + 1;
  OutputIdentityRecord rec;
  rec.id = id;
  rec.submit_id = id.encode_seq;
  rec.slot = slot;
  if (!impl_->tracker.Register(t, rec)) {
    pool_->Complete(id.encode_seq);
    impl_->identity_fault = true;
    last_error_ = EncoderSessionError::kEncoderIdentityMismatch;
    return SubmitResult::kIdentityFault;
  }

  if (!WaitForNeedInput(impl_->unit, kNeedInputWaitMs)) {
    // No submit permission in budget: the MFT never saw this time - roll
    // the registration back and release the lease.
    impl_->tracker.RollbackNewest();
    impl_->last_time = impl_->tracker.last_registered_time();
    pool_->Complete(id.encode_seq);
    last_error_ = EncoderSessionError::kNotReady;
    return SubmitResult::kNotReady;
  }
  if (impl_->unit.is_async) --impl_->unit.need_credits;

  if (!UnitSubmitTexture(impl_->unit, lease.texture(), t, frame_dur,
                         force_idr)) {
    impl_->tracker.RollbackNewest();
    impl_->last_time = impl_->tracker.last_registered_time();
    pool_->Complete(id.encode_seq);
    return SubmitResult::kRejected;
  }
  impl_->last_seq = id.encode_seq;
  impl_->last_time = t;

  // Opportunistic output collection (non-blocking).
  std::vector<UnitAu> aus;
  std::string cerr_;
  CollectUnitOutputs(impl_->unit, 0, &aus, &cerr_);
  SessionCollectOutputs(impl_->tracker, pool_, aus, impl_->queued,
                        &impl_->identity_fault, true, &last_error_);
  return SubmitResult::kOk;
}

bool MfGpuEncoder::TakeOutput(EncoderOutput* out, uint32_t timeout_ms) {
  last_error_ = EncoderSessionError::kNone;
  if (out == nullptr) return false;
  if (impl_ == nullptr || impl_->unit.mft.Get() == nullptr) {
    last_error_ = EncoderSessionError::kNotReady;
    return false;
  }
  if (impl_->identity_fault) {
    last_error_ = EncoderSessionError::kEncoderIdentityMismatch;
    return false;
  }
  const bool is_async = impl_->unit.is_async;
  const ULONGLONG deadline = GetTickCount64() + timeout_ms;
  for (;;) {
    if (!impl_->queued.empty()) {
      *out = std::move(impl_->queued.front());
      impl_->queued.erase(impl_->queued.begin());
      return true;
    }
    std::vector<UnitAu> aus;
    std::string cerr_;
    // Async: a short event-wait slice per pass (the MFT raises HaveOutput
    // between inputs). Sync: sweep once - a sync MFT produces output only
    // when new input arrives, so sleeping out the timeout is pointless.
    CollectUnitOutputs(impl_->unit, is_async ? 8 : 0, &aus, &cerr_);
    if (!cerr_.empty()) last_error_ = EncoderSessionError::kCollectFailed;
    if (!aus.empty() &&
        !SessionCollectOutputs(impl_->tracker, pool_, aus, impl_->queued,
                               &impl_->identity_fault, true, &last_error_)) {
      return false;
    }
    if (!impl_->queued.empty()) {
      *out = std::move(impl_->queued.front());
      impl_->queued.erase(impl_->queued.begin());
      return true;
    }
    if (impl_->identity_fault) {
      last_error_ = EncoderSessionError::kEncoderIdentityMismatch;
      return false;
    }
    if (!is_async || GetTickCount64() >= deadline) {
      if (last_error_ == EncoderSessionError::kNone)
        last_error_ = EncoderSessionError::kTimeout;
      return false;
    }
    Sleep(2);  // bounded poll cadence (no hot spin)
  }
}

bool MfGpuEncoder::Reconfigure(uint32_t bitrate, uint32_t fps) {
  if (impl_ == nullptr || impl_->unit.mft.Get() == nullptr) return false;
  if (fps != 0) impl_->fps = fps;  // future sample-time base (§8.2 hot fps)
  if (bitrate == 0) return fps != 0;
  if (impl_->unit.codec_api.Get() == nullptr) return false;
  VARIANT v{};
  v.vt = VT_UI4;
  v.ulVal = bitrate;
  const HRESULT hr =
      impl_->unit.codec_api->SetValue(&CODECAPI_AVEncCommonMeanBitRate, &v);
  if (FAILED(hr)) {
    XNC_LOG_INFO("gpu_reconfigure_rate_rejected hr=0x%08x",
                 static_cast<unsigned int>(hr));
    return false;
  }
  impl_->bitrate = bitrate;
  return true;
}

void MfGpuEncoder::Shutdown(ShutdownMode mode) {
  if (impl_ == nullptr) return;
  if (mode == ShutdownMode::kDrain && !impl_->identity_fault &&
      impl_->unit.mft.Get() != nullptr) {
    impl_->unit.mft->ProcessMessage(MFT_MESSAGE_NOTIFY_END_OF_STREAM, 0);
    impl_->unit.mft->ProcessMessage(MFT_MESSAGE_COMMAND_DRAIN, 0);
    if (impl_->unit.is_async) {
      bool drain_done = false;
      const ULONGLONG deadline = GetTickCount64() + kDrainWaitMs;
      while (!drain_done && GetTickCount64() < deadline)
        PumpEvents(impl_->unit, 8, &drain_done);
    }
    std::vector<UnitAu> aus;
    std::string cerr_;
    CollectUnitOutputs(impl_->unit, impl_->unit.is_async ? 16 : 0, &aus,
                       &cerr_);
    // The session is torn down below - the tail AUs cannot be collected
    // afterward; map them through the tracker anyway (identity validation
    // + lease completion) and log the count.
    std::vector<EncoderOutput> tail;
    SessionCollectOutputs(impl_->tracker, pool_, aus, tail,
                          &impl_->identity_fault, true, &last_error_);
    if (!tail.empty())
      XNC_LOG_INFO("gpu_shutdown_drain tail_aus=%zu (dropped at teardown)",
                   tail.size());
  }
  // Every still-pending lease completes EXACTLY once (fault, immediate
  // shutdown, or outputs that never came).
  for (const auto& p : impl_->tracker.PendingRecords())
    pool_->Complete(p.submit_id);
  impl_->tracker.Clear();
  ReleaseUnit(impl_->unit);
  const bool co_owner = impl_->co_init_owner;
  const bool mf_owner = impl_->mf_startup_owner;
  delete impl_;
  impl_ = nullptr;
  if (mf_owner) MFShutdown();
  if (co_owner) CoUninitialize();
}

// ---- MfCpuEncoder ----

struct MfCpuEncoder::Impl {
  ComPtr<ID3D11Device> dev;
  ComPtr<ID3D11DeviceContext> ctx;
  ComPtr<ID3D11Texture2D> staging;  // persistent NV12 CPU_READ target
  uint32_t w = 0, h = 0;
};

MfCpuEncoder::MfCpuEncoder() = default;
MfCpuEncoder::~MfCpuEncoder() { Shutdown(ShutdownMode::kImmediate); }

bool MfCpuEncoder::Init(ID3D11Device* dev, Nv12SurfacePool* pool, uint32_t w,
                        uint32_t h, uint32_t fps, uint32_t bitrate_bps,
                        std::string* err) {
  Shutdown(ShutdownMode::kImmediate);
  auto fail = [err](const std::string& msg) {
    if (err) *err = msg;
    return false;
  };
  if (dev == nullptr || pool == nullptr) return fail("cpu init: null dev/pool");
  if (w == 0 || h == 0 || (w % 2) != 0 || (h % 2) != 0)
    return fail("cpu init: dimensions must be non-zero even");
  if (fps == 0) return fail("cpu init: fps must be > 0");
  std::string ierr;
  if (!enc_.Init(w, h, fps, bitrate_bps, &ierr))
    return fail("cpu init: " + ierr);
  impl_ = new Impl();
  impl_->dev = dev;
  dev->GetImmediateContext(impl_->ctx.GetAddressOf());
  D3D11_TEXTURE2D_DESC sd{};
  sd.Width = w;
  sd.Height = h;
  sd.MipLevels = 1;
  sd.ArraySize = 1;
  sd.SampleDesc.Count = 1;
  sd.Format = DXGI_FORMAT_NV12;
  sd.Usage = D3D11_USAGE_STAGING;
  sd.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
  const HRESULT hr = dev->CreateTexture2D(&sd, nullptr, &impl_->staging);
  if (FAILED(hr)) {
    Shutdown(ShutdownMode::kImmediate);
    return fail(HrStep("CreateTexture2D(cpu staging)", hr));
  }
  impl_->w = w;
  impl_->h = h;
  pool_ = pool;
  fps_ = fps;
  last_seq_ = 0;
  last_time_ = 0;
  identity_fault_ = false;
  queued_.clear();
  tracker_.Clear();
  inited_ = true;
  XNC_LOG_INFO("cpu_session_init w=%u h=%u fps=%u bitrate=%u backend=%s",
               w, h, fps, bitrate_bps, enc_.BackendName());
  return true;
}

SubmitResult MfCpuEncoder::Submit(const FrameIdentity& id, SurfaceLease&& lease,
                                  bool force_idr) {
  last_error_ = EncoderSessionError::kNone;
  if (!inited_ || impl_ == nullptr || !lease) {
    last_error_ = EncoderSessionError::kNotReady;
    lease.Release();
    return SubmitResult::kNotReady;
  }
  if (identity_fault_) {
    last_error_ = EncoderSessionError::kEncoderIdentityMismatch;
    lease.Release();
    return SubmitResult::kIdentityFault;
  }
  if (id.encode_seq <= last_seq_) {
    XNC_LOG_ERROR("cpu_submit_seq_not_increasing seq=%llu last=%llu",
                  static_cast<unsigned long long>(id.encode_seq),
                  static_cast<unsigned long long>(last_seq_));
    lease.Release();
    return SubmitResult::kIdentityFault;
  }

  // 1. Secure the NV12 bytes (the CPU rung's readback - its inherent cost;
  //    the healthy GPU session never does this).
  const uint32_t w = impl_->w, h = impl_->h;
  const size_t tight = static_cast<size_t>(w) * h * 3 / 2;
  std::vector<uint8_t> nv12(tight);
  {
    impl_->ctx->CopyResource(impl_->staging.Get(), lease.texture());
    D3D11_MAPPED_SUBRESOURCE map{};
    if (FAILED(impl_->ctx->Map(impl_->staging.Get(), 0, D3D11_MAP_READ, 0,
                               &map))) {
      last_error_ = EncoderSessionError::kCollectFailed;
      lease.Release();  // never submitted
      return SubmitResult::kNotReady;
    }
    const uint8_t* src = static_cast<const uint8_t*>(map.pData);
    // Y plane then interleaved UV; each row w bytes at map.RowPitch.
    for (uint32_t row = 0; row < h + h / 2; ++row) {
      std::memcpy(nv12.data() + static_cast<size_t>(row) * w,
                  src + static_cast<size_t>(row) * map.RowPitch, w);
    }
    impl_->ctx->Unmap(impl_->staging.Get(), 0);
  }

  // 2. Lease transition + registration.
  const size_t slot = lease.index();
  if (!lease.Submit(id.encode_seq)) {
    lease.Release();
    last_error_ = EncoderSessionError::kNotReady;
    return SubmitResult::kNotReady;
  }
  const int64_t frame_dur = static_cast<int64_t>(10000000 / (fps_ ? fps_ : 30));
  int64_t t = static_cast<int64_t>(id.encode_seq) * frame_dur;
  if (last_time_ != 0 && t <= last_time_) t = last_time_ + 1;
  OutputIdentityRecord rec;
  rec.id = id;
  rec.submit_id = id.encode_seq;
  rec.slot = slot;
  if (!tracker_.Register(t, rec)) {
    pool_->Complete(id.encode_seq);
    identity_fault_ = true;
    last_error_ = EncoderSessionError::kEncoderIdentityMismatch;
    return SubmitResult::kIdentityFault;
  }

  // 3. Feed the M0 engine with the session-owned time.
  if (force_idr) enc_.ForceNextIdr("session-submit");
  std::vector<std::vector<uint8_t>> aus;
  std::vector<int64_t> times;
  std::string eerr;
  const EncoderSubmitResult r =
      enc_.EncodeNV12(nv12.data(), nv12.size(), aus, &eerr, &times, &t);
  if (!r.input_accepted) {
    // ProcessInput refused: the MFT never saw this time - roll back.
    tracker_.RollbackNewest();
    last_time_ = tracker_.last_registered_time();
    pool_->Complete(id.encode_seq);
    XNC_LOG_ERROR("cpu_session_encode_rejected err=\"%s\"", eerr.c_str());
    return SubmitResult::kRejected;
  }

  // 4. Consumption release: the bytes are secured; the CPU MFT's lookahead
  // lives in CPU memory, not GPU surfaces (holding 3 GPU slots for a
  // ~17-frame CPU lookahead would starve the pool - the GPU session is
  // the rung that must hold leases until output).
  pool_->Complete(id.encode_seq);
  last_seq_ = id.encode_seq;
  last_time_ = t;

  // 5. Map whatever came back by exact sample time (the CPU rung completed
  // its leases already; on fault only the bookkeeping must be cleaned).
  bool map_ok = true;
  {
    std::vector<UnitAu> uas(aus.size());
    for (size_t i = 0; i < aus.size(); ++i) {
      uas[i].au = std::move(aus[i]);
      uas[i].time = i < times.size() ? times[i] : 0;
      uas[i].has_time = uas[i].time != 0;
      uas[i].key = NalHasType(uas[i].au.data(), uas[i].au.size(), 5);
    }
    map_ok = SessionCollectOutputs(tracker_, nullptr, uas, queued_,
                                    &identity_fault_, false, &last_error_);
  }
  if (!r.outputs_ok && last_error_ == EncoderSessionError::kNone)
    last_error_ = EncoderSessionError::kCollectFailed;
  return map_ok ? SubmitResult::kOk : SubmitResult::kIdentityFault;
}

bool MfCpuEncoder::TakeOutput(EncoderOutput* out, uint32_t timeout_ms) {
  (void)timeout_ms;  // CPU outputs surface at Submit; nothing to wait for
  last_error_ = EncoderSessionError::kNone;
  if (out == nullptr) return false;
  if (!inited_) {
    last_error_ = EncoderSessionError::kNotReady;
    return false;
  }
  if (identity_fault_) {
    last_error_ = EncoderSessionError::kEncoderIdentityMismatch;
    return false;
  }
  if (queued_.empty()) {
    last_error_ = EncoderSessionError::kTimeout;
    return false;
  }
  *out = std::move(queued_.front());
  queued_.erase(queued_.begin());
  return true;
}

bool MfCpuEncoder::Reconfigure(uint32_t bitrate, uint32_t fps) {
  if (!inited_) return false;
  if (fps != 0) fps_ = fps;
  if (bitrate == 0) return fps != 0;
  return enc_.ReconfigureRate(bitrate);
}

void MfCpuEncoder::Shutdown(ShutdownMode mode) {
  if (impl_ == nullptr) return;
  if (mode == ShutdownMode::kDrain && inited_ && !identity_fault_) {
    std::vector<std::vector<uint8_t>> aus;
    std::vector<int64_t> times;
    enc_.FlushTail(aus, nullptr, &times);
    std::vector<UnitAu> uas(aus.size());
    for (size_t i = 0; i < aus.size(); ++i) {
      uas[i].au = std::move(aus[i]);
      uas[i].time = i < times.size() ? times[i] : 0;
      uas[i].has_time = uas[i].time != 0;
    }
    // The session is torn down below - map the tail through the tracker
    // (identity validation) into a throwaway sink and log the count.
    std::vector<EncoderOutput> tail;
    SessionCollectOutputs(tracker_, nullptr, uas, tail, &identity_fault_,
                          false, &last_error_);
    if (!tail.empty())
      XNC_LOG_INFO("cpu_shutdown_drain tail_aus=%zu (dropped at teardown)",
                   tail.size());
    // Registrations still pending map to leases already completed at
    // consumption - nothing further to release on the CPU rung.
  }
  tracker_.Clear();
  queued_.clear();
  last_seq_ = 0;
  last_time_ = 0;
  identity_fault_ = false;
  delete impl_;
  impl_ = nullptr;
  pool_ = nullptr;
  inited_ = false;
}

const char* MfCpuEncoder::BackendName() const { return enc_.BackendName(); }
EncoderBackend MfCpuEncoder::backend() const { return enc_.backend(); }
const std::string& MfCpuEncoder::FriendlyName() const {
  return enc_.FriendlyName();
}

}  // namespace xnc
