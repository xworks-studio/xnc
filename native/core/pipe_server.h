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
#include <string>
#include <vector>

#include "../common/frame.h"
#include "wts_monitor.h"  // WtsMonitor, WtsSessionChange (Task 4)

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

//   kMsgSas (M2-Slice1 Task 4) request [char reason[24]] (NUL-padded
//   fixed field) -> ok response [u32 hr] | FlagError (SAS_DENIED /
//   SAS_UNAVAILABLE / BAD_PAYLOAD). hr is a SYNTHESIZED HRESULT: the real
//   sas.dll SendSAS returns VOID, so 0 (S_OK) means "the call returned
//   without raising" - NOT proof a SAS was delivered - and a raised SEH
//   exception surfaces as HRESULT_FROM_NT(exception code). Gate = the
//   explicit --allow-sas console flag (default DENY + audit log line per
//   attempt; the M2-Slice1 minimum bar - the capability ticket arrives in
//   M2-Slice3). Audit trail accessor: SasAuditLast/SasAuditCount.
constexpr uint16_t kMsgSas = 0x0110;
inline constexpr size_t kSasReasonLen = 24;

// Pure ok-response codec for kMsgStartCapture (little-endian layout above;
// the pipe name is program-constructed ASCII so the utf8 pass is a plain
// copy). Exposed so the selftest can assert the success layout byte for
// byte (native/desktop rt codec precedent). Secret/gen never logged.
Frame EncodeStartCaptureOk(const Frame& req, DWORD pid, const std::wstring& pipe,
                           const uint8_t* secret, uint32_t gen);

// ---- M2-Slice2 Task 3: CreateShell / KillShell + worker supervision ----
//   kMsgCreateShell request (LE) [u32 wts][u8 token_kind 0=user,1=system]
//     [u8 profile enum 0=POWERSHELL,1=PWSH,2=CMD,3=BASH][u8 mode
//     0=interactive,1=oneshot][u16 cols][u16 rows][u16 cwdLen][cwd utf8]
//     [u16 envLen][env "K=V\n"-joined utf8][u16 cmdLen][cmd utf8]
//     [u32 timeoutSec]
//     -> ok response [u32 pid][u16 nameLen][pipe name utf8][32B secret]
//     -> FlagError stable code (BAD_PAYLOAD / SESSION_MISMATCH /
//        NO_ACTIVE_SESSION / TOKEN_FAILED / RNG_FAILED / SPAWN_FAILED /
//        PIPE_TIMEOUT / INTERNAL)
//   kMsgKillShell request [u32 pid] -> empty FlagResponse (idempotent;
//     scoped by the STORED child handle - never by image name).
constexpr uint16_t kMsgCreateShell = 0x0120, kMsgKillShell = 0x0121;

// Profile whitelist (spec 8.2): core validates the ENUM range only and
// passes the canonical NAME to xnc-shell.exe, which resolves the exe
// itself - core never accepts or builds shell paths.
bool ShellProfileAllowed(uint8_t profile);
const char* ShellProfileName(uint8_t profile);  // "POWERSHELL"|...|"?"
const wchar_t* ShellProfileWName(uint8_t profile);  // wide argv form
// Token kind validation: 0 = user, 1 = system.
bool ShellTokenKindAllowed(uint8_t kind);

// Pure decoder for the 0x0120 request payload (bounds-checked). env stays
// as the raw "K=V\n"-joined blob; the splitter below turns it into the
// individual --env argv entries (empty segments skipped).
struct ShellCreateReq {
  uint32_t wts = 0;
  uint8_t token_kind = 0;
  uint8_t profile = 0;
  uint8_t mode = 0;  // 0 interactive, 1 oneshot
  uint16_t cols = 0, rows = 0;
  std::string cwd, env, cmd;
  uint32_t timeout_sec = 0;
};
bool DecodeShellCreatePayload(const uint8_t* p, size_t n, ShellCreateReq* out);
std::vector<std::string> SplitShellEnv(const std::string& envJoined);

// Pure ok-response codec for 0x0120 (selftest golden bytes). Pipe name is
// program-constructed ASCII.
Frame EncodeCreateShellOk(const Frame& req, DWORD pid, const std::wstring& pipe,
                          const uint8_t* secret);

// ---- Worker supervision (spec 15.2), pure units (injected clock in the
// selftest): desktop crash backoff 1s,2s,4s... capped 60s; crash-loop =
// >= 5 exits inside a rolling 60s window locks the desktop to the degraded
// args (--backend gdi --encoder software). Shells are NOT restarted
// (session-scoped; their exit is only logged). ----

// Backoff for the crash_index-th consecutive desktop crash (1-based
// crash_index; 0 treated as 1): 1000 << (crash_index-1), capped 60000.
uint32_t WorkerBackoffMs(uint32_t crash_index);

