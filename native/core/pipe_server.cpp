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
#include <deque>
#include <map>
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

// ---- M2-Slice2 Task 3 state: shell children + desktop supervision ----
// Shell children (xnc-shell.exe): pid -> owned process handle; one detached
// watcher per spawn reaps + closes. Shells are session-scoped workers and
// are NEVER restarted - their exit is logged (worker_exited kind=shell) and
// the entry dropped; KillShell terminates scoped by the stored handle.
struct ShellHost {
  std::mutex mu;
  std::map<DWORD, HANDLE> children;  // pid -> owned handle
};
ShellHost g_shells;

// RunPipeServer's watchdog, published for the detached desktop-restart
// threads (heartbeat across their backoff sleeps).
Watchdog* g_wd = nullptr;

// Desktop supervision bookkeeping (spec 15.2; guarded by g_capture.mu):
// consecutive-crash backoff index, rolling exit timestamps and the
// crash-loop "degraded" lock. Once degraded, EVERY desktop (re)spawn uses
// the degraded args (--backend gdi --encoder software).
struct DesktopSupervisor {
  uint32_t crash_index = 0;              // consecutive crashes (backoff input)
  std::deque<uint64_t> exit_ms;          // rolling exit timestamps (GetTickCount64)
  uint64_t last_spawn_ms = 0;            // uptime anchor for backoff reset
  uint32_t restart_epoch = 0;            // bumped on every deliberate kill
  bool degraded = false;                 // crash-loop lock (5 exits / 60s)
  bool restart_pending = false;          // a backoff restart thread is armed
};
DesktopSupervisor g_desktop;

// Seams (selftest injection; null = production default below).
ShellTokenFn g_shell_token_fn = nullptr;
ShellSpawnFn g_shell_spawn_fn = nullptr;
SnapshotSpawnFn g_snapshot_spawn_fn = nullptr;

SnapshotSpawnResult RealSnapshotSpawn(uint32_t session, uint32_t max_w);
SnapshotSpawnFn CurrentSnapshotSpawnFn() {
  return g_snapshot_spawn_fn != nullptr ? g_snapshot_spawn_fn
                                        : &RealSnapshotSpawn;
}


bool RealShellToken(uint32_t session, uint8_t token_kind, HANDLE* out);
ShellSpawnResult RealShellSpawn(const ShellCreateReq& req, uint32_t session,
                                const wchar_t* pipe_name, const uint8_t* secret,
                                HANDLE token, Watchdog* wd);

ShellTokenFn CurrentShellTokenFn() {
  return g_shell_token_fn != nullptr ? g_shell_token_fn : &RealShellToken;
}
ShellSpawnFn CurrentShellSpawnFn() {
  return g_shell_spawn_fn != nullptr ? g_shell_spawn_fn : &RealShellSpawn;
}

CaptureSpawnResult RealCaptureSpawn(uint32_t session, const wchar_t* pipe_name,
                                    const uint8_t* secret, Watchdog* wd,
                                    bool degraded);
ShellSpawnResult RealShellSpawn(const ShellCreateReq& req, uint32_t session,
                                const wchar_t* pipe_name, const uint8_t* secret,
                                HANDLE token, Watchdog* wd);
bool RealShellToken(uint32_t session, uint8_t token_kind, HANDLE* out);

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
  // Deliberate kill: bump the epoch so the watcher does NOT arm a restart
  // (backoff restarts are for crashes only, spec 15.2).
  g_desktop.restart_epoch += 1;
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

// Watcher: waits out the child, logs worker_exited, clears the shared
// state if it still describes this spawn, and closes the handle it owns.
// Detached - one per spawn, outlives the connection that requested it.
// Task 3 (M2-Slice2): a NON-deliberate exit (crash; spawn_epoch still
// current) arms the supervised restart with backoff; crash-loop (5 exits
// / 60s) locks the desktop to the degraded args. spawn_epoch is the
// restart epoch captured at spawn time - every deliberate termination
// (StopCapture / session change / shutdown) bumps g_desktop.restart_epoch
// first, which is what tells the watcher "do not restart".
// Supervised desktop restart loop (detached thread): sleep the backoff,
// re-spawn (degraded args once crash-loop locked), retry on spawn failure.
void DesktopRestartLoop(uint32_t spawn_epoch, uint32_t session,
                        std::wstring pipe_name, std::vector<uint8_t> secret);
