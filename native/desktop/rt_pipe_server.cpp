// rt_pipe_server.cpp - real-time pipe server implementation (see
// rt_pipe_server.h). Structure copied from native/core/pipe_server.cpp
// (plan reference index): DACL'd named pipe, overlapped accept with sliced
// waits, server-side mutual-proof handshake (common/handshake.h), then a
// per-connection frame loop. Differences from core: MULTIPLE simultaneous
// subscribers (accept thread + per-connection reader/sender threads), AUs
// are PUSHED as MSG_FRAME events, and every wait is sliced against the
// server stop flag so Shutdown() never hangs.
//
// Thread map (max_subs <= 4):
//   ENCODE thread - Pipeline::Run -> RtServer::OnAu fan-out (this class
//                     is the AuSink; enqueue only, never blocks on IO)
//   accept thread   - one listening instance at a time; on connect hands
//                     the handle to a fresh reader thread
//   reader thread   - handshake -> ATTACH -> HOST_HELLO -> request loop
//                     (DETACH / KEYFRAME_REQ / PING / BYE); owns the pipe
//                     handle and joins its sender on exit
//   sender thread   - pops the subscriber queue (control first) and writes
//                     frames; blocks in sliced overlapped writes when the
//                     client stalls, which is what fills the depth-3 video
//                     queue and triggers the overflow drop policy
#include "rt_pipe_server.h"

#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>

#include <bcrypt.h>
#include <sddl.h>

#include <chrono>
#include <cstring>

#include "../common/handshake.h"
#include "../common/log.h"
#include "cursor_manager.h"
#include "gdi_capture.h"  // TryCreateGdiCapture (M2 T5 backend fallback)
#include "input_manager.h"
#include "media_pipeline_v2.h"  // M2 Task 4: ServeV2's engine

namespace xnc {
namespace {

constexpr DWORD kBufSize = 64 * 1024;
// Sliced IO waits: every blocking pipe op is really a <=500ms poll loop
// against the stop flag, bounding Shutdown() latency.
constexpr DWORD kIoSliceMs = 500;

// Overlapped pipe IO with sliced waits (core pipe_server.cpp TimedIo,
// parameterized by the server stop flag instead of a global).
class TimedIo {
 public:
  TimedIo(HANDLE file, const std::atomic<bool>* stop) : file_(file), stop_(stop) {
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

  // ReadFrame/WriteFrame mirrors of common/frame.cpp on overlapped IO
  // (header then payload; same validation order: magic -> version -> cap).
  bool ReadFrameF(Frame& out) {
    uint8_t h[kHeaderSize];
    if (!ReadFull(h, sizeof(h))) return false;
    if (std::memcmp(h, kMagic, sizeof(kMagic)) != 0) return false;
    if (h[4] != kProtocolVersion) return false;
    const uint32_t n = rt_detail::GetU32(h + 12);
    if (n > kMaxFrameBytes) return false;
    out.flags = h[5];
    out.message_type = rt_detail::GetU16(h + 6);
    out.request_id = rt_detail::GetU32(h + 8);
    out.payload.assign(n, 0);
    if (n > 0 && !ReadFull(out.payload.data(), n)) return false;
    return true;
  }
  bool WriteFrameF(const Frame& f) {
    std::vector<uint8_t> wire;
    if (!EncodeFrame(f, wire)) return false;
    return WriteFull(wire.data(), wire.size());
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
      if (ok) {
        // 同步完成(小帧 < pipe 缓冲):overlapped 事件不置位,若继续
        // WaitForSingleObject 会空等 kIoSliceMs —— 2026-08-25 性能根因
        // (rt 发送被压到 ~7fps)。同步完成直接计入,不等待。
        done += len - done;
        break;
      }
      if (GetLastError() != ERROR_IO_PENDING) return false;
      DWORD got = 0;
      if (!WaitAndGet(&got) || got == 0) return false;
      done += got;
    }
    return true;
  }
  bool WaitAndGet(DWORD* got) {
    for (;;) {
      const DWORD w = WaitForSingleObject(event_, kIoSliceMs);
      if (w == WAIT_OBJECT_0) return GetOverlappedResult(file_, &ov_, got, FALSE) != 0;
      if (w != WAIT_TIMEOUT) return false;
      if (stop_ != nullptr && stop_->load()) return false;  // shutdown: abandon
    }
  }

