// mf_encoder.cpp - Media Foundation H.264 encoder pipeline (see mf_encoder.h).
// Clean-room C++ rewrite of the reference flow (agent/screen-helper/
// encode_windows.go - same SDK calls and parameter alignment, adapted not
// copied), extended with the hardware-first ladder (hw-encode task):
//   CoInitializeEx(MTA) ->
//   hardware ladder: MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER,
//     MFT_ENUM_FLAG_HARDWARE | MFT_ENUM_FLAG_SORTANDFILTER, NV12-in, H264-out)
//     -> per candidate: ActivateObject -> SetOutputType(H264: frame size,
//     frame rate = caller fps, bitrate; enumerated H264-type fallback) ->
//     SetInputType(NV12: tight stride = width, one standard attempt; the
//     Part A variant ladder was dead code on Arc — every CPU-NV12 variant
//     was rejected 0xC00D6D77, D3D11 textures are M4 — and was removed in
//     feat/arch-clean; negotiated-stride padding stays for MFTs that widen
//     the input stride) -> ICodecAPI best-effort shaping (GOP / B-frames=0 /
//     low-delay RC / low-latency) -> GetOutputStreamInfo -> Begin/StartOfStream
//     -> SELF-CHECK: 1..8 synthetic frames through the real submit path,
//     first candidate whose output is non-empty wins; a pristine second
//     instance of the winner is re-activated for the real stream (the
//     self-check frames never leak into the caller's AU sequence)
//   -> all candidates rejected / none present / SetForceSoftware ->
//     CoCreateInstance(CMSH264EncoderMFT) software fallback (the old path).
//   Per frame: BGRA->NV12 (stride-expanded when the MFT negotiated a wider
//   stride) -> IMFSample (wall-clock 100ns PTS) -> one-shot force-key at
//   submission -> ProcessInput -> ProcessOutput loop (NEED_MORE_INPUT ends
//   the loop, STREAM_CHANGE retries) -> one AU per output sample, IDR
//   detection + first-IDR SPS/PPS cache. Backend/FriendlyName/self-check
//   outcome are logged (encoder_backend=hardware|software).
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>

#include <codecapi.h>   // CODECAPI_* property GUIDs + rate-control enums
#include <mfapi.h>      // MFCreate* media type/buffer/sample, MF_MT_*
#include <mferror.h>    // MF_E_TRANSFORM_*
#include <mftransform.h>  // IMFTransform, MFT_MESSAGE_*, output buffers
#include <strmif.h>       // ICodecAPI interface (codecapi.h has only props)
#include <wrl/client.h>   // ComPtr

#include <cstdio>
#include <cstring>
#include <string>

#include "../common/log.h"
#include "mf_encoder.h"
#include "nv12.h"

