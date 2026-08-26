// mf_decoder_probe.cpp - TEST-ONLY H.264 -> NV12 decode probe (see
// mf_decoder_probe.h). Mirrors the binary's Media Foundation idioms from
// mf_encoder.cpp (MTA apartment ownership, MFTEnumEx + ActivateObject,
// ComPtr, /W3-clean, no explicit MFStartup - the rest of the binary uses MF
// without one), inverted: MFT_CATEGORY_VIDEO_DECODER with H264 in / NV12
// out. The decoded Y plane is folded into one FNV-1a 64 hash (offset basis
// and prime identical to dxgi_capture.h's Fnv1a64; the constants are kept
// local so this test-only file has no production includes beyond the header).
//
// Negotiation notes (measured against the MS H.264 decoder MFT on Win11):
//   - The decoder does NOT set MFT_OUTPUT_STREAM_PROVIDES_SAMPLES, so the
//     caller must allocate every output sample; passing null there makes
//     ProcessOutput fail E_INVALIDARG.
//   - Once it actually decodes, the decoder requires a 2D buffer
//     (MFCreate2DMediaBuffer, NV12 fourcc) at the real frame size - a plain
//     MFCreateMemoryBuffer of GetOutputStreamInfo's cbSize is rejected with
//     E_FAIL at the first real frame. Before the SPS is parsed the decoder
//     never writes, so pre-negotiation calls can use any plain buffer.
//   - The output type must NOT be set up front: the decoder enumerates
//     placeholder types (1920x1080) until the SPS is parsed, then reports
//     MF_E_TRANSFORM_STREAM_CHANGE / MF_E_TRANSFORM_TYPE_NOT_SET. Adopting
//     the first NV12 type it offers after the stream change lands on the
//     real frame size. Re-setting the INPUT type after the output change
//     restarts the negotiation loop, so it is never re-set here.
//   - End-of-stream/drain is sent only after the first NEED_MORE_INPUT, so
//     the drain never races the SPS-driven renegotiation.
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>

#include <mfapi.h>        // MFCreate*, MF_MT_*, MFVideoFormat_*, MFGetAttributeSize
#include <mferror.h>      // MF_E_TRANSFORM_*
#include <mftransform.h>  // IMFTransform, MFT_MESSAGE_*, MFT_OUTPUT_DATA_BUFFER
#include <wrl/client.h>   // ComPtr

#include <cstdint>
#include <cstdio>
#include <cstring>
#include <string>
#include <vector>

#include "mf_decoder_probe.h"

