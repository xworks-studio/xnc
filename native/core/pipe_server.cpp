// pipe_server.cpp - console-mode XNIP pipe server (spec 9 server half):
// DACL'd single-instance pipe, accept loop, server-side mutual-proof
// handshake (9.3), PING/PONG frame loop, START/STOP_CAPTURE RPC
// (M1-Slice2 Task 3: spawn xnc-desktop into the console session, report
// its rt pipe name/secret/generation). All blocking waits (accept, reads,
// writes) are sliced into 5s chunks that heartbeat the watchdog, so a quiet
// but healthy server stays alive while a stuck loop is killed after 30s
// (spec 15). IO is overlapped because the accept wait needs the same
// slicing and one handle serves both phases; frame codec and handshake
// payloads reuse frame.cpp/handshake.cpp byte-identical logic.
#include "pipe_server.h"

#include "../common/frame.h"
#include "../common/handshake.h"
#include "../common/log.h"
#include "spawn.h"
#include "token_manager.h"
#include "watchdog.h"

#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>

#include <bcrypt.h>
#include <sddl.h>

#include <atomic>
#include <cstdio>
#include <cstring>
#include <mutex>
#include <string>
#include <thread>
#include <vector>

namespace xnc {
namespace {

constexpr DWORD kBufSize = 64 * 1024;
constexpr DWORD kWaitSliceMs = 5000;  // heartbeat slice for every wait
// DACL: SYSTEM + Administrators full access, everyone else denied (spec 9.1).
constexpr wchar_t kSddl[] = L"D:P(A;;GA;;;SY)(A;;GA;;;BA)";

std::atomic<bool> g_stop{false};  // set by the console Ctrl handler

BOOL WINAPI OnCtrlEvent(DWORD type) {
  XNC_LOG_INFO("console control event %lu, stopping", type);
  g_stop = true;
  return TRUE;  // default termination suppressed; serve loop exits cleanly
}

uint16_t GetU16(const uint8_t* p) {
  return static_cast<uint16_t>(static_cast<uint16_t>(p[0]) |
                               static_cast<uint16_t>(p[1]) << 8);
}

uint32_t GetU32(const uint8_t* p) {
  return static_cast<uint32_t>(p[0]) | static_cast<uint32_t>(p[1]) << 8 |
         static_cast<uint32_t>(p[2]) << 16 | static_cast<uint32_t>(p[3]) << 24;
}

// Overlapped pipe IO whose waits are sliced: while an op is pending we wait
// kWaitSliceMs, heartbeat the watchdog, check Ctrl+C, and keep waiting the
// SAME pending op. Mirrors the blocking helpers in frame.cpp, but the handle
// is overlapped (created that way for the accept wait), so every IO here
// must supply an OVERLAPPED structure.
class TimedIo {
 public:
  TimedIo(HANDLE file, Watchdog* wd) : file_(file), wd_(wd) {
    event_ = CreateEventW(nullptr, TRUE, FALSE, nullptr);
  }
  ~TimedIo() {
    if (event_) CloseHandle(event_);
  }
  TimedIo(const TimedIo&) = delete;
  TimedIo& operator=(const TimedIo&) = delete;
  bool Ok() const { return event_ != nullptr; }

  bool ReadFull(uint8_t* buf, size_t len) { return Xfer(true, buf, len); }
  bool WriteFull(const uint8_t* buf, size_t len) {
    return Xfer(false, const_cast<uint8_t*>(buf), len);
  }

 private:
  bool Xfer(bool read, uint8_t* buf, size_t len) {
    size_t done = 0;
    while (done < len) {
      std::memset(&ov_, 0, sizeof(ov_));
      ov_.hEvent = event_;
      ResetEvent(event_);
      BOOL ok = read ? ReadFile(file_, buf + done, static_cast<DWORD>(len - done),
                                nullptr, &ov_)
                     : WriteFile(file_, buf + done, static_cast<DWORD>(len - done),
                                 nullptr, &ov_);
      if (!ok && GetLastError() != ERROR_IO_PENDING) return false;
      DWORD got = 0;
      if (!WaitAndGet(&got) || got == 0) return false;
      done += got;
    }
    return true;
  }

  // Wait out the pending op; returns GetOverlappedResult, or false on
  // Ctrl+C / wait failure. Heartbeats every slice so idle waits are liveness.
  bool WaitAndGet(DWORD* got) {
    for (;;) {
      DWORD w = WaitForSingleObject(event_, kWaitSliceMs);
      if (w == WAIT_OBJECT_0) return GetOverlappedResult(file_, &ov_, got, FALSE) != 0;
      if (w != WAIT_TIMEOUT) return false;
      wd_->Heartbeat();
      if (g_stop.load()) return false;
    }
  }

