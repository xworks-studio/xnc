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

// ---- START/STOP_CAPTURE host state (M1-Slice2 Task 3) ----
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
constexpr size_t kDesktopSecretLen = 32;
constexpr DWORD kPipeReadyWaitMs = 2000;

struct CaptureHost {
  std::mutex mu;
  bool valid = false;        // descriptor reflects a spawned child
  DWORD pid = 0;
  HANDLE child = nullptr;    // owned by the watcher thread of that spawn
  uint32_t gen = 0;          // increments per spawn (agent-side accounting)
  uint8_t secret[kDesktopSecretLen] = {0};
  std::wstring pipe_name;
};
CaptureHost g_capture;

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

// 0x0100: [u32 wts][u32 pad=0] -> spawn (or reuse) the desktop capture
// child and answer its pipe descriptor. Idempotent while the child lives.
Frame HandleStartCapture(const Frame& req, Watchdog* wd) {
  uint32_t wts = 0;
  if (req.payload.size() != 8 || GetU32(req.payload.data() + 4) != 0) {
    return ErrorFrame(kMsgStartCapture, req.request_id, "BAD_PAYLOAD");
  }
  wts = GetU32(req.payload.data());
  if (!SessionTargetAllowed(wts, WTSGetActiveConsoleSessionId())) {
    return ErrorFrame(kMsgStartCapture, req.request_id, "SESSION_MISMATCH");
  }

  std::lock_guard<std::mutex> lk(g_capture.mu);
  // Idempotent path: child alive -> existing pipe/secret/gen, no respawn.
  if (g_capture.valid && g_capture.child != nullptr &&
      WaitForSingleObject(g_capture.child, 0) == WAIT_TIMEOUT) {
    XNC_LOG_INFO("start_capture: reuse pid=%lu gen=%u", g_capture.pid, g_capture.gen);
    return EncodeStartCaptureOk(req, g_capture.pid, g_capture.pipe_name,
                                g_capture.secret, g_capture.gen);
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

  HANDLE token = nullptr;
  std::string err;
  if (!TokenManager::SessionSystemToken(wts, &token, &err)) {
    // Non-SYSTEM callers (elevated admin lacks SeTcb) land here.
    XNC_LOG_ERROR("start_capture: session token failed err=\"%s\"", err.c_str());
    return ErrorFrame(kMsgStartCapture, req.request_id, "TOKEN_FAILED");
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
    return ErrorFrame(kMsgStartCapture, req.request_id, "INTERNAL");
  }

  // Program-constructed argv only - no user input and no secret reach the
  // command line, so the BuildChildCommandLine quoting TODO stays a Slice3
  // must-fix; the cmdline carries nothing sensitive either way.
  std::vector<std::wstring> args = {L"--console-rt", L"--pipe", pipe_name,
                                    L"--secret-stdin"};
  std::vector<wchar_t*> av;
  for (auto& a : args) av.push_back(&a[0]);
  std::wstring cmd;
  if (!BuildChildCommandLine(L"xnc-desktop.exe", static_cast<int>(av.size()),
                             av.data(), 0, &cmd)) {
    CloseHandle(token);
    CloseHandle(sec_rd);
    CloseHandle(sec_wr);
    return ErrorFrame(kMsgStartCapture, req.request_id, "INTERNAL");
  }

  DWORD pid = 0;
  HANDLE child = nullptr;
  if (!SpawnInSession(token, L"xnc-desktop.exe", cmd.c_str(), &pid, &child,
                      &err, sec_rd)) {
    CloseHandle(token);
    CloseHandle(sec_rd);
    CloseHandle(sec_wr);
    XNC_LOG_ERROR("start_capture: spawn failed err=\"%s\"", err.c_str());
    return ErrorFrame(kMsgStartCapture, req.request_id, "SPAWN_FAILED");
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
    TerminateProcess(child, 1);
    CloseHandle(child);
    return ErrorFrame(kMsgStartCapture, req.request_id, "PIPE_TIMEOUT");
  }

  g_capture.valid = true;
  g_capture.pid = pid;
  g_capture.child = child;
  g_capture.gen += 1;
  std::memcpy(g_capture.secret, secret, kDesktopSecretLen);
  g_capture.pipe_name = pipe_name;
  // Secret/hex never logged; pid + pipe name + gen only.
  XNC_LOG_INFO("start_capture: spawned pid=%lu pipe=%ls gen=%u", pid, pipe_name,
               g_capture.gen);
  std::thread(WatchCaptureChild, child, pid).detach();
  return EncodeStartCaptureOk(req, pid, pipe_name, secret, g_capture.gen);
}

// 0x0101: stop the capture child if any; idempotent. TerminateProcess is
// the accepted Slice2 semantics; the watcher thread reaps + closes.
Frame HandleStopCapture(const Frame& req) {
  std::lock_guard<std::mutex> lk(g_capture.mu);
  if (g_capture.valid && g_capture.child != nullptr) {
    XNC_LOG_INFO("stop_capture: terminating pid=%lu (graceful drain = Slice3)",
                 g_capture.pid);
    TerminateProcess(g_capture.child, 1);
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
  // No orphan capture children: if one is still valid at shutdown,
  // terminate it (the watcher thread reaps + closes the handle).
  {
    std::lock_guard<std::mutex> lk(g_capture.mu);
    if (g_capture.valid && g_capture.child != nullptr) {
      XNC_LOG_INFO("shutdown: terminating capture child pid=%lu", g_capture.pid);
      TerminateProcess(g_capture.child, 1);
      g_capture.valid = false;
    }
  }
  if (rc == 0) XNC_LOG_INFO("shutting down (Ctrl+C)");
  return rc;
}

}  // namespace xnc
