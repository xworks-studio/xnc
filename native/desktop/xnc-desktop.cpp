// xnc-desktop.cpp - entry point + CLI (Task 2). Modes:
//   --selftest      run the native/desktop selftest (arg parse + FrameBlob +
//                   capture-helper units, encoder contract, FrameCache state
//                   machine and the end-to-end pipeline)
//   --help          usage text, exit 0
//   --console-diag [--duration <sec>] [--out <file.h264>] [--fps <n>]
//       diagnostic capture loop in the console session: DXGI capture ->
//       FrameCache state machine -> MF software H.264 encoder -> shaped
//       Annex-B AUs into --out, plus a stats.json sidecar next to it
//       (written even when capture/encoder init fails, with zeroed
//       counters). If desktop duplication is refused (no interactive
//       desktop, e.g. run as SYSTEM in session 0) the process logs
//       dxgi_access_denied_session0 and exits 1; the Task 6 session bridge
//       makes that path work.
// Exit codes: 0 ok; 1 internal error (cannot open --out, capture/encoder
// init or repeated acquire failure); 2 usage error. These flag names are the
// Task 6 spawn contract - do not rename.
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>  // GetTickCount64, Sleep

#include <cstdint>
#include <cstdio>
#include <cwchar>
#include <memory>
#include <string>

#include "../common/log.h"
#include "capture.h"       // capture contract (ICapture/FrameBlob)
#include "diag.h"
#include "dxgi_capture.h"  // TryCreateDxgiCapture, DxgiErrIsDesktopAccessDenied
#include "mf_encoder.h"    // MfSoftEncoder
#include "pipeline.h"      // Pipeline::Run + stats.json sidecar

int SelftestMain();  // desktop_selftest.cpp

namespace {

void Usage(FILE* out) {
  std::fwprintf(out,
      L"xnc-desktop - XNC desktop capture process (M1)\n"
      L"usage: xnc-desktop.exe --console-diag [--duration <sec>] [--out <file.h264>] [--fps <n>]\n"
      L"       xnc-desktop.exe --selftest | --help\n"
      L"  --console-diag  diagnostic capture loop in the console session\n"
      L"  --duration      seconds to run (default 10, must be > 0)\n"
      L"  --out           output H.264 path (required with --console-diag);\n"
      L"                  a stats.json sidecar is written next to it\n"
      L"  --fps           target fps (default 30, must be > 0)\n"
      L"  --selftest      arg parsing + FrameBlob/encoder/pipeline selftest\n"
      L"  --help          this usage text\n"
      L"console-diag runs DXGI capture -> FrameCache -> MF H.264 encode and\n"
      L"writes shaped Annex-B AUs to --out (SPS/PPS before IDR, 4-byte start\n"
      L"codes, no AUD) plus per-second counters and a stats.json sidecar\n");
}

}  // namespace

namespace xnc {

// Pure digits only (no sign/space/trailing chars), rejects overflow and
// empty strings; value 0 is valid here, callers enforce their own >0 rule.
bool ParseU32(const wchar_t* s, uint32_t* out) {
  if (!s || !*s) return false;
  uint64_t v = 0;
  for (const wchar_t* p = s; *p; ++p) {
    if (*p < L'0' || *p > L'9') return false;
    v = v * 10 + static_cast<uint64_t>(*p - L'0');
    if (v > 0xFFFFFFFFull) return false;
  }
  *out = static_cast<uint32_t>(v);
  return true;
}

bool ParseDiagArgs(int argc, wchar_t** argv, DiagOptions* opt, std::wstring* err) {
  auto fail = [err](std::wstring msg) {
    if (err) *err = std::move(msg);
    return false;
  };
  for (int i = 1; i < argc; i++) {
    const wchar_t* a = argv[i];
    auto value_of = [&](const wchar_t* flag) -> const wchar_t* {
      if (i + 1 >= argc) {
        std::wstring msg = std::wstring(flag) + L" requires a value";
        if (err) *err = msg;
        return nullptr;
      }
      return argv[++i];
    };
    if (std::wcscmp(a, L"--console-diag") == 0) {
      opt->console_diag = true;
    } else if (std::wcscmp(a, L"--selftest") == 0) {
      opt->selftest = true;
    } else if (std::wcscmp(a, L"--help") == 0) {
      opt->help = true;
    } else if (std::wcscmp(a, L"--duration") == 0) {
      const wchar_t* v = value_of(L"--duration");
      if (!v) return false;
      if (!ParseU32(v, &opt->duration_s) || opt->duration_s == 0)
        return fail(L"--duration must be a positive integer number of seconds");
    } else if (std::wcscmp(a, L"--fps") == 0) {
      const wchar_t* v = value_of(L"--fps");
      if (!v) return false;
      if (!ParseU32(v, &opt->fps) || opt->fps == 0)
        return fail(L"--fps must be a positive integer");
    } else if (std::wcscmp(a, L"--out") == 0) {
      const wchar_t* v = value_of(L"--out");
      if (!v) return false;
      opt->out_path = v;
    } else {
      return fail(std::wstring(L"unknown argument: ") + a);
    }
  }
  const int modes = (opt->console_diag ? 1 : 0) + (opt->selftest ? 1 : 0) +
                    (opt->help ? 1 : 0);
  if (modes > 1)
    return fail(L"--console-diag, --selftest and --help are mutually exclusive");
  if (modes == 0)
    return fail(L"one of --console-diag, --selftest, --help is required");
  if (opt->console_diag && opt->out_path.empty())
    return fail(L"--console-diag requires --out <file.h264>");
  return true;
}

}  // namespace xnc

