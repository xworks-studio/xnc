// desktop_selftest.cpp - native/desktop selftest (Task 2 scope: console-diag
// arg parsing + FrameBlob/ICapture layout; Task 3 scope: BGRA byte math,
// pitch compaction, FNV-1a hash and 256-point sampling helpers from
// dxgi_capture.h; Task 4 scope: BGRA→NV12 BT.601 color math, Annex-B NAL
// parse helpers, and the MfSoftEncoder contract incl. the E2 one-shot
// force-key regression). Pure-logic cases need no desktop; the encoder
// scenarios (a)-(d) feed synthetic color bars straight into the MF software
// H.264 MFT, so no capture is involved and they run on any Windows box that
// ships CMSH264EncoderMFT (client SKUs). Any failure prints
// "SELFTEST FAIL: <name>" and exits 1; all-pass prints "selftest ok". Entry
// point SelftestMain() is declared by xnc-desktop.cpp and reachable via
// `xnc-desktop.exe --selftest` / `build.bat selftest`.
#include "capture.h"
#include "diag.h"
#include "dxgi_capture.h"
#include "mf_encoder.h"
#include "nv12.h"

#include <cstddef>  // offsetof
#include <cstdio>
#include <algorithm>  // std::find
#include <string>
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

// Selftest-only fake backend: proves ICapture is implementable/abstract and
// is reusable by Task 5's synthetic-capture pipeline tests.
struct FakeCapture final : xnc::ICapture {
  bool Acquire(xnc::FrameBlob&, std::string* = nullptr) override { return false; }
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

}  // namespace

int SelftestMain() {
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
  { // FrameBlob 布局(MSVC x64 ABI):bgra(vector 24B)+w(4)+h(4)+mono_us(8) 紧凑 40B
    CHECK("frameblob-sizeof", sizeof(xnc::FrameBlob) == 40);
    CHECK("frameblob-off-bgra", offsetof(xnc::FrameBlob, bgra) == 0);
    CHECK("frameblob-off-w", offsetof(xnc::FrameBlob, w) == 24);
    CHECK("frameblob-off-h", offsetof(xnc::FrameBlob, h) == 28);
    CHECK("frameblob-off-mono", offsetof(xnc::FrameBlob, mono_us) == 32);
    CHECK("frameblob-field-sizes", sizeof(xnc::FrameBlob::w) == 4 && sizeof(xnc::FrameBlob::h) == 4 &&
                                  sizeof(xnc::FrameBlob::mono_us) == 8);
    xnc::FrameBlob fb;
    CHECK("frameblob-default", fb.bgra.empty() && fb.w == 0 && fb.h == 0 && fb.mono_us == 0);
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
    CHECK("mf-drain-before-init-noop", (enc.Drain(aus), aus.empty()));
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
        enc.Drain(aus);  // 冷启动缓冲排空:重复 ProcessOutput 至 NEED_MORE_INPUT
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
  if (fails == 0) std::printf("selftest ok\n");
  return fails == 0 ? 0 : 1;
}
