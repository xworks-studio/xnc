// xnc-desktop.cpp - entry point + CLI (Task 2). Modes:
//   --selftest      run the native/desktop selftest (arg parse + FrameBlob +
//                   capture-helper units, encoder contract, FrameCache state
//                   machine, the end-to-end pipeline and the rt pipe server)
//   --help          usage text, exit 0
//   --console-diag [--duration <sec>] [--out <file.h264>] [--fps <n>]
//       diagnostic capture loop in the console session: DXGI capture ->
//       FrameCache state machine -> MF software H.264 encoder -> shaped
//       Annex-B AUs into --out, plus a stats.json sidecar next to it
//       (written even when capture/encoder init fails, with zeroed
//       counters). If desktop duplication is refused (no interactive
//       desktop, e.g. run as SYSTEM in session 0) the process logs
//       dxgi_access_denied_session0 and exits 1; the Task 6 session bridge
//       makes that path work. Optional --pipe <name> --secret <hex> also
//       serves the real-time pipe off the same single pipeline run.
//   --console-rt (--secret-stdin | --secret <hex>) [--pipe <name>]
//       [--max-subs <n>] [--fps <n>]
//       real-time mode (M1-Slice2): same pipeline, AUs fanned out to pipe
//       subscribers (ATTACH/DETACH/KEYFRAME_REQ in, HOST_HELLO/FRAME/STATE
//       out) instead of a file. Runs until Ctrl+C. The secret comes from
//       stdin (--secret-stdin; the core service path, spec 1.5: never argv)
//       or from --secret <hex> (interactive diagnostics only).
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
#include <vector>

#include "../common/log.h"
#include "capture.h"       // capture contract (ICapture/FrameBlob)
#include "capture_reset.h"  // CaptureReset (M2-Slice1 Task 2)
#include "cursor_manager.h"  // CursorManager (M1-Slice3)
#include "desktop_watch.h"  // DesktopWatch (M2-Slice1 Task 1/2)
#include "diag.h"
#include "dxgi_capture.h"  // TryCreateDxgiCapture, DxgiErrIsDesktopAccessDenied
#include "input_manager.h"  // InputManager (M1-Slice3)
#include "mf_encoder.h"    // MfSoftEncoder
#include "pipeline.h"      // Pipeline::Run + stats.json sidecar
#include "rt_pipe_server.h"  // RtServer (real-time fan-out)

int SelftestMain();  // desktop_selftest.cpp

