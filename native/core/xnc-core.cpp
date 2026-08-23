// xnc-core.cpp - entry point + CLI. Modes:
//   --selftest                       run the native/core selftest
//   --console --smoke-secret <hex>   foreground pipe server (diagnostic)
//       [--pipe-name \.\pipe\<name>] default \\.\pipe\xnc-core; secret is
//       hex (nominal 32B = 64 chars; the smoke vector is 16B/32 chars)
//   --console --diag-spawn [xnc-desktop.exe] <args...>   session bridge
//       (Task 6): mint the console-session SYSTEM token, spawn the
//       whitelisted exe (xnc-desktop.exe next to xnc-core.exe) on
//       winsta0\default, wait, propagate its exit code. Everything after
//       --diag-spawn goes to the child verbatim (an explicit whitelisted
//       exe name as the first token is consumed as the exe); the pipe
//       server is NOT started in this mode.
// Service mode (SCM) arrives in M2; in service mode the pipe secret comes
// from the spawn channel in M1, so --smoke-secret outside --console is a
// usage error. These flag names are the Task 6 spawn contract - do not
// rename.
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>

#include <cstdio>
#include <cstring>
#include <string>

#include "../common/log.h"
#include "pipe_server.h"
#include "spawn.h"
#include "token_manager.h"

int SelftestMain();  // selftest.cpp

namespace {

const wchar_t kDefaultPipe[] = L"\\\\.\\pipe\\xnc-core";

void Usage(FILE* out) {
  std::fwprintf(out,
      L"xnc-core - XNC node core (M0)\n"
      L"usage: xnc-core.exe --console --smoke-secret <hex> [--pipe-name <name>]\n"
      L"       xnc-core.exe --console --diag-spawn <exe> <args...>\n"
      L"       xnc-core.exe --selftest\n"
      L"  --console       foreground: serve the XNIP pipe (DACL SYSTEM+Admins)\n"
      L"  --pipe-name     pipe name (default %s)\n"
      L"  --smoke-secret  hex pipe secret (nominal 32B = 64 hex chars);\n"
      L"                  diagnostic --console mode only (service mode reads\n"
      L"                  the spawn channel in M1)\n"
      L"  --diag-spawn    session bridge: spawn xnc-desktop.exe (whitelist;\n"
      L"                  resolved next to xnc-core.exe; an explicit\n"
      L"                  'xnc-desktop.exe' first arg is also accepted) in\n"
      L"                  the active console session via TokenManager, wait\n"
      L"                  for it and propagate its exit code; remaining args\n"
      L"                  go to the child verbatim; no pipe server here\n"
      L"  --selftest      frame + handshake selftest\n"
      L"service mode (SCM) is not implemented yet - arrives in M2\n",
      kDefaultPipe);
}

int HexVal(wchar_t c) {
  if (c >= L'0' && c <= L'9') return c - L'0';
  if (c >= L'a' && c <= L'f') return c - L'a' + 10;
  if (c >= L'A' && c <= L'F') return c - L'A' + 10;
  return -1;
}

// Even-count hex string of 2..2*kMaxPipeSecretBytes chars -> bytes. The
// nominal secret is 32B (64 chars); the smoke vector "test-pipe-secret"
// (32 chars, 16B) is accepted too - HMAC takes any key length.
bool ParseSecretHex(const wchar_t* hex, std::string& out) {
  size_t n = std::wcslen(hex);
  if (n < 2 || n > 2 * xnc::kMaxPipeSecretBytes || n % 2 != 0) return false;
  out.resize(n / 2, 0);
  for (size_t i = 0; i < n / 2; i++) {
    int hi = HexVal(hex[2 * i]), lo = HexVal(hex[2 * i + 1]);
    if (hi < 0 || lo < 0) return false;
    out[i] = static_cast<char>((hi << 4) | lo);
  }
  return true;
}

// Session-bridge diagnostic path (Task 6): mint the console-session SYSTEM
// token (TokenManager, spec 4.2), spawn the whitelisted exe on
// winsta0\default (SpawnInSession), wait, propagate the child's exit code.
// Internal failures (no console session, token or spawn errors) exit 1;
// the child's own exit code (0/1/2 per xnc-desktop contract) passes
// through unchanged. No pipe server is started here - the point is one
// remote exec command proving the whole capture spine end to end.
int RunDiagSpawn(const wchar_t* child_exe, int argc, wchar_t** argv, int from) {
  const DWORD session = WTSGetActiveConsoleSessionId();
  if (session == 0xFFFFFFFF) {
    XNC_LOG_ERROR("diag_spawn: no active console session (headless node?)");
    return 1;
  }
  XNC_LOG_INFO("diag_spawn start exe=%ls session=%lu", child_exe, session);

  HANDLE token = nullptr;
  std::string err;
  if (!xnc::TokenManager::SessionSystemToken(session, &token, &err)) {
    XNC_LOG_ERROR("diag_spawn: session token failed err=\"%s\"", err.c_str());
    return 1;
  }

  std::wstring cmd;
  std::string cmd_err;
  if (!xnc::BuildChildCommandLine(child_exe, argc, argv, from, &cmd,
                                  &cmd_err)) {
    CloseHandle(token);
    std::fwprintf(stderr,
        L"xnc-core: --diag-spawn rejected a child argument (%hs)\n",
        cmd_err.c_str());
    return 2;
  }
  XNC_LOG_INFO("diag_spawn cmdline=\"%ls\" (no secrets in argv per spec 4.2)",
               cmd.c_str());

  DWORD pid = 0;
  HANDLE child = nullptr;
  if (!xnc::SpawnInSession(token, child_exe, cmd.c_str(), &pid, &child, &err)) {
    CloseHandle(token);
    XNC_LOG_ERROR("diag_spawn: spawn failed err=\"%s\"", err.c_str());
    return 1;
  }
  CloseHandle(token);
  XNC_LOG_INFO("diag_spawn child pid=%lu waiting", pid);

  WaitForSingleObject(child, INFINITE);
  DWORD code = 0;
  if (!GetExitCodeProcess(child, &code)) code = 1;
  CloseHandle(child);
  XNC_LOG_INFO("diag_spawn child pid=%lu exit=%lu", pid, code);
  return static_cast<int>(code);
}

}  // namespace

