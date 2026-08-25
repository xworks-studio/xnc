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

  // Box filter: each destination pixel averages the source rectangle it
  // covers (the rectangles exactly tile the source, so no pixel is
  // weighted twice or skipped).
  //
  // pipeline-decouple perf pass (the capture thread's per-frame downscale
  // was the XIAOXIN fps limiter: ~40 ms at 2880x1800->1920x1200 here, ~90 ms
  // on the target): the box tiling is precomputed once per call and the
  // per-pixel 64-bit DIVISIONS are replaced by an exact multiply-high
  // reciprocal. Every source span is xb or xb+1 (same for rows), so each
  // destination pixel's box size n takes at most 4 distinct values - one
  // reciprocal each, then per channel: q = (sum * (2^32/n)) >> 32, with a
  // conditional correction that makes the result bit-identical to sum / n
  // (see the exactness argument below). Results are byte-identical to the
  // original naive loop (selftest ds-box-avg is the regression anchor).
  std::vector<uint32_t> x0(nw + 1), xs(nw), y0(nh + 1), ys(nh);
  for (uint32_t dx = 0; dx <= nw; dx++)
    x0[dx] = (uint32_t)(((uint64_t)w * dx) / nw);
  for (uint32_t dx = 0; dx < nw; dx++) xs[dx] = x0[dx + 1] - x0[dx];
  for (uint32_t dy = 0; dy <= nh; dy++)
    y0[dy] = (uint32_t)(((uint64_t)h * dy) / nh);
  for (uint32_t dy = 0; dy < nh; dy++) ys[dy] = y0[dy + 1] - y0[dy];
  // All spans are base or base+1: n = xspan*yspan takes <= 4 values.
  const uint32_t xb = xs[0], yb = ys[0];
  uint64_t rtab[2][2] = {{0, 0}, {0, 0}};
  uint32_t ntab[2][2] = {{0, 0}, {0, 0}};
  for (int i = 0; i < 2; i++)
    for (int j = 0; j < 2; j++) {
      ntab[i][j] = (xb + i) * (yb + j);
      rtab[i][j] = 0x100000000ull / ntab[i][j];  // floor(2^32 / n); n=1 -> 2^32
    }
  out->assign((size_t)nw * nh * 4, 0);
  for (uint32_t dy = 0; dy < nh; dy++) {
    const uint32_t yi = ys[dy] == yb ? 0 : 1;
    const uint32_t yspan = ys[dy];
    uint8_t* drow = out->data() + (size_t)dy * nw * 4;
    for (uint32_t dx = 0; dx < nw; dx++) {
      const uint32_t xi = xs[dx] == xb ? 0 : 1;
      const uint32_t xspan = xs[dx];
      const uint32_t n = ntab[xi][yi];
      const uint64_t r = rtab[xi][yi];
      uint32_t b = 0, g = 0, r_ = 0, a = 0;
      const uint8_t* srow = src + (size_t)y0[dy] * w * 4 + (size_t)x0[dx] * 4;
      for (uint32_t y = 0; y < yspan; y++, srow += (size_t)w * 4) {
        const uint8_t* p = srow;
        for (uint32_t x = 0; x < xspan; x++, p += 4) {
          b += p[0];
          g += p[1];
          r_ += p[2];
          a += p[3];
        }
      }
      // Exact division by n via multiply-high + correction:
      //   q' = (sum * floor(2^32/n)) / 2^32 = sum/n - sum*f/(n*2^32),
      //   f = 2^32 mod n. With sum <= 255*n the error term is < 1/n and the
      //   true quotient is either q = floor(q') (no fix) or q = floor(q')+1
      //   (fix when sum - q'*n >= n) - identical to the C++ sum / n.
      uint8_t* d = drow + (size_t)dx * 4;
      uint32_t qb = (uint32_t)(((uint64_t)b * r) >> 32);
      uint32_t t = (uint32_t)((uint64_t)b - (uint64_t)qb * n);
      if (t >= n) qb += t / n;
      uint32_t qg = (uint32_t)(((uint64_t)g * r) >> 32);
      t = (uint32_t)((uint64_t)g - (uint64_t)qg * n);
      if (t >= n) qg += t / n;
      uint32_t qr = (uint32_t)(((uint64_t)r_ * r) >> 32);
      t = (uint32_t)((uint64_t)r_ - (uint64_t)qr * n);
      if (t >= n) qr += t / n;
      uint32_t qa = (uint32_t)(((uint64_t)a * r) >> 32);
      t = (uint32_t)((uint64_t)a - (uint64_t)qa * n);
      if (t >= n) qa += t / n;
      d[0] = (uint8_t)qb;
      d[1] = (uint8_t)qg;
      d[2] = (uint8_t)qr;
      d[3] = (uint8_t)qa;
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