namespace {

// Diag default bitrate (task 5 wiring): 2.3 Mbps.
constexpr uint32_t kDiagBitrateBps = 2300000;

int RunConsoleDiag(const xnc::DiagOptions& opt) {
  FILE* out = nullptr;
  const errno_t open_err = _wfopen_s(&out, opt.out_path.c_str(), L"wb");
  if (open_err != 0 || !out) {
    XNC_LOG_ERROR("open out failed path=%ls errno=%d", opt.out_path.c_str(), open_err);
    return 1;
  }
  XNC_LOG_INFO("console_diag_start duration=%us fps=%u bitrate=%u out=%ls",
               opt.duration_s, opt.fps, kDiagBitrateBps, opt.out_path.c_str());

  xnc::PipelineOpts popt;
  popt.duration_s = opt.duration_s;
  popt.fps = opt.fps;
  popt.target_bitrate_bps = kDiagBitrateBps;

  // Every diag run leaves a stats.json sidecar next to --out - including the
  // init-failure paths below (zeroed counters, ok=false + the reason), so a
  // missing sidecar always means "the process never got that far", never
  // "it ran and vanished".
  const auto write_stats = [&opt, &popt](const xnc::PipelineResult& r) {
    std::wstring serr;
    if (!xnc::WriteStatsJson(opt.out_path, r, popt, &serr))
      XNC_LOG_ERROR("stats_json_write_failed err=\"%ls\"", serr.c_str());
  };
  const auto fail_result = [](const char* why) {
    xnc::PipelineResult r;
    r.ok = false;
    r.err = why;
    return r;
  };

  // Task 3: real DXGI capture. Init failure paths:
  //   - desktop access denied (session 0 SYSTEM direct run) is EXPECTED
  //     until the Task 6 session bridge: log a clear marker, exit 1;
  //   - anything else is a genuine backend failure, exit 1.
  std::string cap_err;
  std::unique_ptr<xnc::ICapture> capture = xnc::TryCreateDxgiCapture(&cap_err);
  if (!capture) {
    if (xnc::DxgiErrIsDesktopAccessDenied(cap_err)) {
      XNC_LOG_ERROR("dxgi_access_denied_session0 err=\"%s\"", cap_err.c_str());
      write_stats(fail_result("dxgi_access_denied_session0"));
    } else {
      XNC_LOG_ERROR("capture_init_failed err=\"%s\"", cap_err.c_str());
      write_stats(fail_result("capture_init_failed"));
    }
    std::fclose(out);
    return 1;
  }
  XNC_LOG_INFO("capture_init w=%u h=%u", capture->Width(), capture->Height());

  // Task 5: capture -> FrameCache -> MF software encode -> shaped Annex-B
  // AUs into --out; encoder at the capture's dimensions, fps from args.
  xnc::MfSoftEncoder encoder;
  std::string enc_err;
  if (!encoder.Init(capture->Width(), capture->Height(), opt.fps, kDiagBitrateBps,
                    &enc_err)) {
    XNC_LOG_ERROR("encoder_init_failed err=\"%s\"", enc_err.c_str());
    write_stats(fail_result("encoder_init_failed"));
    std::fclose(out);
    return 1;
  }

  xnc::PipelineResult res = xnc::Pipeline::Run(*capture, encoder, out, popt);
  write_stats(res);
  const xnc::FrameCacheCounters& c = res.counters;
  XNC_LOG_INFO("console_diag_stop duration=%us captured=%llu encoded=%llu keyframes=%llu timeouts=%llu warmup_feeds=%llu rebuilds=%u aus=%llu ok=%d",
               opt.duration_s, static_cast<unsigned long long>(c.captured),
               static_cast<unsigned long long>(c.encoded),
               static_cast<unsigned long long>(c.keyframes),
               static_cast<unsigned long long>(c.timeouts),
               static_cast<unsigned long long>(c.warmup_feeds), c.rebuilds,
               static_cast<unsigned long long>(res.aus_written), res.ok ? 1 : 0);
  std::fclose(out);
  return res.ok ? 0 : 1;
}

}  // namespace

int wmain(int argc, wchar_t** argv) {
  xnc::SetLogProcessName("desktop");
  xnc::DiagOptions opt;
  std::wstring err;
  if (!xnc::ParseDiagArgs(argc, argv, &opt, &err)) {
    std::fwprintf(stderr, L"xnc-desktop: %s\n", err.c_str());
    Usage(stderr);
    return 2;
  }
  if (opt.help) {
    Usage(stdout);
    return 0;
  }
  if (opt.selftest) return SelftestMain();
  return RunConsoleDiag(opt);
}