namespace {

// PipelineOpts desktop-name thunk (M2-Slice1 Task 1): the beat runs on the
// single pipeline thread, so one static buffer is enough. Returns "" before
// the first observation (the beat then omits the field).
const char* DesktopNameThunk(void* ctx) {
  static char buf[xnc::kDesktopNameMax];
  static_cast<xnc::DesktopWatch*>(ctx)->CurrentName(buf, sizeof(buf));
  return buf;
}

// M2-Slice1 Task 2 wiring (both console modes): DesktopWatch transitions
// request unified resets, and the reset's desktop gate (watch snapshot)
// suspends rebuild attempts while the secure desktop is up. The holder is
// filled after both objects are constructed and before Start() - nothing
// reads it earlier.
struct ResetWiring {
  xnc::DesktopWatch* watch = nullptr;
  xnc::CaptureReset* reset = nullptr;
};

xnc::ResetDesktop ResetGateThunk(void* ctx) {
  const auto* w = static_cast<const ResetWiring*>(ctx);
  if (w == nullptr || w->watch == nullptr) return xnc::ResetDesktop::kDefault;
  return w->watch->Snapshot().state == xnc::DesktopState::kDefault
             ? xnc::ResetDesktop::kDefault
             : xnc::ResetDesktop::kNonDefault;
}

uint64_t ResetClockThunk() { return GetTickCount64(); }

// Watch opts: every transition INTO a non-default desktop requests a
// unified reset (any entry into the secure desktop revokes the duplication
// - T1 evidence); the reset's debounce merges this with the ACCESS_LOST
// error that follows within ~100ms.
xnc::DesktopWatch::Opts WatchOptsFor(ResetWiring* wiring) {
  xnc::DesktopWatch::Opts o;
  o.on_transition = [wiring](const xnc::DesktopTransition& t) {
    // Any entry into the secure desktop (TRANSITION/WINLOGON) revokes the
    // duplication (T1 evidence) - suspend via the unified reset.
    if (wiring != nullptr && wiring->reset != nullptr &&
        t.to != xnc::DesktopState::kDefault)
      wiring->reset->RequestReset(xnc::kResetReasonDesktopSwitch);
  };
  return o;
}

xnc::CaptureReset::Opts ResetOptsFor(ResetWiring* wiring) {
  xnc::CaptureReset::Opts o;
  o.clock_ms = &ResetClockThunk;
  o.desktop_fn = &ResetGateThunk;
  o.desktop_ctx = wiring;
  return o;
}

void Usage(FILE* out) {
  std::fwprintf(out,
      L"xnc-desktop - XNC desktop capture process (M1)\n"
      L"usage: xnc-desktop.exe --console-diag [--duration <sec>] [--out <file.h264>] [--fps <n>]\n"
      L"                    [--pipe <name> --secret <hex>]\n"
      L"       xnc-desktop.exe --console-rt (--secret-stdin | --secret <hex>)\n"
      L"                    [--pipe <name>] [--max-subs <n>] [--fps <n>]\n"
      L"       xnc-desktop.exe --selftest | --help\n"
      L"  --console-diag  diagnostic capture loop in the console session\n"
      L"  --duration      seconds to run (default 10, must be > 0)\n"
      L"  --out           output H.264 path (required with --console-diag);\n"
      L"                  a stats.json sidecar is written next to it\n"
      L"  --fps           target fps (default 30, must be > 0)\n"
      L"  --console-rt    real-time pipe server mode: subscribers ATTACH over\n"
      L"                  the M0 handshake and receive FRAME events (until Ctrl+C)\n"
      L"  --secret-stdin  read the pipe secret from stdin: exactly 64 hex chars\n"
      L"                  (32 bytes), optional trailing newline. This is the\n"
      L"                  SERVICE path: xnc-core hands the secret over an\n"
      L"                  inherited stdin pipe so it never appears in argv\n"
      L"  --secret        pipe secret as hex (interactive diagnostics only:\n"
      L"                  argv is visible in the process list; the service\n"
      L"                  path uses --secret-stdin; e.g. 0011ff)\n"
      L"  --pipe          pipe name (default \\\\.\\pipe\\xnc-desktop-rt)\n"
      L"  --max-subs      max subscribers (default 4, must be 1..4)\n"
      L"  --selftest      arg parsing + FrameBlob/encoder/pipeline/rt selftest\n"
      L"  --help          this usage text\n"
      L"console-diag runs DXGI capture -> FrameCache -> MF H.264 encode and\n"
      L"writes shaped Annex-B AUs to --out (SPS/PPS before IDR, 4-byte start\n"
      L"codes, no AUD) plus per-second counters and a stats.json sidecar;\n"
      L"with --pipe/--secret the same run also serves the rt subscribers\n");
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

// --secret: even-length hex string, 1..64 bytes. No 0x prefix, no spaces.
bool ParseHexSecret(const wchar_t* s, std::vector<uint8_t>* out) {
  if (out == nullptr || s == nullptr) return false;
  out->clear();
  size_t n = 0;
  while (s[n] != L'\0') ++n;
  if (n == 0 || (n % 2) != 0 || n > 128) return false;
  out->reserve(n / 2);
  for (size_t i = 0; i < n; i += 2) {
    auto nib = [](wchar_t c) -> int {
      if (c >= L'0' && c <= L'9') return c - L'0';
      if (c >= L'a' && c <= L'f') return c - L'a' + 10;
      if (c >= L'A' && c <= L'F') return c - L'A' + 10;
      return -1;
    };
    const int hi = nib(s[i]), lo = nib(s[i + 1]);
    if (hi < 0 || lo < 0) return false;
    out->push_back(static_cast<uint8_t>((hi << 4) | lo));
  }
  return !out->empty();
}

// --secret-stdin line validator. The stdin format is FIXED (see diag.h):
// exactly 64 hex chars = the canonical 32-byte pipe secret, with an
// optional trailing "\n" or "\r\n" (or no newline at all). Everything else
// is a usage error. Pure - covered by the selftest byte-for-byte.
bool ParseSecretStdinLine(const char* line, std::vector<uint8_t>* out) {
  if (out == nullptr || line == nullptr) return false;
  size_t n = 0;
  while (line[n] != '\0') ++n;
  // Strip one optional trailing newline pair: "\r\n", "\n" or "\r".
  if (n >= 2 && line[n - 2] == '\r' && line[n - 1] == '\n') n -= 2;
  else if (n >= 1 && (line[n - 1] == '\n' || line[n - 1] == '\r')) n -= 1;
  if (n != 64) return false;  // 64 hex chars = 32 bytes, no other length
  std::vector<uint8_t> bytes;
  bytes.reserve(32);
  for (size_t i = 0; i < n; i += 2) {  // pairs: n/2 = 32 bytes
    auto nib = [](char c) -> int {
      if (c >= '0' && c <= '9') return c - '0';
      if (c >= 'a' && c <= 'f') return c - 'a' + 10;
      if (c >= 'A' && c <= 'F') return c - 'A' + 10;
      return -1;
    };
    const int hi = nib(line[i]), lo = nib(line[i + 1]);
    if (hi < 0 || lo < 0) return false;
    bytes.push_back(static_cast<uint8_t>((hi << 4) | lo));
  }
  *out = std::move(bytes);
  return true;
}

// Fills *out with the secret read from stdin when --secret-stdin was given
// (the parser has already verified a mode wants it). Reads the FIRST line
// off the real stdin handle - a pipe (core service path) or an interactive
// console - then delegates to the pure validator. Error text never echoes
// the input.
bool ReadSecretFromStdin(std::vector<uint8_t>* out, std::wstring* err) {
  auto fail = [err](const wchar_t* msg) {
    if (err) *err = msg;
    return false;
  };
  HANDLE in = GetStdHandle(STD_INPUT_HANDLE);
  if (in == nullptr || in == INVALID_HANDLE_VALUE)
    return fail(L"--secret-stdin: no usable stdin handle");
  std::string line;
  for (;;) {
    char buf[128];
    DWORD got = 0;
    if (!ReadFile(in, buf, sizeof(buf), &got, nullptr)) {
      const DWORD e = GetLastError();
      if (e == ERROR_BROKEN_PIPE) break;  // write end closed: EOF
      return fail(L"--secret-stdin: reading stdin failed");
    }
    if (got == 0) break;  // EOF
    line.append(buf, buf + got);
    // Stop at the first line's newline (console line input delivers it;
    // a pipe writer that omits it closes the pipe -> EOF above).
    if (line.find('\n') != std::string::npos) break;
    if (line.size() > 256) return fail(L"--secret-stdin: input line too long");
  }
  if (!ParseSecretStdinLine(line.c_str(), out))
    return fail(L"--secret-stdin: expected exactly 64 hex chars (32 bytes), "
                L"optional trailing newline");
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
    } else if (std::wcscmp(a, L"--console-rt") == 0) {
      opt->console_rt = true;
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
    } else if (std::wcscmp(a, L"--pipe") == 0) {
      const wchar_t* v = value_of(L"--pipe");
      if (!v) return false;
      opt->pipe_name = v;
    } else if (std::wcscmp(a, L"--secret") == 0) {
      const wchar_t* v = value_of(L"--secret");
      if (!v) return false;
      if (!ParseHexSecret(v, &opt->secret))
        return fail(L"--secret must be a non-empty even-length hex string (1..64 bytes)");
    } else if (std::wcscmp(a, L"--secret-stdin") == 0) {
      opt->secret_stdin = true;  // value-less flag; the read happens in wmain
    } else if (std::wcscmp(a, L"--max-subs") == 0) {
      const wchar_t* v = value_of(L"--max-subs");
      if (!v) return false;
      if (!ParseU32(v, &opt->max_subs) || opt->max_subs == 0 ||
          opt->max_subs > kMaxSubsHardCap)
        return fail(L"--max-subs must be an integer in 1..4");
    } else {
      return fail(std::wstring(L"unknown argument: ") + a);
    }
  }
  const int modes = (opt->console_diag ? 1 : 0) + (opt->console_rt ? 1 : 0) +
                    (opt->selftest ? 1 : 0) + (opt->help ? 1 : 0);
  if (modes > 1)
    return fail(L"--console-diag, --console-rt, --selftest and --help are mutually exclusive");
  if (modes == 0)
    return fail(L"one of --console-diag, --console-rt, --selftest, --help is required");
  if (opt->console_diag && opt->out_path.empty())
    return fail(L"--console-diag requires --out <file.h264>");
  // The secret arrives from exactly ONE channel: --secret-stdin (service
  // path, spec 1.5 - never argv) or --secret <hex> (interactive diag).
  if (!opt->secret.empty() && opt->secret_stdin)
    return fail(L"--secret and --secret-stdin are mutually exclusive");
  if (opt->console_rt && opt->secret.empty() && !opt->secret_stdin)
    return fail(L"--console-rt requires --secret-stdin (service path) or "
                L"--secret <hex> (interactive diagnostics)");
  // Optional rt server riding on a diag run (XIAOXIN validation path):
  // --pipe/--secret with --console-diag turns it on; both-or-none.
  const bool diag_rt_extra = opt->console_diag &&
                             (!opt->secret.empty() || opt->secret_stdin ||
                              !opt->pipe_name.empty());
  if (diag_rt_extra && opt->secret.empty() && !opt->secret_stdin)
    return fail(L"--pipe with --console-diag also requires --secret <hex> "
                L"(or --secret-stdin)");
  if (opt->console_rt || diag_rt_extra) {
    if (opt->pipe_name.empty()) opt->pipe_name = kDefaultRtPipe;
  }
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

