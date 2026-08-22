// pipe_server.cpp - console-mode XNIP pipe server (spec 9 server half):
// DACL'd single-instance pipe, accept loop, server-side mutual-proof
// handshake (9.3), PING/PONG frame loop. All blocking waits (accept, reads,
// writes) are sliced into 5s chunks that heartbeat the watchdog, so a quiet
// but healthy server stays alive while a stuck loop is killed after 30s
// (spec 15). IO is overlapped because the accept wait needs the same
// slicing and one handle serves both phases; frame codec and handshake
// payloads reuse frame.cpp/handshake.cpp byte-identical logic.
#include "pipe_server.h"

#include "../common/frame.h"
#include "../common/handshake.h"
#include "../common/log.h"
#include "watchdog.h"

#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>

#include <bcrypt.h>
#include <sddl.h>

#include <atomic>
#include <cstring>
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
  if (rc == 0) XNC_LOG_INFO("shutting down (Ctrl+C)");
  return rc;
}

}  // namespace xnc
