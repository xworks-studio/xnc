// xnc-desktop.cpp - entry point + CLI (Task 2). Modes:
//   --selftest      run the native/desktop selftest (arg parse + FrameBlob +
//                   capture-helper units, encoder contract, FrameCache state
//                   machine, the end-to-end pipeline and the rt pipe server)
//   --help          usage text, exit 0
//   --console-diag [--duration <sec>] [--out <file.h264>] [--fps <n>]
//       diagnostic capture loop in the console session: DXGI capture ->
//       FrameCache state machine -> MF H.264 encoder (hardware MFT first
//       with software fallback; --encoder software pins the software rung)
//       -> shaped Annex-B AUs into --out, plus a stats.json sidecar next to
//       it
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
#include <cstring>  // _stricmp (XNC_DESKTOP_PIPELINE_V2 parse)
#include <cwchar>
#include <memory>
#include <string>
#include <vector>

#include "../common/log.h"
#include "backend_ladder.h"  // LadderCapture (M2-Slice1 Task 3)
#include "capture.h"       // capture contract (ICapture/FrameBlob)
#include "capture_reset.h"  // CaptureReset (M2-Slice1 Task 2)
#include "cursor_manager.h"  // CursorManager (M1-Slice3)
#include "desktop_watch.h"  // DesktopWatch (M2-Slice1 Task 1/2)
#include "diag.h"
#include "dxgi_capture.h"  // DxgiErrIsDesktopAccessDenied
#include "gdi_capture.h"  // TryCreateGdiCapture (M2 T4 v2 backend)
#include "input_manager.h"  // InputManager (M1-Slice3)
#include "jpeg_wic.h"      // DownscaleBgra / WicEncodeJpeg (M2-Slice3 T3)
#include "media_pipeline_v2.h"  // MediaPipelineV2 (M2 Task 4)
#include "mf_encoder.h"    // MfSoftEncoder
#include "pipeline.h"      // Pipeline::Run + stats.json sidecar
#include "rt_pipe_server.h"  // RtServer (real-time fan-out)
#include "scaled_capture.h"  // ScaledCapture (--max-w rt/diag downscale)

int SelftestMain(bool desktop_pipeline_v2);  // desktop_selftest.cpp

namespace xnc {

// XNC_DESKTOP_PIPELINE_V2 value parser (pure, no env access; M1 Task 5
// ruling 1d extracted it from DesktopPipelineV2Enabled below so the selftest
// can pin the value matrix without mutating process env): "1"/"true"
// (case-insensitive) enable; "0"/"false", empty, garbage and null leave the
// v1 default. The wrapper still owns the env read, the 31-char bound
// (over-long values are ignored - never parsed) and the logging.
bool ParsePipelineV2Env(const char* v) {
  return v != nullptr && (_stricmp(v, "1") == 0 || _stricmp(v, "true") == 0);
}

// M2 Task 4 (ruling 1): the NEW CLI flag --desktop-pipeline-v2 selects
// MediaPipelineV2 AND the v2 wire TOGETHER (selftest/diag lever; the env
// XNC_DESKTOP_PIPELINE_V2 is the production switch). ParseDiagArgs's
// DiagOptions has no field for it, so wmain strips it from argv BEFORE
// parsing and threads the boolean through the mode entry points. Pure
// (compacts argv[1..argc) in place, NUL-terminated pointers only) so the
// selftest can pin the strip matrix. Returns how many were removed.
int StripDesktopPipelineV2Flag(int* argc, wchar_t** argv) {
  if (argc == nullptr || argv == nullptr) return 0;
  int removed = 0;
  int out = 1;  // argv[0] stays
  for (int i = 1; i < *argc; ++i) {
    if (std::wcscmp(argv[i], L"--desktop-pipeline-v2") == 0) {
      ++removed;
      continue;
    }
    argv[out++] = argv[i];
  }
  *argc = out;
  return removed;
}

}  // namespace xnc

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

// M2-Slice1 Task 3: backend-ladder opts from the CLI --backend selector and
// the diagnostic env hooks (XNC_FORCE_DXGI_HEALTH / XNC_FORCE_BACKEND, both
// documented in --help; CLI wins over env). Reads the environment ONCE at
// startup - the flags exist to force ladder transitions for live diagnosis.
xnc::LadderOpts LadderOptsFor(const xnc::DiagOptions& opt) {
  xnc::LadderOpts lo;
  // GetEnvironmentVariableA returns the REQUIRED size (incl. NUL) when the
  // buffer is too small and the buffer contents are then UNDEFINED - a value
  // >= 32 chars left buf unterminated and the parse/log below overread the
  // stack (T3 deferred fix, closed in Task 6). Only a strictly-shorter copy
  // is NUL-terminated and safe to touch; oversized values log truncated-safe.
  char buf[32];
  const DWORD health_len = GetEnvironmentVariableA("XNC_FORCE_DXGI_HEALTH", buf,
                                                   sizeof(buf));
  if (health_len > 0 && health_len < sizeof(buf)) {
    int32_t v = -1;
    if (xnc::ParseForceHealthEnv(buf, &v)) {
      lo.force_health = v;
      XNC_LOG_INFO("backend_env_override force_health=%d", v);
    } else {
      XNC_LOG_ERROR("backend_env_override invalid XNC_FORCE_DXGI_HEALTH=\"%s\" "
                    "(want 0..100; ignored)", buf);
    }
  } else if (health_len > 0) {
    XNC_LOG_ERROR("backend_env_override XNC_FORCE_DXGI_HEALTH too long "
                  "(len=%lu, max 31; ignored)", health_len);
  }
  const DWORD backend_len = GetEnvironmentVariableA("XNC_FORCE_BACKEND", buf,
                                                    sizeof(buf));
  if (backend_len > 0 && backend_len < sizeof(buf)) {
    if (xnc::ParseForceBackendEnv(buf)) {
      lo.force_gdi = true;
      XNC_LOG_INFO("backend_env_override force_backend=gdi");
    }
  }
  if (opt.backend == xnc::DiagBackend::kGdi) lo.force_gdi = true;  // CLI wins
  return lo;
}

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