int wmain(int argc, wchar_t** argv) {
  bool console = false, selftest = false, diag_spawn = false;
  const wchar_t* pipe_name = kDefaultPipe;
  const wchar_t* secret_hex = nullptr;
  const wchar_t* child_exe = nullptr;
  int child_args_from = 0;
  for (int i = 1; i < argc; i++) {
    if (std::wcscmp(argv[i], L"--console") == 0) {
      console = true;
    } else if (std::wcscmp(argv[i], L"--selftest") == 0) {
      selftest = true;
    } else if (std::wcscmp(argv[i], L"--pipe-name") == 0 && i + 1 < argc) {
      pipe_name = argv[++i];
    } else if (std::wcscmp(argv[i], L"--smoke-secret") == 0 && i + 1 < argc) {
      secret_hex = argv[++i];
    } else if (std::wcscmp(argv[i], L"--diag-spawn") == 0) {
      if (i + 1 >= argc) {  // at least the child's first arg must follow
        std::fwprintf(stderr,
            L"xnc-core: --diag-spawn requires <args...> to forward to the "
            L"child (verbatim)\n");
        Usage(stderr);
        return 2;
      }
      diag_spawn = true;
      // Two accepted forms (both whitelist-enforced at spawn time):
      //   --diag-spawn xnc-desktop.exe <args...>   explicit (plan form)
      //   --diag-spawn <args...>                   exe implied
      if (xnc::SpawnExeArgAllowed(argv[i + 1])) {
        child_exe = argv[++i];
      } else {
        child_exe = L"xnc-desktop.exe";
      }
      child_args_from = i + 1;
      break;  // rest of argv belongs to the child, verbatim
    } else {
      std::fwprintf(stderr, L"xnc-core: unknown or incomplete argument: %s\n", argv[i]);
      Usage(stderr);
      return 2;
    }
  }

  if (selftest) return SelftestMain();
  if (diag_spawn) {
    if (!console) {
      std::fwprintf(stderr, L"xnc-core: --diag-spawn is only valid with --console\n");
      Usage(stderr);
      return 2;
    }
    if (secret_hex) {
      std::fwprintf(stderr,
          L"xnc-core: --diag-spawn does not serve the pipe; "
          L"--smoke-secret is not used in this mode\n");
      return 2;
    }
    return RunDiagSpawn(child_exe, argc, argv, child_args_from);
  }
  if (secret_hex && !console) {
    std::fwprintf(stderr,
        L"xnc-core: --smoke-secret is only valid with --console "
        L"(service mode reads the spawn channel in M1)\n");
    return 2;
  }
  if (!console) {
    Usage(stdout);  // no-arg run: service mode arrives in M2 (SCM)
    return 2;
  }
  if (!secret_hex) {
    std::fwprintf(stderr, L"xnc-core: --console requires --smoke-secret <64-hex> in M0\n");
    Usage(stderr);
    return 2;
  }
  std::string secret;
  if (!ParseSecretHex(secret_hex, secret)) {
    std::fwprintf(stderr,
        L"xnc-core: --smoke-secret must be an even-count hex string "
        L"(2..%d chars; 64 chars = nominal 32-byte secret)\n",
        2 * (int)xnc::kMaxPipeSecretBytes);
    return 2;
  }
  return xnc::RunPipeServer(pipe_name,
                            reinterpret_cast<const uint8_t*>(secret.data()),
                            secret.size());
}
