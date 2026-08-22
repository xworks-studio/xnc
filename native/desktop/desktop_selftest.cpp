// desktop_selftest.cpp - native/desktop selftest (Task 2 scope: console-diag
// arg parsing + FrameBlob/ICapture layout; Task 3 scope: BGRA byte math,
// pitch compaction, FNV-1a hash and 256-point sampling helpers from
// dxgi_capture.h; Task 4 scope: BGRA→NV12 BT.601 color math, Annex-B NAL
// parse helpers, and the MfSoftEncoder contract incl. the E2 one-shot
// force-key regression; Task 5 scope: FrameCache state-machine transitions,
// VclNalus/ShapeAu stream-contract shaping, FlushTail tail recovery, and the
// full Pipeline::Run end-to-end over a scripted fake ICapture + the real MF
// encoder). Pure-logic cases need no desktop; the encoder/pipeline scenarios
// feed synthetic color bars straight into the MF software H.264 MFT, so no
// capture is involved and they run on any Windows box that ships
// CMSH264EncoderMFT (client SKUs). Any failure prints
// "SELFTEST FAIL: <name>" and exits 1; all-pass prints "selftest ok". Entry
// point SelftestMain() is declared by xnc-desktop.cpp and reachable via
// `xnc-desktop.exe --selftest` / `build.bat selftest`.
#include "capture.h"
#include "diag.h"
#include "dxgi_capture.h"
#include "frame_cache.h"
#include "mf_encoder.h"
#include "nv12.h"
#include "pipeline.h"

#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>  // GetTempPathW, GetCurrentProcessId, DeleteFileW

#include <cstddef>  // offsetof
#include <cstdio>
#include <algorithm>  // std::find
#include <cstring>
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
  bool Acquire(xnc::FrameBlob& blob, std::string* err = nullptr) override {
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
      blob.mono_us = ++mono_;
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
  { // Task 5:致命 Acquire 错误 → Run 立即失败(未初始化编码器也安全)
    struct FatalCapture final : xnc::ICapture {
      bool Acquire(xnc::FrameBlob&, std::string* err = nullptr) override {
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
    uninit.FlushTail(noop);
    CHECK("mf-flush-before-init-noop", noop.empty());  // 与 Drain 同样的防御
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
        enc.FlushTail(aus);  // 追加
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
        xnc::PipelineResult res = xnc::Pipeline::Run(cap, enc, tf.get(), o);
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
  if (fails == 0) std::printf("selftest ok\n");
  return fails == 0 ? 0 : 1;
}
