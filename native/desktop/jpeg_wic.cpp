// jpeg_wic.cpp - M2-Slice3 Task 3 implementation (see jpeg_wic.h).
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>

#include <objbase.h>
#include <wincodec.h>  // WIC (header; the import lib is windowscodecs.lib)

#include <cstring>
#include <string>
#include <vector>

#include "jpeg_wic.h"

namespace xnc {

bool DownscaleBgra(const uint8_t* src, uint32_t w, uint32_t h, uint32_t max_w,
                   std::vector<uint8_t>* out, uint32_t* ow, uint32_t* oh) {
  if (out == nullptr || ow == nullptr || oh == nullptr || src == nullptr)
    return false;
  if (w == 0 || h == 0) return false;
  if (max_w == 0 || w <= max_w) {  // identity pass-through
    out->assign(src, src + (size_t)w * h * 4);
    *ow = w;
    *oh = h;
    return true;
  }
  // Target width: max_w; height keeps the aspect (>= 1 px).
  const uint32_t nw = max_w;
  const uint32_t nh = (uint32_t)(((uint64_t)h * nw + w - 1) / w);
  if (nw == 0 || nh == 0 || nh > h) return false;  // degenerate guard
  out->assign((size_t)nw * nh * 4, 0);
  // Box filter: each destination pixel averages the source rectangle it
  // covers (the rectangles exactly tile the source, so no pixel is
  // weighted twice or skipped).
  for (uint32_t dy = 0; dy < nh; dy++) {
    const uint32_t y0 = (uint32_t)(((uint64_t)h * dy) / nh);
    const uint32_t y1 = (uint32_t)(((uint64_t)h * (dy + 1)) / nh);
    for (uint32_t dx = 0; dx < nw; dx++) {
      const uint32_t x0 = (uint32_t)(((uint64_t)w * dx) / nw);
      const uint32_t x1 = (uint32_t)(((uint64_t)w * (dx + 1)) / nw);
      uint64_t b = 0, g = 0, r = 0, a = 0;
      uint32_t n = 0;
      for (uint32_t y = y0; y < y1 && y < h; y++) {
        const uint8_t* row = src + (size_t)y * w * 4 + (size_t)x0 * 4;
        for (uint32_t x = x0; x < x1 && x < w; x++, row += 4) {
          b += row[0];
          g += row[1];
          r += row[2];
          a += row[3];
          n++;
        }
      }
      if (n == 0) return false;  // degenerate cell (nw > w is excluded above)
      uint8_t* d = out->data() + ((size_t)dy * nw + dx) * 4;
      d[0] = (uint8_t)(b / n);
      d[1] = (uint8_t)(g / n);
      d[2] = (uint8_t)(r / n);
      d[3] = (uint8_t)(a / n);
    }
  }
  *ow = nw;
  *oh = nh;
  return true;
}

bool WicEncodeJpeg(const uint8_t* bgra, uint32_t w, uint32_t h, float quality,
                   std::vector<uint8_t>* jpeg, std::string* err) {
  if (err) err->clear();
  if (jpeg == nullptr || bgra == nullptr || w == 0 || h == 0) {
    if (err) *err = "bad_args";
    return false;
  }
  if (quality < 0.0f) quality = 0.0f;
  if (quality > 1.0f) quality = 1.0f;
  jpeg->clear();

  // COINIT_APARTMENTTHREADED when this thread has no apartment yet; a
  // caller that already initialized a (possibly different) model keeps
  // its own - WIC works under either, and we only uninitialize what WE
  // initialized (RPC_E_CHANGED_MODE means "not ours").
  const HRESULT hr_co = CoInitializeEx(nullptr, COINIT_APARTMENTTHREADED);
  const bool co_owned = SUCCEEDED(hr_co);

  bool ok = false;
  IWICImagingFactory* factory = nullptr;
  IWICBitmap* bitmap = nullptr;
  IStream* stream = nullptr;
  IWICBitmapEncoder* encoder = nullptr;
  IWICBitmapFrameEncode* frame = nullptr;
  IPropertyBag2* props = nullptr;
  const char* tag = "wic_factory";

  if (SUCCEEDED(CoCreateInstance(CLSID_WICImagingFactory, nullptr,
                                 CLSCTX_INPROC_SERVER,
                                 IID_PPV_ARGS(&factory))) &&
      SUCCEEDED(factory->CreateBitmapFromMemory(
          w, h, GUID_WICPixelFormat32bppBGRA, (UINT)w * 4, (UINT)w * h * 4,
          const_cast<uint8_t*>(bgra), &bitmap)) &&
      SUCCEEDED(CreateStreamOnHGlobal(nullptr, TRUE, &stream)) &&
      SUCCEEDED(factory->CreateEncoder(GUID_ContainerFormatJpeg, nullptr,
                                       &encoder)) &&
      SUCCEEDED(encoder->Initialize(stream, WICBitmapEncoderNoCache)) &&
      SUCCEEDED(encoder->CreateNewFrame(&frame, &props))) {
    tag = "wic_quality";
    // Quality rides the frame's property bag (classic WIC contract:
    // ImageQuality, VT_R4, 0..1).
    PROPBAG2 bag{};
    bag.dwType = PROPBAG2_TYPE_DATA;
    bag.pstrName = (LPOLESTR)L"ImageQuality";
    VARIANT v;
    VariantInit(&v);
    v.vt = VT_R4;
    v.fltVal = quality;
    const HRESULT hq = props->Write(1, &bag, &v);
    if (SUCCEEDED(hq) || hq == E_NOTIMPL /* encoder default quality */) {
      tag = "wic_frame";
      if (SUCCEEDED(frame->Initialize(nullptr)) &&
          SUCCEEDED(frame->SetSize(w, h)) &&
          SUCCEEDED(frame->WriteSource(bitmap, nullptr)) &&
          SUCCEEDED(frame->Commit()) &&
          SUCCEEDED(encoder->Commit())) {
        // Pull the encoded bytes out of the HGLOBAL-backed stream.
        HGLOBAL hg = nullptr;
        STATSTG st{};
        if (SUCCEEDED(GetHGlobalFromStream(stream, &hg)) &&
            SUCCEEDED(stream->Stat(&st, STATFLAG_NONAME)) &&
            st.cbSize.QuadPart > 0 &&
            st.cbSize.QuadPart <= (LONGLONG)GlobalSize(hg)) {
          void* mem = GlobalLock(hg);
          if (mem != nullptr) {
            jpeg->assign((const uint8_t*)mem,
                         (const uint8_t*)mem + st.cbSize.QuadPart);
            GlobalUnlock(hg);
            ok = true;
            tag = nullptr;
          } else {
            tag = "wic_lock";
          }
        } else {
          tag = "wic_stat";
        }
      }
    }
  }

  if (props) props->Release();
  if (frame) frame->Release();
  if (encoder) encoder->Release();
  if (stream) stream->Release();
  if (bitmap) bitmap->Release();
  if (factory) factory->Release();
  if (co_owned) CoUninitialize();
  if (!ok) {
    if (err) *err = tag;
    return false;
  }
  return true;
}

}  // namespace xnc