void WatchCaptureChild(HANDLE child, DWORD pid, uint32_t spawn_epoch) {
  WaitForSingleObject(child, INFINITE);
  DWORD code = 0;
  if (!GetExitCodeProcess(child, &code)) code = 1;
  XNC_LOG_INFO("worker_exited kind=desktop pid=%lu exit=%lu", pid, code);

  uint32_t session = 0;
  std::wstring pipe_name;
  uint8_t secret[kDesktopSecretLen] = {0};
  bool arm_restart = false;
  const uint64_t now = GetTickCount64();
  {
    std::lock_guard<std::mutex> lk(g_capture.mu);
    session = g_capture.session;
    pipe_name = g_capture.pipe_name;
    std::memcpy(secret, g_capture.secret, kDesktopSecretLen);
    if (g_capture.child == child) {
      g_capture.valid = false;
      g_capture.child = nullptr;
      g_capture.pid = 0;
      g_capture.session = 0;
    }
    if (spawn_epoch == g_desktop.restart_epoch) {
      // Genuine crash: rolling crash-loop accounting + backoff index.
      g_desktop.exit_ms.push_back(now);
      while (!g_desktop.exit_ms.empty() &&
             now - g_desktop.exit_ms.front() >= kCrashLoopWindowMs)
        g_desktop.exit_ms.pop_front();
      if (!g_desktop.degraded &&
          CrashLoopReached(
              std::vector<uint64_t>(g_desktop.exit_ms.begin(),
                                    g_desktop.exit_ms.end()), now)) {
        g_desktop.degraded = true;
        XNC_LOG_INFO(
            "crash_loop_degraded kind=desktop exits=%zu window_ms=%zu "
            "(desktop locked to --backend gdi --encoder software)",
            g_desktop.exit_ms.size(), (size_t)kCrashLoopWindowMs);
      }
      // A child that survived longer than the crash-loop window resets the
      // consecutive-crash backoff (spec 15.2 intent: storms back off,
      // one-off crashes after stable uptime do not).
      if (now - g_desktop.last_spawn_ms >= kCrashLoopWindowMs)
        g_desktop.crash_index = 0;
      g_desktop.crash_index += 1;
      if (!g_desktop.restart_pending) {
        g_desktop.restart_pending = true;
        arm_restart = true;
      }
    }
  }
  CloseHandle(child);
  if (arm_restart && session != 0xFFFFFFFF && !pipe_name.empty()) {
    std::thread(DesktopRestartLoop, spawn_epoch, session, pipe_name,
                std::vector<uint8_t>(secret, secret + kDesktopSecretLen))
        .detach();
  }
}

// ASCII error-frame helper (payload = stable code, mirrors Go respText).
Frame ErrorFrame(uint16_t type, uint32_t request_id, const char* code) {
  return Frame{kFlagResponse | kFlagError, type, request_id,
               std::vector<uint8_t>(code, code + std::strlen(code))};
}

// M2-Slice3 Task 3 production snapshot spawn: SessionSystemToken ->
// SpawnInSession("xnc-desktop.exe --jpeg-single <tmp> --max-w <w>") ->
// wait exit (15s) -> read file (<= 8 MiB) -> delete temp. The temp path is
// program-constructed under core's %TEMP% (SYSTEM context; the child runs
// under the same SYSTEM token, so both sides can write it).
SnapshotSpawnResult RealSnapshotSpawn(uint32_t session, uint32_t max_w) {
  SnapshotSpawnResult r{};
  auto fail = [&r](const char* code) {
    snprintf(r.err, sizeof(r.err), "%s", code);
    return r;
  };

  wchar_t tmp_dir[MAX_PATH] = {0};
  const DWORD tl = GetTempPathW(MAX_PATH, tmp_dir);
  if (tl == 0 || tl >= MAX_PATH) return fail("INTERNAL");
  // Temp name carries pid + per-process monotonic seq: concurrent snapshot
  // spawns (multiple sessions / rapid retries) never collide on one path
  // (T3 review fix carried into T4).
  static std::atomic<unsigned> snap_seq{0};
  wchar_t path[MAX_PATH + 64] = {0};
  swprintf(path, MAX_PATH + 64, L"%lsxnc-snap-%lu-%u.jpg", tmp_dir,
           GetCurrentProcessId(), snap_seq.fetch_add(1) + 1);

  HANDLE token = nullptr;
  std::string err;
  if (!TokenManager::SessionSystemToken(session, &token, &err)) {
    XNC_LOG_ERROR("snapshot: session token failed err=\"%s\"", err.c_str());
    return fail("TOKEN_FAILED");
  }

  wchar_t maxw[16] = {0};
  swprintf(maxw, 16, L"%u", max_w);
  std::vector<std::wstring> args = {L"--jpeg-single", path};
  if (max_w > 0) {
    args.push_back(L"--max-w");
    args.push_back(maxw);
  }
  // --log-file: same single log channel as the RT desktop spawn - service
  // spawns have no console (2026-08-24 observability incident follow-up).
  // Snapshot child shares xnc-desktop.log with the RT child (append mode).
  args.push_back(L"--log-file");
  args.push_back(JoinSiblingPath(OwnModuleDir(), L"xnc-desktop.log"));
  std::vector<wchar_t*> av;
  for (auto& a : args) av.push_back(&a[0]);
  std::wstring cmd;
  std::string cmd_err;
  if (!BuildChildCommandLine(L"xnc-desktop.exe", static_cast<int>(av.size()),
                             av.data(), 0, &cmd, &cmd_err)) {
    CloseHandle(token);
    XNC_LOG_ERROR("snapshot: cmdline rejected err=\"%s\"", cmd_err.c_str());
    return fail("INTERNAL");
  }

  DWORD pid = 0;
  HANDLE child = nullptr;
  if (!SpawnInSession(token, L"xnc-desktop.exe", cmd.c_str(), &pid, &child,
                      &err)) {
    CloseHandle(token);
    XNC_LOG_ERROR("snapshot: spawn failed err=\"%s\"", err.c_str());
    return fail("SPAWN_FAILED");
  }
  CloseHandle(token);

  // 15s wait budget, sliced for watchdog heartbeats (spec 15 pattern).
  const ULONGLONG deadline = GetTickCount64() + 15000;
  bool exited = false;
  DWORD exit_code = 1;
  for (;;) {
    const DWORD w = WaitForSingleObject(child, 1000);
    if (w == WAIT_OBJECT_0) {
      exited = true;
      GetExitCodeProcess(child, &exit_code);
      break;
    }
    if (w != WAIT_TIMEOUT) break;
    if (g_wd != nullptr) g_wd->Heartbeat();
    if (GetTickCount64() >= deadline) break;
  }
  if (!exited) {
    TerminateProcess(child, 1);
    // reap the deliberate kill
    WaitForSingleObject(child, 3000);
    CloseHandle(child);
    DeleteFileW(path);
    XNC_LOG_ERROR("snapshot: child pid=%lu timed out", pid);
    return fail("SNAPSHOT_TIMEOUT");
  }
  CloseHandle(child);
  if (exit_code != 0) {
    DeleteFileW(path);
    XNC_LOG_ERROR("snapshot: child pid=%lu exit=%lu", pid, exit_code);
    return fail("SNAPSHOT_FAILED");
  }

  // Read the JPEG (cap: 8 MiB payload budget inside the 9 MiB frame cap).
  HANDLE f = CreateFileW(path, GENERIC_READ, FILE_SHARE_READ, nullptr,
                         OPEN_EXISTING, 0, nullptr);
  if (f == INVALID_HANDLE_VALUE) {
    XNC_LOG_ERROR("snapshot: open temp failed err=%lu", GetLastError());
    DeleteFileW(path);
    return fail("SNAPSHOT_FAILED");
  }
  LARGE_INTEGER sz{};
  bool too_large = false;
  if (GetFileSizeEx(f, &sz) && sz.QuadPart > 0 &&
      sz.QuadPart <= (LONGLONG)8 * 1024 * 1024) {
    r.jpeg.resize(static_cast<size_t>(sz.QuadPart));
    DWORD got = 0;
    if (ReadFile(f, r.jpeg.data(), static_cast<DWORD>(r.jpeg.size()), &got,
                 nullptr) &&
        got == r.jpeg.size()) {
      r.ok = true;
    }
  } else {
    too_large = true;
  }
  CloseHandle(f);
  DeleteFileW(path);
  if (!r.ok) {
    r.jpeg.clear();
    return fail(too_large ? "SNAPSHOT_TOO_LARGE" : "SNAPSHOT_FAILED");
  }
  XNC_LOG_INFO("snapshot ok session=%u max_w=%u bytes=%zu", session, max_w,
               r.jpeg.size());
  return r;
}