namespace xnc {

namespace {

using Microsoft::WRL::ComPtr;

// CLSID_CMSH264EncoderMFT (wmcodecdsp.h; not included to keep the Windows
// surface small - value verified against the SDK header. 62ce7e72-… is the
// DEcoder, do not "fix" this GUID).
const CLSID kClsidCMSH264EncoderMFT = {0x6ca50344, 0x051a, 0x4ded,
                                       {0x97, 0x79, 0xa4, 0x33, 0x05, 0x16, 0x5e, 0x35}};
// Software rung's display name for logs (the CLSID has no friendly name).
const wchar_t kSoftwareFriendlyName[] = L"CMSH264EncoderMFT (Microsoft H.264 Software Encoder)";

// Hardware ladder self-check bound: feed at most this many synthetic frames
// before declaring a candidate dead. Hardware encoders in low-latency mode
// emit within the first 1-2 submissions; the bound guards drivers with a
// deeper pipeline without stalling Init.
constexpr uint32_t kSelfCheckMaxFrames = 8;

std::string HrStep(const char* step, HRESULT hr) {
  char buf[160];
  std::snprintf(buf, sizeof(buf), "%s: hr=0x%08X", step, static_cast<unsigned int>(hr));
  return std::string(buf);
}

// Best-effort ICodecAPI VT_UI4 setter: log-and-continue on rejection
// (spec §7.10: 低延迟配置可设则设,失败容忍并记日志).
bool CodecApiSetUi4(ICodecAPI* api, const GUID* prop, uint32_t value, const char* name) {
  VARIANT v{};  // zero-init => no VariantInit/oleaut32 dependency
  v.vt = VT_UI4;
  v.ulVal = value;
  const HRESULT hr = api->SetValue(prop, &v);
  if (FAILED(hr)) {
    XNC_LOG_INFO("codec_api_set_skip name=%s hr=0x%08x", name, static_cast<unsigned int>(hr));
    return false;
  }
  return true;
}

// The caller's H.264 output type (frame size, caller fps, bitrate).
HRESULT MakeH264OutputType(uint32_t w, uint32_t h, uint32_t fps, uint32_t bitrate,
                           IMFMediaType** out) {
  const uint64_t frame_size = (static_cast<uint64_t>(w) << 32) | h;
  const uint64_t frame_rate = (static_cast<uint64_t>(fps) << 32) | 1;
  ComPtr<IMFMediaType> mt;
  HRESULT hr = MFCreateMediaType(mt.GetAddressOf());
  if (SUCCEEDED(hr)) hr = mt->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
  if (SUCCEEDED(hr)) hr = mt->SetGUID(MF_MT_SUBTYPE, MFVideoFormat_H264);
  if (SUCCEEDED(hr)) hr = mt->SetUINT64(MF_MT_FRAME_SIZE, frame_size);
  if (SUCCEEDED(hr)) hr = mt->SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
  if (SUCCEEDED(hr)) hr = mt->SetUINT64(MF_MT_FRAME_RATE, frame_rate);
  if (SUCCEEDED(hr)) hr = mt->SetUINT32(MF_MT_AVG_BITRATE, bitrate);
  if (SUCCEEDED(hr)) *out = mt.Detach();
  return hr;
}

// The caller's NV12 input type (tight stride = width) — the ONE standard
// input attempt for both rungs. Hardware MFTs that reject CPU NV12 (QSV
// needs D3D11 textures, M4) log the failure and the ladder falls back to
// the next candidate / the software rung.
HRESULT MakeNv12InputType(uint32_t w, uint32_t h, uint32_t fps, IMFMediaType** out) {
  const uint64_t frame_size = (static_cast<uint64_t>(w) << 32) | h;
  const uint64_t frame_rate = (static_cast<uint64_t>(fps) << 32) | 1;
  ComPtr<IMFMediaType> mt;
  HRESULT hr = MFCreateMediaType(mt.GetAddressOf());
  if (SUCCEEDED(hr)) hr = mt->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
  if (SUCCEEDED(hr)) hr = mt->SetGUID(MF_MT_SUBTYPE, MFVideoFormat_NV12);
  if (SUCCEEDED(hr)) hr = mt->SetUINT32(MF_MT_DEFAULT_STRIDE, w);
  if (SUCCEEDED(hr)) hr = mt->SetUINT64(MF_MT_FRAME_SIZE, frame_size);
  if (SUCCEEDED(hr)) hr = mt->SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
  if (SUCCEEDED(hr)) hr = mt->SetUINT64(MF_MT_FRAME_RATE, frame_rate);
  if (SUCCEEDED(hr)) *out = mt.Detach();
  return hr;
}

// Expands a tight-stride NV12 frame (w-byte rows) into a padded one
// (stride-byte rows, zero padding) - needed when a hardware MFT negotiated
// an aligned stride wider than the frame width.
void ExpandToStride(const uint8_t* tight, uint8_t* padded, uint32_t w, uint32_t h,
                    uint32_t stride) {
  const size_t row = w;
  for (uint32_t y = 0; y < h; ++y) {
    uint8_t* dst = padded + static_cast<size_t>(y) * stride;
    std::memcpy(dst, tight + static_cast<size_t>(y) * row, row);
    std::memset(dst + row, 0, stride - w);
  }
  const size_t uv_off = static_cast<size_t>(stride) * h;
  const size_t uv_src = static_cast<size_t>(w) * h;
  for (uint32_t y = 0; y < h / 2; ++y) {
    uint8_t* dst = padded + uv_off + static_cast<size_t>(y) * stride;
    std::memcpy(dst, tight + uv_src + static_cast<size_t>(y) * row, row);
    std::memset(dst + row, 0, stride - w);
  }
}

// Fetches an IMFActivate's MFT_FRIENDLY_NAME_Attribute ("" on failure).
std::wstring ActivateFriendlyName(IMFActivate* act) {
  wchar_t* name = nullptr;
  UINT32 nlen = 0;
  std::wstring out;
  if (SUCCEEDED(act->GetAllocatedString(MFT_FRIENDLY_NAME_Attribute, &name, &nlen)) &&
      name != nullptr) {
    out.assign(name, nlen);
  }
  CoTaskMemFree(name);
  return out;
}

std::string WideToNarrow(const std::wstring& w) {
  std::string out;
  for (wchar_t c : w) {
    if (c < 0x80) out.push_back(static_cast<char>(c));
    else out.push_back('?');
  }
  return out;
}

}  // namespace

struct MfSoftEncoder::Impl {
  ComPtr<IMFTransform> mft;
  ComPtr<ICodecAPI> codec_api;
  ComPtr<IMFActivate> activate;  // hardware ladder: factory of the live MFT
  bool mft_provides_samples = false;  // MFT_OUTPUT_STREAM_PROVIDES_SAMPLES
  size_t out_buf_size = 0;            // client-allocated output buffer size
  LARGE_INTEGER qpc0{};               // wall-clock PTS base
  LARGE_INTEGER qpc_freq{};
  int64_t rt_last = 0;                // last sample time, 100ns units
  bool co_init_owner = false;
  uint32_t in_stride = 0;             // negotiated input stride (0 = tight)
  std::vector<uint8_t> in_padded_;    // stride-expanded input (in_stride>w)
  std::string friendly_narrow;        // FriendlyName for logs (narrow)