// M1 Task 2: XNC_DESKTOP_PIPELINE_V2 gates the v2 media wire (extended
// HOST_HELLO + validated 0x0205 frames). Default off = v1 wire, byte-
// identical to today. Read ONCE at startup ("1"/"true" enable; the flag is
// threaded into RtServer::Opts - the host's only startup-time config path).
bool DesktopPipelineV2Enabled() {
  char buf[32];
  const DWORD len =
      GetEnvironmentVariableA("XNC_DESKTOP_PIPELINE_V2", buf, sizeof(buf));
  if (len > 0 && len < sizeof(buf)) {
    const bool on = xnc::ParsePipelineV2Env(buf);
    XNC_LOG_INFO("desktop_pipeline_v2 env=\"%s\" -> %s", buf, on ? "on" : "off");
    return on;
  }
  if (len > 0)
    XNC_LOG_ERROR("desktop_pipeline_v2 env too long (len=%lu, max 31; ignored)", len);
  return false;
}

void Usage(FILE* out) {
  std::fwprintf(out,
      L"xnc-desktop - XNC desktop capture process (M1)\n"
      L"usage: xnc-desktop.exe --console-diag [--duration <sec>] [--out <file.h264>] [--fps <n>]\n"
      L"                    [--pipe <name> --secret <hex>] [--backend dxgi|gdi]\n"
      L"       xnc-desktop.exe --console-rt (--secret-stdin | --secret <hex>)\n"
      L"                    [--pipe <name>] [--max-subs <n>] [--fps <n>] [--backend dxgi|gdi]\n"
      L"       xnc-desktop.exe --jpeg-single <out.jpg> [--max-w <n>]\n"
      L"       xnc-desktop.exe --selftest | --help\n"
      L"  --console-diag  diagnostic capture loop in the console session\n"
      L"  --duration      seconds to run (default 10, must be > 0)\n"
      L"  --out           output H.264 path (required with --console-diag);\n"
      L"                  a stats.json sidecar is written next to it\n"
      L"  --fps           target fps (default 30, must be > 0)\n"
      L"  --backend       capture backend selector (default dxgi; diagnostic-only).\n"
      L"                  gdi = GetDC/BitBlt fallback capped at 15 fps (spec 7.6).\n"
      L"                  Both backends sit on the health-score ladder: DXGI health\n"
      L"                  < 60 switches to GDI mid-run, a 30 s DXGI probe switches\n"
      L"                  back; every swap forces one IDR and emits STATE\n"
      L"                  backend_changed\n"
      L"  --encoder       encoder selector (default hardware): hardware = hardware\n"
      L"                  MFT first (Intel QSV / NVENC / AMF) with software\n"
      L"                  fallback; software = force the software MFT (diagnostics)\n"
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
      L"  --jpeg-single   one-shot snapshot (M2-Slice3 Task 3): acquire ONE\n"
      L"                  full frame (DXGI only), box-filter downscale to\n"
      L"                  --max-w if given, encode JPEG via WIC (quality\n"
      L"                  0.85) to <out.jpg>, exit 0; errors exit 1\n"
      L"  --max-w         max width in px (0 = no clamp): with --jpeg-single\n"
      L"                  clamps the snapshot; with --console-rt/\n"
      L"                  --console-diag downscales every captured frame\n"
      L"                  before encode (rt fluency fix - encoder Init and\n"
      L"                  HOST_HELLO carry the scaled dims)\n"
      L"  --selftest      arg parsing + FrameBlob/encoder/pipeline/rt selftest\n"
      L"                  (--selftest --desktop-pipeline-v2 additionally runs\n"
      L"                  the MediaPipelineV2 scenarios)\n"
      L"  --desktop-pipeline-v2  select the M2 depth-one GPU media pipeline\n"
      L"                  AND the v2 media wire TOGETHER (default off = the\n"
      L"                  M0 pipeline; the env XNC_DESKTOP_PIPELINE_V2 is the\n"
      L"                  production switch, the CLI flag the selftest/diag\n"
      L"                  lever). Falls back to the M0 pipeline when the\n"
      L"                  surface backend cannot start\n"
      L"  --help          this usage text\n"
      L"desktop watch: always on - the secure-desktop observer runs in every\n"
      L"                mode (there is no --desktop-watch flag); it gates the\n"
      L"                unified CaptureReset on the winlogon desktop and feeds\n"
      L"                the desktop=<name> diag_pipeline field\n"
      L"diagnostic env hooks (diag-only, no production effect):\n"
      L"  XNC_FORCE_DXGI_HEALTH=N  initial DXGI health score (0..100); N<60\n"
      L"                           forces the GDI downgrade on the first frame\n"
      L"  XNC_FORCE_BACKEND=gdi    start on the GDI backend (--backend wins)\n"
      L"console-diag runs capture -> FrameCache -> MF H.264 encode and\n"
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
    } else if (std::wcscmp(a, L"--log-file") == 0) {
      const wchar_t* v = value_of(L"--log-file");
      if (!v) return false;
      opt->log_file = v;
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
    } else if (std::wcscmp(a, L"--backend") == 0) {
      const wchar_t* v = value_of(L"--backend");
      if (!v) return false;
      if (std::wcscmp(v, L"dxgi") == 0) {
        opt->backend = DiagBackend::kDxgi;
      } else if (std::wcscmp(v, L"gdi") == 0) {
        opt->backend = DiagBackend::kGdi;
      } else {
        return fail(L"--backend must be dxgi or gdi (diagnostic-only selector)");
      }
    } else if (std::wcscmp(a, L"--jpeg-single") == 0) {
      const wchar_t* v = value_of(L"--jpeg-single");
      if (!v) return false;
      opt->jpeg_single = true;
      opt->jpeg_path = v;
    } else if (std::wcscmp(a, L"--max-w") == 0) {
      const wchar_t* v = value_of(L"--max-w");
      if (!v) return false;
      if (!ParseU32(v, &opt->max_width))
        return fail(L"--max-w must be a positive integer (0 = no clamp)");
    } else if (std::wcscmp(a, L"--encoder") == 0) {
      // M2-Slice2 Task 3: crash-loop degraded-restart contract (spec 15.2)
      // passes "--backend gdi --encoder software". hw-encode task: the
      // default "hardware" runs the hardware-first ladder with software
      // fallback; "software" pins the software rung for diagnostics (a
      // broken GPU must never block the stream).
      const wchar_t* v = value_of(L"--encoder");
      if (!v) return false;
      if (std::wcscmp(v, L"hardware") == 0) {
        opt->encoder = DiagEncoder::kHardware;
      } else if (std::wcscmp(v, L"software") == 0) {
        opt->encoder = DiagEncoder::kSoftware;
        XNC_LOG_INFO("encoder selector: software (forced; hardware ladder off)");
      } else {
        return fail(L"--encoder must be hardware or software");
      }
    } else {
      return fail(std::wstring(L"unknown argument: ") + a);
    }
  }
  const int modes = (opt->console_diag ? 1 : 0) + (opt->console_rt ? 1 : 0) +
                    (opt->selftest ? 1 : 0) + (opt->help ? 1 : 0) +
                    (opt->jpeg_single ? 1 : 0);
  if (modes > 1)
    return fail(L"--console-diag, --console-rt, --jpeg-single, --selftest "
                L"and --help are mutually exclusive");
  if (modes == 0)
    return fail(L"one of --console-diag, --console-rt, --jpeg-single, "
                L"--selftest, --help is required");
  if (opt->jpeg_single && opt->jpeg_path.empty())
    return fail(L"--jpeg-single requires an output path: --jpeg-single "
                L"<out.jpg>");
  if (opt->max_width != 0 && !opt->jpeg_single && !opt->console_rt &&
      !opt->console_diag)
    return fail(L"--max-w only applies to --jpeg-single, --console-rt or "
                L"--console-diag");
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