  // M2-Slice1 Task 1/2: DesktopWatch (OpenInputDesktop 500ms poll,
  // DEFAULT/TRANSITION/WINLOGON machine) + the unified CaptureReset it
  // drives. Watch transitions request resets; ACCESS_LOST-class acquire
  // errors and frame-size changes route through the same coordinator
  // (suspend -> wait-desktop -> rebuild -> new base -> ForceIDR; STATE
  // recovering/capture_rebuilt; DISPLAY_CHANGED on dimension change).
  ResetWiring wiring;
  xnc::DesktopWatch watch(WatchOptsFor(&wiring));
  xnc::CaptureReset capture_reset(ResetOptsFor(&wiring));
  wiring.watch = &watch;
  wiring.reset = &capture_reset;
  watch.Start();
  popt.desktop_name_fn = &DesktopNameThunk;
  popt.desktop_name_ctx = &watch;
  popt.reset = &capture_reset;

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

  // Optional rt server on the same run (--console-diag --pipe --secret):
  // one pipeline, two sinks (file dump + subscriber fan-out). M1-Slice3:
  // the same server also carries 0x0108 input (SendInput) and 0x0109 cursor
  // events; both managers are owned here and outlive the server (their
  // dimensions come from the live capture, hence the late construction).
  xnc::RtServer rt;
  std::unique_ptr<xnc::InputManager> input;
  std::unique_ptr<xnc::CursorManager> cursor;
  const bool rt_extra = !opt.secret.empty() && !opt.pipe_name.empty();
  if (rt_extra) {
    xnc::InputManager::Opts iopt;
    iopt.hello_w = capture->Width();  // MOVE coords are HOST_HELLO-space
    iopt.hello_h = capture->Height();
    input = std::make_unique<xnc::InputManager>(iopt);
    xnc::CursorManager::Opts copt;
    copt.hello_w = capture->Width();
    copt.hello_h = capture->Height();
    cursor = std::make_unique<xnc::CursorManager>(copt);
    xnc::RtServer::Opts ro;
    ro.pipe_name = opt.pipe_name;
    ro.secret = opt.secret.data();
    ro.secret_len = opt.secret.size();
    ro.max_subs = opt.max_subs;
    ro.fps = opt.fps;
    ro.bitrate_bps = kDiagBitrateBps;
    ro.input = input.get();
    ro.cursor = cursor.get();
    input->StartJanitor();
    if (!rt.Start(ro, capture->Width(), capture->Height())) {
      write_stats(fail_result("rt_server_start_failed"));
      std::fclose(out);
      return 1;
    }
  }

