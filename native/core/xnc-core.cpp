// xnc-core.cpp - entry point + CLI (Task 5). Modes:
//   --selftest                       run the native/core selftest
//   --console --smoke-secret <hex>   foreground pipe server (diagnostic)
//       [--pipe-name \.\pipe\<name>] default \\.\pipe\xnc-core; secret is
//       hex (nominal 32B = 64 chars; the smoke vector is 16B/32 chars)
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

int SelftestMain();  // selftest.cpp

namespace {

const wchar_t kDefaultPipe[] = L"\\\\.\\pipe\\xnc-core";

void Usage(FILE* out) {
  std::fwprintf(out,
      L"xnc-core - XNC node core (M0)\n"
      L"usage: xnc-core.exe --console --smoke-secret <hex> [--pipe-name <name>]\n"
      L"       xnc-core.exe --selftest\n"
      L"  --console       foreground: serve the XNIP pipe (DACL SYSTEM+Admins)\n"
      L"  --pipe-name     pipe name (default %s)\n"
      L"  --smoke-secret  hex pipe secret (nominal 32B = 64 hex chars);\n"
      L"                  diagnostic --console mode only (service mode reads\n"
      L"                  the spawn channel in M1)\n"
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

}  // namespace

int wmain(int argc, wchar_t** argv) {
  bool console = false, selftest = false;
  const wchar_t* pipe_name = kDefaultPipe;
  const wchar_t* secret_hex = nullptr;
  for (int i = 1; i < argc; i++) {
    if (std::wcscmp(argv[i], L"--console") == 0) {
      console = true;
    } else if (std::wcscmp(argv[i], L"--selftest") == 0) {
      selftest = true;
    } else if (std::wcscmp(argv[i], L"--pipe-name") == 0 && i + 1 < argc) {
      pipe_name = argv[++i];
    } else if (std::wcscmp(argv[i], L"--smoke-secret") == 0 && i + 1 < argc) {
      secret_hex = argv[++i];
    } else {
      std::fwprintf(stderr, L"xnc-core: unknown or incomplete argument: %s\n", argv[i]);
      Usage(stderr);
      return 2;
    }
  }

  if (selftest) return SelftestMain();
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