// Crash-loop predicate: how many of the exit timestamps (ms, ascending or
// unsorted - sorted internally by the caller contract here: pass as recorded)
// fall inside (now-60000, now]; >= kCrashLoopExits means degraded.
constexpr size_t kCrashLoopWindowMs = 60000;
constexpr size_t kCrashLoopExits = 5;
bool CrashLoopReached(const std::vector<uint64_t>& exit_ms, uint64_t now_ms);

// Injectable seams (selftest only; nullptr restores production default):
// user-token mint (default WTSQueryUserToken - needs SYSTEM) and the whole
// shell-spawn step (default = RealShellSpawn). err = stable ASCII code.
struct ShellSpawnResult {
  bool ok = false;
  DWORD pid = 0;
  HANDLE child = nullptr;  // owned; the watcher thread closes it
  char err[24] = {0};
};
using ShellTokenFn = bool (*)(uint32_t session, uint8_t token_kind, HANDLE* out);
using ShellSpawnFn = ShellSpawnResult (*)(const ShellCreateReq& req,
                                          uint32_t session,
                                          const wchar_t* pipe_name,
                                          const uint8_t* secret, HANDLE token,
                                          Watchdog* wd);
void SetShellTokenForTest(ShellTokenFn fn);
void SetShellSpawnForTest(ShellSpawnFn fn);

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

// ---- M2-Slice1 Task 4: WTS monitor + SAS gate (wts_monitor.h for the
// monitor itself) ----

// The core-wide WtsMonitor singleton (started/stopped by RunPipeServer).
// StartCapture validates its wts argument against CoreWts().console_
// session() - the LIVE active console session, unified with the monitor's
// cache (replaces the bare WTSGetActiveConsoleSessionId call).
WtsMonitor& CoreWts();

// Monitor-thread callback (production wiring): on active console session
// change, terminate a running capture child - scoped by the STORED HANDLE
// (+ its logged pid; never by image name) - so the next StartCapture
// re-spawns into the new session. Exported for the selftest's direct call.
void OnActiveConsoleSessionChanged(const WtsSessionChange& change);

// Capture reuse decision (pure, table-tested): given the stored child's
// state and the live active session, reuse the child / spawn fresh /
// terminate-and-respawn because the child sits in a stale session.
enum class CaptureReuse { kFresh, kReuse, kRespawnStaleSession };
CaptureReuse DecideCaptureReuse(bool child_valid, bool child_running,
                                uint32_t child_session, uint32_t active_session);

// SAS gate. SetSasAllowed is called by xnc-core's --allow-sas; the default
// (and the M2-Slice3 service mode) is DENY with an audit line per attempt.
void SetSasAllowed(bool allow);
bool SasAllowed();

// One audit record per 0x0110 attempt (mirror of the log line; test seam).
struct SasAuditEvent {
  uint32_t client_pid = 0;
  bool allowed = false;    // gate verdict
  bool attempted = false;  // SendSAS actually called
  char action[20] = {0};   // sas_denied | sas_sent | sas_unavailable
  char reason[kSasReasonLen + 1] = {0};  // NUL-terminated caller reason
  uint32_t hr = 0;         // synthesized HRESULT (sas_sent only)
};
bool SasAuditLast(SasAuditEvent* out);
uint32_t SasAuditCount();

// Injectable seams (selftest only; nullptr restores the production
// default). SendSAS type per sas.h: VOID WINAPI SendSAS(BOOL AsUser).
using SasSendFn = void (WINAPI*)(BOOL as_user);
using SasResolveFn = SasSendFn (*)();  // default: dynamic sas.dll resolve
void SetSasSendForTest(SasSendFn fn);
void SetSasResolveForTest(SasResolveFn fn);

// Capture-spawn seam: everything from token minting to WaitPipeReady
// (production default = RealCaptureSpawn). err = stable ASCII code
// (TOKEN_FAILED / SPAWN_FAILED / INTERNAL / PIPE_TIMEOUT).
struct CaptureSpawnResult {
  bool ok = false;
  DWORD pid = 0;
  HANDLE child = nullptr;  // owned handle; the watcher thread closes it
  char err[24] = {0};
};
using CaptureSpawnFn = CaptureSpawnResult (*)(uint32_t session,
                                              const wchar_t* pipe_name,
                                              const uint8_t* secret,
                                              Watchdog* wd,
                                              bool degraded);
void SetCaptureSpawnForTest(CaptureSpawnFn fn);

// Terminate seam (default TerminateProcess); the fake records + signals.
void SetTerminateForTest(BOOL (WINAPI* fn)(HANDLE, UINT));

}  // namespace xnc

#endif  // XNC_NATIVE_CORE_PIPE_SERVER_H_