  // Wall-clock 100ns ticks since Init (monotonic, QPC-based).
  int64_t Now100ns() const {
    LARGE_INTEGER now{};
    QueryPerformanceCounter(&now);
    const double ticks = static_cast<double>(now.QuadPart - qpc0.QuadPart);
    return static_cast<int64_t>(ticks * 1e7 / static_cast<double>(qpc_freq.QuadPart));
  }
};

MfSoftEncoder::MfSoftEncoder() = default;

MfSoftEncoder::~MfSoftEncoder() { Shutdown(); }

void MfSoftEncoder::ReleaseMft() {
  if (!impl_) return;
  if (impl_->activate.Get() != nullptr) {
    impl_->activate->ShutdownObject();  // release the hardware session first
    impl_->activate.Reset();
  }
  if (impl_->mft.Get() != nullptr) {
    impl_->mft->ProcessMessage(MFT_MESSAGE_NOTIFY_END_STREAMING, 0);
  }
  impl_->codec_api.Reset();
  impl_->mft.Reset();
  impl_->mft_provides_samples = false;
  impl_->out_buf_size = 0;
  impl_->rt_last = 0;
  impl_->in_stride = 0;
  impl_->in_padded_.clear();
  sps_pps_.clear();
  last_was_key_ = false;
  force_pending_ = false;
}

void MfSoftEncoder::Shutdown() {
  if (impl_) {
    ReleaseMft();
    if (impl_->co_init_owner) {
      CoUninitialize();
      impl_->co_init_owner = false;
    }
    delete impl_;
    impl_ = nullptr;
  }
  w_ = h_ = fps_ = bitrate_ = 0;
  nv12_.clear();
  backend_ = EncoderBackend::kSoftware;
  friendly_name_.clear();
}

// Full negotiation + streaming start on a given MFT instance. Owns no
// pointers; impl_ must exist. Output type: caller's H.264 first, the MFT's
// own enumerated H.264 types as fallback (hardware MFTs may not accept the
// caller's verbatim type). Input type: ONE standard tight-NV12 attempt —
// the Part A variant ladder was removed (feat/arch-clean) because every
// CPU-NV12 variant was rejected on Arc (QSV needs D3D11 textures, M4); a
// rejection just logs and the ladder falls back. The negotiated input
// stride is read back (padding buffer allocated when wider than w).
bool MfSoftEncoder::InitWithMft(IMFTransform* mft, const std::wstring& friendly,
                                bool hardware, std::string* err) {
  if (mft == nullptr) {
    if (err) *err = "InitWithMft: null mft";
    return false;
  }
  const uint64_t frame_size = (static_cast<uint64_t>(w_) << 32) | h_;
  const uint64_t frame_rate = (static_cast<uint64_t>(fps_) << 32) | 1;

  // Output type: caller's H.264 type, then enumerated fallback for hardware.
  ComPtr<IMFMediaType> out_mt;
  HRESULT hr = MakeH264OutputType(w_, h_, fps_, bitrate_, out_mt.GetAddressOf());
  if (SUCCEEDED(hr)) hr = mft->SetOutputType(0, out_mt.Get(), 0);
  if (FAILED(hr) && hardware) {
    for (DWORD i = 0;; ++i) {
      ComPtr<IMFMediaType> t;
      hr = mft->GetOutputAvailableType(0, i, t.GetAddressOf());
      if (FAILED(hr)) break;
      GUID sub{};
      if (FAILED(t->GetGUID(MF_MT_SUBTYPE, &sub)) || sub != MFVideoFormat_H264) continue;
      hr = t->SetUINT64(MF_MT_FRAME_SIZE, frame_size);
      if (SUCCEEDED(hr)) hr = t->SetUINT64(MF_MT_FRAME_RATE, frame_rate);
      if (SUCCEEDED(hr)) hr = t->SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
      if (SUCCEEDED(hr)) hr = t->SetUINT32(MF_MT_AVG_BITRATE, bitrate_);
      if (SUCCEEDED(hr)) hr = mft->SetOutputType(0, t.Get(), 0);
      if (SUCCEEDED(hr)) {
        out_mt = t;
        break;
      }
    }
  }
  if (FAILED(hr)) {
    if (err) *err = HrStep(hardware ? "SetOutputType(hw)" : "SetOutputType", hr);
    return false;
  }

  // Input type: the caller's tight NV12 (stride = width), ONE standard
  // attempt for both rungs. A hardware MFT that rejects CPU NV12 logs the
  // rejection and the ladder falls back (next candidate, then software) —
  // no variant ladder: the Part A attempts were dead code on Arc (all
  // rejected 0xC00D6D77; QSV CPU-NV12 needs D3D11 textures, M4).
  ComPtr<IMFMediaType> in_mt;
  hr = MakeNv12InputType(w_, h_, fps_, in_mt.GetAddressOf());
  if (SUCCEEDED(hr)) hr = mft->SetInputType(0, in_mt.Get(), 0);
  if (FAILED(hr)) {
    XNC_LOG_INFO("encoder_input_attempt backend=%s hr=0x%08x (rejected, falling back)",
                 hardware ? "hardware" : "software", static_cast<unsigned int>(hr));
    if (err) *err = HrStep(hardware ? "SetInputType(hw)" : "SetInputType", hr);
    return false;
  }

  // Negotiated input stride: honor a wider aligned stride (pad input rows);
  // 0/absent/odd/<w means tight is in effect. Log the readback so the
  // MFT's chosen stride is diagnosable after every successful negotiation.
  impl_->in_stride = 0;
  {
    ComPtr<IMFMediaType> cur;
    UINT32 s = 0;
    if (SUCCEEDED(mft->GetInputCurrentType(0, cur.GetAddressOf()))) {
      if (SUCCEEDED(cur->GetUINT32(MF_MT_DEFAULT_STRIDE, &s))) impl_->in_stride = s;
    }
    XNC_LOG_INFO("encoder_input_negotiated_stride backend=%s stride=%u w=%u",
                 hardware ? "hardware" : "software", s, w_);
    if (impl_->in_stride < w_ || (impl_->in_stride % 2) != 0) impl_->in_stride = 0;
  }
  if (impl_->in_stride != 0 && impl_->in_stride != w_) {
    const size_t n = static_cast<size_t>(impl_->in_stride) * h_ * 3u / 2u;
    impl_->in_padded_.assign(n, 0);
    XNC_LOG_INFO("encoder_input_stride_padded stride=%u w=%u", impl_->in_stride, w_);
  }

  // Best-effort shaping via ICodecAPI. GOP long (IDRs are forced on demand,
  // spec §7.5 recovery cadence lives above this class); B-frames 0 and
  // low-delay/low-latency per spec §7.10 V1 全档. Hardware MFTs may reject
  // any of these (or omit ICodecAPI) - best-effort log-and-continue.
  impl_->codec_api.Reset();
  hr = mft->QueryInterface(IID_PPV_ARGS(impl_->codec_api.GetAddressOf()));
  if (SUCCEEDED(hr)) {
    CodecApiSetUi4(impl_->codec_api.Get(), &CODECAPI_AVEncMPVGOPSize, fps_ * 10, "gop_size");
    CodecApiSetUi4(impl_->codec_api.Get(), &CODECAPI_AVEncMPVDefaultBPictureCount, 0,
                   "b_picture_count");
    CodecApiSetUi4(impl_->codec_api.Get(), &CODECAPI_AVEncCommonRateControlMode,
                   eAVEncCommonRateControlMode_LowDelayVBR, "rate_control_low_delay");
    VARIANT v{};  // AVLowLatencyMode is VT_BOOL
    v.vt = VT_BOOL;
    v.boolVal = VARIANT_TRUE;
    if (FAILED(impl_->codec_api->SetValue(&CODECAPI_AVLowLatencyMode, &v)))
      XNC_LOG_INFO("codec_api_set_skip name=low_latency hr=rejected");
  } else {
    XNC_LOG_INFO("codec_api_unavailable hr=0x%08x", static_cast<unsigned int>(hr));
  }

  // Output allocation contract. CMSH264EncoderMFT does NOT set
  // MFT_OUTPUT_STREAM_PROVIDES_SAMPLES - passing a null sample to
  // ProcessOutput yields E_INVALIDARG - so the client-provided path below
  // is the one that runs, but both are implemented (hardware MFTs vary).
  MFT_OUTPUT_STREAM_INFO osi{};
  hr = mft->GetOutputStreamInfo(0, &osi);
  if (SUCCEEDED(hr)) {
    impl_->mft_provides_samples = (osi.dwFlags & MFT_OUTPUT_STREAM_PROVIDES_SAMPLES) != 0;
    if (osi.cbSize > impl_->out_buf_size) impl_->out_buf_size = osi.cbSize;
  }
  if (impl_->out_buf_size == 0) impl_->out_buf_size = static_cast<size_t>(w_) * h_ * 4 + 65536;

  impl_->mft = mft;
  impl_->friendly_narrow = WideToNarrow(friendly);
  hr = impl_->mft->ProcessMessage(MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0);
  if (SUCCEEDED(hr)) hr = impl_->mft->ProcessMessage(MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0);
  if (FAILED(hr)) {
    if (err) *err = HrStep("ProcessMessage(start streaming)", hr);
    return false;
  }
  return true;
}

// Hardware ladder self-check: synthetic gradient frames (moving stripe so
// consecutive frames differ and P frames flow) through the real submit path.
// Success = at least one output AU before the feed bound.
bool MfSoftEncoder::SelfCheckEncode(std::string* err) {
  const size_t need = static_cast<size_t>(w_) * h_ * 4;
  std::vector<uint8_t> bgra(need);
  std::string e2;
  for (uint32_t i = 0; i < kSelfCheckMaxFrames; ++i) {
    for (uint32_t y = 0; y < h_; ++y) {
      for (uint32_t x = 0; x < w_; ++x) {
        uint8_t* px = bgra.data() + (static_cast<size_t>(y) * w_ + x) * 4;
        px[0] = static_cast<uint8_t>(x & 0xFF);        // B gradient
        px[1] = static_cast<uint8_t>(y & 0xFF);        // G gradient
        px[2] = static_cast<uint8_t>((x * 3 + i * 41) & 0xFF);  // moving R
        px[3] = 0xFF;
      }
    }
    std::vector<std::vector<uint8_t>> aus;
    if (!Encode(bgra.data(), bgra.size(), aus, &e2)) {
      if (err) *err = e2;
      return false;
    }
    if (!aus.empty()) return true;  // MFT produced output: candidate is alive
  }
  if (err) *err = "self-check: no output AU in " + std::to_string(kSelfCheckMaxFrames) +
                  " feeds (encoder buffering or broken session)";
  return false;
}

bool MfSoftEncoder::Init(uint32_t w, uint32_t h, uint32_t fps, uint32_t bitrate_bps,
                         std::string* err) {
  Shutdown();
  auto fail = [err](std::string msg) {
    if (err) *err = msg;
    return false;
  };
  if (w == 0 || h == 0 || (w % 2) != 0 || (h % 2) != 0) {
    return fail("invalid dimensions " + std::to_string(w) + "x" + std::to_string(h) +
                " (must be non-zero even)");
  }
  if (fps == 0) return fail("fps must be > 0");
  if (Nv12Bytes(w, h) == 0) return fail("frame too large");
  if (bitrate_bps == 0) bitrate_bps = 2000000;  // caller default (reference behavior)
  impl_ = new Impl();

  // This exe does not CoInitializeEx anywhere else (checked xnc-desktop.cpp);
  // the encoder owns its apartment. MTA per the capture-spine plan. If the
  // thread is already initialized in another mode, keep going without owning
  // it (no CoUninitialize on shutdown).
  HRESULT hr = CoInitializeEx(nullptr, COINIT_MULTITHREADED);
  if (hr == RPC_E_CHANGED_MODE) {
    XNC_LOG_INFO("com_init already_initialized_other_apartment");
  } else if (FAILED(hr)) {
    const std::string msg = HrStep("CoInitializeEx", hr);
    Shutdown();
    return fail(msg);
  } else {
    impl_->co_init_owner = true;  // S_OK or S_FALSE: both refcount a release
  }

  w_ = w;
  h_ = h;
  fps_ = fps;
  bitrate_ = bitrate_bps;

  // ---- Hardware-first ladder (hw-encode): MFT_ENUM_FLAG_HARDWARE H.264
  // encoders in merit order; first one that negotiates AND self-checks wins.
  if (!force_software_) {
    MFT_REGISTER_TYPE_INFO in_ri{MFMediaType_Video, MFVideoFormat_NV12};
    MFT_REGISTER_TYPE_INFO out_ri{MFMediaType_Video, MFVideoFormat_H264};
    IMFActivate** acts = nullptr;
    UINT32 nacts = 0;
    hr = MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER,
                   MFT_ENUM_FLAG_HARDWARE | MFT_ENUM_FLAG_SORTANDFILTER,
                   &in_ri, &out_ri, &acts, &nacts);
    if (FAILED(hr)) {
      XNC_LOG_INFO("encoder_hw_enum_failed hr=0x%08x (falling back to software)",
                   static_cast<unsigned int>(hr));
    } else if (nacts == 0) {
      XNC_LOG_INFO("encoder_hw_none_found (falling back to software)");
    } else {
      for (UINT32 i = 0; i < nacts; ++i) {
        const std::wstring friendly = ActivateFriendlyName(acts[i]);
        std::string ierr, perr, serr;
        bool ok = false;
        // Probe instance: full negotiation + self-check encode.
        ComPtr<IMFTransform> probe;
        hr = acts[i]->ActivateObject(IID_PPV_ARGS(probe.GetAddressOf()));
        if (SUCCEEDED(hr)) ok = InitWithMft(probe.Get(), friendly, true, &perr);
        if (ok) ok = SelfCheckEncode(&serr);
        if (ok) {
          // The self-check consumed probe frames; re-activate a PRISTINE
          // instance for the real stream so no synthetic AU ever reaches the
          // caller (selftest scenario (c) asserts exact AU-index mapping).
          ReleaseMft();
          ComPtr<IMFTransform> live;
          hr = acts[i]->ActivateObject(IID_PPV_ARGS(live.GetAddressOf()));
          if (SUCCEEDED(hr)) {
            ok = InitWithMft(live.Get(), friendly, true, &ierr);
            if (ok) impl_->activate.Attach(acts[i]);  // take the enum ref for
                                                      // session teardown at Shutdown
          } else {
            ok = false;
            ierr = HrStep("ActivateObject(second instance)", hr);
          }
        }
        if (ok) {
          XNC_LOG_INFO("encoder_backend=hardware friendly=\"%s\" selfcheck=ok",
                       impl_->friendly_narrow.c_str());
          break;  // ladder winner
        }
        XNC_LOG_INFO("encoder_backend=hardware candidate_rejected friendly=\"%ls\" "
                     "init_err=\"%s\" selfcheck_err=\"%s\"",
                     friendly.c_str(), perr.c_str(), serr.empty() ? ierr.c_str() : serr.c_str());
        ReleaseMft();
        acts[i]->ShutdownObject();
        acts[i]->Release();  // rejected candidate: drop the enum's ref
      }
    }
    if (acts != nullptr) CoTaskMemFree(acts);
    if (impl_->mft.Get() != nullptr) backend_ = EncoderBackend::kHardware;
  }

