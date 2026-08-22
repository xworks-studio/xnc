// rt_pipe_server.h - real-time XNIP pipe server for xnc-desktop (plan
// M1-Slice2 Task 2). Fan-out sink for the capture->encode Pipeline: every
// shaped Annex-B AU is broadcast as a MSG_FRAME event to all attached
// subscribers; subscribers ATTACH over the M0 mutual-HMAC handshake
// (common/handshake.h) and get HOST_HELLO immediately.
//
// Message set (plan Global Constraints - fixed-binary subset of spec §9.4,
// protobuf migration is Slice3; all payloads little-endian):
//   MSG_ATTACH      0x0102 req  [u32 sub_id][u32 max_fps][u32 max_w][u32 bitrate]
//   MSG_DETACH      0x0103 req  [u32 sub_id]
//   MSG_KEYFRAME_REQ 0x0104 req [u32 sub_id][char reason[32]] (NUL padded)
//   MSG_FRAME       0x0105 event [u32 sub_id_target=0 broadcast][u64 mono_us]
//                               [u8 key][u32 len][au bytes]
//   MSG_HOST_HELLO  0x0106 event [u32 gen][u32 w][u32 h][u32 fps][u32 max_subs]
//   MSG_STATE       0x0107 event [char code[32]][u8 recoverable]
//
// sub_ids are client-chosen (non-zero, unique per server). IDR semantics
// (spec §7.5 + Slice1 carry-over): attach/queue-overflow/explicit requests
// are MERGED by SubscriberTable into one pending reason; the Pipeline arms
// MfSoftEncoder::ForceNextIdr at most once per 500ms and, on a static
// screen, re-feeds the cached base frame (bounded, never re-forcing) until
// the IDR AU emerges - the on-demand warm-up.
//
// Payload codecs below are pure header-only functions so the selftest can
// assert exact wire bytes without a pipe; RtServer itself is implemented
// in rt_pipe_server.cpp (accept thread + per-subscriber reader/sender).
#ifndef XNC_NATIVE_DESKTOP_RT_PIPE_SERVER_H_
#define XNC_NATIVE_DESKTOP_RT_PIPE_SERVER_H_

#include <atomic>
#include <cstddef>
#include <cstdint>
#include <cstring>
#include <condition_variable>
#include <map>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <vector>

#include "../common/frame.h"
#include "capture.h"     // ICapture
#include "mf_encoder.h"  // MfSoftEncoder
#include "pipeline.h"    // AuSink, PipelineOpts
#include "subscribers.h"