// M2 Task 4: --console-diag on the depth-one GPU media pipeline. The direct
// ICaptureSurface backend + the single media loop replace the M0 two-thread
// pipeline; the file dump (and the optional rt server) are plain AuSinks of
// the same shaped AUs. Any startup failure returns non-zero AFTER the
// stats.json sidecar (same "every diag run leaves a sidecar" contract).
int RunConsoleDiagV2(const xnc::DiagOptions& opt, FILE* out, xnc::ICapture* cap,
                     xnc::ICaptureSurface* surf, xnc::DesktopWatch* watch,
                     xnc::CaptureReset* reset, bool force_software) {
  uint32_t w = cap->Width(), h = cap->Height();
  if (opt.max_width > 0) {  // stream dims = the VideoProcessor output space
    uint32_t sw = 0, sh = 0;
    if (xnc::GpuScaledDims(w, h, xnc::Rotate::kNone, opt.max_width, &sw, &sh)) {
      w = sw;
      h = sh;
    }
  }
  const auto fail_result = [](const char* why) {
    xnc::PipelineResult r;
    r.ok = false;
    r.err = why;
    return r;
  };
  const auto write_stats = [&opt, &w, &h](const xnc::PipelineResult& r) {
    xnc::PipelineOpts po;
    po.duration_s = opt.duration_s;
    po.fps = opt.fps;
    po.target_bitrate_bps = xnc::BitrateForDims(w, h);
    std::wstring serr;
    if (!xnc::WriteStatsJson(opt.out_path, r, po, &serr))
      XNC_LOG_ERROR("stats_json_write_failed err=\"%ls\"", serr.c_str());
  };
  if ((w % 2) != 0 || (h % 2) != 0) {  // NV12 pool needs even dims
    XNC_LOG_ERROR("console_diag_v2 odd dims w=%u h=%u (nv12 requires even)", w, h);
    write_stats(fail_result("media_v2_odd_dims"));
    std::fclose(out);
    return 1;
  }
  const uint32_t bitrate_bps = xnc::BitrateForDims(w, h);
  XNC_LOG_INFO("console_diag_v2 w=%u h=%u bitrate=%u fps=%u max_w=%u", w, h,
               bitrate_bps, opt.fps, opt.max_width);

  // Optional rt server on the same run (the M0 wiring shape: one run, two
  // sinks via the Tee).
  xnc::RtServer rt;
  std::unique_ptr<xnc::InputManager> input;
  std::unique_ptr<xnc::CursorManager> cursor;
  const bool rt_extra = !opt.secret.empty() && !opt.pipe_name.empty();
  if (rt_extra) {
    xnc::InputManager::Opts iopt;
    iopt.hello_w = w;
    iopt.hello_h = h;
    input = std::make_unique<xnc::InputManager>(iopt);
    xnc::CursorManager::Opts copt;
    copt.hello_w = w;
    copt.hello_h = h;
    cursor = std::make_unique<xnc::CursorManager>(copt);
    xnc::RtServer::Opts ro;
    ro.pipe_name = opt.pipe_name;
    ro.secret = opt.secret.data();
    ro.secret_len = opt.secret.size();
    ro.max_subs = opt.max_subs;
    ro.fps = opt.fps;
    ro.bitrate_bps = bitrate_bps;
    ro.input = input.get();
    ro.cursor = cursor.get();
    ro.pipeline_v2 = true;  // ruling 1: v2 pipeline => v2 wire
    ro.reset = reset;
    ro.displays_fn = [](void*) { return xnc::DxgiDisplaysSnapshot(); };
    ro.switch_display_fn = [](void*, uint32_t idx) { return xnc::DxgiSelectDisplay(idx); };
    input->StartJanitor();
    if (!rt.Start(ro, w, h)) {
      write_stats(fail_result("rt_server_start_failed"));
      std::fclose(out);
      return 1;
    }
  }
  xnc::MediaFileSink file_sink(out);
  xnc::TeeAuSink tee(&file_sink, &rt);
  xnc::MediaPipelineV2::Config cfg;
  cfg.cap = cap;
  cfg.surf = surf;
  cfg.sink = rt_extra ? static_cast<xnc::AuSink*>(&tee)
                      : static_cast<xnc::AuSink*>(&file_sink);
  cfg.fps = opt.fps;
  cfg.bitrate_bps = bitrate_bps;
  cfg.duration_s = opt.duration_s;
  cfg.max_width = opt.max_width;
  cfg.force_software_encoder = force_software;
  cfg.desktop_name_fn = &DesktopNameThunk;
  cfg.desktop_name_ctx = watch;
  cfg.reset = reset;

  xnc::MediaPipelineV2 pipe;
  xnc::MediaPipelineV2::Result res;
  if (pipe.Start(cfg)) {
    while (pipe.running()) Sleep(100);
    res = pipe.Stop();
  } else {
    res.ok = false;
    res.err = "media pipeline v2 start failed: " + pipe.start_error();
  }
  if (rt_extra) {
    rt.Shutdown();
    input->StopJanitor();
  }

  // stats.json sidecar (the M0 writer, field-mapped).
  xnc::PipelineResult pr;
  pr.ok = res.ok;
  pr.err = res.err;
  pr.width = res.width != 0 ? res.width : w;
  pr.height = res.height != 0 ? res.height : h;
  pr.counters.captured = res.captured;
  pr.counters.encoded = res.encoded;
  pr.counters.keyframes = res.keyframes;
  pr.counters.timeouts = res.timeouts;
  pr.counters.warmup_feeds = res.warmup_feeds;
  pr.counters.rebuilds = res.rebuilds;
  pr.aus_written = res.aus_written;
  pr.bytes_written = res.bytes_written;
  pr.resets = res.resets;
  xnc::CopyReason(pr.last_reset_reason, sizeof(pr.last_reset_reason),
                  res.last_reset_reason);
  write_stats(pr);
  XNC_LOG_INFO("console_diag_v2_stop duration=%us captured=%llu encoded=%llu keyframes=%llu timeouts=%llu warmup_feeds=%llu resets=%u aus=%llu ok=%d backend=%s",
               opt.duration_s, static_cast<unsigned long long>(res.captured),
               static_cast<unsigned long long>(res.encoded),
               static_cast<unsigned long long>(res.keyframes),
               static_cast<unsigned long long>(res.timeouts),
               static_cast<unsigned long long>(res.warmup_feeds), res.resets,
               static_cast<unsigned long long>(res.aus_written), res.ok ? 1 : 0,
               res.encoder_backend);
  std::fclose(out);
  return res.ok ? 0 : 1;
}