  // ---- Software fallback (the pre-hw-encode path; --encoder software or
  // every hardware candidate failed) ----
  if (impl_->mft.Get() == nullptr) {
    ReleaseMft();
    hr = CoCreateInstance(kClsidCMSH264EncoderMFT, nullptr, CLSCTX_INPROC_SERVER,
                          IID_PPV_ARGS(impl_->mft.GetAddressOf()));
    if (FAILED(hr)) {
      const std::string msg = HrStep("CoCreateInstance(CMSH264EncoderMFT)", hr);
      Shutdown();
      return fail(msg);
    }
    std::string ierr;
    if (!InitWithMft(impl_->mft.Get(), kSoftwareFriendlyName, false, &ierr)) {
      const std::string msg = "software encoder init: " + ierr;
      Shutdown();
      return fail(msg);
    }
    backend_ = EncoderBackend::kSoftware;
    XNC_LOG_INFO("encoder_backend=software friendly=\"%s\" selfcheck=skipped",
                 impl_->friendly_narrow.c_str());
  }

  friendly_name_ = impl_->friendly_narrow;
  nv12_.resize(Nv12Bytes(w_, h_));
  QueryPerformanceFrequency(&impl_->qpc_freq);
  QueryPerformanceCounter(&impl_->qpc0);
  impl_->rt_last = 0;
  XNC_LOG_INFO("encoder_init w=%u h=%u fps=%u bitrate=%u provides_samples=%d out_buf=%zu "
               "backend=%s friendly=\"%s\"",
               w_, h_, fps_, bitrate_, impl_->mft_provides_samples ? 1 : 0,
               impl_->out_buf_size, BackendName(), friendly_name_.c_str());
  return true;
}

