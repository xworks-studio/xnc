// diag.h - CLI options + parser for xnc-desktop, shared between
// xnc-desktop.cpp (wmain) and desktop_selftest.cpp (pure-logic arg
// assertions). Flag names are the Task 6 spawn contract - do not rename.
// M1-Slice2 Task 2 adds the real-time mode flags (--console-rt/--pipe/
// --secret/--max-subs); --pipe/--secret may also ride along with
// --console-diag to serve the rt pipe off the same diag pipeline run.
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

struct DiagOptions {
  bool console_diag = false;  // --console-diag
  bool console_rt = false;    // --console-rt
  bool selftest = false;      // --selftest
  bool help = false;          // --help
  uint32_t duration_s = 10;   // --duration (seconds, must be > 0)
  uint32_t fps = 30;          // --fps (target fps, must be > 0)
  uint32_t max_subs = 4;      // --max-subs (1..4, rt mode)
  std::wstring out_path;      // --out (required with --console-diag)
  std::wstring pipe_name;     // --pipe (default kDefaultRtPipe in rt mode)
  std::vector<uint8_t> secret;  // --secret <hex> (required with --console-rt)
};

// Parses argv[1..] (argv[0] is skipped). Returns true on success; on failure
// returns false and sets *err to a one-line reason (caller prints it and
// exits 2). Modes --console-diag / --console-rt / --selftest / --help are
// mutually exclusive and one is required; --console-diag requires a
// non-empty --out; --console-rt requires a valid --secret (hex, >=1 byte);
// --duration/--fps must be positive integers; --max-subs must be 1..4.
bool ParseDiagArgs(int argc, wchar_t** argv, DiagOptions* opt, std::wstring* err);

// Hex string -> bytes for --secret. Accepts even-length strings of hex
// digits (1..64 bytes); rejects empty/odd/garbage. Pure (selftest-covered).
bool ParseHexSecret(const wchar_t* s, std::vector<uint8_t>* out);

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_DIAG_H_
