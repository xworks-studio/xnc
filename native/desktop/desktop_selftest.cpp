// desktop_selftest.cpp - native/desktop selftest (Task 2 scope: console-diag
// arg parsing + FrameBlob/ICapture layout; Task 3 scope: BGRA byte math,
// pitch compaction, FNV-1a hash and 256-point sampling helpers from
// dxgi_capture.h). Pure logic - no desktop or DXGI needed, so it runs on
// LABS-DEV (owner is RDP'd in; capture only on XIAOXIN). Any failure prints
// "SELFTEST FAIL: <name>" and exits 1; all-pass prints "selftest ok". Entry
// point SelftestMain() is declared by xnc-desktop.cpp and reachable via
// `xnc-desktop.exe --selftest` / `build.bat selftest`.
#include "capture.h"
#include "diag.h"
#include "dxgi_capture.h"

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
  if (fails == 0) std::printf("selftest ok\n");
  return fails == 0 ? 0 : 1;
}