bool MfSoftEncoder::SubmitNv12(const uint8_t* nv12, size_t len,
                               std::vector<std::vector<uint8_t>>& aus, std::string* err) {
  const uint8_t* src = nv12;
  size_t src_len = len;
  if (impl_->in_stride != 0 && impl_->in_stride != w_) {
    // Hardware MFT negotiated an aligned stride: expand rows into the
    // padded buffer (zero-filled tails) once per frame.
    if (impl_->in_padded_.size() != static_cast<size_t>(impl_->in_stride) * h_ * 3u / 2u) {
      if (err) *err = "input stride buffer size mismatch";
      return false;
    }
    if (len < static_cast<size_t>(w_) * h_ * 3u / 2u) {
      if (err) *err = "short nv12 frame";
      return false;
    }
    ExpandToStride(nv12, impl_->in_padded_.data(), w_, h_, impl_->in_stride);
    src = impl_->in_padded_.data();
    src_len = impl_->in_padded_.size();
  }

  // Input IMFSample carrying the NV12 bytes.
  ComPtr<IMFSample> sample;
  ComPtr<IMFMediaBuffer> buffer;
  HRESULT hr = MFCreateSample(sample.GetAddressOf());
  if (SUCCEEDED(hr)) hr = MFCreateMemoryBuffer(static_cast<DWORD>(src_len), buffer.GetAddressOf());
  if (SUCCEEDED(hr)) {
    BYTE* base = nullptr;
    hr = buffer->Lock(&base, nullptr, nullptr);
    if (SUCCEEDED(hr)) {
      std::memcpy(base, src, src_len);
      buffer->Unlock();
      hr = buffer->SetCurrentLength(static_cast<DWORD>(src_len));
    }
  }
  if (SUCCEEDED(hr)) hr = sample->AddBuffer(buffer.Get());
  if (FAILED(hr)) {
    if (err) *err = HrStep("input sample", hr);
    return false;
  }

  // PTS = wall clock since Init in 100ns units (the media-type frame rate is
  // only the rate-control reference; fixed-step timestamps drift under
  // variable capture cadence). Monotonic, duration = gap to previous frame.
  int64_t now = impl_->Now100ns();
  if (now <= impl_->rt_last) now = impl_->rt_last + 1;
  int64_t dur = impl_->rt_last == 0 ? static_cast<int64_t>(10000000 / fps_) : now - impl_->rt_last;
  if (dur <= 0) dur = 1;
  sample->SetSampleTime(now);
  sample->SetSampleDuration(dur);
  impl_->rt_last = now;

  // One-shot force-key (E2 contract, spec §7.10): the property is set
  // exactly ONCE here, at input submission, and the pending state clears
  // unconditionally - even if the set failed or no keyframe output follows.
  // Re-arming "because no keyframe has been seen yet" is the E2 storm bug.
  if (force_pending_) {
    force_pending_ = false;
    if (impl_->codec_api.Get() != nullptr) {
      VARIANT v{};
      v.vt = VT_UI4;
      v.ulVal = 1;
      if (FAILED(impl_->codec_api->SetValue(&CODECAPI_AVEncVideoForceKeyFrame, &v)))
        XNC_LOG_INFO("force_key_set_rejected hr=0x%08x (request consumed regardless)",
                     static_cast<unsigned int>(hr));
    }
  }

  hr = impl_->mft->ProcessInput(0, sample.Get(), 0);
  if (FAILED(hr)) {
    if (err) *err = HrStep("ProcessInput", hr);
    return false;
  }
  return CollectOutputs(aus, err);
}