namespace xnc {

// ---- message types (plan Task 2; 0x0100/0x0101 are core's capture RPCs) ----
constexpr uint16_t kMsgAttach = 0x0102, kMsgDetach = 0x0103, kMsgKeyframeReq = 0x0104,
                   kMsgFrame = 0x0105, kMsgHostHello = 0x0106, kMsgState = 0x0107;

// AU payload bound (proto.MaxSessionFrameBytes).
inline constexpr size_t kMaxAuBytes = size_t(8) << 20;

namespace rt_detail {

inline void PutU16(uint8_t* p, uint16_t v) {
  p[0] = static_cast<uint8_t>(v);
  p[1] = static_cast<uint8_t>(v >> 8);
}
inline void PutU32(uint8_t* p, uint32_t v) {
  p[0] = static_cast<uint8_t>(v);
  p[1] = static_cast<uint8_t>(v >> 8);
  p[2] = static_cast<uint8_t>(v >> 16);
  p[3] = static_cast<uint8_t>(v >> 24);
}
inline void PutU64(uint8_t* p, uint64_t v) {
  for (int i = 0; i < 8; ++i) p[i] = static_cast<uint8_t>(v >> (8 * i));
}
inline uint16_t GetU16(const uint8_t* p) {
  return static_cast<uint16_t>(static_cast<uint16_t>(p[0]) |
                               static_cast<uint16_t>(p[1]) << 8);
}
inline uint32_t GetU32(const uint8_t* p) {
  return static_cast<uint32_t>(p[0]) | static_cast<uint32_t>(p[1]) << 8 |
         static_cast<uint32_t>(p[2]) << 16 | static_cast<uint32_t>(p[3]) << 24;
}
inline uint64_t GetU64(const uint8_t* p) {
  uint64_t v = 0;
  for (int i = 7; i >= 0; --i) v = (v << 8) | p[i];
  return v;
}

}  // namespace rt_detail

// ---- payload structs + codecs ----

struct AttachPayload {
  uint32_t sub_id = 0, max_fps = 0, max_w = 0, bitrate = 0;
};
struct FrameEventPayload {
  uint32_t target_sub_id = 0;  // 0 = broadcast
  uint64_t mono_us = 0;
  uint8_t key = 0;
  std::vector<uint8_t> au;
};
struct HostHelloPayload {
  uint32_t gen = 0, w = 0, h = 0, fps = 0, max_subs = 0;
};
struct StateEventPayload {
  char code[32] = {0};  // NUL-padded fixed field
  uint8_t recoverable = 0;
};
struct KeyframeReqPayload {
  uint32_t sub_id = 0;
  char reason[32] = {0};  // NUL-padded fixed field (client accounting)
};

// Bounded NUL-padded copy of `src` into dst[n-1可见] (strncpy-free: /W3
// clean); dst must have room for n bytes and is zero-padded past the copy.
inline void CopyPad32(char* dst, const char* src) {
  size_t i = 0;
  if (src != nullptr)
    for (; i < 31 && src[i] != '\0'; ++i) dst[i] = src[i];
  for (; i < 32; ++i) dst[i] = '\0';
}

inline std::vector<uint8_t> EncodeAttach(const AttachPayload& a) {
  std::vector<uint8_t> p(16, 0);
  rt_detail::PutU32(p.data(), a.sub_id);
  rt_detail::PutU32(p.data() + 4, a.max_fps);
  rt_detail::PutU32(p.data() + 8, a.max_w);
  rt_detail::PutU32(p.data() + 12, a.bitrate);
  return p;
}
inline bool DecodeAttach(const Frame& f, AttachPayload* out) {
  if (out == nullptr || f.payload.size() != 16) return false;
  out->sub_id = rt_detail::GetU32(f.payload.data());
  out->max_fps = rt_detail::GetU32(f.payload.data() + 4);
  out->max_w = rt_detail::GetU32(f.payload.data() + 8);
  out->bitrate = rt_detail::GetU32(f.payload.data() + 12);
  return out->sub_id != 0;
}

inline std::vector<uint8_t> EncodeDetach(uint32_t sub_id) {
  std::vector<uint8_t> p(4, 0);
  rt_detail::PutU32(p.data(), sub_id);
  return p;
}

inline std::vector<uint8_t> EncodeKeyframeReq(uint32_t sub_id, const char* reason) {
  std::vector<uint8_t> p(36, 0);
  rt_detail::PutU32(p.data(), sub_id);
  CopyPad32(reinterpret_cast<char*>(p.data()) + 4, reason);
  return p;
}
inline bool DecodeKeyframeReq(const Frame& f, KeyframeReqPayload* out) {
  if (out == nullptr || f.payload.size() != 36) return false;
  out->sub_id = rt_detail::GetU32(f.payload.data());
  std::memcpy(out->reason, f.payload.data() + 4, 32);
  out->reason[31] = '\0';
  return true;
}

// MSG_FRAME event payload. Oversize AUs (17+len > kMaxFrameBytes) yield an
// empty vector - callers drop the AU (8MiB AU bound honored).
inline std::vector<uint8_t> EncodeFrameEvent(uint64_t mono_us, bool key,
                                             const uint8_t* au, size_t len) {
  if (au == nullptr) len = 0;
  if (size_t(17) + len > kMaxFrameBytes) return {};
  std::vector<uint8_t> p(17 + len, 0);
  rt_detail::PutU32(p.data(), 0);  // sub_id_target = 0 (broadcast)
  rt_detail::PutU64(p.data() + 4, mono_us);
  p[12] = key ? 1 : 0;
  rt_detail::PutU32(p.data() + 13, static_cast<uint32_t>(len));
  if (len != 0) std::memcpy(p.data() + 17, au, len);
  return p;
}
inline bool DecodeFrameEvent(const Frame& f, FrameEventPayload* out) {
  if (out == nullptr || f.payload.size() < 17) return false;
  const uint32_t len = rt_detail::GetU32(f.payload.data() + 13);
  if (f.payload.size() != size_t(17) + len) return false;
  out->target_sub_id = rt_detail::GetU32(f.payload.data());
  out->mono_us = rt_detail::GetU64(f.payload.data() + 4);
  out->key = f.payload[12];
  out->au.assign(f.payload.begin() + 17, f.payload.end());
  return true;
}

inline std::vector<uint8_t> EncodeHostHello(const HostHelloPayload& h) {
  std::vector<uint8_t> p(20, 0);
  rt_detail::PutU32(p.data(), h.gen);
  rt_detail::PutU32(p.data() + 4, h.w);
  rt_detail::PutU32(p.data() + 8, h.h);
  rt_detail::PutU32(p.data() + 12, h.fps);
  rt_detail::PutU32(p.data() + 16, h.max_subs);
  return p;
}
inline bool DecodeHostHello(const Frame& f, HostHelloPayload* out) {
  if (out == nullptr || f.payload.size() != 20) return false;
  out->gen = rt_detail::GetU32(f.payload.data());
  out->w = rt_detail::GetU32(f.payload.data() + 4);
  out->h = rt_detail::GetU32(f.payload.data() + 8);
  out->fps = rt_detail::GetU32(f.payload.data() + 12);
  out->max_subs = rt_detail::GetU32(f.payload.data() + 16);
  return true;
}

inline std::vector<uint8_t> EncodeStateEvent(const char* code, bool recoverable) {
  std::vector<uint8_t> p(33, 0);
  CopyPad32(reinterpret_cast<char*>(p.data()), code);
  p[32] = recoverable ? 1 : 0;
  return p;
}
inline bool DecodeStateEvent(const Frame& f, StateEventPayload* out) {
  if (out == nullptr || f.payload.size() != 33) return false;
  std::memcpy(out->code, f.payload.data(), 32);
  out->code[31] = '\0';
  out->recoverable = f.payload[32];
  return true;
}

// ---- server ----

class RtServer : public AuSink {
 public:
  struct Opts {
    std::wstring pipe_name;              // full \\.\pipe\... name
    const uint8_t* secret = nullptr;     // pipe secret (M0 HMAC)
    size_t secret_len = 0;
    uint32_t max_subs = 4;               // capacity (plan: max 4)
    uint32_t fps = 30;                   // HOST_HELLO + pipeline pacing
    uint32_t bitrate_bps = 2300000;
    // Selftest-only permissive DACL (precedent: native/core/selftest.cpp
    // loopback). Production DACL is built from SYSTEM+Administrators+the
    // spawning user; the pipe secret carries the real authentication.
    const wchar_t* sddl_override = nullptr;
  };

