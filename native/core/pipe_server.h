// pipe_server.h - console-mode XNIP named-pipe server (spec 9.2/9.3 server
// half). Single instance, DACL SYSTEM+Administrators, FILE_FLAG_FIRST_PIPE_
// INSTANCE; per connection: mutual-proof handshake then the frame loop
// (PING->PONG, unknown types answered with FlagError) until disconnect.
#ifndef XNC_NATIVE_CORE_PIPE_SERVER_H_
#define XNC_NATIVE_CORE_PIPE_SERVER_H_

#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>

#include <cstddef>
#include <cstdint>

namespace xnc {

class Watchdog;

// HMAC-SHA256 takes any key length; the nominal pipe_secret is 32 bytes
// (64 hex chars). The real one arrives via the spawn channel in M1.
constexpr size_t kMaxPipeSecretBytes = 128;

// Serve <pipe_name> with the pipe secret (secret_len bytes). Blocks for the
// process lifetime; returns the process exit code (0 on Ctrl+C, 1 on fatal).
int RunPipeServer(const wchar_t* pipe_name, const uint8_t* secret, size_t secret_len);

// Serve a single already-accepted overlapped pipe instance: handshake
// (spec 9.3) then the frame loop until disconnect. RunPipeServer's exact
// per-connection path, exposed for the in-process loopback selftest (which
// uses a permissive test-only DACL; production keeps spec 9.1). Returns
// true when the handshake succeeded (client_pid filled on success).
bool ServeConnection(HANDLE pipe, Watchdog* wd, const uint8_t* secret,
                     size_t secret_len, uint32_t* client_pid);

}  // namespace xnc

#endif  // XNC_NATIVE_CORE_PIPE_SERVER_H_