bool MfSoftEncoder::Encode(const uint8_t* bgra, size_t len,
                           std::vector<std::vector<uint8_t>>& aus, std::string* err) {
  aus.clear();
  if (!impl_ || impl_->mft.Get() == nullptr) {
    if (err) *err = "encoder not initialized";
    return false;
  }
  const size_t need = static_cast<size_t>(w_) * h_ * 4;
  if (!bgra || len < need) {
    if (err)
      *err = "short frame: " + std::to_string(len) + " < " + std::to_string(need);
    return false;
  }
  if (!BgraToNv12(bgra, len, nv12_.data(), nv12_.size(), w_, h_)) {
    if (err) *err = "nv12 convert rejected arguments";
    return false;
  }
  return SubmitNv12(nv12_.data(), nv12_.size(), aus, err);
}

bool MfSoftEncoder::EncodeNV12(const uint8_t* nv12, size_t len,
                               std::vector<std::vector<uint8_t>>& aus,
                               std::string* err) {
  aus.clear();
  if (!impl_ || impl_->mft.Get() == nullptr) {
    if (err) *err = "encoder not initialized";
    return false;
  }
  const size_t need = Nv12Bytes(w_, h_);
  if (need == 0 || !nv12 || len < need) {
    if (err)
      *err = "short nv12 frame: " + std::to_string(len) + " < " + std::to_string(need);
    return false;
  }
  return SubmitNv12(nv12, len, aus, err);
}

