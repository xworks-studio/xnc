// xnc-desktop.cpp - entry point + CLI (Task 2). Modes:
//   --selftest      run the native/desktop selftest (arg parse + FrameBlob)
//   --help          usage text, exit 0
//   --console-diag [--duration <sec>] [--out <file.h264>] [--fps <n>]
//       diagnostic capture loop in the console session. Task 2 has no
//       capture backend yet (TryCreateDxgiCapture lands in Task 3, encoder
//       in Task 4/5): the loop heartbeats once per second with
//       "diag_no_capture" and exits 0 when the duration elapses. The --out
//       file is created (empty) so an unwritable path surfaces immediately,
//       not after a 60s diag run.
// Exit codes: 0 ok; 1 internal error (e.g. cannot open --out); 2 usage
// error. These flag names are the Task 6 spawn contract - do not rename.
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>  // GetTickCount64, Sleep

#include <cstdint>
#include <cstdio>
#include <cwchar>
#include <string>

#include "../common/log.h"
#include "capture.h"  // capture contract; Task 3 wires TryCreateDxgiCapture
#include "diag.h"

int SelftestMain();  // desktop_selftest.cpp

namespace {

void Usage(FILE* out) {
  std::fwprintf(out,
      L"xnc-desktop - XNC desktop capture process (M1)\n"
      L"usage: xnc-desktop.exe --console-diag [--duration <sec>] [--out <file.h264>] [--fps <n>]\n"
      L"       xnc-desktop.exe --selftest | --help\n"
      L"  --console-diag  diagnostic capture loop in the console session\n"
      L"  --duration      seconds to run (default 10, must be > 0)\n"
      L"  --out           output H.264 path (required with --console-diag)\n"
      L"  --fps           target fps (default 30, must be > 0)\n"
      L"  --selftest      arg parsing + FrameBlob layout selftest\n"
      L"  --help          this usage text\n"
      L"capture/encoder backends arrive in Tasks 3-5; console-diag currently\n"
      L"only heartbeats (diag_no_capture) and writes no stream data\n");
}

uint64_t NowMs() { return GetTickCount64(); }

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

int RunConsoleDiag(const xnc::DiagOptions& opt) {
  FILE* out = nullptr;
  const errno_t open_err = _wfopen_s(&out, opt.out_path.c_str(), L"wb");
  if (open_err != 0 || !out) {
    XNC_LOG_ERROR("open out failed path=%ls errno=%d", opt.out_path.c_str(), open_err);
    return 1;
  }
  XNC_LOG_INFO("console_diag_start duration=%us fps=%u out=%ls",
               opt.duration_s, opt.fps, opt.out_path.c_str());
  // Task 3 constructs TryCreateDxgiCapture() here; with no capture source
  // the Task 2 contract is a per-second heartbeat until the duration
  // elapses. First beat fires immediately (elapsed=0), then one per second.
  const uint64_t t0 = NowMs();
  uint32_t next_beat_s = 0;
  for (;;) {
    const uint32_t elapsed_s = static_cast<uint32_t>((NowMs() - t0) / 1000);
    if (elapsed_s >= opt.duration_s) break;
    if (elapsed_s >= next_beat_s) {
      XNC_LOG_INFO("diag_no_capture elapsed=%us duration=%us captured=0",
                   elapsed_s, opt.duration_s);
      next_beat_s = elapsed_s + 1;
    }
    Sleep(50);
  }
  XNC_LOG_INFO("console_diag_stop elapsed=%us captured=0", opt.duration_s);
  std::fclose(out);
  return 0;
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
