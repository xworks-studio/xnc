// pipe_server.h - console-mode XNIP named-pipe server (spec 9.2/9.3 server
// half). Single instance, DACL SYSTEM+Administrators, FILE_FLAG_FIRST_PIPE_
// INSTANCE; per connection: mutual-proof handshake then the frame loop
// (PING->PONG, unknown types answered with FlagError) until disconnect.
#ifndef XNC_NATIVE_CORE_PIPE_SERVER_H_
#define XNC_NATIVE_CORE_PIPE_SERVER_H_

#include <cstddef>
#include <cstdint>

namespace xnc {

// HMAC-SHA256 takes any key length; the nominal pipe_secret is 32 bytes
// (64 hex chars). The real one arrives via the spawn channel in M1.
constexpr size_t kMaxPipeSecretBytes = 128;

// Serve <pipe_name> with the pipe secret (secret_len bytes). Blocks for the
// process lifetime; returns the process exit code (0 on Ctrl+C, 1 on fatal).
int RunPipeServer(const wchar_t* pipe_name, const uint8_t* secret, size_t secret_len);

}  // namespace xnc

#endif  // XNC_NATIVE_CORE_PIPE_SERVER_H_