  HANDLE file_;  // pipe instance, not owned
  HANDLE event_ = nullptr;  // manual-reset
  OVERLAPPED ov_{};
  Watchdog* wd_;
};

// Framing mirrors frame.cpp ReadFrame/WriteFrame on overlapped IO (header
// then payload; same validation order: magic -> version -> size cap).
DecodeResult ReadFrameTimed(TimedIo& io, Frame& out) {
  uint8_t h[kHeaderSize];
  if (!io.ReadFull(h, sizeof(h))) return DecodeResult::Truncated;
  if (std::memcmp(h, kMagic, sizeof(kMagic)) != 0) return DecodeResult::BadMagic;
  if (h[4] != kProtocolVersion) return DecodeResult::BadVersion;
  uint32_t n = GetU32(h + 12);
  if (n > kMaxFrameBytes) return DecodeResult::TooLarge;
  out.flags = h[5];
  out.message_type = GetU16(h + 6);
  out.request_id = GetU32(h + 8);
  out.payload.assign(n, 0);
  if (n > 0 && !io.ReadFull(out.payload.data(), n)) return DecodeResult::Truncated;
  return DecodeResult::Ok;
}

bool WriteFrameTimed(TimedIo& io, const Frame& f) {
  std::vector<uint8_t> wire;
  if (!EncodeFrame(f, wire)) return false;
  return io.WriteFull(wire.data(), wire.size());
}

// ---- START/STOP_CAPTURE host state (M1-Slice2 Task 3 + M2-Slice1 Task 4) ----
//
// One desktop capture child per core instance. The child handle is owned by
// its watcher thread: the ONLY closer (each spawn gets exactly one watcher,
// so no double-close / use-after-close races between StopCapture handling
// and exit reaping). StopCapture = TerminateProcess (graceful drain is a
// Slice3 carry-over; the child's own sender drains queued frames within its
// sliced-IO budget before exit, but no DRAIN handshake exists yet). Child
// exit is logged + state cleared here; a core->agent event push for the
// exit is not on the Slice2 wire (agent observes via StartCapture
// idempotency / its own desktop pipe dying).
// Task 4: the child remembers WHICH session it was spawned into; the wts
// monitor (below) and StartCapture's DecideCaptureReuse both terminate a
// live child whose session is no longer the active console session (scoped
// by stored handle), and the next StartCapture re-spawns into the current
// one (gen++).
constexpr size_t kDesktopSecretLen = 32;
constexpr DWORD kPipeReadyWaitMs = 2000;

struct CaptureHost {
  std::mutex mu;
  bool valid = false;        // descriptor reflects a spawned child
  DWORD pid = 0;
  HANDLE child = nullptr;    // owned by the watcher thread of that spawn
  uint32_t session = 0;      // session the child was spawned into (Task 4)
  uint32_t gen = 0;          // increments per spawn (agent-side accounting)
  uint8_t secret[kDesktopSecretLen] = {0};
  std::wstring pipe_name;
};
CaptureHost g_capture;

// Core-wide WTS monitor (M2-Slice1 Task 4): notification+poll hybrid
// watching the live active console session; started/stopped in
// RunPipeServer. HandleStartCapture reads CoreWts().console_session().
WtsMonitor g_wts;

// SAS gate + audit state (Task 4). g_sas_allowed mirrors the --allow-sas
// console flag (SetSasAllowed); every 0x0110 attempt appends one audit
// record (RecordSasAudit) mirroring the sas_audit log line.
std::atomic<bool> g_sas_allowed{false};
std::mutex g_sas_audit_mu;
SasAuditEvent g_sas_audit_last;
uint32_t g_sas_audit_count = 0;
// Seams (selftest injection; null = production default below).
SasSendFn g_sas_send = nullptr;
SasResolveFn g_sas_resolve = nullptr;
CaptureSpawnFn g_capture_spawn_fn = nullptr;
BOOL (WINAPI* g_terminate_process)(HANDLE, UINT) = nullptr;

CaptureSpawnResult RealCaptureSpawn(uint32_t session, const wchar_t* pipe_name,
                                    const uint8_t* secret, Watchdog* wd);

CaptureSpawnFn CurrentSpawnFn() {
  return g_capture_spawn_fn != nullptr ? g_capture_spawn_fn
                                       : &RealCaptureSpawn;
}

// Terminate a capture child. ALWAYS scoped by the STORED HANDLE (pid only
// in the log) - never by image name: killing by name on a shared host is
// the T2 incident class (ledger rule). Caller holds g_capture.mu.
void TerminateCaptureChildLocked(DWORD pid, HANDLE child) {
  XNC_LOG_INFO("capture child terminate pid=%lu (scoped by stored handle)",
               pid);
  if (g_terminate_process != nullptr)
    g_terminate_process(child, 1);
  else
    TerminateProcess(child, 1);
}

void RecordSasAudit(uint32_t pid, bool allowed, bool attempted,
                    const char* action, const char* reason, uint32_t hr) {
  XNC_LOG_INFO(
      "sas_audit action=%s caller_pid=%lu allowed=%d attempted=%d "
      "reason=\"%s\" hr=0x%08lX",
      action, pid, allowed ? 1 : 0, attempted ? 1 : 0, reason, hr);
  std::lock_guard<std::mutex> lk(g_sas_audit_mu);
  g_sas_audit_last = SasAuditEvent{};
  g_sas_audit_last.client_pid = pid;
  g_sas_audit_last.allowed = allowed;
  g_sas_audit_last.attempted = attempted;
  snprintf(g_sas_audit_last.action, sizeof(g_sas_audit_last.action), "%s",
           action);
  snprintf(g_sas_audit_last.reason, sizeof(g_sas_audit_last.reason), "%s",
           reason);
  g_sas_audit_last.hr = hr;
  g_sas_audit_count += 1;
}

// SEH guard around the VOID SendSAS: the sas.h API has NO return value and
// reports "not entitled" by raising (community-observed C0000022-class
// exceptions). Returns 0 when the call returned normally, else the
// exception code. No C++ objects here - required for __try under /EHsc.
DWORD RunSendSasGuarded(SasSendFn fn, BOOL as_user) {
  __try {
    fn(as_user);
    return 0;
  } __except (EXCEPTION_EXECUTE_HANDLER) {
    return GetExceptionCode();
  }
}

// Production resolver: dynamic-load sas.dll (plan choice - no Sas.lib
// import), resolve SendSAS once and keep it (plus the module) for the
// process lifetime; FreeLibrary would race any in-flight call.
SasSendFn RealResolveSendSas() {
  static SasSendFn cached = []() -> SasSendFn {
    HMODULE m = LoadLibraryW(L"sas.dll");
    if (m == nullptr) {
      XNC_LOG_ERROR("sas: LoadLibrary(sas.dll) failed err=%lu", GetLastError());
      return nullptr;
    }
    SasSendFn fn = reinterpret_cast<SasSendFn>(GetProcAddress(m, "SendSAS"));
    if (fn == nullptr)
      XNC_LOG_ERROR("sas: SendSAS not exported by sas.dll err=%lu",
                    GetLastError());
    return fn;
  }();
  return cached;
}

// Watcher: waits out the child, logs, clears the shared state if it still
// describes this spawn, and closes the handle it owns. Detached - one per
// spawn, outlives the connection that requested it.
void WatchCaptureChild(HANDLE child, DWORD pid) {
  WaitForSingleObject(child, INFINITE);
  DWORD code = 0;
  if (!GetExitCodeProcess(child, &code)) code = 1;
  XNC_LOG_INFO("capture child pid=%lu exited code=%lu, clearing state", pid, code);
  std::lock_guard<std::mutex> lk(g_capture.mu);
  if (g_capture.child == child) {
    g_capture.valid = false;
    g_capture.child = nullptr;
    g_capture.pid = 0;
    g_capture.session = 0;
  }
  CloseHandle(child);
}

// ASCII error-frame helper (payload = stable code, mirrors Go respText).
Frame ErrorFrame(uint16_t type, uint32_t request_id, const char* code) {
  return Frame{kFlagResponse | kFlagError, type, request_id,
               std::vector<uint8_t>(code, code + std::strlen(code))};
}

// Wait until the child's rt pipe has a listenable instance. FILE_NOT_FOUND
// (not created yet) and PIPE_BUSY (all instances connected mid-handoff) are
// retryable; anything else fails fast. Sliced + heartbeated like every
// other wait.
bool WaitPipeReady(const wchar_t* name, Watchdog* wd) {
  const ULONGLONG deadline = GetTickCount64() + kPipeReadyWaitMs;
  for (;;) {
    if (WaitNamedPipeW(name, 50)) return true;
    // GetLastError FIRST: any intervening call (Heartbeat included) may
    // reset the thread's last-error before we get to it.
    const DWORD e = GetLastError();
    wd->Heartbeat();
    if (e != ERROR_FILE_NOT_FOUND && e != ERROR_PIPE_BUSY) {
      XNC_LOG_ERROR("capture pipe wait failed err=%lu", e);
      return false;
    }
    if (GetTickCount64() >= deadline) return false;
    Sleep(50);
  }
}

// Production capture spawn: session SYSTEM token -> whitelisted
// xnc-desktop.exe via SpawnInSession with the secret on inherited stdin
// (never argv, spec 1.5) -> wait for the child's rt pipe. Every failure
// path closes what it opened; err carries the stable ASCII code.
CaptureSpawnResult RealCaptureSpawn(uint32_t session, const wchar_t* pipe_name,
                                    const uint8_t* secret, Watchdog* wd) {
  CaptureSpawnResult r{};

  HANDLE token = nullptr;
  std::string err;
  if (!TokenManager::SessionSystemToken(session, &token, &err)) {
    // Non-SYSTEM callers (elevated admin lacks SeTcb) land here.
    XNC_LOG_ERROR("start_capture: session token failed err=\"%s\"", err.c_str());
    snprintf(r.err, sizeof(r.err), "%s", "TOKEN_FAILED");
    return r;
  }

  // Secret handoff (spec 1.5 red line): the secret NEVER enters argv - the
  // process list would expose it. It travels over an anonymous pipe the
  // child consumes as its stdin under xnc-desktop --secret-stdin: the read
  // end becomes the child's inherited hStdInput (STARTF_USESTDHANDLES),
  // then the parent writes the 64-hex-char line and closes the write end.
  // The write end is stripped of the inherit flag so ONLY the read end
  // crosses the process boundary.
  HANDLE sec_rd = nullptr, sec_wr = nullptr;
  SECURITY_ATTRIBUTES inherit_sa{sizeof(inherit_sa), nullptr, TRUE};
  if (!CreatePipe(&sec_rd, &sec_wr, &inherit_sa, 0) ||
      !SetHandleInformation(sec_wr, HANDLE_FLAG_INHERIT, 0)) {
    const DWORD pipe_err = GetLastError();
    if (sec_rd) CloseHandle(sec_rd);
    if (sec_wr) CloseHandle(sec_wr);
    CloseHandle(token);
    XNC_LOG_ERROR("start_capture: secret stdin pipe failed err=%lu", pipe_err);
    snprintf(r.err, sizeof(r.err), "%s", "INTERNAL");
    return r;
  }

  // Program-constructed argv only - no user input and no secret reach the
  // command line, and BuildChildCommandLine additionally rejects unsafe
  // quoting shapes (embedded quotes / trailing backslash; the slice3
  // must-fix is resolved) as defense in depth - the cmdline carries
  // nothing sensitive either way.
  std::vector<std::wstring> args = {L"--console-rt", L"--pipe", pipe_name,
                                    L"--secret-stdin"};
  std::vector<wchar_t*> av;
  for (auto& a : args) av.push_back(&a[0]);
  std::wstring cmd;
  std::string cmd_err;
  if (!BuildChildCommandLine(L"xnc-desktop.exe", static_cast<int>(av.size()),
                             av.data(), 0, &cmd, &cmd_err)) {
    CloseHandle(token);
    CloseHandle(sec_rd);
    CloseHandle(sec_wr);
    XNC_LOG_ERROR("start_capture: child cmdline rejected err=\"%s\"",
                  cmd_err.c_str());
    snprintf(r.err, sizeof(r.err), "%s", "INTERNAL");
    return r;
  }

  DWORD pid = 0;
  HANDLE child = nullptr;
  if (!SpawnInSession(token, L"xnc-desktop.exe", cmd.c_str(), &pid, &child,
                      &err, sec_rd)) {
    CloseHandle(token);
    CloseHandle(sec_rd);
    CloseHandle(sec_wr);
    XNC_LOG_ERROR("start_capture: spawn failed err=\"%s\"", err.c_str());
    snprintf(r.err, sizeof(r.err), "%s", "SPAWN_FAILED");
    return r;
  }
  CloseHandle(token);
  CloseHandle(sec_rd);  // the child's inheritance settled at CreateProcess

  // One line - 64 hex chars + '\n' - then close. Matches xnc-desktop's
  // --secret-stdin contract exactly. Hex/secret never logged.
  {
    char hexline[2 * kDesktopSecretLen + 2] = {0};
    for (size_t i = 0; i < kDesktopSecretLen; i++)
      sprintf_s(hexline + 2 * i, 3, "%02x", secret[i]);
    hexline[2 * kDesktopSecretLen] = '\n';
    DWORD wrote = 0;
    if (!WriteFile(sec_wr, hexline, 2 * kDesktopSecretLen + 1, &wrote,
                   nullptr) ||
        wrote != 2 * kDesktopSecretLen + 1) {
      // Without the secret the child cannot serve and exits on its own
      // stdin error; the pipe wait below times out and reaps any survivor.
      XNC_LOG_ERROR("start_capture: secret stdin write failed err=%lu",
                    GetLastError());
    }
    CloseHandle(sec_wr);
  }

  wd->Heartbeat();
  if (!WaitPipeReady(pipe_name, wd)) {
    XNC_LOG_ERROR("start_capture: rt pipe not ready in %lums, killing pid=%lu",
                  kPipeReadyWaitMs, pid);
    TerminateProcess(child, 1);  // fresh child, before any watcher exists
    CloseHandle(child);
    snprintf(r.err, sizeof(r.err), "%s", "PIPE_TIMEOUT");
    return r;
  }

  r.ok = true;
  r.pid = pid;
  r.child = child;
  return r;
}

// 0x0110 MSG_SAS [char reason[24]] (Task 4). The caller identity IS the
// mutual-HMAC handshake (only secret holders reach the frame loop);
// client_pid rides the audit line. Gate: --allow-sas only (ledger minimum
// bar; M2-Slice3 wires the capability ticket on top).
Frame HandleSas(const Frame& req, uint32_t client_pid) {
  if (req.payload.size() != kSasReasonLen) {
    return ErrorFrame(kMsgSas, req.request_id, "BAD_PAYLOAD");
  }
  char reason[kSasReasonLen + 1];
  std::memcpy(reason, req.payload.data(), kSasReasonLen);
  reason[kSasReasonLen] = '\0';

  if (!g_sas_allowed.load()) {
    RecordSasAudit(client_pid, false, false, "sas_denied", reason, 0);
    return ErrorFrame(kMsgSas, req.request_id, "SAS_DENIED");
  }
  SasSendFn send = g_sas_send != nullptr
                       ? g_sas_send
                       : (g_sas_resolve != nullptr ? g_sas_resolve()
                                                   : RealResolveSendSas());
  if (send == nullptr) {
    RecordSasAudit(client_pid, true, false, "sas_unavailable", reason, 0);
    return ErrorFrame(kMsgSas, req.request_id, "SAS_UNAVAILABLE");
  }
  // AsUser=FALSE: xnc-core runs as LocalSystem (production service / dev
  // console core), not "the current user" - the sas.h contract for the
  // service path. The dev session-1 user-token run records what really
  // happens (expected denial; live T4 evidence).
  const DWORD code = RunSendSasGuarded(send, FALSE);
  const uint32_t hr =
      code == 0 ? 0
                : static_cast<uint32_t>(HRESULT_FROM_NT(
                      static_cast<NTSTATUS>(code)));
  RecordSasAudit(client_pid, true, true, "sas_sent", reason, hr);
  std::vector<uint8_t> p(4, 0);
  for (int i = 0; i < 4; i++)
    p[i] = static_cast<uint8_t>(hr >> (8 * i));
  return Frame{kFlagResponse, kMsgSas, req.request_id, std::move(p)};
}

// 0x0100: [u32 wts][u32 pad=0] -> spawn (or reuse) the desktop capture
// child and answer its pipe descriptor. Idempotent while the child lives
// IN THE CURRENT SESSION: the wts argument is validated against the LIVE
// active console session (CoreWts(), unified with the WTS monitor - Task
// 4), and a child left over from a previous console session is terminated
// (scoped by stored handle) and re-spawned into the current one.
Frame HandleStartCapture(const Frame& req, Watchdog* wd) {
  uint32_t wts = 0;
  if (req.payload.size() != 8 || GetU32(req.payload.data() + 4) != 0) {
    return ErrorFrame(kMsgStartCapture, req.request_id, "BAD_PAYLOAD");
  }
  wts = GetU32(req.payload.data());
  const uint32_t active_pre = CoreWts().console_session();
  if (!SessionTargetAllowed(wts, active_pre)) {
    return ErrorFrame(kMsgStartCapture, req.request_id, "SESSION_MISMATCH");
  }

  uint8_t secret[kDesktopSecretLen];
  NTSTATUS rng = BCryptGenRandom(nullptr, secret, sizeof(secret),
                                 BCRYPT_USE_SYSTEM_PREFERRED_RNG);
  if (!BCRYPT_SUCCESS(rng)) {
    XNC_LOG_ERROR("start_capture: secret RNG failed status=0x%08lX", (unsigned long)rng);
    return ErrorFrame(kMsgStartCapture, req.request_id, "RNG_FAILED");
  }

  wchar_t pipe_name[80];
  swprintf(pipe_name, 80, L"\\\\.\\pipe\\xnc-desktop-rt-%lu", GetCurrentProcessId());

  std::lock_guard<std::mutex> lk(g_capture.mu);
  // TOCTOU fix (Task 6 deferred list): the active console session is
  // re-read INSIDE the lock and re-validated - a console switch landing in
  // the window between the unlocked pre-check above and this critical
  // section must not reuse/respawn against the stale snapshot (the WTS
  // monitor callback takes this same mutex, so post-lock reads are the
  // coherent view; a moved console now fails SESSION_MISMATCH and the next
  // StartCapture spawns into the current session).
  const uint32_t active = CoreWts().console_session();
  if (!SessionTargetAllowed(wts, active)) {
    return ErrorFrame(kMsgStartCapture, req.request_id, "SESSION_MISMATCH");
  }
  const bool alive = g_capture.valid && g_capture.child != nullptr &&
                     WaitForSingleObject(g_capture.child, 0) == WAIT_TIMEOUT;
  switch (DecideCaptureReuse(g_capture.valid, alive, g_capture.session,
                             active)) {
    case CaptureReuse::kReuse:
      XNC_LOG_INFO("start_capture: reuse pid=%lu gen=%u session=%u",
                   g_capture.pid, g_capture.gen, g_capture.session);
      return EncodeStartCaptureOk(req, g_capture.pid, g_capture.pipe_name,
                                  g_capture.secret, g_capture.gen);
    case CaptureReuse::kRespawnStaleSession:
      // Console moved (fast user switch / logoff->logon) under a live
      // child: it belongs to the OLD session. Terminate by stored handle
      // and fall through to spawn into the current one (gen++).
      XNC_LOG_INFO(
          "start_capture: console moved %u->%u, terminating stale child "
          "pid=%lu",
          g_capture.session, active, g_capture.pid);
      TerminateCaptureChildLocked(g_capture.pid, g_capture.child);
      g_capture.valid = false;
      break;
    case CaptureReuse::kFresh:
      break;  // no live child: never spawned / already exited / reaped
  }

  const CaptureSpawnResult r = CurrentSpawnFn()(wts, pipe_name, secret, wd);
  if (!r.ok) {
    XNC_LOG_ERROR("start_capture: spawn failed code=%s", r.err);
    return ErrorFrame(kMsgStartCapture, req.request_id, r.err);
  }

  g_capture.valid = true;
  g_capture.pid = r.pid;
  g_capture.child = r.child;
  g_capture.session = wts;
  g_capture.gen += 1;
  std::memcpy(g_capture.secret, secret, kDesktopSecretLen);
  g_capture.pipe_name = pipe_name;
  // Secret/hex never logged; pid + pipe name + gen + session only.
  XNC_LOG_INFO("start_capture: spawned pid=%lu pipe=%ls gen=%u session=%u",
               r.pid, pipe_name, g_capture.gen, wts);
  std::thread(WatchCaptureChild, r.child, r.pid).detach();
  return EncodeStartCaptureOk(req, r.pid, pipe_name, secret, g_capture.gen);
}

// 0x0101: stop the capture child if any; idempotent. TerminateProcess is
// the accepted Slice2 semantics; the watcher thread reaps + closes.
Frame HandleStopCapture(const Frame& req) {
  std::lock_guard<std::mutex> lk(g_capture.mu);
  if (g_capture.valid && g_capture.child != nullptr) {
    XNC_LOG_INFO("stop_capture: terminating pid=%lu (graceful drain = Slice3)",
                 g_capture.pid);
    TerminateCaptureChildLocked(g_capture.pid, g_capture.child);
    g_capture.valid = false;
  } else {
    XNC_LOG_INFO("stop_capture: idle (idempotent ok)");
  }
  return Frame{kFlagResponse, kMsgStopCapture, req.request_id, {}};
}

// Server half of the mutual-proof handshake (spec 9.3), same timing as Go
// coreclient.ServerHandshake: read HELLO(client pid+nonce) -> send
// HELLO_PROOF(own pid + own nonce + HMAC(secret, client nonce)) -> read
// PROOF and verify HMAC(secret, own nonce). Logs never carry nonce/proof.
bool ServerHandshake(TimedIo& io, const uint8_t* secret, size_t secret_len,
                     uint32_t* client_pid) {
  Frame f;
  if (ReadFrameTimed(io, f) != DecodeResult::Ok || f.message_type != kMsgHello) {
    XNC_LOG_INFO("handshake: expected HELLO, disconnecting");
    return false;
  }
  uint32_t pid = 0;
  uint8_t cnonce[kNonceSize];
  if (DecodeHello(f, pid, cnonce) != DecodeResult::Ok) {
    XNC_LOG_INFO("handshake: malformed HELLO, disconnecting");
    return false;
  }
  uint8_t ononce[kNonceSize];
  // NULL algorithm handle requires BCRYPT_USE_SYSTEM_PREFERRED_RNG; without
  // the flag BCryptGenRandom fails with STATUS_INVALID_HANDLE and every
  // handshake (real IO path) would abort right after HELLO (M0 elevated-gate
  // bug; caught by the in-process loopback selftest).
  NTSTATUS rng = BCryptGenRandom(nullptr, ononce, static_cast<ULONG>(kNonceSize),
                                 BCRYPT_USE_SYSTEM_PREFERRED_RNG);
  if (!BCRYPT_SUCCESS(rng)) {
    XNC_LOG_ERROR("handshake: BCryptGenRandom failed status=0x%08lX", (unsigned long)rng);
    return false;
  }
  uint8_t proof[kProofSize];
  if (!HmacSha256(secret, secret_len, cnonce, kNonceSize, proof)) {
    XNC_LOG_ERROR("handshake: HMAC failed");
    return false;
  }
  Frame hp{0, kMsgHelloProof, 0, EncodeHelloProof(GetCurrentProcessId(), ononce, proof)};
  if (!WriteFrameTimed(io, hp)) {
    XNC_LOG_INFO("handshake: send HELLO_PROOF failed, disconnecting");
    return false;
  }
  if (ReadFrameTimed(io, f) != DecodeResult::Ok || f.message_type != kMsgProof) {
    XNC_LOG_INFO("handshake: expected PROOF, disconnecting");
    return false;
  }
  uint8_t cproof[kProofSize], want[kProofSize];
  if (DecodeProof(f, cproof) != DecodeResult::Ok ||
      !HmacSha256(secret, secret_len, ononce, kNonceSize, want)) {
    XNC_LOG_INFO("handshake: malformed PROOF, disconnecting");
    return false;
  }
  uint8_t diff = 0;  // constant-time compare of the two proofs
  for (size_t i = 0; i < kProofSize; i++) diff |= want[i] ^ cproof[i];
  if (diff != 0) {
    XNC_LOG_ERROR("handshake: client proof rejected, disconnecting");
    return false;
  }
  *client_pid = pid;
  return true;
}

// Established-connection frame loop: PING -> PONG(FlagResponse, same
// RequestID); anything else is logged and answered with a FlagError frame
// carrying the same type/id; BYE or IO end closes the connection.
void ServeFrames(TimedIo& io, Watchdog* wd, uint32_t client_pid) {
  XNC_LOG_INFO("connection established (client pid=%lu)", client_pid);
  for (;;) {
    Frame f;
    DecodeResult r = ReadFrameTimed(io, f);
    wd->Heartbeat();
    if (r != DecodeResult::Ok) {
      if (!g_stop.load()) {
        XNC_LOG_INFO("client pid=%lu disconnected (decode=%d)", client_pid, (int)r);
      }
      return;
    }
    switch (f.message_type) {
      case kMsgPing: {
        Frame pong{kFlagResponse, kMsgPong, f.request_id, {}};
        if (!WriteFrameTimed(io, pong)) return;
        break;
      }
      case kMsgStartCapture: {
        // Note: the handler holds g_capture.mu across the ~2s pipe wait;
        // connections are served serially (single-instance accept loop), so
        // only the detached watcher can briefly contend.
        Frame resp = HandleStartCapture(f, wd);
        wd->Heartbeat();
        if (!WriteFrameTimed(io, resp)) return;
        break;
      }
      case kMsgStopCapture: {
        Frame resp = HandleStopCapture(f);
        if (!WriteFrameTimed(io, resp)) return;
        break;
      }
      case kMsgSas: {
        // Task 4: gated SendSAS. client_pid comes from the mutual-HMAC
        // handshake (only secret holders get this far).
        Frame resp = HandleSas(f, client_pid);
        wd->Heartbeat();
        if (!WriteFrameTimed(io, resp)) return;
        break;
      }
      case kMsgBye:
        XNC_LOG_INFO("client pid=%lu said BYE", client_pid);
        return;
      default: {
        XNC_LOG_INFO("unknown message type=0x%04x from pid=%lu, replying FlagError",
                     f.message_type, client_pid);
        Frame err{kFlagResponse | kFlagError, f.message_type, f.request_id, {}};
        if (!WriteFrameTimed(io, err)) return;
        break;
      }
    }
  }
}

}  // namespace

// ---- M2-Slice1 Task 4 public surface (pipe_server.h declarations) ----

WtsMonitor& CoreWts() { return g_wts; }

// Pure table: reuse the live child only when it sits in the session that
// is STILL the active console session; a live child elsewhere is stale
// (console moved under it) and must be terminated + respawned; no live
// child means a plain fresh spawn. Selftest table-drives all rows.
CaptureReuse DecideCaptureReuse(bool child_valid, bool child_running,
                                uint32_t child_session,
                                uint32_t active_session) {
  if (!child_valid || !child_running) return CaptureReuse::kFresh;
  if (child_session == active_session) return CaptureReuse::kReuse;
  return CaptureReuse::kRespawnStaleSession;
}

// Monitor-thread callback: the active console session changed - a capture
// child spawned into the old session is capturing the wrong desktop.
// Terminate it (scoped by stored handle; the watcher reaps + closes) and
// clear the state so the next StartCapture re-spawns into the new session.
// May briefly block on g_capture.mu behind an in-flight StartCapture
// (bounded by the pipe-ready wait).
void OnActiveConsoleSessionChanged(const WtsSessionChange& change) {
  // session_changed from/to/reason is already logged by the monitor.
  std::lock_guard<std::mutex> lk(g_capture.mu);
  if (g_capture.valid && g_capture.child != nullptr) {
    XNC_LOG_INFO(
        "session_changed: terminating capture child pid=%lu (child "
        "session=%u, console now %u); next start_capture respawns",
        g_capture.pid, g_capture.session, change.to);
    TerminateCaptureChildLocked(g_capture.pid, g_capture.child);
    g_capture.valid = false;
  }
}

void SetSasAllowed(bool allow) { g_sas_allowed.store(allow); }
bool SasAllowed() { return g_sas_allowed.load(); }

bool SasAuditLast(SasAuditEvent* out) {
  std::lock_guard<std::mutex> lk(g_sas_audit_mu);
  if (out != nullptr) *out = g_sas_audit_last;
  return g_sas_audit_count > 0;
}

uint32_t SasAuditCount() {
  std::lock_guard<std::mutex> lk(g_sas_audit_mu);
  return g_sas_audit_count;
}

void SetSasSendForTest(SasSendFn fn) { g_sas_send = fn; }
void SetSasResolveForTest(SasResolveFn fn) { g_sas_resolve = fn; }
void SetCaptureSpawnForTest(CaptureSpawnFn fn) { g_capture_spawn_fn = fn; }
void SetTerminateForTest(BOOL(WINAPI* fn)(HANDLE, UINT)) {
  g_terminate_process = fn;
}

// [u32 pid][u16 nameLen][name utf8 bytes][32B secret][u32 gen]; the pipe
// name is program-constructed ASCII, so the "utf8" pass is a plain copy.
// Declared in pipe_server.h for the selftest's byte-level layout check.
Frame EncodeStartCaptureOk(const Frame& req, DWORD pid, const std::wstring& pipe,
                           const uint8_t* secret, uint32_t gen) {
  std::string name;
  for (wchar_t c : pipe) name.push_back(c < 128 ? static_cast<char>(c) : '?');
  std::vector<uint8_t> p;
  p.reserve(6 + name.size() + kDesktopSecretLen + 4);
  auto put32 = [&p](uint32_t v) {
    for (int i = 0; i < 4; i++) p.push_back(static_cast<uint8_t>(v >> (8 * i)));
  };
  put32(pid);
  p.push_back(static_cast<uint8_t>(name.size()));
  p.push_back(static_cast<uint8_t>(name.size() >> 8));  // u16 LE nameLen
  p.insert(p.end(), name.begin(), name.end());
  p.insert(p.end(), secret, secret + kDesktopSecretLen);
  put32(gen);
  return Frame{kFlagResponse, kMsgStartCapture, req.request_id, std::move(p)};
}

// Serve one already-accepted overlapped pipe instance: the mutual-proof
// handshake, then the PING/PONG frame loop until the client disconnects.
// This is RunPipeServer's exact per-connection path, exposed so the
// selftest loopback can drive it in-process against a permissive
// test-only DACL pipe (production DACL is spec 9.1, untouched).
bool ServeConnection(HANDLE pipe, Watchdog* wd, const uint8_t* secret,
                     size_t secret_len, uint32_t* client_pid) {
  TimedIo io(pipe, wd);
  if (!io.Ok()) {
    XNC_LOG_ERROR("CreateEvent failed err=%lu", GetLastError());
    return false;
  }
  *client_pid = 0;
  if (!ServerHandshake(io, secret, secret_len, client_pid)) return false;
  ServeFrames(io, wd, *client_pid);
  return true;
}

int RunPipeServer(const wchar_t* pipe_name, const uint8_t* secret, size_t secret_len) {
  SetConsoleCtrlHandler(OnCtrlEvent, TRUE);  // best effort; loop polls g_stop
  Watchdog wd;
  wd.Start();
  XNC_LOG_INFO("console pipe server starting (pipe=%ls)", pipe_name);

  // Task 4: WTS monitor (notification + 500ms poll hybrid). On active
  // console session change the callback terminates a running capture child
  // (scoped by stored handle) so the next StartCapture re-spawns into the
  // new session; notifications() reports which leg registered.
  {
    WtsMonitor::Opts mo;  // defaults: 500ms poll, notifications on
    mo.on_change = OnActiveConsoleSessionChanged;
    g_wts.Configure(mo);
    g_wts.Start();
    // notifications() is still 0 here by design - the monitor thread
    // registers asynchronously; the wts_monitor "registered ..." (or
    // fallback) log line is the authoritative leg report.
    XNC_LOG_INFO("wts monitor active console session=%lu",
                 static_cast<unsigned long>(g_wts.console_session()));
  }

  PSECURITY_DESCRIPTOR sd = nullptr;
  if (!ConvertStringSecurityDescriptorToSecurityDescriptorW(
          kSddl, SDDL_REVISION_1, &sd, nullptr)) {
    XNC_LOG_ERROR("SDDL parse failed err=%lu", GetLastError());
    return 1;
  }
  SECURITY_ATTRIBUTES sa{sizeof(sa), sd, FALSE};

  int rc = 1;  // 0 on Ctrl+C; 1 stays for fatal errors / watchdog exit(1)
  for (;;) {
    HANDLE pipe = CreateNamedPipeW(
        pipe_name,
        PIPE_ACCESS_DUPLEX | FILE_FLAG_FIRST_PIPE_INSTANCE | FILE_FLAG_OVERLAPPED,
        PIPE_TYPE_BYTE | PIPE_READMODE_BYTE | PIPE_WAIT, 1, kBufSize, kBufSize,
        0, &sa);
    if (pipe == INVALID_HANDLE_VALUE) {
      XNC_LOG_ERROR("CreateNamedPipe failed err=%lu (name taken by another instance?)",
                    GetLastError());
      break;
    }
    XNC_LOG_INFO("listening on %ls", pipe_name);

    // Overlapped accept with heartbeat slices (idle server stays alive).
    HANDLE ev = CreateEventW(nullptr, TRUE, FALSE, nullptr);
    OVERLAPPED ov{};
    ov.hEvent = ev;
    BOOL connected = ConnectNamedPipe(pipe, &ov);
    DWORD err = connected ? 0 : GetLastError();
    bool ok = connected || err == ERROR_PIPE_CONNECTED;
    if (!ok && err == ERROR_IO_PENDING && ev) {
      for (;;) {
        DWORD w = WaitForSingleObject(ev, kWaitSliceMs);
        if (w == WAIT_OBJECT_0) {
          DWORD unused = 0;
          ok = GetOverlappedResult(pipe, &ov, &unused, FALSE) != 0;
          break;
        }
        if (w != WAIT_TIMEOUT) break;
        wd.Heartbeat();
        if (g_stop.load()) {
          CancelIoEx(pipe, &ov);
          break;
        }
      }
    }
    if (ev) CloseHandle(ev);
    if (g_stop.load()) {
      CloseHandle(pipe);
      rc = 0;
      break;
    }
    if (!ok) {
      XNC_LOG_ERROR("ConnectNamedPipe failed err=%lu", GetLastError());
      CloseHandle(pipe);
      break;
    }

    wd.Heartbeat();
    XNC_LOG_INFO("client connected");
    {
      uint32_t client_pid = 0;
      ServeConnection(pipe, &wd, secret, secret_len, &client_pid);
    }
    FlushFileBuffers(pipe);  // best-effort drain before teardown
    DisconnectNamedPipe(pipe);
    CloseHandle(pipe);
    XNC_LOG_INFO("connection closed, accepting next");
  }

  LocalFree(sd);
  // Stop the monitor first (its callback takes g_capture.mu; with no
  // connection alive that is uncontended).
  g_wts.Stop();
  // No orphan capture children: if one is still valid at shutdown,
  // terminate it (the watcher thread reaps + closes the handle).
  {
    std::lock_guard<std::mutex> lk(g_capture.mu);
    if (g_capture.valid && g_capture.child != nullptr) {
      XNC_LOG_INFO("shutdown: terminating capture child pid=%lu", g_capture.pid);
      TerminateCaptureChildLocked(g_capture.pid, g_capture.child);
      g_capture.valid = false;
    }
  }
  if (rc == 0) XNC_LOG_INFO("shutting down (Ctrl+C)");
  return rc;
}

}  // namespace xnc