void MfSoftEncoder::ForceNextIdr(const char* reason) {
  force_pending_ = true;
  XNC_LOG_INFO("force_key_pending reason=%s", reason ? reason : "");
}

void MfSoftEncoder::Drain(std::vector<std::vector<uint8_t>>& aus) {
  if (!impl_ || impl_->mft.Get() == nullptr) return;
  std::string ignored;
  CollectOutputs(aus, &ignored);  // appends; never touches force_pending_
}

void MfSoftEncoder::FlushTail(std::vector<std::vector<uint8_t>>& aus) {
  if (!impl_ || impl_->mft.Get() == nullptr) return;
  // Canonical MFT flush: no more input (END_OF_STREAM), then COMMAND_DRAIN =
  // produce all pending output; CollectOutputs stops at
  // MF_E_TRANSFORM_NEED_MORE_INPUT, which drained encoders return once the
  // window is empty. Message failures are logged and tolerated - collecting
  // is still attempted.
  HRESULT hr = impl_->mft->ProcessMessage(MFT_MESSAGE_NOTIFY_END_OF_STREAM, 0);
  if (FAILED(hr))
    XNC_LOG_INFO("flush_tail end_of_stream hr=0x%08x (continuing)",
                 static_cast<unsigned int>(hr));
  hr = impl_->mft->ProcessMessage(MFT_MESSAGE_COMMAND_DRAIN, 0);
  if (FAILED(hr))
    XNC_LOG_INFO("flush_tail drain hr=0x%08x (continuing)", static_cast<unsigned int>(hr));
  std::string ignored;
  if (!CollectOutputs(aus, &ignored))
    XNC_LOG_INFO("flush_tail collect stopped early (see previous error line)");
}

