// diag.h - console-diag CLI options + parser, shared between xnc-desktop.cpp
// (wmain) and desktop_selftest.cpp (pure-logic arg assertions). Flag names
// are the Task 6 spawn contract - do not rename.
#ifndef XNC_NATIVE_DESKTOP_DIAG_H_
#define XNC_NATIVE_DESKTOP_DIAG_H_

#include <cstdint>
#include <string>

namespace xnc {

struct DiagOptions {
  bool console_diag = false;  // --console-diag
  bool selftest = false;      // --selftest
  bool help = false;          // --help
  uint32_t duration_s = 10;   // --duration (seconds, must be > 0)
  uint32_t fps = 30;          // --fps (target fps, must be > 0)
  std::wstring out_path;      // --out (required with --console-diag)
};

// Parses argv[1..] (argv[0] is skipped). Returns true on success; on failure
// returns false and sets *err to a one-line reason (caller prints it and
// exits 2). Modes --console-diag / --selftest / --help are mutually
// exclusive and one is required; --console-diag requires a non-empty --out;
// --duration/--fps must be positive integers.
bool ParseDiagArgs(int argc, wchar_t** argv, DiagOptions* opt, std::wstring* err);

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_DIAG_H_
