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

#include <bcrypt.h>

#include <cstdio>
#include <cstring>
#include <string>
#include <vector>

#include "../common/frame.h"
#include "../common/handshake.h"
#include "../common/log.h"
#include "pipe_server.h"
#include "service.h"
#include "spawn.h"
#include "token_manager.h"

int SelftestMain();  // selftest.cpp

namespace {

const wchar_t kDefaultPipe[] = L"\\\\.\\pipe\\xnc-core";

void Usage(FILE* out) {
  std::fwprintf(out,
      L"xnc-core - XNC node core (M0)\n"
      L"usage: xnc-core.exe --console --smoke-secret <hex> [--pipe-name <name>]\n"
      L"       xnc-core.exe --console --smoke-secret <hex> --sas-probe [reason]\n"
      L"       xnc-core.exe --console --diag-spawn <exe> <args...>\n"
      L"       xnc-core.exe --service <name> [--smoke-secret <hex>] "
      L"[--pipe-name <name>]\n"
      L"       xnc-core.exe --selftest\n"
      L"  --service <name>  run as Windows service <name> (SCM dispatch;\n"
      L"                  name must match the registered service). Pipe\n"
      L"                  credentials: --pipe-name/--smoke-secret argv (dev;\n"
      L"                  plaintext binPath precedent) or the env fallback\n"
      L"                  XNC_CORE_PIPE_NAME / XNC_CORE_SECRET_HEX. Service\n"
      L"                  mode opens the SAS gate (M2-Slice3 ruling: the\n"
      L"                  server capability ticket gates upstream). SCM stop\n"
      L"                  drains exactly like console Ctrl+C\n"
      L"  --console       foreground: serve the XNIP pipe (DACL SYSTEM+Admins)\n"
      L"  --pipe-name     pipe name (default %s)\n"
      L"  --smoke-secret  hex pipe secret (nominal 32B = 64 hex chars);\n"
      L"                  diagnostic --console mode only (service mode reads\n"
      L"                  the spawn channel in M1)\n"
      L"  --allow-sas     serve mode: open the MSG_SAS (0x0110) gate. Default\n"
      L"                  is DENY + sas_audit log line per attempt (the\n"
      L"                  M2-Slice1 minimum bar); the M2-Slice3 service mode\n"
      L"                  stays deny until the capability ticket lands\n"
      L"  --sas-probe     DIAGNOSTIC CLIENT (Task 4 live check): dial the\n"
      L"                  pipe, run the client handshake, send one MSG_SAS\n"
      L"                  [reason] (default \"diag\") and print the response\n"
      L"                  HRESULT / error code; no server is started. Needs\n"
      L"                  --smoke-secret (the TARGET core's secret)\n"
      L"  --diag-spawn    session bridge: spawn xnc-desktop.exe (whitelist;\n"
      L"                  resolved next to xnc-core.exe; an explicit\n"
      L"                  'xnc-desktop.exe' first arg is also accepted) in\n"
      L"                  the active console session via TokenManager, wait\n"
      L"                  for it and propagate its exit code; remaining args\n"
      L"                  go to the child verbatim; no pipe server here\n"
      L"  --selftest      frame + handshake selftest\n",
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

// Diagnostic 0x0110 probe client (M2-Slice1 Task 4 live check): dial the
// core pipe, run the client half of the spec 9.3 mutual-proof handshake,
// send ONE MSG_SAS with the given reason, print the response and exit.
// This exercises the SAS gate + SendSAS path end to end without touching
// the agent (agent-side secure_attention forwarding arrives in Task 5).
// Exit codes: 0 = ok response received (hr printed - hr is a SYNTHESIZED
// HRESULT, 0 means the VOID sas.dll call returned, not proof of delivery);
// 1 = transport/handshake failure; 2 = FlagError (gate denied etc).
int RunSasProbe(const wchar_t* pipe_name, const uint8_t* secret,
                size_t secret_len, const wchar_t* reason_w) {
  using namespace xnc;
  HANDLE c = CreateFileW(pipe_name, GENERIC_READ | GENERIC_WRITE, 0, nullptr,
                         OPEN_EXISTING, 0, nullptr);
  if (c == INVALID_HANDLE_VALUE) {
    std::fwprintf(stderr, L"xnc-core: sas probe: dial %ls failed err=%lu\n",
                  pipe_name, GetLastError());
    return 1;
  }
  uint8_t nonce[16];
  NTSTATUS rng =
      BCryptGenRandom(nullptr, nonce, 16, BCRYPT_USE_SYSTEM_PREFERRED_RNG);
  if (!BCRYPT_SUCCESS(rng) ||
      !WriteFrame(c, Frame{0, kMsgHello, 0,
                           EncodeHello(GetCurrentProcessId(), nonce)})) {
    std::fwprintf(stderr, L"xnc-core: sas probe: HELLO failed\n");
    CloseHandle(c);
    return 1;
  }
  Frame hp;
  if (ReadFrame(c, hp) != DecodeResult::Ok ||
      hp.message_type != kMsgHelloProof) {
    std::fwprintf(stderr, L"xnc-core: sas probe: expected HELLO_PROOF\n");
    CloseHandle(c);
    return 1;
  }
  uint32_t spid = 0;
  uint8_t snonce[16], sproof[32], want[32];
  // The server proof is HMAC over OUR nonce (client nonce) - the same
  // check the Go client and the selftest loopback make.
  if (DecodeHelloProof(hp, spid, snonce, sproof) != DecodeResult::Ok ||
      !HmacSha256(secret, secret_len, nonce, 16, want) ||
      std::memcmp(want, sproof, 32) != 0) {
    std::fwprintf(stderr, L"xnc-core: sas probe: server proof rejected\n");
    CloseHandle(c);
    return 1;
  }
  uint8_t myproof[32];
  // Our proof = HMAC over the SERVER's nonce (mutual proof, spec 9.3).
  if (!HmacSha256(secret, secret_len, snonce, 16, myproof) ||
      !WriteFrame(c, Frame{0, kMsgProof, 0, EncodeProof(myproof)})) {
    std::fwprintf(stderr, L"xnc-core: sas probe: PROOF send failed\n");
    CloseHandle(c);
    return 1;
  }

  char reason[24] = {0};
  for (int i = 0; i < 23 && reason_w[i] != L'\0'; i++)
    reason[i] = reason_w[i] < 128 ? static_cast<char>(reason_w[i]) : '?';
  std::vector<uint8_t> p(reason, reason + sizeof(reason));
  if (!WriteFrame(c, Frame{0, kMsgSas, 1, p})) {
    std::fwprintf(stderr, L"xnc-core: sas probe: MSG_SAS send failed\n");
    CloseHandle(c);
    return 1;
  }
  Frame resp;
  if (ReadFrame(c, resp) != DecodeResult::Ok) {
    std::fwprintf(stderr, L"xnc-core: sas probe: no response\n");
    CloseHandle(c);
    return 1;
  }
  WriteFrame(c, Frame{0, kMsgBye, 0, {}});
  CloseHandle(c);
  if ((resp.flags & kFlagError) != 0) {
    std::string code(resp.payload.begin(), resp.payload.end());
    std::printf("sas probe: ERROR code=%s\n", code.c_str());
    return 2;
  }
  uint32_t hr = 0;
  if (resp.payload.size() == 4) {
    hr = static_cast<uint32_t>(resp.payload[0]) |
         static_cast<uint32_t>(resp.payload[1]) << 8 |
         static_cast<uint32_t>(resp.payload[2]) << 16 |
         static_cast<uint32_t>(resp.payload[3]) << 24;
  }
  std::printf("sas probe: response hr=0x%08lX (0 = VOID sas.dll call "
              "returned; synthesized HRESULT, not proof of delivery)\n",
              static_cast<unsigned long>(hr));
  return 0;
}

}  // namespace

int wmain(int argc, wchar_t** argv) {
  bool console = false, selftest = false, diag_spawn = false;
  bool allow_sas = false, sas_probe = false, service_mode = false;
  const wchar_t* service_name = nullptr;
  const wchar_t* pipe_name = kDefaultPipe;
  const wchar_t* secret_hex = nullptr;
  const wchar_t* sas_reason = L"diag";
  const wchar_t* child_exe = nullptr;
  int child_args_from = 0;
  for (int i = 1; i < argc; i++) {
    if (std::wcscmp(argv[i], L"--console") == 0) {
      console = true;
    } else if (std::wcscmp(argv[i], L"--service") == 0 && i + 1 < argc) {
      service_mode = true;
      service_name = argv[++i];
    } else if (std::wcscmp(argv[i], L"--selftest") == 0) {
      selftest = true;
    } else if (std::wcscmp(argv[i], L"--pipe-name") == 0 && i + 1 < argc) {
      pipe_name = argv[++i];
    } else if (std::wcscmp(argv[i], L"--smoke-secret") == 0 && i + 1 < argc) {
      secret_hex = argv[++i];
    } else if (std::wcscmp(argv[i], L"--allow-sas") == 0) {
      allow_sas = true;
    } else if (std::wcscmp(argv[i], L"--sas-probe") == 0) {
      sas_probe = true;
      // optional reason (any next arg that is not another flag)
      if (i + 1 < argc && std::wcsncmp(argv[i + 1], L"--", 2) != 0 &&
          argv[i + 1][0] != L'\0')
        sas_reason = argv[++i];
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
  if (service_mode) {
    if (console || diag_spawn || sas_probe || allow_sas) {
      std::fwprintf(stderr,
          L"xnc-core: --service is a standalone mode (no --console/"
          L"--diag-spawn/--sas-probe/--allow-sas; the SAS gate is OPEN in "
          L"service mode by the M2-Slice3 ruling)\n");
      return 2;
    }
    if (!service_name || !service_name[0]) {
      std::fwprintf(stderr, L"xnc-core: --service requires a service <name>\n");
      return 2;
    }
    std::string secret;
    if (secret_hex) {
      if (!ParseSecretHex(secret_hex, secret)) {
        std::fwprintf(stderr, L"xnc-core: bad --smoke-secret hex\n");
        return 2;
      }
    } else {
      // Env fallback (SCM services carry no stdin credential channel yet).
      wchar_t env_hex[2 * xnc::kMaxPipeSecretBytes + 1];
      DWORD n = GetEnvironmentVariableW(L"XNC_CORE_SECRET_HEX", env_hex,
                                        sizeof(env_hex) / sizeof(wchar_t));
      if (n == 0 || n >= sizeof(env_hex) / sizeof(wchar_t) ||
          !ParseSecretHex(env_hex, secret)) {
        std::fwprintf(stderr,
            L"xnc-core: --service needs --smoke-secret <hex> or a valid "
            L"XNC_CORE_SECRET_HEX env (dev argv carries the plaintext "
            L"precedent; the SCM credential channel is a later slice)\n");
        return 2;
      }
    }
    wchar_t env_pipe[256];
    if (pipe_name == kDefaultPipe &&
        GetEnvironmentVariableW(L"XNC_CORE_PIPE_NAME", env_pipe,
                                sizeof(env_pipe) / sizeof(wchar_t)) > 0 &&
        env_pipe[0]) {
      pipe_name = env_pipe;
    }
    return xnc::RunService(service_name, pipe_name,
                           reinterpret_cast<const uint8_t*>(secret.data()),
                           secret.size());
  }
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
    if (allow_sas) {
      std::fwprintf(stderr,
          L"xnc-core: --diag-spawn does not serve the pipe; --allow-sas is "
          L"a serve-mode gate and is not used here\n");
      return 2;
    }
    return RunDiagSpawn(child_exe, argc, argv, child_args_from);
  }
  if (sas_probe) {
    // Diagnostic CLIENT of another core's pipe - never starts a server.
    if (!console) {
      std::fwprintf(stderr, L"xnc-core: --sas-probe is only valid with --console\n");
      Usage(stderr);
      return 2;
    }
    if (!secret_hex) {
      std::fwprintf(stderr,
          L"xnc-core: --sas-probe needs --smoke-secret <hex> (the TARGET "
          L"core's pipe secret)\n");
      return 2;
    }
    std::string secret;
    if (!ParseSecretHex(secret_hex, secret)) {
      std::fwprintf(stderr, L"xnc-core: bad --smoke-secret hex\n");
      return 2;
    }
    if (allow_sas)
      std::fwprintf(stderr,
          L"xnc-core: note: --allow-sas gates the SERVE path; it has no "
          L"effect on --sas-probe (the target core's own gate decides)\n");
    return RunSasProbe(pipe_name,
                       reinterpret_cast<const uint8_t*>(secret.data()),
                       secret.size(), sas_reason);
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
  xnc::SetSasAllowed(allow_sas);
  XNC_LOG_INFO("sas gate %s (0x0110; audit line per attempt)",
               allow_sas ? "OPEN (--allow-sas)" : "DENIED (default)");
  return xnc::RunPipeServer(pipe_name,
                            reinterpret_cast<const uint8_t*>(secret.data()),
                            secret.size());
}