  HANDLE file_;              // pipe instance, not owned
  HANDLE event_ = nullptr;   // manual-reset
  OVERLAPPED ov_{};
  const std::atomic<bool>* stop_;
};

// Server half of the mutual-proof handshake (spec 9.3) - flow copied from
// native/core/pipe_server.cpp ServerHandshake (anonymous there, byte
// semantics in common/handshake.cpp). Logs never carry nonce/proof.
bool RtHandshake(TimedIo& io, const uint8_t* secret, size_t secret_len,
                 uint32_t* client_pid) {
  Frame f;
  if (!io.ReadFrameF(f) || f.message_type != kMsgHello) {
    XNC_LOG_INFO("rt_handshake: expected HELLO, disconnecting");
    return false;
  }
  uint32_t pid = 0;
  uint8_t cnonce[kNonceSize];
  if (DecodeHello(f, pid, cnonce) != DecodeResult::Ok) {
    XNC_LOG_INFO("rt_handshake: malformed HELLO, disconnecting");
    return false;
  }
  uint8_t ononce[kNonceSize];
  NTSTATUS rng = BCryptGenRandom(nullptr, ononce, static_cast<ULONG>(kNonceSize),
                                 BCRYPT_USE_SYSTEM_PREFERRED_RNG);
  if (!BCRYPT_SUCCESS(rng)) {
    XNC_LOG_ERROR("rt_handshake: BCryptGenRandom failed status=0x%08lX",
                  static_cast<unsigned long>(rng));
    return false;
  }
  uint8_t proof[kProofSize];
  if (!HmacSha256(secret, secret_len, cnonce, kNonceSize, proof)) {
    XNC_LOG_ERROR("rt_handshake: HMAC failed");
    return false;
  }
  Frame hp{0, kMsgHelloProof, 0, EncodeHelloProof(GetCurrentProcessId(), ononce, proof)};
  if (!io.WriteFrameF(hp)) {
    XNC_LOG_INFO("rt_handshake: send HELLO_PROOF failed, disconnecting");
    return false;
  }
  if (!io.ReadFrameF(f) || f.message_type != kMsgProof) {
    XNC_LOG_INFO("rt_handshake: expected PROOF, disconnecting");
    return false;
  }
  uint8_t cproof[kProofSize], want[kProofSize];
  if (DecodeProof(f, cproof) != DecodeResult::Ok ||
      !HmacSha256(secret, secret_len, ononce, kNonceSize, want)) {
    XNC_LOG_INFO("rt_handshake: malformed PROOF, disconnecting");
    return false;
  }
  uint8_t diff = 0;  // constant-time compare
  for (size_t i = 0; i < kProofSize; i++) diff |= want[i] ^ cproof[i];
  if (diff != 0) {
    XNC_LOG_ERROR("rt_handshake: client proof rejected, disconnecting");
    return false;
  }
  *client_pid = pid;
  return true;
}

// Production DACL: SYSTEM + Administrators + the spawning user (the pipe
// secret carries the real authentication; the user ACE lets the
// console-run dev agent dial in without elevation - Task 3 topology).
bool BuildUserSddl(std::wstring* out) {
  HANDLE tok = nullptr;
  if (!OpenProcessToken(GetCurrentProcess(), TOKEN_QUERY, &tok)) return false;
  DWORD need = 0;
  GetTokenInformation(tok, TokenUser, nullptr, 0, &need);
  if (need == 0) {
    CloseHandle(tok);
    return false;
  }
  std::vector<uint8_t> buf(need);
  if (!GetTokenInformation(tok, TokenUser, buf.data(), need, &need)) {
    CloseHandle(tok);
    return false;
  }
  CloseHandle(tok);
  wchar_t* sid = nullptr;
  if (!ConvertSidToStringSidW(
          static_cast<PSID>(static_cast<TOKEN_USER*>(static_cast<void*>(buf.data()))->User.Sid),
          &sid) ||
      sid == nullptr)
    return false;
  *out = std::wstring(L"D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;") + sid + L")";
  LocalFree(sid);
  return true;
}

// Ctrl+C (console-rt runs in the foreground until stopped). The active
// server pointer is set/cleared by Serve on the main thread.
std::atomic<RtServer*> g_active_rt{nullptr};
BOOL WINAPI OnRtCtrlEvent(DWORD type) {
  XNC_LOG_INFO("console control event %lu, stopping rt server", type);
  RtServer* s = g_active_rt.load();
  if (s != nullptr) s->RequestStop();
  return TRUE;
}

}  // namespace

RtServer::~RtServer() { Shutdown(); }

bool RtServer::Start(const Opts& o, uint32_t src_w, uint32_t src_h) {
  if (started_) return false;
  if (o.pipe_name.empty() || o.secret == nullptr || o.secret_len == 0 ||
      o.max_subs == 0 || o.fps == 0) {
    XNC_LOG_ERROR("rt_start invalid opts (pipe/secret/max_subs/fps)");
    return false;
  }
  opts_ = o;
  src_w_ = src_w;
  src_h_ = src_h;
  stop_.store(false);

  PSECURITY_DESCRIPTOR sd = nullptr;
  std::wstring user_sddl;
  const wchar_t* sddl = o.sddl_override != nullptr ? o.sddl_override : nullptr;
  if (sddl == nullptr) {
    if (!BuildUserSddl(&user_sddl)) {
      XNC_LOG_INFO("rt_dacl user ACE unavailable, falling back to SY+BA");
      user_sddl = L"D:P(A;;GA;;;SY)(A;;GA;;;BA)";
    }
    sddl = user_sddl.c_str();
  }
  if (!ConvertStringSecurityDescriptorToSecurityDescriptorW(sddl, SDDL_REVISION_1,
                                                            &sd, nullptr)) {
    XNC_LOG_ERROR("rt_dacl SDDL parse failed err=%lu", GetLastError());
    return false;
  }
  SECURITY_ATTRIBUTES sa{sizeof(sa), sd, FALSE};
  HANDLE pipe = CreateNamedPipeW(
      opts_.pipe_name.c_str(),
      PIPE_ACCESS_DUPLEX | FILE_FLAG_FIRST_PIPE_INSTANCE | FILE_FLAG_OVERLAPPED,
      PIPE_TYPE_BYTE | PIPE_READMODE_BYTE | PIPE_WAIT, PIPE_UNLIMITED_INSTANCES,
      kBufSize, kBufSize, 0, &sa);
  LocalFree(sd);
  if (pipe == INVALID_HANDLE_VALUE) {
    XNC_LOG_ERROR("rt CreateNamedPipe failed err=%lu name=%ls (taken by another instance?)",
                  GetLastError(), opts_.pipe_name.c_str());
    return false;
  }
  started_ = true;
  accept_ = std::thread(&RtServer::AcceptLoop, this, pipe);
  XNC_LOG_INFO("rt_server listening pipe=%ls max_subs=%u w=%u h=%u",
               opts_.pipe_name.c_str(), opts_.max_subs, src_w_, src_h_);
  return true;
}

int RtServer::Serve(ICapture& cap, MfSoftEncoder& enc, const Opts& o) {
  // M2-S3 Task 6: a logon-UI boot pre-starts the pipe (Start) BEFORE the
  // capture exists so the core's 2s spawn window sees it; Serve must adopt
  // the already-started server instead of failing Start's started_ guard.
  if (!started_) {
    if (!Start(o, cap.Width(), cap.Height())) return 1;
  }
  g_active_rt.store(this);
  SetConsoleCtrlHandler(OnRtCtrlEvent, TRUE);
  XNC_LOG_INFO("console_rt_start pipe=%ls fps=%u bitrate=%u max_subs=%u w=%u h=%u",
               o.pipe_name.c_str(), o.fps, o.bitrate_bps, o.max_subs, cap.Width(),
               cap.Height());

  PipelineOpts popt;
  popt.duration_s = 0xFFFFFFFFu;  // until Ctrl+C / RequestStop
  popt.fps = o.fps;
  popt.target_bitrate_bps = o.bitrate_bps;
  popt.stop = &stop_;
  popt.desktop_name_fn = o.desktop_name_fn;      // DesktopWatch beat (M2-S1 T1)
  popt.desktop_name_ctx = o.desktop_name_ctx;
  popt.reset = o.reset;                          // unified CaptureReset (M2-S1 T2)
  popt.fps_hint = o.fps_hint;        // M3 T3: hot 0x0129 fps pacing + reset reinit
  popt.bitrate_hint = o.bitrate_hint;  // (bitrate re-keying after a reset)
  const PipelineResult res = Pipeline::Run(cap, enc, *this, popt);

  SetConsoleCtrlHandler(OnRtCtrlEvent, FALSE);
  g_active_rt.store(nullptr);
  Shutdown();
  const FrameCacheCounters& c = res.counters;
  XNC_LOG_INFO("console_rt_stop captured=%llu encoded=%llu keyframes=%llu timeouts=%llu warmup_feeds=%llu rebuilds=%u aus=%llu ok=%d",
               static_cast<unsigned long long>(c.captured),
               static_cast<unsigned long long>(c.encoded),
               static_cast<unsigned long long>(c.keyframes),
               static_cast<unsigned long long>(c.timeouts),
               static_cast<unsigned long long>(c.warmup_feeds), c.rebuilds,
               static_cast<unsigned long long>(res.aus_written), res.ok ? 1 : 0);
  return res.ok ? 0 : 1;
}

// M2 Task 4: the real-time mode on the depth-one GPU media pipeline. Same
// shape as Serve above (adopt an already-started server, console Ctrl+C
// stops the loop, Shutdown drains); the single media loop thread owns
// capture -> convert -> encode and publishes through OnAu exactly like the
// M0 encode thread did, so subscribers see the identical wire traffic.
// Fix round 1 (finding 2): max_width threads --max-w into the pipeline's
// VideoProcessor and the HOST_HELLO carries the SCALED stream dims (the
// M0 path's ScaledCapture equivalent lives inside MediaPipelineV2).
int RtServer::ServeV2(ICapture& cap, ICaptureSurface& surf, const Opts& o,
                      uint32_t max_width, MediaBackend initial_backend,
                      bool force_software) {
  // Stream dims: the VideoProcessor output space (== the encoder, hello,
  // input and cursor spaces when the caller sizes them the same way).
  uint32_t w = cap.Width(), h = cap.Height();
  if (max_width > 0) {
    uint32_t sw = 0, sh = 0;
    if (!GpuScaledDims(w, h, Rotate::kNone, max_width, &sw, &sh)) {
      XNC_LOG_ERROR("console_rt_v2 bad scaled dims w=%u h=%u max_w=%u", w, h,
                    max_width);
      return 1;
    }
    w = sw;
    h = sh;
  }
  if (!started_) {
    if (!Start(o, w, h)) return 1;
  }
  g_active_rt.store(this);
  SetConsoleCtrlHandler(OnRtCtrlEvent, TRUE);
  XNC_LOG_INFO("console_rt_v2_start pipe=%ls fps=%u bitrate=%u max_subs=%u w=%u h=%u max_w=%u src=%ux%u",
               o.pipe_name.c_str(), o.fps, o.bitrate_bps, o.max_subs, w, h,
               max_width, cap.Width(), cap.Height());

  MediaPipelineV2::Config cfg;
  cfg.cap = &cap;
  cfg.surf = &surf;
  cfg.sink = this;
  cfg.fps = o.fps;
  cfg.bitrate_bps = o.bitrate_bps;
  cfg.duration_s = 0;  // until Ctrl+C / RequestStop
  cfg.stop = &stop_;
  cfg.max_width = max_width;  // fix round 1: --max-w honored end to end
  cfg.force_software_encoder = force_software;  // fix 2026-08: --encoder pin
  cfg.desktop_name_fn = o.desktop_name_fn;  // DesktopWatch beat (M2-S1 T1)
  cfg.desktop_name_ctx = o.desktop_name_ctx;
  cfg.reset = o.reset;  // unified CaptureReset (M2-S1 T2)
  // M2 Task 5 (ruling 3): backend fallback through the unified reset -
  // DXGI failure falls to GDI, a 30 s probe returns once DXGI works.
  cfg.initial_backend = initial_backend;
  cfg.make_backend = [](void*, MediaBackend kind, std::string* err) ->
      std::unique_ptr<ICapture> {
    return kind == MediaBackend::kGdi ? TryCreateGdiCapture(err)
                                      : TryCreateDxgiCapture(err);
  };
  // Fix 4: the keepalive's liveness probe - a zero-subscriber stream
  // neither re-feeds nor burns refresh IDRs.
  cfg.subscribers_fn = [](void* ctx) -> size_t {
    return static_cast<RtServer*>(ctx)->SubscriberCount();
  };
  cfg.subscribers_ctx = this;
  MediaPipelineV2 pipe;
  MediaPipelineV2::Result res;
  if (pipe.Start(cfg)) {
    // M3 Task 3: 0x0129 now reaches the live pipeline (bitrate/fps HOT via
    // the mailbox; max_w via SetMaxWidth's resolution reset). Until this
    // store a racing request falls through to Opts::set_video_config_fn
    // (xnc-desktop wires a stub there so the capability is advertised from
    // the FIRST hello).
    v2_pipe_.store(&pipe);
    while (pipe.running() && !stop_.load()) Sleep(100);
    res = pipe.Stop();
  } else {
    res.ok = false;
    res.err = "media pipeline v2 start failed: " + pipe.start_error();
  }

  SetConsoleCtrlHandler(OnRtCtrlEvent, FALSE);
  g_active_rt.store(nullptr);
  Shutdown();
  v2_pipe_.store(nullptr);  // readers are joined; the pipe is going away
  XNC_LOG_INFO("console_rt_v2_stop captured=%llu encoded=%llu keyframes=%llu timeouts=%llu keepalive_feeds=%llu resets=%u aus=%llu reorder_gap_skips=%llu reorder_late_drops=%llu ok=%d backend=%s",
               static_cast<unsigned long long>(res.captured),
               static_cast<unsigned long long>(res.encoded),
               static_cast<unsigned long long>(res.keyframes),
               static_cast<unsigned long long>(res.timeouts),
               static_cast<unsigned long long>(res.keepalive_feeds), res.resets,
               static_cast<unsigned long long>(res.aus_written),
               static_cast<unsigned long long>(res.reorder_gap_skips),
               static_cast<unsigned long long>(res.reorder_late_drops),
               res.ok ? 1 : 0, res.encoder_backend);
  return res.ok ? 0 : 1;
}

void RtServer::Shutdown() {
  if (!started_) return;
  started_ = false;
  stop_.store(true);
  // Note: the "stream_end" STATE broadcast comes from the pipeline's
  // OnState hook, not here, so Start/Shutdown users get it exactly once.
  if (accept_.joinable()) accept_.join();  // no new readers after this
  {
    std::lock_guard<std::mutex> lk(mu_);
    for (auto& kv : conns_) {
      kv.second->dead.store(true);
      kv.second->cv.notify_all();
      if (kv.second->pipe != INVALID_HANDLE_VALUE)
        CancelIoEx(kv.second->pipe, nullptr);  // unblock sliced waits now
    }
  }
  for (auto& t : readers_)
    if (t.joinable()) t.join();
  readers_.clear();
  // DRAIN: readers are gone, so no more input can arrive - stop the cursor
  // poller and force-release every recorded key/button (session-global
  // state; the process may be about to exit).
  if (opts_.cursor != nullptr) opts_.cursor->Stop();
  if (opts_.input != nullptr) opts_.input->ReleaseAll();
  std::lock_guard<std::mutex> lk(mu_);
  conns_.clear();  // readers erase themselves; any stragglers are dead
  XNC_LOG_INFO("rt_server stopped");
}

// ---- accept / connection threads ----

void RtServer::AcceptLoop(HANDLE first_pipe) {
  HANDLE pipe = first_pipe;  // Start() created it with FIRST_PIPE_INSTANCE
  while (!stop_.load()) {
    HANDLE ev = CreateEventW(nullptr, TRUE, FALSE, nullptr);
    OVERLAPPED ov{};
    ov.hEvent = ev;
    BOOL connected = ConnectNamedPipe(pipe, &ov);
    DWORD err = connected ? 0 : GetLastError();
    bool ok = connected || err == ERROR_PIPE_CONNECTED;  // client-first race
    if (!ok && err == ERROR_IO_PENDING && ev != nullptr) {
      for (;;) {
        const DWORD w = WaitForSingleObject(ev, kIoSliceMs);
        if (w == WAIT_OBJECT_0) {
          DWORD unused = 0;
          ok = GetOverlappedResult(pipe, &ov, &unused, FALSE) != 0;
          break;
        }
        if (w != WAIT_TIMEOUT) break;
        if (stop_.load()) {
          CancelIoEx(pipe, &ov);
          break;
        }
      }
    }
    if (ev != nullptr) CloseHandle(ev);
    if (stop_.load()) break;
    if (!ok) {
      XNC_LOG_ERROR("rt ConnectNamedPipe failed err=%lu, accept loop ends",
                    GetLastError());
      break;
    }
    XNC_LOG_INFO("rt connection incoming");
    {
      auto c = std::make_shared<SubConn>();
      c->pipe = pipe;
      std::lock_guard<std::mutex> lk(mu_);
      readers_.emplace_back(&RtServer::ReaderLoop, this, c);
    }
    // Next listening instance (no FIRST_PIPE_INSTANCE: that claim is the
    // Start() instance's job).
    PSECURITY_DESCRIPTOR sd = nullptr;
    std::wstring user_sddl;
    const wchar_t* sddl = opts_.sddl_override;
    if (sddl == nullptr) {
      if (!BuildUserSddl(&user_sddl)) user_sddl = L"D:P(A;;GA;;;SY)(A;;GA;;;BA)";
      sddl = user_sddl.c_str();
    }
    HANDLE next = INVALID_HANDLE_VALUE;
    if (ConvertStringSecurityDescriptorToSecurityDescriptorW(sddl, SDDL_REVISION_1,
                                                             &sd, nullptr)) {
      SECURITY_ATTRIBUTES sa{sizeof(sa), sd, FALSE};
      next = CreateNamedPipeW(
          opts_.pipe_name.c_str(),
          PIPE_ACCESS_DUPLEX | FILE_FLAG_OVERLAPPED,
          PIPE_TYPE_BYTE | PIPE_READMODE_BYTE | PIPE_WAIT, PIPE_UNLIMITED_INSTANCES,
          kBufSize, kBufSize, 0, &sa);
      LocalFree(sd);
    } else {
      XNC_LOG_ERROR("rt_dacl SDDL parse failed err=%lu", GetLastError());
    }
    if (next == INVALID_HANDLE_VALUE) {
      XNC_LOG_ERROR("rt CreateNamedPipe(next) failed err=%lu, accept loop ends",
                    GetLastError());
      break;
    }
    pipe = next;
  }
  if (pipe != INVALID_HANDLE_VALUE) CloseHandle(pipe);  // unused instance
}

// Reader: handshake -> ATTACH (capacity/dup checks) -> HOST_HELLO -> request
// loop. Owns the pipe handle; starts SenderLoop after ATTACH and joins it
// before closing. All exits go through the common cleanup at the bottom.
void RtServer::ReaderLoop(std::shared_ptr<SubConn> c) {
  uint32_t client_pid = 0;
  bool attached = false;
  TimedIo io(c->pipe, &stop_);
  do {
    if (!io.Ok()) {
      XNC_LOG_ERROR("rt CreateEvent failed err=%lu", GetLastError());
      break;
    }
    if (!RtHandshake(io, opts_.secret, opts_.secret_len, &client_pid)) {
      std::lock_guard<std::mutex> lk(mu_);
      stats_.rejected_handshake++;
      break;
    }
    XNC_LOG_INFO("rt client connected pid=%lu",
                 static_cast<unsigned long>(client_pid));

    Frame f;
    if (!io.ReadFrameF(f) || f.message_type != kMsgAttach) {
      XNC_LOG_INFO("rt expected ATTACH from pid=%lu, disconnecting",
                   static_cast<unsigned long>(client_pid));
      const Frame err{kFlagResponse | kFlagError, kMsgAttach,
                      f.message_type == kMsgAttach ? f.request_id : 0, {}};
      io.WriteFrameF(err);
      break;
    }
    AttachPayload ap;
    if (!DecodeAttach(f, &ap)) {
      XNC_LOG_INFO("rt malformed ATTACH from pid=%lu, disconnecting",
                   static_cast<unsigned long>(client_pid));
      const Frame err{kFlagResponse | kFlagError, kMsgAttach, f.request_id, {}};
      io.WriteFrameF(err);
      break;
    }
    enum class AttachReject { kNone, kMaxSubs, kDup };
    AttachReject reject = AttachReject::kNone;
    bool first_sub = false;
    {
      std::lock_guard<std::mutex> lk(mu_);
      if (table_.size() >= opts_.max_subs) {
        stats_.rejected_max_subs++;
        reject = AttachReject::kMaxSubs;
        XNC_LOG_INFO("rt attach rejected (max_subs=%u) sub=%u pid=%lu", opts_.max_subs,
                     ap.sub_id, static_cast<unsigned long>(client_pid));
      } else if (!table_.Attach(ap.sub_id, c->q)) {
        stats_.rejected_dup++;
        reject = AttachReject::kDup;
        XNC_LOG_INFO("rt attach rejected (duplicate sub_id=%u) pid=%lu", ap.sub_id,
                     static_cast<unsigned long>(client_pid));
      } else {
        c->sub_id = ap.sub_id;
        conns_[ap.sub_id] = c;
        stats_.attaches++;
        attached = true;
        first_sub = table_.size() == 1;
      }
    }
    if (reject == AttachReject::kMaxSubs) {
      const Frame st{kFlagEvent, kMsgState, 0,
                     EncodeStateEvent("too_many_subs", true)};
      io.WriteFrameF(st);
      break;
    }
    if (reject == AttachReject::kDup) {
      const Frame err{kFlagResponse | kFlagError, kMsgAttach, f.request_id,
                      std::vector<uint8_t>({'D', 'U', 'P'})};
      io.WriteFrameF(err);
      break;
    }
    XNC_LOG_INFO("rt attach sub=%u pid=%lu fps=%u w=%u bitrate=%u", c->sub_id,
                 static_cast<unsigned long>(client_pid), ap.max_fps, ap.max_w,
                 ap.bitrate);

    // HOST_HELLO immediately after attach (plan Task 2). M2-S3 Task 5: the
    // payload carries the full displays table (empty when no provider).
    // M1 Task 2: pipeline_v2 appends the u32 media_protocol=2 field; M3 Task 3
    // additionally appends the capabilities u32 when a 0x0129 applier is
    // wired. fps is snapshotted under mu_ (a 0x0129 from another connection
    // may update it concurrently).
    uint32_t hello_fps = 0;
    {
      std::lock_guard<std::mutex> lk(mu_);
      hello_fps = opts_.fps;
    }
    if (!PushControlTo(c.get(),
                       Frame{kFlagEvent, kMsgHostHello, 0, BuildHelloPayload(hello_fps)}))
      break;  // control backlog: wedged connection
    // First subscriber: the cursor poller runs only while someone watches
    // (8ms GetCursorInfo cadence, change-only 0x0109 events).
    if (first_sub && opts_.cursor != nullptr) {
      opts_.cursor->Start([this](int32_t x, int32_t y, uint8_t visible) {
        BroadcastCursor(x, y, visible);
      });
    }
    c->sender = std::thread(&RtServer::SenderLoop, this, c);

    // Request loop.
    for (;;) {
      if (stop_.load() || c->dead.load()) break;
      if (!io.ReadFrameF(f)) break;  // client gone / shutdown
      switch (f.message_type) {
        case kMsgDetach: {
          uint32_t sid = 0;
          if (f.payload.size() == 4)
            sid = rt_detail::GetU32(f.payload.data());
          if (f.payload.size() != 4 || (sid != 0 && sid != c->sub_id)) {
            PushControlTo(c.get(), Frame{kFlagResponse | kFlagError, kMsgDetach,
                                         f.request_id, {}});
            break;
          }
          {
            std::lock_guard<std::mutex> lk(mu_);
            table_.Detach(c->sub_id);
            conns_.erase(c->sub_id);
            stats_.detaches++;
          }
          XNC_LOG_INFO("rt detach sub=%u pid=%lu", c->sub_id,
                       static_cast<unsigned long>(client_pid));
          attached = false;  // reader loop ends; sender drains then exits
          OnSubscriberRemoved(c->sub_id);
          goto conn_done;
        }
        case kMsgInput: {
          // 0x0108: wire-shape validation + own-sub_id check here, seq
          // monotonicity + SendInput inside InputManager (see header).
          InputMsg im;
          {
            std::lock_guard<std::mutex> lk(mu_);
            stats_.input_received++;
          }
          if (opts_.input != nullptr && DecodeInputMsg(f, &im) &&
              im.sub_id == c->sub_id) {
            const InputManager::Result r = opts_.input->Inject(im);
            if (r != InputManager::Result::kInjected) {
              std::lock_guard<std::mutex> lk(mu_);
              stats_.input_dropped++;
              XNC_LOG_INFO("rt input dropped sub=%u seq=%llu type=%u", c->sub_id,
                           static_cast<unsigned long long>(im.seq), im.type);
            }
          } else {
            std::lock_guard<std::mutex> lk(mu_);
            stats_.input_rejected++;
            XNC_LOG_INFO("rt input rejected sub=%u pid=%lu", c->sub_id,
                         static_cast<unsigned long>(client_pid));
          }
          break;
        }
        case kMsgKeyframeReq: {
          KeyframeReqPayload kr;
          if (DecodeKeyframeReq(f, &kr) && (kr.sub_id == 0 || kr.sub_id == c->sub_id)) {
            std::lock_guard<std::mutex> lk(mu_);
            table_.MarkNeedsKeyframe(c->sub_id, "explicit");
            XNC_LOG_INFO("rt keyframe_req sub=%u client_reason=%.32s", c->sub_id,
                         kr.reason);
          } else {
            PushControlTo(c.get(),
                          Frame{kFlagResponse | kFlagError, kMsgKeyframeReq,
                                f.request_id, {}});
          }
          break;
        }
        case kMsgSwitchDisplay: {
          // 0x0128 (M2-S3 Task 5): validate idx < displays count -> select +
          // unified reset(reason="switch"; the rebuild binds the new output,
          // ForceIDR comes from the reset's base-frame rewind); invalid ->
          // STATE{invalid_display} (recoverable) + error response, no reset.
          uint32_t idx = 0;
          if (!DecodeSwitchDisplay(f, &idx)) {
            std::lock_guard<std::mutex> lk(mu_);
            stats_.switch_invalid++;
            PushControlTo(c.get(),
                          Frame{kFlagResponse | kFlagError, kMsgSwitchDisplay,
                                f.request_id, {}});
            break;
          }
          const std::vector<DisplayInfo> table = CurrentDisplays();
          const bool ok = idx < table.size() && opts_.switch_display_fn != nullptr &&
                          opts_.switch_display_fn(opts_.switch_display_ctx, idx);
          if (!ok) {
            {
              std::lock_guard<std::mutex> lk(mu_);
              stats_.switch_invalid++;
            }
            XNC_LOG_INFO("rt switch_display rejected sub=%u idx=%u count=%zu",
                         c->sub_id, idx, table.size());
            PushControlTo(c.get(),
                          Frame{kFlagEvent, kMsgState, 0,
                                EncodeStateEvent("invalid_display", true)});
            PushControlTo(c.get(),
                          Frame{kFlagResponse | kFlagError, kMsgSwitchDisplay,
                                f.request_id, {}});
            break;
          }
          {
            std::lock_guard<std::mutex> lk(mu_);
            stats_.switch_accepted++;
          }
          XNC_LOG_INFO("rt switch_display sub=%u idx=%u (reset reason=switch)",
                       c->sub_id, idx);
          if (opts_.reset != nullptr)
            opts_.reset->RequestReset(kResetReasonSwitch);
          if (!PushControlTo(c.get(), Frame{kFlagResponse, kMsgSwitchDisplay,
                                             f.request_id, {}}))
            goto conn_done;
          break;
        }
        case kMsgSetVideoConfig: {
          // M3 Task 3: the agent-side QoS decision. bitrate/fps are HOT; a
          // max_w change rides the reset/dims path. Preference: the V2
          // pipeline ServeV2 owns (Reconfigure/SetMaxWidth), then the Opts
          // applier (the M0 wiring). Neither = the capability was never
          // advertised; answer unsupported (the agent stops sending and keeps
          // its decisions local).
          VideoConfigPayload vc;
          if (!DecodeSetVideoConfig(f, &vc)) {
            std::lock_guard<std::mutex> lk(mu_);
            stats_.video_config_rejected++;
            PushControlTo(c.get(), Frame{kFlagResponse | kFlagError,
                                          kMsgSetVideoConfig, f.request_id, {}});
            break;
          }
          const uint32_t bitrate_bps =
              vc.bitrate_kbps != 0 ? vc.bitrate_kbps * 1000u : 0;
          // Effective values for partial updates (0 field = keep current).
          uint32_t eff_bitrate = 0, eff_fps = 0;
          {
            std::lock_guard<std::mutex> lk(mu_);
            eff_bitrate = bitrate_bps != 0 ? bitrate_bps : opts_.bitrate_bps;
            eff_fps = vc.fps != 0 ? vc.fps : opts_.fps;
          }
          bool ok = false;
          if (MediaPipelineV2* p = v2_pipe_.load(std::memory_order_acquire)) {
            if (bitrate_bps != 0 || vc.fps != 0) p->Reconfigure(eff_bitrate, eff_fps);
            if (vc.max_w != 0) p->SetMaxWidth(vc.max_w);
            ok = true;
          } else if (opts_.set_video_config_fn != nullptr) {
            ok = opts_.set_video_config_fn(opts_.set_video_config_ctx, bitrate_bps,
                                           vc.fps, vc.max_w);
          }
          {
            std::lock_guard<std::mutex> lk(mu_);
            if (ok) {
              stats_.video_configs++;
              // Future HOST_HELLOs carry the effective config (snapshot sites
              // read under mu_).
              if (vc.fps != 0) opts_.fps = vc.fps;
              if (bitrate_bps != 0) opts_.bitrate_bps = bitrate_bps;
            } else {
              stats_.video_config_rejected++;
            }
          }
          XNC_LOG_INFO("rt set_video_config sub=%u bitrate_kbps=%u fps=%u max_w=%u ok=%d",
                       c->sub_id, vc.bitrate_kbps, vc.fps, vc.max_w, ok ? 1 : 0);
          const uint8_t resp_flags =
              ok ? kFlagResponse
                 : static_cast<uint8_t>(kFlagResponse | kFlagError);
          if (!PushControlTo(c.get(),
                             Frame{resp_flags, kMsgSetVideoConfig, f.request_id, {}}))
            goto conn_done;
          break;
        }
        case kMsgPing: {
          if (!PushControlTo(c.get(), Frame{kFlagResponse, kMsgPong, f.request_id, {}}))
            goto conn_done;
          break;
        }
        case kMsgBye: {
          XNC_LOG_INFO("rt client pid=%lu said BYE",
                       static_cast<unsigned long>(client_pid));
          goto conn_done;
        }
        default: {
          XNC_LOG_INFO("rt unknown message type=0x%04x from pid=%lu",
                       f.message_type, static_cast<unsigned long>(client_pid));
          if (!PushControlTo(c.get(),
                             Frame{kFlagResponse | kFlagError, f.message_type,
                                   f.request_id, {}}))
            goto conn_done;
          break;
        }
      }
    }
  } while (false);
conn_done:
  if (attached) {  // non-DETACH exit: still registered
    {
      std::lock_guard<std::mutex> lk(mu_);
      table_.Detach(c->sub_id);
      conns_.erase(c->sub_id);
    }
    OnSubscriberRemoved(c->sub_id);
  }
  c->dead.store(true);
  c->cv.notify_all();
  if (c->sender.joinable()) c->sender.join();  // drains queued frames first
  // No FlushFileBuffers here on purpose: on a named pipe it blocks until
  // the CLIENT has read everything, so a stalled subscriber would hang
  // teardown forever (caught by selftest scenario 3). The sender's
  // completed overlapped writes already carry the bytes into the pipe
  // buffer; DisconnectNamedPipe may drop the last buffered chunk for a
  // client that stopped reading - acceptable, that client is gone.
  DisconnectNamedPipe(c->pipe);
  CloseHandle(c->pipe);
  c->pipe = INVALID_HANDLE_VALUE;
  XNC_LOG_INFO("rt connection closed sub=%u pid=%lu", c->sub_id,
               static_cast<unsigned long>(client_pid));
}

// Sender: pops control-first and writes. The sliced overlapped write blocks
// while the client stalls - the video queue then fills (depth 3) and OnAu's
// drop policy kicks in; dead+empty exits (drain on detach).
void RtServer::SenderLoop(std::shared_ptr<SubConn> c) {
  TimedIo io(c->pipe, &stop_);
  if (!io.Ok()) return;
  for (;;) {
    Frame f;
    {
      std::unique_lock<std::mutex> lk(c->mu);
      c->cv.wait_for(lk, std::chrono::milliseconds(kIoSliceMs), [&c] {
        return c->dead.load() || c->q->HasWork();
      });
      if (!c->q->Pop(&f)) {
        if (c->dead.load()) break;
        continue;
      }
    }
    if (!io.WriteFrameF(f)) {
      XNC_LOG_INFO("rt send failed sub=%u (client stalled or gone)", c->sub_id);
      c->dead.store(true);
      break;
    }
  }
}

// ---- AuSink (encode thread; OnState/OnDisplayChanged from the capture thread) ----

const char* RtServer::OnAu(const EncodedAU& au) {
  if (au.annexb == nullptr || au.annexb->empty()) return nullptr;
  const uint8_t* p = au.annexb->data();
  const size_t len = au.annexb->size();
  if (len > kMaxAuBytes) {  // 8MiB AU bound (proto.MaxSessionFrameBytes)
    XNC_LOG_ERROR("rt_au_oversize len=%llu (dropped)",
                  static_cast<unsigned long long>(len));
    std::lock_guard<std::mutex> lk(mu_);
    stats_.frames_oversize++;
    return nullptr;
  }
  // M1 Task 1: the v1 0x0105 wire event is unchanged while pipeline_v2 is
  // off - the key bit and the timeline timestamp come out of the immutable
  // AU's flags/identity. M1 Task 2: with pipeline_v2 on, the validated
  // 0x0205 frame (full identity + CRC32C) rides the wire instead.
  const bool is_idr = (au.flags & AuFlags::kAuFlagKey) != 0;
  const uint64_t mono_us = au.id.present_mono_us;
  std::vector<uint8_t> payload;
  const uint16_t msg_type = opts_.pipeline_v2 ? kMsgFrameV2 : kMsgFrame;
  if (opts_.pipeline_v2)
    payload = EncodeFrameEventV2(au);
  else
    payload = EncodeFrameEvent(mono_us, is_idr, p, len);
  if (payload.empty()) {  // > AU bound / > XNIP frame cap safety net
    XNC_LOG_ERROR("rt_au_frame_cap len=%llu (dropped)",
                  static_cast<unsigned long long>(len));
    std::lock_guard<std::mutex> lk(mu_);
    stats_.frames_oversize++;
    return nullptr;
  }
  const Frame ev{kFlagEvent, msg_type, 0, std::move(payload)};
  std::lock_guard<std::mutex> lk(mu_);
  stats_.aus_emitted++;
  // M1 Task 4: remember the fan-out epoch pair - OnState reads it as the
  // rebuild floor for SubSendQueue::OnRebuildDiscontinuity (suppresses the
  // pre-rebuild lookahead tail; see subscribers.h).
  last_au_cap_ = au.id.capture_epoch;
  last_au_codec_ = au.id.codec_epoch;
  fan_out_seen_ = true;
  for (auto& kv : conns_) {
    SubConn* c = kv.second.get();
    std::lock_guard<std::mutex> clk(c->mu);
    if (!opts_.pipeline_v2) {
      // ---- v1 wire (M0-pinned): §7.9 drop policy, unchanged ----
      CountAuAction(kv.first, c->q->PushAu(is_idr, ev), is_idr);
    } else {
      // ---- v2 wire (M1 Task 4): WAIT_IDR state machine + 0x020B ----
      const SubSendQueue::V2Push r =
          c->q->PushAuV2(is_idr, au.id.capture_epoch, au.id.codec_epoch, ev);
      if (r.discontinuity) {
        // Epoch advance: tell the client to discard buffered frames and
        // wait for the IDR of exactly these epochs. Control frames pop
        // before video AUs, so the 0x020B reaches the wire ahead of the
        // recovery IDR regardless of push order.
        c->q->PushControl(
            Frame{kFlagEvent, kMsgStreamDiscontinuity, 0,
                  EncodeStreamDiscontinuity(r.capture_epoch, r.codec_epoch,
                                            kStreamDiscontinuityReason)});
        stats_.stream_discontinuities++;
        XNC_LOG_INFO("rt stream discontinuity sub=%u capture_epoch=%llu codec_epoch=%llu",
                     kv.first, static_cast<unsigned long long>(r.capture_epoch),
                     static_cast<unsigned long long>(r.codec_epoch));
        if (r.action == SubSendQueue::AuAction::kDroppedNeedKey) {
          // The new epoch's first AU was NOT its IDR (encoder lookahead can
          // delay the reset's forced IDR past the warm-up window). Re-arm a
          // merged request: the pipeline's on-demand path re-forces an IDR
          // of the new epoch (spec §13 lists codec-epoch change as a
          // request source). Never blocks OnAu.
          table_.MarkNeedsKeyframe(kv.first, "epoch_change");
          XNC_LOG_INFO("rt epoch change without IDR sub=%u (merged request)", kv.first);
        }
      }
      CountAuAction(kv.first, r.action, is_idr);
    }
    c->cv.notify_all();
  }
  return nullptr;  // never fatal: drops, does not abort the pipeline
}

void RtServer::CountAuAction(uint32_t sub_id, SubSendQueue::AuAction a,
                             bool is_idr) {
  switch (a) {
    case SubSendQueue::AuAction::kEnqueued:
    case SubSendQueue::AuAction::kEnqueuedDisplacingDeltas:
      stats_.frames_enqueued++;
      if (is_idr) stats_.keys_enqueued++;
      break;
    case SubSendQueue::AuAction::kDroppedNeedKey:
      // joiner awaiting its IDR (expected, spec §7.5 join-from-IDR); also
      // every delta suppressed by the v2 WAIT_IDR machine (M1 Task 4).
      stats_.frames_dropped++;
      stats_.frames_dropped_needkey++;
      break;
    case SubSendQueue::AuAction::kDroppedQueueFull:
      stats_.frames_dropped++;
      stats_.frames_dropped_overflow++;
      // Overflow bookkeeping: re-mark through the table so the merged
      // pending reason becomes "queue_overflow" (spec §7.5/§7.9). In v2
      // mode PushAuV2 already flushed the queue and re-armed WAIT_IDR.
      table_.MarkNeedsKeyframe(sub_id, "queue_overflow");
      XNC_LOG_INFO("rt queue overflow sub=%u (delta dropped)", sub_id);
      break;
  }
}

const char* RtServer::PendingIdrReason() {
  std::lock_guard<std::mutex> lk(mu_);
  const char* r = table_.pending_reason();
  if (r == nullptr) return nullptr;
  std::snprintf(pending_reason_buf_, sizeof(pending_reason_buf_), "%s", r);
  return pending_reason_buf_;
}

void RtServer::ConsumePendingIdr(const char* reason) {
  std::lock_guard<std::mutex> lk(mu_);
  if (reason == nullptr) return;
  if (std::strcmp(reason, "sub_join") == 0) stats_.idr_sub_join++;
  else if (std::strcmp(reason, "queue_overflow") == 0) stats_.idr_queue_overflow++;
  else if (std::strcmp(reason, "explicit") == 0) stats_.idr_explicit++;
  else stats_.idr_other++;
}

void RtServer::OnState(const char* code, bool recoverable) {
  if (code == nullptr) return;
  const bool rebuilt = std::strcmp(code, "capture_rebuilt") == 0;
  if (rebuilt) {
    gen_.fetch_add(1);  // next HOST_HELLO carries the new generation
    XNC_LOG_INFO("rt generation++ gen=%u", gen_.load());
  }
  std::lock_guard<std::mutex> lk(mu_);
  if (conns_.empty()) return;
  const Frame ev{kFlagEvent, kMsgState, 0, EncodeStateEvent(code, recoverable)};
  for (auto& kv : conns_) {
    std::lock_guard<std::mutex> clk(kv.second->mu);
    kv.second->q->PushControl(ev);
    if (rebuilt) {
      // M1 Task 4 (v2): the rebuild invalidated every pre-rebuild AU still
      // queued or in flight (encoder lookahead tail) - flush + WAIT_IDR;
      // the floor (last pre-rebuild fan-out epoch pair) suppresses the old
      // generation until the new one publishes, and 0x020B rides that first
      // AU of the new epoch, which the reset forces to be an IDR. A rebuild
      // BEFORE any AU was fanned out has no floor (nothing is stale yet):
      // skip - baseline-less subscribers keep join-from-IDR semantics and
      // still see the 0x020B at the real epoch advance. v1 has no epoch
      // concept: its flow below stays byte-identical to the M0-pinned
      // behavior.
      if (opts_.pipeline_v2 && fan_out_seen_)
        kv.second->q->OnRebuildDiscontinuity(last_au_cap_, last_au_codec_);
      kv.second->q->PushControl(Frame{
          kFlagEvent, kMsgHostHello, 0, BuildHelloPayload(opts_.fps)});
    }
    kv.second->cv.notify_all();
  }
  XNC_LOG_INFO("rt_state code=%s recoverable=%d subs=%zu", code, recoverable ? 1 : 0,
               conns_.size());
}

// DISPLAY_CHANGED 0x010A (M2-S1 Task 2): the pipeline completed a unified
// reset that changed the stream geometry. Updates the HOST_HELLO geometry
// for future subscribers and broadcasts the event (with the CURRENT gen -
// the preceding "capture_rebuilt" OnState already incremented it) to every
// attached connection as a control frame (never dropped by video policy).
void RtServer::OnDisplayChanged(uint32_t w, uint32_t h, const char* reason) {
  if (w == 0 || h == 0) return;
  std::lock_guard<std::mutex> lk(mu_);
  src_w_ = w;
  src_h_ = h;
  DisplayChangedPayload d;
  d.gen = gen_.load();
  d.w = w;
  d.h = h;
  CopyReason(d.reason, sizeof(d.reason), reason != nullptr ? reason : "?");
  const Frame ev{kFlagEvent, kMsgDisplayChanged, 0, EncodeDisplayChanged(d)};
  for (auto& kv : conns_) {
    std::lock_guard<std::mutex> clk(kv.second->mu);
    kv.second->q->PushControl(ev);
    kv.second->cv.notify_all();
  }
  stats_.display_changes++;
  XNC_LOG_INFO("rt_display_changed gen=%u w=%u h=%u reason=%s subs=%zu", d.gen, w, h,
               d.reason, conns_.size());
}

// ---- helpers / accessors ----

// Displays-table snapshot (M2-S3 Task 5): the provider is expected to be
// cheap + thread-safe (xnc-desktop wires DxgiDisplaysSnapshot; the selftest
// a fixed table). Empty when no provider is configured.
std::vector<DisplayInfo> RtServer::CurrentDisplays() {
  if (opts_.displays_fn == nullptr) return {};
  return opts_.displays_fn(opts_.displays_ctx);
}

// HOST_HELLO payload (M3 Task 3): legacy / v2 / v2+capabilities per opts_.
// Callers on threads that can race a 0x0129 snapshot fps under mu_ first.
// Displays are fetched via the provider (documented thread-safe).
std::vector<uint8_t> RtServer::BuildHelloPayload(uint32_t fps) {
  HostHelloPayload hh{gen_.load(), src_w_, src_h_, fps, opts_.max_subs};
  hh.displays = CurrentDisplays();
  if (!opts_.pipeline_v2) return EncodeHostHello(hh);
  if (opts_.set_video_config_fn != nullptr)
    return EncodeHostHelloV2Caps(hh, kHostCapSetVideoConfig);
  return EncodeHostHelloV2(hh);
}

// Pushes a control frame (never dropped while healthy). False = control
// backlog exhausted: the connection is wedged, disconnect it.
bool RtServer::PushControlTo(SubConn* c, const Frame& f) {
  std::lock_guard<std::mutex> clk(c->mu);
  if (!c->q->PushControl(f)) return false;
  c->cv.notify_all();
  return true;
}

// CursorManager sink target (cursor poll thread): one 0x0109 event to
// every attached subscriber. Cursor events are control frames - they must
// never be dropped by the video-queue policy, and at 8ms cadence worst
// case they share the bounded control backlog with STATE/PONG.
void RtServer::BroadcastCursor(int32_t x, int32_t y, uint8_t visible) {
  const Frame ev{kFlagEvent, kMsgCursor, 0, EncodeCursorEvent(x, y, visible)};
  std::lock_guard<std::mutex> lk(mu_);
  if (conns_.empty()) return;
  for (auto& kv : conns_) {
    std::lock_guard<std::mutex> clk(kv.second->mu);
    kv.second->q->PushControl(ev);
    kv.second->cv.notify_all();
  }
  stats_.cursor_events++;
}

// Post-detach bookkeeping: the sub's input seq baseline dies with its
// identity; the LAST subscriber leaving also stops the cursor poller and
// force-releases every injected key/button still held (keys are
// session-global OS state - releasing on any single detach would disrupt
// the other viewers' in-flight drags/shortcuts).
void RtServer::OnSubscriberRemoved(uint32_t sub_id) {
  bool empty = false;
  {
    std::lock_guard<std::mutex> lk(mu_);
    empty = table_.size() == 0;
  }
  if (opts_.input != nullptr) opts_.input->ForgetSub(sub_id);
  if (empty) {
    if (opts_.cursor != nullptr) opts_.cursor->Stop();
    if (opts_.input != nullptr) opts_.input->ReleaseAll();
    XNC_LOG_INFO("rt last subscriber gone: cursor stopped, inputs released");
  }
}

RtServer::Stats RtServer::stats() {
  std::lock_guard<std::mutex> lk(mu_);
  return stats_;
}

size_t RtServer::SubscriberCount() {
  std::lock_guard<std::mutex> lk(mu_);
  return table_.size();
}

uint32_t RtServer::generation() { return gen_.load(); }

}  // namespace xnc