// 0x0111: decode + session validate + the (injectable) one-shot spawn.
Frame HandleSnapshot(const Frame& req, Watchdog* wd) {
  SnapshotReq sr;
  if (!DecodeSnapshotPayload(req.payload.data(), req.payload.size(), &sr) ||
      sr.max_w > kSnapshotMaxWCap)
    return ErrorFrame(kMsgSnapshot, req.request_id, "BAD_PAYLOAD");
  const uint32_t wts = sr.wts == 0xFFFFFFFFu ? CoreWts().console_session()
                                             : sr.wts;
  if (!SessionTargetAllowed(wts, CoreWts().console_session()))
    return ErrorFrame(kMsgSnapshot, req.request_id, "SESSION_MISMATCH");
  wd->Heartbeat();
  const SnapshotSpawnResult r = CurrentSnapshotSpawnFn()(wts, sr.max_w);
  if (!r.ok) {
    XNC_LOG_ERROR("snapshot failed code=%s", r.err);
    return ErrorFrame(kMsgSnapshot, req.request_id, r.err);
  }
  return EncodeSnapshotResp(req, r.jpeg.data(), r.jpeg.size());
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
                                    const uint8_t* secret, Watchdog* wd,
                                    bool degraded) {
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
  // feat/rt-scale (hw-encode task 2 Part B): downscale wide sources to
  // 1920 before encode - the software MFT ceiling at 3440x1440 is ~20 fps,
  // at 1920 it clears 30 fps; the hardware ladder stays in front either way.
  args.push_back(L"--max-w");
  args.push_back(L"1920");
  // --log-file: desktop opens its own log (service spawns have no console;
  // inherited-stdio redirection proved unreliable — 2026-08-24 incident).
  args.push_back(L"--log-file");
  args.push_back(JoinSiblingPath(OwnModuleDir(), L"xnc-desktop.log"));
  if (degraded) {
    // Crash-loop lock (spec 15.2): GDI capture + software encoder only.
    args.push_back(L"--backend");
    args.push_back(L"gdi");
    args.push_back(L"--encoder");
    args.push_back(L"software");
  }
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

// Supervised desktop restart (spec 15.2): sleep WorkerBackoffMs(crashes),
// re-spawn via the CURRENT spawn seam (degraded args once locked), retry
// when the re-spawn itself fails (each failure counts as a crash, so the
// backoff keeps growing up to the 60s cap). Bails out on Ctrl+C/shutdown,
// on a deliberate stop (epoch moved on) or when an agent-driven
// StartCapture re-spawned first (g_capture.valid).
void DesktopRestartLoop(uint32_t spawn_epoch, uint32_t session,
                        std::wstring pipe_name, std::vector<uint8_t> secret) {
  for (;;) {
    uint32_t delay = 0, idx = 0;
    bool degraded = false;
    {
      std::lock_guard<std::mutex> lk(g_capture.mu);
      idx = g_desktop.crash_index;
      degraded = g_desktop.degraded;
    }
    delay = WorkerBackoffMs(idx);
    XNC_LOG_INFO("desktop restart in %lums (crash #%lu%s)",
                 static_cast<unsigned long>(delay),
                 static_cast<unsigned long>(idx), degraded ? " degraded" : "");
    for (uint32_t slept = 0; slept < delay && !g_stop.load(); slept += 200)
      Sleep(200);
    if (g_stop.load()) {
      std::lock_guard<std::mutex> lk(g_capture.mu);
      g_desktop.restart_pending = false;
      return;
    }
    if (g_wd != nullptr) g_wd->Heartbeat();

    Watchdog* wd = g_wd;  // may predate Start() in the selftest loopback
    const CaptureSpawnResult r =
        CurrentSpawnFn()(session, pipe_name.c_str(), secret.data(), wd,
                         degraded);
    std::lock_guard<std::mutex> lk(g_capture.mu);
    if (!r.ok) {
      XNC_LOG_ERROR("desktop restart spawn failed code=%s", r.err);
      // counts as another crash: loop back around with a grown backoff
      const uint64_t now = GetTickCount64();
      g_desktop.exit_ms.push_back(now);
      while (!g_desktop.exit_ms.empty() &&
             now - g_desktop.exit_ms.front() >= kCrashLoopWindowMs)
        g_desktop.exit_ms.pop_front();
      if (!g_desktop.degraded &&
          CrashLoopReached(std::vector<uint64_t>(g_desktop.exit_ms.begin(),
                                                 g_desktop.exit_ms.end()),
                           now)) {
        g_desktop.degraded = true;
        XNC_LOG_INFO(
            "crash_loop_degraded kind=desktop exits=%zu window_ms=%zu "
            "(desktop locked to --backend gdi --encoder software)",
            g_desktop.exit_ms.size(), (size_t)kCrashLoopWindowMs);
      }
      g_desktop.crash_index += 1;
      continue;  // retry (lock released by loop iteration)
    }
    if (g_desktop.restart_epoch != spawn_epoch || g_capture.valid) {
      // Deliberate stop or a racing StartCapture owns the slot now: drop
      // the fresh child (no watcher exists yet - terminate + close here).
      XNC_LOG_INFO("desktop restart superseded, killing fresh pid=%lu", r.pid);
      TerminateProcess(r.child, 1);
      CloseHandle(r.child);
      g_desktop.restart_pending = false;
      return;
    }
    g_capture.valid = true;
    g_capture.pid = r.pid;
    g_capture.child = r.child;
    g_capture.session = session;
    g_capture.gen += 1;
    std::memcpy(g_capture.secret, secret.data(), kDesktopSecretLen);
    g_capture.pipe_name = pipe_name;
    g_desktop.last_spawn_ms = GetTickCount64();
    XNC_LOG_INFO("desktop restart spawned pid=%lu pipe=%ls gen=%u session=%u%s",
                 r.pid, pipe_name.c_str(), g_capture.gen, session,
                 degraded ? " degraded" : "");
    g_desktop.restart_pending = false;
    std::thread(WatchCaptureChild, r.child, r.pid, g_desktop.restart_epoch)
        .detach();
    return;
  }
}

// ---- 0x0120/0x0121 CreateShell / KillShell (M2-Slice2 Task 3) ----

// Shell watcher: reap + log + drop the registry entry. Shells are
// session-scoped workers and are NOT restarted (their client observes the
// exit via the shell pipe dying).
void WatchShellChild(DWORD pid, HANDLE child) {
  WaitForSingleObject(child, INFINITE);
  DWORD code = 0;
  if (!GetExitCodeProcess(child, &code)) code = 1;
  XNC_LOG_INFO("worker_exited kind=shell pid=%lu exit=%lu", pid, code);
  {
    std::lock_guard<std::mutex> lk(g_shells.mu);
    auto it = g_shells.children.find(pid);
    if (it != g_shells.children.end() && it->second == child)
      g_shells.children.erase(it);
  }
  CloseHandle(child);
}

// UTF-8 (wire payload) -> UTF-16 (argv) conversion; '?' fallback mirrors
// EncodeStartCaptureOk's narrow pass for unpaired sequences.
std::wstring WidenUtf8(const std::string& s) {
  if (s.empty()) return L"";
  int n = MultiByteToWideChar(CP_UTF8, 0, s.data(), static_cast<int>(s.size()),
                              nullptr, 0);
  if (n <= 0) return L"";
  std::wstring w(static_cast<size_t>(n), L'\0');
  MultiByteToWideChar(CP_UTF8, 0, s.data(), static_cast<int>(s.size()), &w[0],
                      n);
  return w;
}

// Production token mint (spec 8.4): user kind = WTSQueryUserToken on the
// LIVE active console session (needs SYSTEM core; no live user logon ->
// false -> NO_ACTIVE_SESSION, never implicit elevation); system kind =
// SessionSystemToken, same as the desktop worker.
bool RealShellToken(uint32_t session, uint8_t token_kind, HANDLE* out) {
  if (out == nullptr) return false;
  *out = nullptr;
  if (token_kind == 0) {
    if (WTSQueryUserToken(session, out) == FALSE) {
      XNC_LOG_ERROR("create_shell: WTSQueryUserToken(%u) failed err=%lu",
                    session, GetLastError());
      return false;
    }
    return true;
  }
  std::string err;
  if (!TokenManager::SessionSystemToken(session, out, &err)) {
    XNC_LOG_ERROR("create_shell: session token failed err=\"%s\"", err.c_str());
    return false;
  }
  return true;
}

// Production shell spawn: xnc-shell.exe (whitelisted next to xnc-core.exe)
// with the secret on inherited stdin (spec 1.5 - never argv). cwd/env/cmd
// are operator data, not secrets: they ride argv as individual entries via
// SpawnInSession's arg list (quoting handled by BuildChildCommandLine,
// which rejects embedded quotes / trailing backslashes - surfacing as
// INTERNAL here). The shell resolves its own profile exe (core passes the
// whitelist NAME, never a path).
ShellSpawnResult RealShellSpawn(const ShellCreateReq& req, uint32_t session,
                                const wchar_t* pipe_name, const uint8_t* secret,
                                HANDLE token, Watchdog* wd) {
  ShellSpawnResult r{};

  HANDLE sec_rd = nullptr, sec_wr = nullptr;
  SECURITY_ATTRIBUTES inherit_sa{sizeof(inherit_sa), nullptr, TRUE};
  if (!CreatePipe(&sec_rd, &sec_wr, &inherit_sa, 0) ||
      !SetHandleInformation(sec_wr, HANDLE_FLAG_INHERIT, 0)) {
    const DWORD pipe_err = GetLastError();
    if (sec_rd) CloseHandle(sec_rd);
    if (sec_wr) CloseHandle(sec_wr);
    XNC_LOG_ERROR("create_shell: secret stdin pipe failed err=%lu", pipe_err);
    snprintf(r.err, sizeof(r.err), "%s", "INTERNAL");
    return r;
  }

  wchar_t cols[8], rows[8], timeout[12];
  swprintf(cols, 8, L"%u", req.cols);
  swprintf(rows, 8, L"%u", req.rows);
  swprintf(timeout, 12, L"%u", req.timeout_sec);
  std::vector<std::wstring> args = {L"--pipe",         pipe_name,
                                    L"--secret-stdin",
                                    L"--profile",       ShellProfileWName(req.profile),
                                    L"--mode",          req.mode == 1 ? L"oneshot" : L"interactive",
                                    L"--cols",          cols,
                                    L"--rows",          rows};
  const std::wstring cwd_w = WidenUtf8(req.cwd);
  const std::wstring cmd_w = WidenUtf8(req.cmd);
  if (!cwd_w.empty()) {
    args.push_back(L"--cwd");
    args.push_back(cwd_w);
  }
  for (const auto& kv : SplitShellEnv(req.env)) {
    args.push_back(L"--env");
    args.push_back(WidenUtf8(kv));
  }
  if (!cmd_w.empty()) {
    args.push_back(L"--command");
    args.push_back(cmd_w);
  }
  args.push_back(L"--timeout");
  args.push_back(timeout);
  // --log-file: shell opens its own log (service spawns have no console;
  // inherited-stdio redirection proved unreliable — same single log channel
  // as desktop; xnc-shell.log next to xnc-core.exe).
  args.push_back(L"--log-file");
  args.push_back(JoinSiblingPath(OwnModuleDir(), L"xnc-shell.log"));

  std::vector<wchar_t*> av;
  for (auto& a : args) av.push_back(&a[0]);
  std::wstring cmd;
  std::string cmd_err;
  if (!BuildChildCommandLine(L"xnc-shell.exe", static_cast<int>(av.size()),
                             av.data(), 0, &cmd, &cmd_err)) {
    CloseHandle(sec_rd);
    CloseHandle(sec_wr);
    XNC_LOG_ERROR("create_shell: child cmdline rejected err=\"%s\"",
                  cmd_err.c_str());
    snprintf(r.err, sizeof(r.err), "%s", "BAD_PAYLOAD");
    return r;
  }

  DWORD pid = 0;
  HANDLE child = nullptr;
  std::string err;
  if (!SpawnInSession(token, L"xnc-shell.exe", cmd.c_str(), &pid, &child,
                      &err, sec_rd)) {
    CloseHandle(sec_rd);
    CloseHandle(sec_wr);
    XNC_LOG_ERROR("create_shell: spawn failed err=\"%s\"", err.c_str());
    snprintf(r.err, sizeof(r.err), "%s", "SPAWN_FAILED");
    return r;
  }
  CloseHandle(sec_rd);  // inheritance settled at CreateProcess

  // One line - 64 hex chars + '\n' (65B) - then close (xnc-shell's
  // --secret-stdin contract). Hex/secret never logged.
  {
    char hexline[2 * kDesktopSecretLen + 2] = {0};
    for (size_t i = 0; i < kDesktopSecretLen; i++)
      sprintf_s(hexline + 2 * i, 3, "%02x", secret[i]);
    hexline[2 * kDesktopSecretLen] = '\n';
    DWORD wrote = 0;
    if (!WriteFile(sec_wr, hexline, 2 * kDesktopSecretLen + 1, &wrote,
                   nullptr) ||
        wrote != 2 * kDesktopSecretLen + 1) {
      XNC_LOG_ERROR("create_shell: secret stdin write failed err=%lu",
                    GetLastError());
      // the child exits on its stdin error; the pipe wait reaps below
    }
    CloseHandle(sec_wr);
  }

  if (wd != nullptr) wd->Heartbeat();
  if (!WaitPipeReady(pipe_name, wd)) {
    XNC_LOG_ERROR("create_shell: shell pipe not ready in %lums, killing pid=%lu",
                  kPipeReadyWaitMs, pid);
    TerminateProcess(child, 1);
    CloseHandle(child);
    snprintf(r.err, sizeof(r.err), "%s", "PIPE_TIMEOUT");
    return r;
  }

  r.ok = true;
  r.pid = pid;
  r.child = child;
  return r;
}

// 0x0120: validate + mint token + spawn xnc-shell.exe + answer its pipe
// descriptor. Error ladder: BAD_PAYLOAD (layout/enum/mode/malformed env) ->
// SESSION_MISMATCH (wts neither the live console session nor the 0xFFFFFFFF
// "live active" sentinel) -> RNG_FAILED -> NO_ACTIVE_SESSION (user kind, no
// live user token; never implicit elevation, spec 8.4) / TOKEN_FAILED
// (system kind) -> SPAWN_FAILED / PIPE_TIMEOUT.
Frame HandleCreateShell(const Frame& req, Watchdog* wd) {
  ShellCreateReq sr;
  if (!DecodeShellCreatePayload(req.payload.data(), req.payload.size(), &sr) ||
      !ShellProfileAllowed(sr.profile) || !ShellTokenKindAllowed(sr.token_kind) ||
      sr.mode > 1 || (sr.mode == 1 && sr.cmd.empty()))
    return ErrorFrame(kMsgCreateShell, req.request_id, "BAD_PAYLOAD");
  // Env blob contract (T3 review): the producer joins "K=V" entries with
  // '\n' and rejects values containing '\n' itself (a newline inside a
  // value is unrepresentable). Core-side guard: every split segment must
  // carry a key - an embedded newline splitting a value yields a bare
  // segment that must be rejected here, never silently passed through.
  for (const auto& kv : SplitShellEnv(sr.env)) {
    if (kv.find('=') == std::string::npos)
      return ErrorFrame(kMsgCreateShell, req.request_id, "BAD_PAYLOAD");
  }
  // 0xFFFFFFFF = "live active console" sentinel (documented agent<->core
  // contract; the agent cannot know wts from its session context): resolve
  // to the live console session before the target check.
  uint32_t wts = sr.wts == 0xFFFFFFFFu ? CoreWts().console_session() : sr.wts;
  if (!SessionTargetAllowed(wts, CoreWts().console_session()))
    return ErrorFrame(kMsgCreateShell, req.request_id, "SESSION_MISMATCH");

  uint8_t secret[kDesktopSecretLen];
  NTSTATUS rng = BCryptGenRandom(nullptr, secret, sizeof(secret),
                                 BCRYPT_USE_SYSTEM_PREFERRED_RNG);
  if (!BCRYPT_SUCCESS(rng)) {
    XNC_LOG_ERROR("create_shell: secret RNG failed status=0x%08lX",
                  (unsigned long)rng);
    return ErrorFrame(kMsgCreateShell, req.request_id, "RNG_FAILED");
  }

  // Unique pipe per shell: core pid + monotonic counter (the child pid is
  // not known before CreateProcess, and argv is fixed at spawn time).
  static std::atomic<uint32_t> s_shell_counter{0};
  wchar_t pipe_name[96];
  swprintf(pipe_name, 96, L"\\\\.\\pipe\\xnc-shell-%lu-%u",
           GetCurrentProcessId(), s_shell_counter.fetch_add(1) + 1);

  HANDLE token = nullptr;
  if (!CurrentShellTokenFn()(wts, sr.token_kind, &token)) {
    if (token != nullptr) CloseHandle(token);
    return ErrorFrame(kMsgCreateShell, req.request_id,
                      sr.token_kind == 0 ? "NO_ACTIVE_SESSION" : "TOKEN_FAILED");
  }
  ShellSpawnResult r =
      CurrentShellSpawnFn()(sr, wts, pipe_name, secret, token, wd);
  CloseHandle(token);
  if (!r.ok) {
    XNC_LOG_ERROR("create_shell: spawn failed code=%s", r.err);
    return ErrorFrame(kMsgCreateShell, req.request_id, r.err);
  }

  {
    std::lock_guard<std::mutex> lk(g_shells.mu);
    g_shells.children[r.pid] = r.child;
  }
  XNC_LOG_INFO(
      "shell_spawned kind=%s token=%s pid=%lu pipe=%ls mode=%s",
      ShellProfileName(sr.profile), sr.token_kind == 0 ? "user" : "system",
      r.pid, pipe_name, sr.mode == 1 ? "oneshot" : "interactive");
  std::thread(WatchShellChild, r.pid, r.child).detach();
  return EncodeCreateShellOk(req, r.pid, pipe_name, secret);
}

// 0x0121: terminate the shell child scoped by the STORED HANDLE (pid only
// in the log; never by image name - T2 incident class). Idempotent.
Frame HandleKillShell(const Frame& req) {
  if (req.payload.size() != 4)
    return ErrorFrame(kMsgKillShell, req.request_id, "BAD_PAYLOAD");
  const uint32_t pid = GetU32(req.payload.data());
  std::lock_guard<std::mutex> lk(g_shells.mu);
  auto it = g_shells.children.find(pid);
  if (it != g_shells.children.end()) {
    XNC_LOG_INFO("kill_shell pid=%u (scoped by stored handle)", pid);
    if (g_terminate_process != nullptr)
      g_terminate_process(it->second, 1);
    else
      TerminateProcess(it->second, 1);
  } else {
    XNC_LOG_INFO("kill_shell pid=%u unknown (idempotent ok)", pid);
  }
  return Frame{kFlagResponse, kMsgKillShell, req.request_id, {}};
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

  const CaptureSpawnResult r =
      CurrentSpawnFn()(wts, pipe_name, secret, wd, g_desktop.degraded);
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
  g_desktop.last_spawn_ms = GetTickCount64();
  // Secret/hex never logged; pid + pipe name + gen + session only.
  XNC_LOG_INFO("start_capture: spawned pid=%lu pipe=%ls gen=%u session=%u%s",
               r.pid, pipe_name, g_capture.gen, wts,
               g_desktop.degraded ? " degraded" : "");
  std::thread(WatchCaptureChild, r.child, r.pid, g_desktop.restart_epoch)
      .detach();
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
      case kMsgSnapshot: {
        // M2-Slice3 Task 3: one-shot --jpeg-single snapshot (screen
        // retirement lane). Blocking up to ~15s; connections are served
        // on their own threads.
        Frame resp = HandleSnapshot(f, wd);
        wd->Heartbeat();
        if (!WriteFrameTimed(io, resp)) return;
        break;
      }
      case kMsgCreateShell: {
        // M2-Slice2 Task 3: spawn xnc-shell.exe (user or SYSTEM token) and
        // answer its pipe descriptor; same ~2s pipe-wait-under-lock note as
        // StartCapture above.
        Frame resp = HandleCreateShell(f, wd);
        wd->Heartbeat();
        if (!WriteFrameTimed(io, resp)) return;
        break;
      }
      case kMsgKillShell: {
        Frame resp = HandleKillShell(f);
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

void RequestCoreStop() {
  // SCM stop path (M2-Slice3 Task 1): same flag, same drain as Ctrl+C.
  XNC_LOG_INFO("core stop requested (service), stopping");
  g_stop = true;
}

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
void SetShellTokenForTest(ShellTokenFn fn) { g_shell_token_fn = fn; }
void SetShellSpawnForTest(ShellSpawnFn fn) { g_shell_spawn_fn = fn; }
void SetSnapshotSpawnForTest(SnapshotSpawnFn fn) { g_snapshot_spawn_fn = fn; }

// ---- M2-Slice3 Task 3: 0x0111 payload codecs (selftest golden) ----

// [u32 wts][u32 max_w], exactly 8 bytes; trailing/garbage -> false.
bool DecodeSnapshotPayload(const uint8_t* p, size_t n, SnapshotReq* out) {
  if (p == nullptr || out == nullptr || n != 8) return false;
  out->wts = static_cast<uint32_t>(p[0]) | static_cast<uint32_t>(p[1]) << 8 |
             static_cast<uint32_t>(p[2]) << 16 |
             static_cast<uint32_t>(p[3]) << 24;
  out->max_w = static_cast<uint32_t>(p[4]) | static_cast<uint32_t>(p[5]) << 8 |
               static_cast<uint32_t>(p[6]) << 16 |
               static_cast<uint32_t>(p[7]) << 24;
  return true;
}

// [u32 len][jpeg bytes] (little-endian length prefix).
Frame EncodeSnapshotResp(const Frame& req, const uint8_t* jpeg, size_t len) {
  std::vector<uint8_t> p;
  p.reserve(4 + len);
  for (int i = 0; i < 4; i++)
    p.push_back(static_cast<uint8_t>((static_cast<uint32_t>(len)) >> (8 * i)));
  if (jpeg != nullptr && len > 0) p.insert(p.end(), jpeg, jpeg + len);
  return Frame{kFlagResponse, kMsgSnapshot, req.request_id, std::move(p)};
}

// ---- M2-Slice2 Task 3 public surface (pipe_server.h declarations) ----

// Profile whitelist enum (spec 8.2): 0=POWERSHELL 1=PWSH 2=CMD 3=BASH.
// Core never resolves paths - xnc-shell.exe does (it receives the NAME).
static const char* const kShellProfileNames[] = {"POWERSHELL", "PWSH", "CMD",
                                                 "BASH"};
bool ShellProfileAllowed(uint8_t profile) {
  return profile < sizeof(kShellProfileNames) / sizeof(kShellProfileNames[0]);
}
const char* ShellProfileName(uint8_t profile) {
  return ShellProfileAllowed(profile) ? kShellProfileNames[profile] : "?";
}
const wchar_t* ShellProfileWName(uint8_t profile) {
  static const wchar_t* const w[] = {L"POWERSHELL", L"PWSH", L"CMD", L"BASH"};
  return ShellProfileAllowed(profile) ? w[profile] : L"?";
}
bool ShellTokenKindAllowed(uint8_t kind) { return kind <= 1; }

// Bounds-checked LE decode of the 0x0120 request payload. Truncation,
// length overruns or trailing bytes all fail (caller answers BAD_PAYLOAD).
bool DecodeShellCreatePayload(const uint8_t* p, size_t n, ShellCreateReq* out) {
  if (p == nullptr || out == nullptr || n < 21) return false;  // 21 = all-empty minimum
  size_t off = 0;
  auto u8 = [&]() { return p[off++]; };
  auto u16 = [&]() {
    uint16_t v = static_cast<uint16_t>(static_cast<uint16_t>(p[off]) |
                                       static_cast<uint16_t>(p[off + 1]) << 8);
    off += 2;
    return v;
  };
  auto u32 = [&]() {
    uint32_t v = static_cast<uint32_t>(p[off]) |
                 static_cast<uint32_t>(p[off + 1]) << 8 |
                 static_cast<uint32_t>(p[off + 2]) << 16 |
                 static_cast<uint32_t>(p[off + 3]) << 24;
    off += 4;
    return v;
  };
  auto blob = [&](const char** dst, size_t* dst_len) {
    const uint16_t len = u16();
    if (off + len > n) return false;
    *dst = reinterpret_cast<const char*>(p + off);
    *dst_len = len;
    off += len;
    return true;
  };
  out->wts = u32();
  out->token_kind = u8();
  out->profile = u8();
  out->mode = u8();
  out->cols = u16();
  out->rows = u16();
  const char *cwd = nullptr, *env = nullptr, *cmd = nullptr;
  size_t cwd_len = 0, env_len = 0, cmd_len = 0;
  if (!blob(&cwd, &cwd_len) || !blob(&env, &env_len) || !blob(&cmd, &cmd_len))
    return false;
  out->timeout_sec = u32();
  if (off != n) return false;  // trailing bytes: reject
  out->cwd.assign(cwd, cwd_len);
  out->env.assign(env, env_len);
  out->cmd.assign(cmd, cmd_len);
  return true;
}

// "K=V\n"-joined env blob -> individual entries (empty segments skipped;
// the last segment may or may not carry a trailing newline).
std::vector<std::string> SplitShellEnv(const std::string& envJoined) {
  std::vector<std::string> out;
  size_t start = 0;
  while (start <= envJoined.size()) {
    size_t nl = envJoined.find('\n', start);
    if (nl == std::string::npos) {
      if (start < envJoined.size()) out.push_back(envJoined.substr(start));
      break;
    }
    if (nl > start) out.push_back(envJoined.substr(start, nl - start));
    start = nl + 1;
  }
  return out;
}

// [u32 pid][u16 nameLen][name utf8 bytes][32B secret]; pipe name is
// program-constructed ASCII (plain copy), mirrors EncodeStartCaptureOk.
Frame EncodeCreateShellOk(const Frame& req, DWORD pid, const std::wstring& pipe,
                          const uint8_t* secret) {
  std::string name;
  for (wchar_t c : pipe) name.push_back(c < 128 ? static_cast<char>(c) : '?');
  std::vector<uint8_t> p;
  p.reserve(6 + name.size() + kDesktopSecretLen);
  auto put32 = [&p](uint32_t v) {
    for (int i = 0; i < 4; i++) p.push_back(static_cast<uint8_t>(v >> (8 * i)));
  };
  put32(pid);
  p.push_back(static_cast<uint8_t>(name.size()));
  p.push_back(static_cast<uint8_t>(name.size() >> 8));  // u16 LE nameLen
  p.insert(p.end(), name.begin(), name.end());
  p.insert(p.end(), secret, secret + kDesktopSecretLen);
  return Frame{kFlagResponse, kMsgCreateShell, req.request_id, std::move(p)};
}

// Backoff table (spec 15.2): crash #1 -> 1s, #2 -> 2s, #3 -> 4s ... capped
// at 60s. crash_index 0 is treated as 1 (first crash).
uint32_t WorkerBackoffMs(uint32_t crash_index) {
  if (crash_index == 0) crash_index = 1;
  uint64_t ms = 1000;
  for (uint32_t i = 1; i < crash_index; i++) {
    ms <<= 1;
    if (ms >= 60000) return 60000;
  }
  return static_cast<uint32_t>(ms < 60000 ? ms : 60000);
}

// Crash-loop predicate: exits strictly inside the trailing (now-60s, now]
// window; kCrashLoopExits or more -> degraded.
bool CrashLoopReached(const std::vector<uint64_t>& exit_ms, uint64_t now_ms) {
  size_t in_window = 0;
  for (uint64_t t : exit_ms)
    if (t <= now_ms && now_ms - t < kCrashLoopWindowMs) in_window++;
  return in_window >= kCrashLoopExits;
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
  // Heap on purpose: g_wd is published for the DETACHED desktop-restart
  // threads, which outlive this frame once RunPipeServer returns (T3 review:
  // a stack Watchdog here is a use-after-scope). Intentionally leaked - it
  // lives for the rest of the process.
  Watchdog* wd = new Watchdog();
  wd->Start();
  g_wd = wd;
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
  // T5: connections are served CONCURRENTLY (thread per accepted instance;
  // handler state is mutex-guarded). M2-Slice2 gave the agent a second core
  // client (session shellhost alongside the desktop starter), and external
  // diagnostics (xnc-shell-probe) need a slot too - the old sequential loop
  // blocked the second dialer until the first hung up. The FIRST instance
  // keeps FILE_FLAG_FIRST_PIPE_INSTANCE (rival-server detection); later
  // instances drop it and the instance cap rises accordingly.
  bool first_instance = true;
  for (;;) {
    const DWORD access = PIPE_ACCESS_DUPLEX | FILE_FLAG_OVERLAPPED |
                         (first_instance ? FILE_FLAG_FIRST_PIPE_INSTANCE : 0);
    first_instance = false;
    HANDLE pipe = CreateNamedPipeW(
        pipe_name, access,
        PIPE_TYPE_BYTE | PIPE_READMODE_BYTE | PIPE_WAIT,
        PIPE_UNLIMITED_INSTANCES, kBufSize, kBufSize,
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
        wd->Heartbeat();
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

    wd->Heartbeat();
    XNC_LOG_INFO("client connected");
    // Hand the accepted instance to a detached server thread; the accept
    // loop immediately creates the next listening instance.
    std::thread([pipe, wd, secret, secret_len]() {
      uint32_t client_pid = 0;
      ServeConnection(pipe, wd, secret, secret_len, &client_pid);
      FlushFileBuffers(pipe);  // best-effort drain before teardown
      DisconnectNamedPipe(pipe);
      CloseHandle(pipe);
      XNC_LOG_INFO("connection closed, accepting next");
    }).detach();
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
  // No orphan shell children either: terminate scoped by stored handle
  // (the per-shell watcher reaps + closes).
  {
    std::lock_guard<std::mutex> lk(g_shells.mu);
    for (auto& e : g_shells.children) {
      XNC_LOG_INFO("shutdown: terminating shell child pid=%lu", e.first);
      TerminateProcess(e.second, 1);
    }
  }
  if (rc == 0) XNC_LOG_INFO("shutting down (Ctrl+C)");
  return rc;
}

}  // namespace xnc