namespace xnc {
namespace {

using Microsoft::WRL::ComPtr;

// FNV-1a 64 running hash (same constants as dxgi_capture.h's Fnv1a64).
inline uint64_t Fnv1a64UpdateLocal(uint64_t h, const uint8_t* data, size_t n) {
  for (size_t i = 0; i < n; ++i) {
    h ^= data[i];
    h *= 1099511628211ull;
  }
  return h;
}

std::string ProbeHr(const char* step, HRESULT hr) {
  char buf[160];
  std::snprintf(buf, sizeof(buf), "%s: hr=0x%08X", step,
                static_cast<unsigned int>(hr));
  return std::string(buf);
}

}  // namespace

bool DecodeAnnexBToLumaHash(const std::vector<uint8_t>& annexb,
                            uint64_t* luma_hash, std::string* err) {
  if (luma_hash != nullptr) *luma_hash = 0;
  auto fail = [&](const std::string& msg) {
    if (err != nullptr) *err = msg;
    return false;
  };
  if (annexb.empty()) return fail("empty annex-b input");

  // Apartment policy matches mf_encoder.cpp: own the MTA reference we add
  // here. S_OK = we incremented the refcount (must CoUninitialize at exit);
  // S_FALSE = the thread was already initialized (e.g. the selftest's
  // MfSoftEncoder::Init on this very thread) - still usable, but we must NOT
  // uninitialize it; RPC_E_CHANGED_MODE = another apartment, also no uninit.
  HRESULT hr = CoInitializeEx(nullptr, COINIT_MULTITHREADED);
  const bool co_owner = (hr == S_OK);
  if (FAILED(hr) && hr != RPC_E_CHANGED_MODE)
    return fail(ProbeHr("CoInitializeEx", hr));

  // 1. Find the system H.264 -> NV12 decoder MFT.
  MFT_REGISTER_TYPE_INFO in_ri{MFMediaType_Video, MFVideoFormat_H264};
  MFT_REGISTER_TYPE_INFO out_ri{MFMediaType_Video, MFVideoFormat_NV12};
  ComPtr<IMFTransform> mft;
  {
    IMFActivate** acts = nullptr;
    UINT32 nacts = 0;
    hr = MFTEnumEx(MFT_CATEGORY_VIDEO_DECODER,
                   MFT_ENUM_FLAG_SYNCMFT | MFT_ENUM_FLAG_SORTANDFILTER, &in_ri,
                   &out_ri, &acts, &nacts);
    if (SUCCEEDED(hr) && nacts == 0) {
      // Some SKUs register the H.264 decoder without the sync flag; fall
      // back to an unfiltered enumeration (still driven synchronously via
      // ProcessInput/ProcessOutput - the ASYNCMFT flag is what selects
      // event-mode instances, and it is not set here).
      if (acts != nullptr) CoTaskMemFree(acts);
      hr = MFTEnumEx(MFT_CATEGORY_VIDEO_DECODER, MFT_ENUM_FLAG_SORTANDFILTER,
                     &in_ri, &out_ri, &acts, &nacts);
    }
    if (FAILED(hr) || nacts == 0) {
      if (acts != nullptr) CoTaskMemFree(acts);
      mft.Reset();
      if (co_owner) CoUninitialize();
      return fail(FAILED(hr) ? ProbeHr("MFTEnumEx(H264->NV12 decoder)", hr)
                             : "no H264->NV12 decoder MFT found");
    }
    hr = acts[0]->ActivateObject(IID_PPV_ARGS(mft.GetAddressOf()));
    for (UINT32 i = 0; i < nacts; ++i) acts[i]->Release();
    CoTaskMemFree(acts);
    if (FAILED(hr)) {
      mft.Reset();
      if (co_owner) CoUninitialize();
      return fail(ProbeHr("ActivateObject(decoder)", hr));
    }
  }

  bool ok = false;
  std::string why;

  // 2. Input type: raw Annex-B H.264 (frame size arrives via the SPS).
  if (SUCCEEDED(hr)) {
    ComPtr<IMFMediaType> in_type;
    hr = MFCreateMediaType(in_type.GetAddressOf());
    if (SUCCEEDED(hr)) hr = in_type->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
    if (SUCCEEDED(hr)) hr = in_type->SetGUID(MF_MT_SUBTYPE, MFVideoFormat_H264);
    if (SUCCEEDED(hr))
      hr = in_type->SetUINT32(MF_MT_INTERLACE_MODE,
                              MFVideoInterlace_MixedInterlaceOrProgressive);
    if (SUCCEEDED(hr)) hr = mft->SetInputType(0, in_type.Get(), 0);
    if (FAILED(hr)) why = ProbeHr("SetInputType(H264)", hr);
  }
  // 3. Output type deliberately NOT set up front (see header note).
  if (SUCCEEDED(hr)) {
    hr = mft->ProcessMessage(MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0);
    if (SUCCEEDED(hr))
      hr = mft->ProcessMessage(MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0);
    if (FAILED(hr)) why = ProbeHr("streaming start", hr);
  }
  // 4. Feed the whole access unit (SPS/PPS + IDR) as one sample.
  if (SUCCEEDED(hr)) {
    ComPtr<IMFSample> sample;
    ComPtr<IMFMediaBuffer> buf;
    hr = MFCreateSample(sample.GetAddressOf());
    if (SUCCEEDED(hr))
      hr = MFCreateMemoryBuffer(static_cast<DWORD>(annexb.size()),
                                buf.GetAddressOf());
    if (SUCCEEDED(hr)) {
      BYTE* p = nullptr;
      hr = buf->Lock(&p, nullptr, nullptr);
      if (SUCCEEDED(hr)) {
        std::memcpy(p, annexb.data(), annexb.size());
        buf->Unlock();
        hr = buf->SetCurrentLength(static_cast<DWORD>(annexb.size()));
      }
    }
    if (SUCCEEDED(hr)) hr = sample->AddBuffer(buf.Get());
    if (SUCCEEDED(hr)) {
      sample->SetSampleTime(0);
      sample->SetSampleDuration(10000);
      hr = mft->ProcessInput(0, sample.Get(), 0);
    }
    if (FAILED(hr)) why = ProbeHr("ProcessInput", hr);
  }

  // 5. Drain decoded frames and fold every frame's Y plane into one running
  //    FNV-1a state. One IDR access unit decodes to exactly one frame in
  //    practice; multi-frame output would fold in deterministically.
  uint64_t hash = 14695981039346656037ull;  // FNV-1a 64 offset basis
  uint32_t w = 0, h = 0;
  size_t frames = 0;
  if (SUCCEEDED(hr)) {
    MFT_OUTPUT_STREAM_INFO osi{0};
    bool have_osi = false;
    bool eos_sent = false;
    int reneg_count = 0;
    for (int guard = 0; guard < 24 && frames == 0 && SUCCEEDED(hr); ++guard) {
      if (!have_osi) {
        hr = mft->GetOutputStreamInfo(0, &osi);
        if (FAILED(hr)) {
          why = ProbeHr("GetOutputStreamInfo", hr);
          break;
        }
        have_osi = true;
      }
      // Caller-allocated output sample (the decoder does not provide
      // samples). 2D NV12 buffer at the real frame size once it is known;
      // a plain cbSize buffer before the SPS is parsed (never written then).
      ComPtr<IMFSample> out_sample;
      if ((osi.dwFlags & MFT_OUTPUT_STREAM_PROVIDES_SAMPLES) == 0) {
        ComPtr<IMFMediaBuffer> ob;
        if (w != 0 && h != 0) {
          hr = MFCreate2DMediaBuffer(w, h, MFVideoFormat_NV12.Data1, FALSE,
                                     ob.GetAddressOf());
        } else {
          if (osi.cbSize == 0) {
            why = "decoder output buffer size is 0";
            hr = E_FAIL;
            break;
          }
          hr = MFCreateMemoryBuffer(osi.cbSize, ob.GetAddressOf());
          if (SUCCEEDED(hr)) hr = ob->SetCurrentLength(osi.cbSize);
        }
        if (SUCCEEDED(hr)) hr = MFCreateSample(out_sample.GetAddressOf());
        if (SUCCEEDED(hr)) hr = out_sample->AddBuffer(ob.Get());
        if (FAILED(hr)) {
          why = ProbeHr("alloc output sample", hr);
          break;
        }
      }
      DWORD status = 0;
      MFT_OUTPUT_DATA_BUFFER odb{0, out_sample.Get(), 0, nullptr};
      hr = mft->ProcessOutput(0, 1, &odb, &status);
      if (hr == MF_E_TRANSFORM_NEED_MORE_INPUT) {
        if (eos_sent) {
          hr = S_OK;  // drained; nothing more can surface
          break;
        }
        hr = mft->ProcessMessage(MFT_MESSAGE_NOTIFY_END_OF_STREAM, 0);
        if (SUCCEEDED(hr))
          hr = mft->ProcessMessage(MFT_MESSAGE_COMMAND_DRAIN, 0);
        if (FAILED(hr)) {
          why = ProbeHr("end-of-stream drain", hr);
          break;
        }
        eos_sent = true;
        continue;
      }
      if (hr == MF_E_TRANSFORM_STREAM_CHANGE ||
          hr == MF_E_TRANSFORM_TYPE_NOT_SET) {
        // SPS parsed: adopt the decoder's NV12 output type at the real
        // frame size, then re-query the stream info (cbSize tracks it).
        if (++reneg_count > 4) {
          why = "output type negotiation did not converge";
          hr = E_FAIL;
          break;
        }
        ComPtr<IMFMediaType> adopt;
        HRESULT ghr = E_FAIL;
        for (DWORD ai = 0; ai < 8; ++ai) {
          ComPtr<IMFMediaType> av;
          const HRESULT ahr = mft->GetOutputAvailableType(0, ai, av.GetAddressOf());
          if (FAILED(ahr)) break;
          GUID sub = {0};
          if (SUCCEEDED(av->GetGUID(MF_MT_SUBTYPE, &sub)) &&
              sub == MFVideoFormat_NV12) {
            adopt = av;
            ghr = S_OK;
            break;
          }
        }
        if (FAILED(ghr))
          ghr = mft->GetOutputCurrentType(0, adopt.GetAddressOf());
        if (SUCCEEDED(ghr)) {
          UINT32 aw = 0, ah = 0;
          MFGetAttributeSize(adopt.Get(), MF_MT_FRAME_SIZE, &aw, &ah);
          if (aw != 0 && ah != 0) {
            w = aw;
            h = ah;
          }
          ghr = mft->SetOutputType(0, adopt.Get(), 0);
        }
        if (FAILED(ghr)) {
          why = ProbeHr("SetOutputType(NV12)", ghr);
          break;
        }
        have_osi = false;  // re-query cbSize for the adopted frame size
        hr = S_OK;         // the stream change itself was consumed
        continue;
      }
      if (FAILED(hr)) {
        why = ProbeHr("ProcessOutput", hr);
        break;
      }
      if (odb.pSample == nullptr) continue;
      ComPtr<IMFSample> sample;
      if (out_sample != nullptr) {
        sample = out_sample;  // our own buffer: keep our reference
      } else {
        sample.Attach(odb.pSample);  // MFT-allocated: we own it now
      }
      if (w == 0 || h == 0) {
        ComPtr<IMFMediaType> cur;
        if (SUCCEEDED(mft->GetOutputCurrentType(0, cur.GetAddressOf())))
          MFGetAttributeSize(cur.Get(), MF_MT_FRAME_SIZE, &w, &h);
        if (w == 0 || h == 0)
          MFGetAttributeSize(sample.Get(), MF_MT_FRAME_SIZE, &w, &h);
      }
      if (w == 0 || h == 0) {
        why = "decoded frame size unresolved";
        hr = E_FAIL;
        break;
      }
      ComPtr<IMFMediaBuffer> mb;
      hr = sample->GetBufferByIndex(0, mb.GetAddressOf());
      if (FAILED(hr)) break;
      ComPtr<IMF2DBuffer> d2;
      mb->QueryInterface(IID_PPV_ARGS(d2.GetAddressOf()));
      if (d2 != nullptr) {
        BYTE* data = nullptr;
        LONG stride = 0;
        hr = d2->Lock2D(&data, &stride);
        if (SUCCEEDED(hr)) {
          if (stride > 0) {  // top-down (the MS H.264 decoder's layout)
            for (uint32_t y = 0; y < h; ++y)
              hash = Fnv1a64UpdateLocal(hash,
                                        data + static_cast<size_t>(y) * stride, w);
          } else {  // bottom-up
            const LONG abs_stride = -stride;
            for (uint32_t y = 0; y < h; ++y)
              hash = Fnv1a64UpdateLocal(
                  hash, data + static_cast<size_t>(h - 1 - y) * abs_stride, w);
          }
          d2->Unlock2D();
        }
      } else {
        BYTE* data = nullptr;
        DWORD maxlen = 0, curlen = 0;
        hr = mb->Lock(&data, &maxlen, &curlen);
        if (SUCCEEDED(hr)) {
          if (curlen < static_cast<DWORD>(static_cast<size_t>(w) * h)) {
            why = "decoded buffer smaller than the Y plane";
            hr = E_FAIL;
          } else {
            hash = Fnv1a64UpdateLocal(hash, data, static_cast<size_t>(w) * h);
          }
          mb->Unlock();
        }
      }
      if (FAILED(hr)) break;
      ++frames;
    }
  }

  if (SUCCEEDED(hr) && frames >= 1 && w != 0 && h != 0) {
    *luma_hash = hash;
    ok = true;
  } else if (why.empty()) {
    why = FAILED(hr) ? ProbeHr("ProcessOutput", hr)
                     : "decoder produced no frame";
  }
  mft.Reset();  // release the MFT before the apartment goes away
  if (co_owner) CoUninitialize();
  return ok ? true : fail(why);
}

}  // namespace xnc
