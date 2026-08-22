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

// App RPC types, start of the 0x0100 registry block (spec 9.2: 0x0001..0x000F
// are frame-level, 0x0100+ are app RPC). M1-Slice2 fixed-binary layouts
// (little-endian; protobuf migration is Slice3):
//   kMsgStartCapture request  [wts_session u32][pad u32 = 0]
//     -> ok response  [pid u32][name_len u16][pipe name utf8 bytes]
//                     [secret 32B][gen u32]
//     -> error response = FlagError frame, payload ASCII stable code
//        (BAD_PAYLOAD / SESSION_MISMATCH / RNG_FAILED / TOKEN_FAILED /
//         SPAWN_FAILED / PIPE_TIMEOUT / INTERNAL)
//   kMsgStopCapture  request  (empty) -> empty FlagResponse (idempotent)
constexpr uint16_t kMsgStartCapture = 0x0100, kMsgStopCapture = 0x0101;

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