int RunConsoleDiag(const xnc::DiagOptions& opt, bool desktop_pipeline_v2) {
  FILE* out = nullptr;
  const errno_t open_err = _wfopen_s(&out, opt.out_path.c_str(), L"wb");
  if (open_err != 0 || !out) {
    XNC_LOG_ERROR("open out failed path=%ls errno=%d", opt.out_path.c_str(), open_err);
    return 1;
  }
  XNC_LOG_INFO("console_diag_start duration=%us fps=%u out=%ls",
               opt.duration_s, opt.fps, opt.out_path.c_str());

  xnc::PipelineOpts popt;
  popt.duration_s = opt.duration_s;
  popt.fps = opt.fps;

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

  // M2 Task 4 (ruling 1): --desktop-pipeline-v2 / XNC_DESKTOP_PIPELINE_V2
  // select the depth-one GPU media pipeline + the v2 wire TOGETHER. The V2
  // path needs an ICaptureSurface backend - the backend ladder does not
  // expose one, so a direct DXGI/GDI backend is constructed. Any failure
  // here falls back to the M0 pipeline below (rollback guarantee); with the
  // env flag set that fallback is exactly the M1 shape: v1 pipeline + v2
  // wire.
  const bool v2_env = DesktopPipelineV2Enabled();
  if (desktop_pipeline_v2 || v2_env) {
    std::string v2_err;
    std::unique_ptr<xnc::ICapture> v2_cap =
        opt.backend == xnc::DiagBackend::kGdi
            ? xnc::TryCreateGdiCapture(&v2_err)
            : xnc::TryCreateDxgiCapture(&v2_err);
    xnc::ICaptureSurface* v2_surf =
        v2_cap ? dynamic_cast<xnc::ICaptureSurface*>(v2_cap.get()) : nullptr;
    if (v2_cap && v2_surf) {
      return RunConsoleDiagV2(opt, out, v2_cap.get(), v2_surf, &watch,
                              &capture_reset,
                              opt.encoder == xnc::DiagEncoder::kSoftware);
    }
    XNC_LOG_ERROR("desktop_pipeline_v2 backend unavailable err=\"%s\" - "
                  "falling back to the M0 pipeline",
                  v2_err.c_str());
  }

  // M2-Slice1 Task 3: the backend ladder IS the capture. DXGI is the
  // default rung; GDI joins via --backend gdi / XNC_FORCE_BACKEND=gdi, a
  // health drop below 60, or a genuine DXGI create failure (NOT a
  // session-0 desktop denial - that keeps the historical exit below).
  // Mid-run swaps ride the unified CaptureReset wired above; a 30s DXGI
  // probe upgrades back once DXGI works again (STATE backend_changed).
  xnc::LadderOpts lopt = LadderOptsFor(opt);
  lopt.reset = &capture_reset;
  // gpu-readback: --max-w > 0 asks the DXGI rung for the GPU downscale+NV12
  // pipeline (scale+convert in the VideoProcessor, small NV12 readback);
  // the ScaledCapture wrapper below then passes NV12 frames through.
  lopt.gpu_max_w = opt.max_width;
  std::unique_ptr<xnc::LadderCapture> ladder(new xnc::LadderCapture(lopt));
  std::string cap_err;
  if (!ladder->Init(&cap_err)) {
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
  // feat/rt-scale (hw-encode task 2 Part B): --max-w downscales every
  // captured frame BEFORE encode; the encoder, HOST_HELLO and input/cursor
  // mapping all see the scaled dims (ScaledCapture wrapper).
  std::unique_ptr<xnc::ICapture> capture;
  if (opt.max_width > 0) {
    const uint32_t sw = ladder->Width(), sh = ladder->Height();
    uint32_t dw = 0, dh = 0;
    xnc::ScaledDims(sw, sh, opt.max_width, &dw, &dh);
    capture = std::make_unique<xnc::ScaledCapture>(std::move(ladder), opt.max_width);
    // gpu-readback: with the GPU pipeline the ladder's DXGI dims are ALREADY
    // the scaled ones (GPU VideoProcessor) - the wrapper is a pass-through;
    // with GDI/degraded DXGI it is the CPU downscale as before.
    XNC_LOG_INFO("capture_scale enabled max_w=%u src=%ux%u -> %ux%u%s",
                 opt.max_width, sw, sh, dw, dh,
                 sw == dw && sh == dh ? " (gpu path: scale+NV12 in video processor)"
                                      : "");
  } else {
    capture = std::move(ladder);
  }
  XNC_LOG_INFO("capture_init w=%u h=%u", capture->Width(), capture->Height());

  // feat/arch-clean: encode bitrate follows the (scaled) encode width
  // (capture is the ScaledCapture when --max-w is set, so Width() here is
  // already the scaled width). Set once, feeds popt/encoder/rt sink alike.
  const uint32_t bitrate_bps =
      xnc::BitrateForDims(capture->Width(), capture->Height());
  popt.target_bitrate_bps = bitrate_bps;
  XNC_LOG_INFO("console_diag_bitrate w=%u h=%u bitrate=%u",
               capture->Width(), capture->Height(), bitrate_bps);

  // Task 5: capture -> FrameCache -> MF encode (hardware-first ladder with
  // software fallback) -> shaped Annex-B AUs into --out; encoder at the
  // capture's dimensions, fps from args. --encoder software pins the
  // software rung (diagnostic lever).
  xnc::MfSoftEncoder encoder;
  encoder.SetForceSoftware(opt.encoder == xnc::DiagEncoder::kSoftware);
  std::string enc_err;
  if (!encoder.Init(capture->Width(), capture->Height(), opt.fps, bitrate_bps,
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
    ro.bitrate_bps = bitrate_bps;
    ro.input = input.get();
    ro.cursor = cursor.get();
    ro.pipeline_v2 = v2_env || desktop_pipeline_v2;  // M1 env gate (+M2 T4 CLI)
    // M2-S3 Task 5: 0x0128 switch + HOST_HELLO displays[].
    ro.displays_fn = [](void*) { return xnc::DxgiDisplaysSnapshot(); };
    ro.switch_display_fn = [](void*, uint32_t idx) { return xnc::DxgiSelectDisplay(idx); };
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
int RunConsoleRt(const xnc::DiagOptions& opt, bool desktop_pipeline_v2) {
  XNC_LOG_INFO("console_rt_boot pipe=%ls max_subs=%u fps=%u",
               opt.pipe_name.c_str(), opt.max_subs, opt.fps);
  // M2-Slice1 Task 1/2 + 3: desktop watch + unified reset + backend ladder
  // (same wiring as the diag mode; the ladder's swaps ride the reset and
  // surface STATE backend_changed to the subscribers via SetStateSink).
  ResetWiring wiring;
  xnc::DesktopWatch watch(WatchOptsFor(&wiring));
  xnc::CaptureReset capture_reset(ResetOptsFor(&wiring));
  wiring.watch = &watch;
  wiring.reset = &capture_reset;

  // M2 Task 4 (ruling 1): the depth-one GPU media pipeline + the v2 wire
  // together when the CLI flag or the env gate selects them. Same fallback
  // contract as the diag mode: a backend that cannot start (session 0, GDI
  // refused, odd dims) falls through to the M0 pipeline below - which with
  // the env flag set is exactly the M1 shape (v1 pipeline + v2 wire).
  const bool v2_env = DesktopPipelineV2Enabled();
  if (desktop_pipeline_v2 || v2_env) {
    std::string v2_err;
    std::unique_ptr<xnc::ICapture> v2_cap =
        opt.backend == xnc::DiagBackend::kGdi
            ? xnc::TryCreateGdiCapture(&v2_err)
            : xnc::TryCreateDxgiCapture(&v2_err);
    xnc::ICaptureSurface* v2_surf =
        v2_cap ? dynamic_cast<xnc::ICaptureSurface*>(v2_cap.get()) : nullptr;
    uint32_t sw = v2_cap ? v2_cap->Width() : 0, sh = v2_cap ? v2_cap->Height() : 0;
    if (opt.max_width > 0 && sw != 0) {
      uint32_t dw = 0, dh = 0;
      if (xnc::GpuScaledDims(sw, sh, xnc::Rotate::kNone, opt.max_width, &dw, &dh)) {
        sw = dw;
        sh = dh;
      }
    }
    const bool v2_dims_ok = sw != 0 && (sw % 2) == 0 && (sh % 2) == 0;
    if (v2_cap && v2_surf && v2_dims_ok) {
      const uint32_t bitrate_bps = xnc::BitrateForDims(sw, sh);
      XNC_LOG_INFO("console_rt_v2_bitrate w=%u h=%u bitrate=%u", sw, sh,
                   bitrate_bps);
      xnc::InputManager::Opts iopt;
      iopt.hello_w = sw;
      iopt.hello_h = sh;
      xnc::InputManager input(iopt);
      input.StartJanitor();
      xnc::CursorManager::Opts copt;
      copt.hello_w = sw;
      copt.hello_h = sh;
      xnc::CursorManager cursor(copt);
      xnc::RtServer::Opts ro;
      ro.pipe_name = opt.pipe_name;
      ro.secret = opt.secret.data();
      ro.secret_len = opt.secret.size();
      ro.max_subs = opt.max_subs;
      ro.fps = opt.fps;
      ro.bitrate_bps = bitrate_bps;
      ro.input = &input;
      ro.cursor = &cursor;
      ro.pipeline_v2 = true;  // ruling 1: v2 pipeline => v2 wire
      watch.Start();
      ro.desktop_name_fn = &DesktopNameThunk;
      ro.desktop_name_ctx = &watch;
      ro.reset = &capture_reset;
      ro.displays_fn = [](void*) { return xnc::DxgiDisplaysSnapshot(); };
      ro.switch_display_fn = [](void*, uint32_t idx) { return xnc::DxgiSelectDisplay(idx); };
      xnc::RtServer server;
      const int rc = server.ServeV2(*v2_cap, *v2_surf, ro);
      watch.Stop();
      input.StopJanitor();  // ReleaseAll already ran in RtServer::Shutdown
      return rc;
    }
    XNC_LOG_ERROR("desktop_pipeline_v2 backend unavailable err=\"%s\" dims=%ux%u - "
                  "falling back to the M0 pipeline",
                  v2_err.c_str(), sw, sh);
  }

  xnc::LadderOpts lopt = LadderOptsFor(opt);
  lopt.reset = &capture_reset;
  // gpu-readback: --max-w > 0 asks the DXGI rung for the GPU downscale+NV12
  // pipeline (see RunConsoleDiag wiring).
  lopt.gpu_max_w = opt.max_width;
  // M2-Slice3 Task 6 (logoff/logon self-heal gate): spawning into a session
  // whose input desktop is the logon UI (WTSConnected console, Winlogon
  // desktop) DENIES duplication 0x80070005 even as SYSTEM (M2-Slice1 T1
  // evidence). The old path constructed the ladder, Init failed, the child
  // exited before the rt pipe existed -> core PIPE_TIMEOUT killed it and
  // the agent's reattach attempts all failed (run-9 gate evidence).
  // Now: probe reachability with a THROWAWAY DxgiCapture (LadderCapture::
  // Init is single-shot - a second call assigns a joinable probe thread
  // and std::terminate-aborts, the run-10 crash); while denied, start the
  // rt pipe IMMEDIATELY with provisional display geometry and wait for the
  // Default desktop (logon completes / unlock). The ladder itself is
  // constructed and Init'd EXACTLY ONCE, after the wait.
  bool logon_wait = false;
  {
    std::string perr;
    std::unique_ptr<xnc::ICapture> reach(xnc::TryCreateDxgiCapture(&perr));
    if (reach == nullptr && xnc::DxgiErrIsDesktopAccessDenied(perr)) {
      logon_wait = true;
      XNC_LOG_INFO("console_rt reachability denied err=\"%s\" (logon-UI wait)", perr.c_str());
    }
  }

  // Provisional HOST_HELLO/input geometry: real table when it enumerates,
  // else a safe 1920x1080 placeholder (logon-UI wait only; the real dims
  // ride the post-wait display_changed + capture_rebuilt). With --max-w the
  // provisional geometry is the SCALED one so the first HOST_HELLO already
  // matches the stream's (scaled) space.
  uint32_t pw = 1920, ph = 1080;
  {
    const std::vector<xnc::DisplayInfo> ds = xnc::DxgiDisplaysSnapshot();
    for (const auto& d : ds) {
      if (d.primary || (pw == 1920 && ph == 1080 && d.w)) {
        pw = d.w; ph = d.h;
        if (d.primary) break;
      }
    }
  }
  uint32_t hpw = pw, hph = ph;
  if (opt.max_width > 0 && !xnc::ScaledDims(pw, ph, opt.max_width, &hpw, &hph))
    hpw = pw, hph = ph;
  // feat/arch-clean: encode bitrate follows the (scaled) encode width; the
  // provisional dims already carry the max_w scale (logon-UI wait included),
  // so the boot log / HOST_HELLO bitrate agree with the stream from t=0.
  const uint32_t bitrate_bps = xnc::BitrateForDims(hpw, hph);
  XNC_LOG_INFO("console_rt_bitrate w=%u h=%u bitrate=%u", hpw, hph, bitrate_bps);

  xnc::InputManager::Opts iopt;
  iopt.hello_w = hpw;  // MOVE coords are HOST_HELLO-space px
  iopt.hello_h = hph;
  xnc::InputManager input(iopt);
  input.StartJanitor();
  xnc::CursorManager::Opts copt;
  copt.hello_w = hpw;  // HOST_HELLO stream space
  copt.hello_h = hph;
  xnc::CursorManager cursor(copt);

  xnc::RtServer::Opts ro;
  ro.pipe_name = opt.pipe_name;
  ro.secret = opt.secret.data();
  ro.secret_len = opt.secret.size();
  ro.max_subs = opt.max_subs;
  ro.fps = opt.fps;
  ro.bitrate_bps = bitrate_bps;
  ro.input = &input;
  ro.cursor = &cursor;
  ro.pipeline_v2 = v2_env || desktop_pipeline_v2;  // M1 env gate (+M2 T4 CLI)
  // M2-Slice1 Task 1/2: DesktopWatch + unified CaptureReset (RtServer::Serve
  // forwards both into PipelineOpts; the wiring pair was built above so the
  // ladder could bind the same coordinator).
  watch.Start();
  ro.desktop_name_fn = &DesktopNameThunk;
  ro.desktop_name_ctx = &watch;
  ro.reset = &capture_reset;
  // M2-S3 Task 5: 0x0128 switch + HOST_HELLO displays[]. NOTE: while the
  // GDI rung is active the selection lands but binds at the next DXGI
  // rebuild (GDI cannot select an output; the ladder's DXGI probe/upgrade
  // picks the desired index up at SwapToDxgi's fresh Init).
  ro.displays_fn = [](void*) { return xnc::DxgiDisplaysSnapshot(); };
  ro.switch_display_fn = [](void*, uint32_t idx) { return xnc::DxgiSelectDisplay(idx); };
  xnc::RtServer server;
  if (logon_wait) {
    XNC_LOG_INFO("console_rt_logon_wait (pipe up with provisional dims; probing for the Default desktop)");
    if (!server.Start(ro, hpw, hph)) { input.StopJanitor(); return 1; }
    const ULONGLONG deadline = GetTickCount64() + 110000;  // agent intent window 90s + margin
    for (;;) {
      Sleep(500);
      // Probe with a THROWAWAY DxgiCapture: LadderCapture::Init is single
      // -shot (a second call assigns a joinable probe thread -> terminate).
      std::string perr;
      std::unique_ptr<xnc::ICapture> probe(xnc::TryCreateDxgiCapture(&perr));
      if (probe != nullptr) break;
      if (!xnc::DxgiErrIsDesktopAccessDenied(perr)) {
        XNC_LOG_ERROR("logon_wait probe failed err=\"%s\"", perr.c_str());
        server.Shutdown();
        input.StopJanitor();
        return 1;
      }
      if (GetTickCount64() >= deadline) {
        XNC_LOG_ERROR("logon_wait gave up after 110s (still denied)");
        server.Shutdown();
        input.StopJanitor();
        return 1;
      }
    }
  }

  // The ladder is constructed and Init'd EXACTLY ONCE (single-shot Init).
  std::unique_ptr<xnc::LadderCapture> ladder(new xnc::LadderCapture(lopt));
  ladder->SetStateSink(&server);  // backend swaps -> STATE backend_changed
  {
    std::string ierr;
    if (!ladder->Init(&ierr)) {
      XNC_LOG_ERROR("capture_init_failed err=\"%s\"", ierr.c_str());
      if (logon_wait) { server.Shutdown(); input.StopJanitor(); }
      return 1;
    }
    XNC_LOG_INFO("capture_init w=%u h=%u", ladder->Width(), ladder->Height());
  }
  // feat/rt-scale: --max-w downscales every frame before encode (the
  // encoder, HOST_HELLO and input/cursor mapping all see the scaled dims).
  std::unique_ptr<xnc::ICapture> capture;
  if (opt.max_width > 0) {
    const uint32_t sw = ladder->Width(), sh = ladder->Height();
    uint32_t dw = 0, dh = 0;
    xnc::ScaledDims(sw, sh, opt.max_width, &dw, &dh);
    capture = std::make_unique<xnc::ScaledCapture>(std::move(ladder), opt.max_width);
    XNC_LOG_INFO("capture_scale enabled max_w=%u src=%ux%u -> %ux%u%s",
                 opt.max_width, sw, sh, dw, dh,
                 sw == dw && sh == dh ? " (gpu path: scale+NV12 in video processor)"
                                      : "");
  } else {
    capture = std::move(ladder);
  }
  if (logon_wait) {
    XNC_LOG_INFO("logon_wait recovered w=%u h=%u", capture->Width(),
                 capture->Height());
    // Notify subscribers exactly like a unified reset: geometry first (the
    // rebuilt hello re-emit then carries the NEW dims), then rebuilt.
    server.OnDisplayChanged(capture->Width(), capture->Height(), "reattach");
    server.OnState("capture_rebuilt", true);
  }

  // Encoder: hardware-first ladder with software fallback; --encoder
  // software pins the software rung (diagnostic lever, spec 15.2 degraded
  // restart also lands here). Bitrate re-keyed on the REAL scaled dims
  // (logon-UI wait may have refined the provisional geometry) and pushed
  // back into the rt opts before Serve picks them up.
  const uint32_t enc_bitrate_bps =
      xnc::BitrateForDims(capture->Width(), capture->Height());
  ro.bitrate_bps = enc_bitrate_bps;
  xnc::MfSoftEncoder encoder;
  encoder.SetForceSoftware(opt.encoder == xnc::DiagEncoder::kSoftware);
  std::string enc_err;
  if (!encoder.Init(capture->Width(), capture->Height(), opt.fps, enc_bitrate_bps,
                    &enc_err)) {
    XNC_LOG_ERROR("encoder_init_failed err=\"%s\"", enc_err.c_str());
    if (logon_wait) { server.Shutdown(); input.StopJanitor(); }
    return 1;
  }
  const int rc = server.Serve(*capture, encoder, ro);
  watch.Stop();
  input.StopJanitor();  // ReleaseAll already ran in RtServer::Shutdown
  return rc;
}

// M2-Slice3 Task 3: one-shot JPEG snapshot. DXGI-only (clean error when
// desktop duplication is unavailable - no GDI fallback in this mode; the
// rt pipeline keeps its backend ladder). Acquire retry loop honors the
// base-frame semantics (err_timeout = static screen, err_rebuilt = fresh
// duplication) within a 2s budget; the first real frame is downscaled to
// --max-w (box filter) and encoded via WIC at quality 0.85.
int RunJpegSingle(const xnc::DiagOptions& opt) {
  std::string err;
  std::unique_ptr<xnc::ICapture> capture = xnc::TryCreateDxgiCapture(&err);
  if (!capture) {
    XNC_LOG_ERROR("jpeg_single: capture init failed err=\"%s\"", err.c_str());
    return 1;
  }
  xnc::FrameBlob frame;
  const ULONGLONG deadline = GetTickCount64() + 2000;
  for (;;) {
    if (capture->Acquire(frame, &err)) break;
    if (err != "err_timeout" && err != "err_rebuilt") {
      XNC_LOG_ERROR("jpeg_single: acquire failed err=\"%s\"", err.c_str());
      return 1;
    }
    if (GetTickCount64() >= deadline) {
      XNC_LOG_ERROR("jpeg_single: no frame within 2000ms");
      return 1;
    }
    Sleep(50);
  }
  uint32_t w = frame.w, h = frame.h;
  std::vector<uint8_t> scaled;
  if (!xnc::DownscaleBgra(frame.bgra.data(), frame.w, frame.h,
                          opt.max_width, &scaled, &w, &h)) {
    XNC_LOG_ERROR("jpeg_single: downscale failed w=%u h=%u max_w=%u", frame.w,
                  frame.h, opt.max_width);
    return 1;
  }
  std::vector<uint8_t> jpeg;
  if (!xnc::WicEncodeJpeg(scaled.data(), w, h, 0.85f, &jpeg, &err)) {
    XNC_LOG_ERROR("jpeg_single: encode failed err=\"%s\"", err.c_str());
    return 1;
  }
  FILE* out = nullptr;
  const errno_t open_err = _wfopen_s(&out, opt.jpeg_path.c_str(), L"wb");
  if (open_err != 0 || !out) {
    XNC_LOG_ERROR("jpeg_single: open out failed path=%ls errno=%d",
                  opt.jpeg_path.c_str(), open_err);
    return 1;
  }
  const size_t wrote = fwrite(jpeg.data(), 1, jpeg.size(), out);
  std::fclose(out);
  if (wrote != jpeg.size()) {
    XNC_LOG_ERROR("jpeg_single: short write %zu/%zu", wrote, jpeg.size());
    return 1;
  }
  XNC_LOG_INFO("jpeg_single ok src=%ux%u out=%ux%u bytes=%zu max_w=%u",
               frame.w, frame.h, w, h, jpeg.size(), opt.max_width);
  return 0;
}

}  // namespace

int wmain(int argc, wchar_t** argv) {
  xnc::SetLogProcessName("desktop");
  // M2 Task 4: strip --desktop-pipeline-v2 BEFORE parsing (ruling 1 - the
  // flag is valid in every mode and threads through as a boolean).
  const int v2_flags = xnc::StripDesktopPipelineV2Flag(&argc, argv);
  const bool desktop_pipeline_v2 = v2_flags > 0;
  if (v2_flags > 1)
    XNC_LOG_INFO("desktop_pipeline_v2 flag repeated %d times (idempotent)",
                 v2_flags);
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
  // --log-file: production service spawns have no console and inherited
  // stdio proved unreliable — log to a file the child owns (2026-08-24
  // observability incident; UAC/backend diagnostics depend on this).
  if (!opt.log_file.empty()) {
    std::string narrow(opt.log_file.begin(), opt.log_file.end());
    xnc::SetLogFile(narrow.c_str());
  }
  if (opt.selftest) return SelftestMain(desktop_pipeline_v2);
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
  if (opt.console_rt) return RunConsoleRt(opt, desktop_pipeline_v2);
  if (opt.jpeg_single) return RunJpegSingle(opt);
  return RunConsoleDiag(opt, desktop_pipeline_v2);
}
