// xnc-desktop.cpp - entry point + CLI (Task 2). Modes:
//   --selftest      run the native/desktop selftest (arg parse + FrameBlob +
//                   Task 3 capture-helper units: blob math, pitch
//                   compaction, FNV hash, point sampling)
//   --help          usage text, exit 0
//   --console-diag [--duration <sec>] [--out <file.h264>] [--fps <n>]
//       diagnostic capture loop in the console session. Task 3 wires the
//       DXGI capture backend: per-second counters (captured/timeouts/
//       rebuilds/w/h), a first-frame hash + non-black check, captured frames
//       throttled to --fps. The --out file is created (empty) so an
//       unwritable path surfaces immediately - the encoder lands in Task 4/5.
//       If desktop duplication is refused (no interactive desktop, e.g. run
//       as SYSTEM in session 0) the process logs dxgi_access_denied_session0
//       and exits 1; the Task 6 session bridge makes that path work.
// Exit codes: 0 ok; 1 internal error (cannot open --out, capture init or
// repeated acquire failure); 2 usage error. These flag names are the Task 6
// spawn contract - do not rename.
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>  // GetTickCount64, Sleep

#include <algorithm>  // std::min
#include <cstdint>
#include <cstdio>
#include <cwchar>
#include <memory>
#include <string>

#include "../common/log.h"
#include "capture.h"       // capture contract (ICapture/FrameBlob)
#include "diag.h"
#include "dxgi_capture.h"  // TryCreateDxgiCapture, Fnv1a64, sampling

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
      L"  --selftest      arg parsing + FrameBlob/capture-helper selftest\n"
      L"  --help          this usage text\n"
      L"console-diag runs the DXGI capture loop (captured/timeouts/rebuilds\n"
      L"per-second counters, first-frame non-black check); stream encoding\n"
      L"arrives in Tasks 4-5\n");
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

  // Task 3: real DXGI capture. Init failure paths:
  //   - desktop access denied (session 0 SYSTEM direct run) is EXPECTED
  //     until the Task 6 session bridge: log a clear marker, exit 1;
  //   - anything else is a genuine backend failure, exit 1.
  std::string cap_err;
  std::unique_ptr<xnc::ICapture> capture = xnc::TryCreateDxgiCapture(&cap_err);
  if (!capture) {
    if (xnc::DxgiErrIsDesktopAccessDenied(cap_err)) {
      XNC_LOG_ERROR("dxgi_access_denied_session0 err=\"%s\"", cap_err.c_str());
    } else {
      XNC_LOG_ERROR("capture_init_failed err=\"%s\"", cap_err.c_str());
    }
    std::fclose(out);
    return 1;
  }
  XNC_LOG_INFO("capture_init w=%u h=%u", capture->Width(), capture->Height());

  const uint64_t t0 = NowMs();
  uint32_t next_beat_s = 0;
  unsigned long long captured = 0, timeouts = 0;
  bool first_frame_logged = false;
  uint64_t last_capture_ms = 0;
  const uint32_t spf_ms = 1000u / opt.fps;  // throttle captured frames to --fps
  xnc::FrameBlob blob;
  std::string acq_err;
  for (;;) {
    const uint64_t now = NowMs();
    if (now - t0 >= static_cast<uint64_t>(opt.duration_s) * 1000ull) break;
    acq_err.clear();
    if (capture->Acquire(blob, &acq_err)) {
      ++captured;
      if (!first_frame_logged) {
        first_frame_logged = true;
        const size_t head = std::min<size_t>(64, blob.bgra.size());
        const unsigned long long hash =
            static_cast<unsigned long long>(xnc::Fnv1a64(blob.bgra.data(), head));
        const bool non_black = xnc::SamplePointsNotUniform(blob.bgra.data(), blob.w, blob.h);
        XNC_LOG_INFO("first_frame hash_head64=%016llx non_black=%d w=%u h=%u mono_us=%llu",
                     hash, non_black ? 1 : 0, blob.w, blob.h,
                     static_cast<unsigned long long>(blob.mono_us));
        if (!non_black)
          XNC_LOG_ERROR("first_frame_uniform (all 256 sampled pixels equal - suspect black/garbage frame)");
      }
      // Pace captured frames to the target fps (timeouts are already paced
      // by the 100ms AcquireNextFrame wait).
      if (last_capture_ms != 0) {
        const uint64_t since = NowMs() - last_capture_ms;
        if (since < spf_ms) Sleep(static_cast<DWORD>(spf_ms - since));
      }
      last_capture_ms = NowMs();
    } else if (acq_err == "err_timeout") {
      ++timeouts;  // static screen: silent, no blob
    } else if (acq_err == "err_rebuilt") {
      // access lost + in-place rebuild; rebuilds_ counter covers it
    } else {
      XNC_LOG_ERROR("acquire_failed err=\"%s\"", acq_err.c_str());
      std::fclose(out);
      return 1;
    }
    const uint32_t elapsed_s = static_cast<uint32_t>((NowMs() - t0) / 1000);
    if (elapsed_s >= next_beat_s) {
      XNC_LOG_INFO("diag_capture elapsed=%us captured=%llu timeouts=%llu rebuilds=%u w=%u h=%u",
                   elapsed_s, captured, timeouts, capture->RebuildCount(),
                   capture->Width(), capture->Height());
      next_beat_s = elapsed_s + 1;
    }
  }
  XNC_LOG_INFO("console_diag_stop elapsed=%us captured=%llu timeouts=%llu rebuilds=%u",
               opt.duration_s, captured, timeouts, capture->RebuildCount());
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