bool MfSoftEncoder::CollectOutputs(std::vector<std::vector<uint8_t>>& aus, std::string* err) {
  bool any_au = false;
  bool has_idr = false;
  for (;;) {
    MFT_OUTPUT_DATA_BUFFER ob{};
    ComPtr<IMFSample> client_sample;  // holds the client-provided case
    if (!impl_->mft_provides_samples) {
      ComPtr<IMFMediaBuffer> client_buffer;
      HRESULT hrb = MFCreateSample(client_sample.GetAddressOf());
      if (SUCCEEDED(hrb))
        hrb = MFCreateMemoryBuffer(static_cast<DWORD>(impl_->out_buf_size),
                                   client_buffer.GetAddressOf());
      if (SUCCEEDED(hrb)) hrb = client_sample->AddBuffer(client_buffer.Get());
      if (FAILED(hrb)) {
        if (err) *err = HrStep("output sample alloc", hrb);
        return false;
      }
      ob.pSample = client_sample.Get();
    }
    DWORD status = 0;
    HRESULT hr = impl_->mft->ProcessOutput(0, 1, &ob, &status);
    if (ob.pEvents) {
      ob.pEvents->Release();
      ob.pEvents = nullptr;
    }
    if (hr == MF_E_TRANSFORM_NEED_MORE_INPUT) break;  // drained
    if (hr == MF_E_TRANSFORM_STREAM_CHANGE) {  // format renegotiated: retry
      if (impl_->mft_provides_samples && ob.pSample) ob.pSample->Release();
      continue;
    }
    if (FAILED(hr)) {
      if (err) *err = HrStep("ProcessOutput", hr);
      return false;
    }
    if (!ob.pSample) continue;

    ComPtr<IMFMediaBuffer> buf;
    hr = ob.pSample->ConvertToContiguousBuffer(buf.GetAddressOf());
    if (SUCCEEDED(hr)) {
      BYTE* base = nullptr;
      DWORD cur = 0;
      hr = buf->Lock(&base, nullptr, &cur);
      if (SUCCEEDED(hr) && base) {
        aus.emplace_back(base, base + cur);
        buf->Unlock();
        any_au = true;
        const std::vector<uint8_t>& au = aus.back();
        if (NalHasType(au.data(), au.size(), 5)) {  // IDR
          has_idr = true;
          if (sps_pps_.empty()) {
            const uint8_t types[2] = {7, 8};
            NalExtractTypes(au.data(), au.size(), types, 2, &sps_pps_);
          }
        }
      } else if (SUCCEEDED(hr)) {
        hr = E_FAIL;  // Lock succeeded but gave no pointer
      }
    }
    if (impl_->mft_provides_samples) ob.pSample->Release();  // MFT-allocated
    // client_sample (client-provided case) releases via ComPtr at scope end
    if (FAILED(hr)) {
      if (err) *err = HrStep("output buffer", hr);
      return false;
    }
  }
  if (any_au) last_was_key_ = has_idr;
  return true;
}

}  // namespace xnc
