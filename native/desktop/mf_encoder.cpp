// mf_encoder.cpp - CMSH264EncoderMFT pipeline (see mf_encoder.h). Clean-room
// C++ rewrite of the reference flow (agent/screen-helper/encode_windows.go -
// same SDK calls and parameter alignment, adapted not copied):
//   CoInitializeEx(MTA) -> CoCreateInstance(CMSH264EncoderMFT) ->
//   SetOutputType(H264: frame size, frame rate = caller fps, bitrate) ->
//   SetInputType(NV12: tight stride = width) ->
//   ICodecAPI best-effort shaping (GOP / B-frames=0 / low-delay RC /
//   low-latency) -> GetOutputStreamInfo -> Begin/StartOfStream ->
//   per frame: BGRA->NV12 -> IMFSample (wall-clock 100ns PTS) ->
//   one-shot force-key at submission -> ProcessInput -> ProcessOutput loop
//   (NEED_MORE_INPUT ends the loop, STREAM_CHANGE retries) -> one AU per
//   output sample, IDR detection + first-IDR SPS/PPS cache.
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

}  // namespace

struct MfSoftEncoder::Impl {
  ComPtr<IMFTransform> mft;
  ComPtr<ICodecAPI> codec_api;
  bool mft_provides_samples = false;  // MFT_OUTPUT_STREAM_PROVIDES_SAMPLES
  size_t out_buf_size = 0;            // client-allocated output buffer size
  LARGE_INTEGER qpc0{};               // wall-clock PTS base
  LARGE_INTEGER qpc_freq{};
  int64_t rt_last = 0;                // last sample time, 100ns units
  bool co_init_owner = false;

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

void MfSoftEncoder::Shutdown() {
  if (impl_) {
    if (impl_->mft.Get() != nullptr) {
      impl_->mft->ProcessMessage(MFT_MESSAGE_NOTIFY_END_STREAMING, 0);
    }
    impl_->codec_api.Reset();
    impl_->mft.Reset();
    if (impl_->co_init_owner) {
      CoUninitialize();
      impl_->co_init_owner = false;
    }
    delete impl_;
    impl_ = nullptr;
  }
  w_ = h_ = fps_ = bitrate_ = 0;
  last_was_key_ = false;
  force_pending_ = false;
  nv12_.clear();
  sps_pps_.clear();
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

  hr = CoCreateInstance(kClsidCMSH264EncoderMFT, nullptr, CLSCTX_INPROC_SERVER,
                        IID_PPV_ARGS(impl_->mft.GetAddressOf()));
  if (FAILED(hr)) {
    const std::string msg = HrStep("CoCreateInstance(CMSH264EncoderMFT)", hr);
    Shutdown();
    return fail(msg);
  }

  w_ = w;
  h_ = h;
  fps_ = fps;
  bitrate_ = bitrate_bps;
  const uint64_t frame_size = (static_cast<uint64_t>(w) << 32) | h;
  const uint64_t frame_rate = (static_cast<uint64_t>(fps) << 32) | 1;  // = caller fps

  // Output type: H.264 with caller dimensions/rate/bitrate.
  ComPtr<IMFMediaType> out_mt;
  hr = MFCreateMediaType(out_mt.GetAddressOf());
  if (SUCCEEDED(hr)) hr = out_mt->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
  if (SUCCEEDED(hr)) hr = out_mt->SetGUID(MF_MT_SUBTYPE, MFVideoFormat_H264);
  if (SUCCEEDED(hr)) hr = out_mt->SetUINT64(MF_MT_FRAME_SIZE, frame_size);
  if (SUCCEEDED(hr)) hr = out_mt->SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
  if (SUCCEEDED(hr)) hr = out_mt->SetUINT64(MF_MT_FRAME_RATE, frame_rate);
  if (SUCCEEDED(hr)) hr = out_mt->SetUINT32(MF_MT_AVG_BITRATE, bitrate_);
  if (FAILED(hr)) {
    const std::string msg = HrStep("output media type", hr);
    Shutdown();
    return fail(msg);
  }
  hr = impl_->mft->SetOutputType(0, out_mt.Get(), 0);
  if (FAILED(hr)) {
    const std::string msg = HrStep("SetOutputType", hr);
    Shutdown();
    return fail(msg);
  }

  // Input type: NV12 with tight stride (the MFT may otherwise assume an
  // alignment-padded stride and our converted buffer is compact).
  ComPtr<IMFMediaType> in_mt;
  hr = MFCreateMediaType(in_mt.GetAddressOf());
  if (SUCCEEDED(hr)) hr = in_mt->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
  if (SUCCEEDED(hr)) hr = in_mt->SetGUID(MF_MT_SUBTYPE, MFVideoFormat_NV12);
  if (SUCCEEDED(hr)) hr = in_mt->SetUINT32(MF_MT_DEFAULT_STRIDE, w);
  if (SUCCEEDED(hr)) hr = in_mt->SetUINT64(MF_MT_FRAME_SIZE, frame_size);
  if (SUCCEEDED(hr)) hr = in_mt->SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
  if (SUCCEEDED(hr)) hr = in_mt->SetUINT64(MF_MT_FRAME_RATE, frame_rate);
  if (FAILED(hr)) {
    const std::string msg = HrStep("input media type", hr);
    Shutdown();
    return fail(msg);
  }
  hr = impl_->mft->SetInputType(0, in_mt.Get(), 0);
  if (FAILED(hr)) {
    const std::string msg = HrStep("SetInputType", hr);
    Shutdown();
    return fail(msg);
  }

  // Best-effort shaping via ICodecAPI. GOP long (IDRs are forced on demand,
  // spec §7.5 recovery cadence lives above this class); B-frames 0 and
  // low-delay/low-latency per spec §7.10 V1 全档.
  hr = impl_->mft.As(&impl_->codec_api);
  if (SUCCEEDED(hr)) {
    CodecApiSetUi4(impl_->codec_api.Get(), &CODECAPI_AVEncMPVGOPSize, fps * 10, "gop_size");
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
  // is the one that runs, but both are implemented.
  MFT_OUTPUT_STREAM_INFO osi{};
  hr = impl_->mft->GetOutputStreamInfo(0, &osi);
  if (SUCCEEDED(hr)) {
    impl_->mft_provides_samples = (osi.dwFlags & MFT_OUTPUT_STREAM_PROVIDES_SAMPLES) != 0;
    if (osi.cbSize > impl_->out_buf_size) impl_->out_buf_size = osi.cbSize;
  }
  if (impl_->out_buf_size == 0) impl_->out_buf_size = static_cast<size_t>(w) * h * 4 + 65536;

  hr = impl_->mft->ProcessMessage(MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0);
  if (SUCCEEDED(hr)) hr = impl_->mft->ProcessMessage(MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0);
  if (FAILED(hr)) {
    const std::string msg = HrStep("ProcessMessage(start streaming)", hr);
    Shutdown();
    return fail(msg);
  }

  nv12_.resize(Nv12Bytes(w, h));
  QueryPerformanceFrequency(&impl_->qpc_freq);
  QueryPerformanceCounter(&impl_->qpc0);
  impl_->rt_last = 0;
  XNC_LOG_INFO("encoder_init w=%u h=%u fps=%u bitrate=%u provides_samples=%d out_buf=%zu",
               w, h, fps, bitrate_, impl_->mft_provides_samples ? 1 : 0,
               impl_->out_buf_size);
  return true;
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

  // Input IMFSample carrying the NV12 bytes.
  ComPtr<IMFSample> sample;
  ComPtr<IMFMediaBuffer> buffer;
  HRESULT hr = MFCreateSample(sample.GetAddressOf());
  if (SUCCEEDED(hr)) hr = MFCreateMemoryBuffer(static_cast<DWORD>(nv12_.size()), buffer.GetAddressOf());
  if (SUCCEEDED(hr)) {
    BYTE* base = nullptr;
    hr = buffer->Lock(&base, nullptr, nullptr);
    if (SUCCEEDED(hr)) {
      std::memcpy(base, nv12_.data(), nv12_.size());
      buffer->Unlock();
      hr = buffer->SetCurrentLength(static_cast<DWORD>(nv12_.size()));
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

void MfSoftEncoder::ForceNextIdr(const char* reason) {
  force_pending_ = true;
  XNC_LOG_INFO("force_key_pending reason=%s", reason ? reason : "");
}

void MfSoftEncoder::Drain(std::vector<std::vector<uint8_t>>& aus) {
  if (!impl_ || impl_->mft.Get() == nullptr) return;
  std::string ignored;
  CollectOutputs(aus, &ignored);  // appends; never touches force_pending_
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
