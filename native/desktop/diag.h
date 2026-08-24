// diag.h - CLI options + parser for xnc-desktop, shared between
// xnc-desktop.cpp (wmain) and desktop_selftest.cpp (pure-logic arg
// assertions). Flag names are the Task 6 spawn contract - do not rename.
// M1-Slice2 Task 2 adds the real-time mode flags (--console-rt/--pipe/
// --secret/--max-subs); --pipe/--secret may also ride along with
// --console-diag to serve the rt pipe off the same diag pipeline run.
// Task 3 fix wave adds --secret-stdin: the SERVICE path secret channel
// (spec 1.5 red line - the secret must never appear in argv / the process
// list). --secret stays for interactive diagnostics only.
#ifndef XNC_NATIVE_DESKTOP_DIAG_H_
#define XNC_NATIVE_DESKTOP_DIAG_H_

#include <cstdint>
#include <string>
#include <vector>

namespace xnc {

// Default rt pipe name (plan Task 2).
inline constexpr wchar_t kDefaultRtPipe[] = L"\\\\.\\pipe\\xnc-desktop-rt";
// Hard subscriber capacity (plan: max 4).
inline constexpr uint32_t kMaxSubsHardCap = 4;

// Capture backend selector (--backend; M2-Slice1 Task 3). Diagnostic-only:
// DXGI is the production rung; GDI is the manual/degraded path (spec §7.6).
enum class DiagBackend : uint8_t { kDxgi = 0, kGdi };

// Encoder selector (--encoder; M2-Slice2 Task 3 supervision hook, extended
// by the hw-encode task). "hardware" is the default ladder (hardware MFT
// first with software fallback); "software" pins the CMSH264EncoderMFT
// rung for diagnostics (a broken GPU must never block the stream). The core
// supervisor's crash-loop degraded restart passes
// "--backend gdi --encoder software" (spec 15.2).
enum class DiagEncoder : uint8_t { kHardware = 0, kSoftware = 1 };

// Encode bitrate by encode dims (feat/arch-clean): the H.264 target follows
// the SCALED encode width (--max-w applied first; see rt/diag wiring), so
// high-res feeds get enough headroom (no blur/drop) and low-res feeds don't
// waste bandwidth at the fixed 2.3M. Keyed on width; h is reserved for a
// future aspect-aware tier. Tiers: <=1280 -> 1.5M, <=1920 -> 2.3M,
// <=2560 -> 3.2M, larger -> 4.2M. Pure + inline (selftest-covered).
inline uint32_t BitrateForDims(uint32_t w, uint32_t /*h*/) {
  if (w <= 1280) return 1500000;
  if (w <= 1920) return 2300000;
  if (w <= 2560) return 3200000;
  return 4200000;
}


struct DiagOptions {
  bool console_diag = false;  // --console-diag
  bool console_rt = false;    // --console-rt
  bool selftest = false;      // --selftest
  bool help = false;          // --help
  uint32_t duration_s = 10;   // --duration (seconds, must be > 0)
  uint32_t fps = 30;          // --fps (target fps, must be > 0)
  uint32_t max_subs = 4;      // --max-subs (1..4, rt mode)
  DiagBackend backend = DiagBackend::kDxgi;  // --backend dxgi|gdi (default dxgi)
  DiagEncoder encoder = DiagEncoder::kHardware;  // --encoder hardware|software
  std::wstring out_path;      // --out (required with --console-diag)
  std::wstring pipe_name;     // --pipe (default kDefaultRtPipe in rt mode)
  std::vector<uint8_t> secret;  // --secret <hex> OR --secret-stdin (filled by
                                // the caller from stdin for the latter)
  bool secret_stdin = false;    // --secret-stdin (read the secret from stdin)
  bool jpeg_single = false;     // --jpeg-single <out.jpg> (M2-Slice3 Task 3):
                                // one-shot snapshot mode - acquire ONE frame,
                                // encode JPEG (WIC, quality 0.85), write file,
                                // exit 0; errors exit 1
  std::wstring jpeg_path;       // --jpeg-single output path (required)
  std::wstring log_file;        // --log-file <path>: XNC_LOG also appends here
  uint32_t max_width = 0;       // --max-w <n>: box-filter downscale clamp.
                                // --jpeg-single: snapshot max width; rt/diag
                                // (feat/rt-scale): every captured frame is
                                // downscaled before encode, encoder Init +
                                // HOST_HELLO carry the scaled dims (0 = none)
};

// Parses argv[1..] (argv[0] is skipped). Returns true on success; on failure
// returns false and sets *err to a one-line reason (caller prints it and
// exits 2). Modes --console-diag / --console-rt / --selftest / --help are
// mutually exclusive and one is required; --console-diag requires a
// non-empty --out; --console-rt requires the secret from exactly one of
// --secret-stdin (service path) / --secret <hex> (interactive diag);
// --duration/--fps must be positive integers; --max-subs must be 1..4.
bool ParseDiagArgs(int argc, wchar_t** argv, DiagOptions* opt, std::wstring* err);

// Hex string -> bytes for --secret. Accepts even-length strings of hex
// digits (1..64 bytes); rejects empty/odd/garbage. Pure (selftest-covered).
bool ParseHexSecret(const wchar_t* s, std::vector<uint8_t>* out);

// --secret-stdin line validator (pure, selftest-covered). The stdin format
// is FIXED: exactly 64 hex chars = the canonical 32-byte pipe secret, plus
// an optional trailing newline ("\n" or "\r\n"; also accepted without any
// newline, e.g. when the writer closes the pipe right after the hex).
// Anything else - leading whitespace, wrong length, non-hex, a second line
// - is rejected. On success *out holds the 32 bytes.
bool ParseSecretStdinLine(const char* line, std::vector<uint8_t>* out);

// Reads the first line from the real stdin handle and applies
// ParseSecretStdinLine. Failure sets *err (never includes the input).
bool ReadSecretFromStdin(std::vector<uint8_t>* out, std::wstring* err);

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_DIAG_H_