  const xnc::PipelineResult res = rt_extra
      ? xnc::Pipeline::Run(*capture, encoder, out, rt, popt)
      : xnc::Pipeline::Run(*capture, encoder, out, popt);
  if (rt_extra) {
    rt.Shutdown();  // drain: stops the cursor poller, ReleaseAll on inputs
    input->StopJanitor();
  }
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

// Real-time mode (M1-Slice2 Task 2): same capture/encode pipeline, AUs
// fanned out to pipe subscribers until Ctrl+C. M1-Slice3 adds the input
// path (0x0108 -> SendInput, stuck-key janitor) and the cursor channel
// (GetCursorInfo -> 0x0109) on the same server. Capture/encoder init
// failures mirror the diag markers (no stats.json in this mode).
int RunConsoleRt(const xnc::DiagOptions& opt) {
  XNC_LOG_INFO("console_rt_boot pipe=%ls max_subs=%u fps=%u bitrate=%u",
               opt.pipe_name.c_str(), opt.max_subs, opt.fps, kDiagBitrateBps);
  std::string cap_err;
  std::unique_ptr<xnc::ICapture> capture = xnc::TryCreateDxgiCapture(&cap_err);
  if (!capture) {
    if (xnc::DxgiErrIsDesktopAccessDenied(cap_err)) {
      XNC_LOG_ERROR("dxgi_access_denied_session0 err=\"%s\"", cap_err.c_str());
    } else {
      XNC_LOG_ERROR("capture_init_failed err=\"%s\"", cap_err.c_str());
    }
    return 1;
  }
  XNC_LOG_INFO("capture_init w=%u h=%u", capture->Width(), capture->Height());

  xnc::MfSoftEncoder encoder;
  std::string enc_err;
  if (!encoder.Init(capture->Width(), capture->Height(), opt.fps, kDiagBitrateBps,
                    &enc_err)) {
    XNC_LOG_ERROR("encoder_init_failed err=\"%s\"", enc_err.c_str());
    return 1;
  }

  xnc::InputManager::Opts iopt;
  iopt.hello_w = capture->Width();  // MOVE coords are HOST_HELLO-space px
  iopt.hello_h = capture->Height();
  xnc::InputManager input(iopt);
  input.StartJanitor();
  xnc::CursorManager::Opts copt;
  copt.hello_w = capture->Width();
  copt.hello_h = capture->Height();
  xnc::CursorManager cursor(copt);

  xnc::RtServer::Opts ro;
  ro.pipe_name = opt.pipe_name;
  ro.secret = opt.secret.data();
  ro.secret_len = opt.secret.size();
  ro.max_subs = opt.max_subs;
  ro.fps = opt.fps;
  ro.bitrate_bps = kDiagBitrateBps;
  ro.input = &input;
  ro.cursor = &cursor;
  // M2-Slice1 Task 1/2: desktop watch + unified capture reset (same as the
  // diag mode wiring; RtServer::Serve forwards both into PipelineOpts).
  ResetWiring wiring;
  xnc::DesktopWatch watch(WatchOptsFor(&wiring));
  xnc::CaptureReset capture_reset(ResetOptsFor(&wiring));
  wiring.watch = &watch;
  wiring.reset = &capture_reset;
  watch.Start();
  ro.desktop_name_fn = &DesktopNameThunk;
  ro.desktop_name_ctx = &watch;
  ro.reset = &capture_reset;
  xnc::RtServer server;
  const int rc = server.Serve(*capture, encoder, ro);
  watch.Stop();
  input.StopJanitor();  // ReleaseAll already ran in RtServer::Shutdown
  return rc;
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
  if (opt.secret_stdin) {
    // Service path: the secret enters via stdin, never argv (spec 1.5).
    // The parser guarantees --secret-stdin only survives when a mode that
    // needs the secret was selected. A malformed stdin line is a usage
    // error of the spawning side -> exit 2.
    if (!xnc::ReadSecretFromStdin(&opt.secret, &err)) {
      std::fwprintf(stderr, L"xnc-desktop: %s\n", err.c_str());
      return 2;
    }
  }
  if (opt.console_rt) return RunConsoleRt(opt);
  return RunConsoleDiag(opt);
}