  struct Stats {
    uint32_t attaches = 0, detaches = 0;
    uint32_t rejected_max_subs = 0, rejected_dup = 0, rejected_handshake = 0;
    uint64_t aus_emitted = 0;      // OnAu calls with a non-empty AU
    uint64_t frames_enqueued = 0;  // video AUs queued for >=1 subscriber
    uint64_t frames_dropped = 0;   // delta drops total (needkey + overflow)
    uint64_t frames_dropped_needkey = 0;   // joiner awaiting its IDR (expected)
    uint64_t frames_dropped_overflow = 0;  // queue full -> merged IDR request
    uint64_t keys_enqueued = 0;    // key AUs queued for >=1 subscriber
    uint64_t frames_oversize = 0;  // AUs above the 8MiB frame bound (dropped)
    uint64_t idr_sub_join = 0, idr_queue_overflow = 0, idr_explicit = 0,
             idr_other = 0;  // pipeline-armed IDRs by reason (accounting)
  };

  RtServer() = default;
  ~RtServer();
  RtServer(const RtServer&) = delete;
  RtServer& operator=(const RtServer&) = delete;

  // Creates the listening pipe + accept thread. src_w/src_h feed HOST_HELLO.
  // Returns false (logged) when the pipe cannot be created.
  bool Start(const Opts& o, uint32_t src_w, uint32_t src_h);

  // CLI convenience: Start -> Pipeline::Run (this as sink; Ctrl+C ->
  // RequestStop via the console handler; runs until stopped) -> Shutdown.
  int Serve(ICapture& capture, MfSoftEncoder& encoder, const Opts& o);

  // Idempotent: stops the accept loop, broadcasts STATE{stream_end} to the
  // attached subscribers, cancels their IO, joins every connection thread.
  // Blocks up to ~1s (sliced IO waits).
  void Shutdown();

  void RequestStop() { stop_.store(true); }

  // ---- AuSink (called on the pipeline thread) ----
  // Broadcasts one AU; never fatal (drops per subscriber instead).
  const char* OnAu(bool is_idr, uint64_t mono_us, const uint8_t* au,
                   size_t len) override;
  const char* PendingIdrReason() override;
  void ConsumePendingIdr(const char* reason) override;
  void OnState(const char* code, bool recoverable) override;

  Stats stats();
  size_t SubscriberCount();
  uint32_t generation();

 private:
  struct SubConn {
    uint32_t sub_id = 0;
    HANDLE pipe = INVALID_HANDLE_VALUE;  // owned; closed by the reader thread
    std::shared_ptr<SubSendQueue> q = std::make_shared<SubSendQueue>();
    std::mutex mu;                  // guards q and the flags below
    std::condition_variable cv;     // sender wait (work / death)
    std::atomic<bool> dead{false};  // no more sends; sender drains then exits
    std::thread sender;             // joined by the reader thread
  };

  void AcceptLoop(HANDLE first_pipe);
  void ReaderLoop(std::shared_ptr<SubConn> c);
  void SenderLoop(std::shared_ptr<SubConn> c);
  // Pushes a control frame to one subscriber (never dropped; backlog
  // overflow returns false = wedged connection, caller disconnects).
  bool PushControlTo(SubConn* c, const Frame& f);

  Opts opts_;
  uint32_t src_w_ = 0, src_h_ = 0;
  std::atomic<bool> stop_{false};
  std::atomic<uint32_t> gen_{1};
  mutable std::mutex mu_;  // guards table_, conns_, stats_, readers_
  SubscriberTable table_;
  std::map<uint32_t, std::shared_ptr<SubConn>> conns_;
  Stats stats_;
  char pending_reason_buf_[32] = {0};
  std::vector<std::thread> readers_;  // reader threads (joined in Shutdown)
  std::thread accept_;
  bool started_ = false;
};

}  // namespace xnc

#endif  // XNC_NATIVE_DESKTOP_RT_PIPE_SERVER_H_
