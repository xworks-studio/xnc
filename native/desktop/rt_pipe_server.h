// rt_pipe_server.h - real-time XNIP pipe server for xnc-desktop (plan
// M1-Slice2 Task 2). Fan-out sink for the capture->encode Pipeline: every
// shaped Annex-B AU is broadcast as a MSG_FRAME event to all attached
// subscribers; subscribers ATTACH over the M0 mutual-HMAC handshake
// (common/handshake.h) and get HOST_HELLO immediately.
//
// Message set (plan Global Constraints - fixed-binary subset of spec §9.4,
// protobuf migration is deferred to M2; all payloads little-endian):
//   MSG_ATTACH      0x0102 req  [u32 sub_id][u32 max_fps][u32 max_w][u32 bitrate]
//   MSG_DETACH      0x0103 req  [u32 sub_id]
//   MSG_KEYFRAME_REQ 0x0104 req [u32 sub_id][char reason[32]] (NUL padded)
//   MSG_FRAME       0x0105 event [u32 sub_id_target=0 broadcast][u64 mono_us]
//                               [u8 key][u32 len][au bytes]
//   MSG_HOST_HELLO  0x0106 event [u32 gen][u32 w][u32 h][u32 fps][u32 max_subs]
//   MSG_STATE       0x0107 event [char code[32]][u8 recoverable]
//   MSG_INPUT       0x0108 req  [u32 sub_id][u64 seq][u8 type][payload']
//                               (M1-Slice3; see InputMsg below for the six
//                               payload' layouts MOVE/BUTTON/WHEEL/KEY/TEXT/LOCK)
//   MSG_CURSOR      0x0109 event [s32 x][s32 y][u8 visible]
//                               (M1-Slice3; HOST_HELLO-space logical px)
//   MSG_DISPLAY_CHANGED 0x010A event [u32 gen][u32 w][u32 h][char reason[24]]
//                               (M2-Slice1 Task 2; broadcast when a unified
//                               capture reset changed the stream geometry;
//                               subscribers treat it as HOST_HELLO update)
//   MSG_SWITCH_DISPLAY 0x0128 req  [u32 idx]  (M2-Slice3 Task 5)
//                               validate idx < displays count -> select +
//                               unified CaptureReset(reason="switch"; the
//                               rebuild rebinds that output, ForceIDR via the
//                               reset's base-frame rewind, then HOST_HELLO
//                               re-emit + DISPLAY_CHANGED reason=switch);
//                               invalid -> STATE{invalid_display} (recoverable)
//                               + error response, NO reset.
//                               HOST_HELLO displays[] extension (same task,
//                               compatible): 20 legacy bytes + [u32 count]
//                               { [u32 idx][s32 originX][s32 originY][u32 w]
//                                 [u32 h][u8 primary] } (21 bytes/entry);
//                               legacy w/h/fps/max_subs stay = the ACTIVE
//                               display's geometry.
//
// sub_ids are client-chosen (non-zero, unique per server). IDR semantics
// (spec §7.5 + Slice1 carry-over): attach/queue-overflow/explicit requests
// are MERGED by SubscriberTable into one pending reason; the Pipeline arms
// MfSoftEncoder::ForceNextIdr at most once per 500ms and, on a static
// screen, re-feeds the cached base frame (bounded, never re-forcing) until
// the IDR AU emerges - the on-demand warm-up.
//
// Input/cursor wiring (M1-Slice3 Task 1): RtServer.Opts carries OPTIONAL
// pointers to an externally-owned InputManager and CursorManager. 0x0108
// frames from an attached connection are decoded here and forwarded to
// InputManager::Inject (the payload sub_id must equal the connection's own
// sub_id; per-sub_id strictly-increasing seq is enforced INSIDE the
// InputManager next to the state it guards - see input_manager.h). The
// CursorManager polls GetCursorInfo while subscribers exist and pushes
// 0x0109 events back through RtServer::BroadcastCursor. When the LAST
// subscriber detaches the cursor poller stops and InputManager::ReleaseAll
// force-releases every recorded key/button (keys are session-global OS
// state: releasing on ANY single detach would disrupt the other viewers'
// in-flight input - ReleaseAll fires on last-detach / DRAIN only).
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
#include "capture.h"       // ICapture
#include "capture_reset.h"  // kResetReasonMax, CopyReason (0x010A reason field)
#include "dxgi_capture.h"  // DisplayInfo (HOST_HELLO displays[]; M2-S3 Task 5)
#include "mf_encoder.h"    // MfSoftEncoder
#include "pipeline.h"      // AuSink, PipelineOpts
#include "subscribers.h"

namespace xnc {

class InputManager;   // input_manager.h (opts pointers only in this header)
class CursorManager;  // cursor_manager.h

// ---- message types (plan Task 2; 0x0100/0x0101 are core's capture RPCs) ----
constexpr uint16_t kMsgAttach = 0x0102, kMsgDetach = 0x0103, kMsgKeyframeReq = 0x0104,
                   kMsgFrame = 0x0105, kMsgHostHello = 0x0106, kMsgState = 0x0107,
                   kMsgInput = 0x0108, kMsgCursor = 0x0109,
                   kMsgDisplayChanged = 0x010A, kMsgSwitchDisplay = 0x0128;

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
inline int32_t GetS32(const uint8_t* p) {
  return static_cast<int32_t>(GetU32(p));
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
  // M2-S3 Task 5: the displays table (empty = legacy 20-byte payload).
  std::vector<DisplayInfo> displays;
};
struct StateEventPayload {
  char code[32] = {0};  // NUL-padded fixed field
  uint8_t recoverable = 0;
};
struct KeyframeReqPayload {
  uint32_t sub_id = 0;
  char reason[32] = {0};  // NUL-padded fixed field (client accounting)
};
struct DisplayChangedPayload {
  uint32_t gen = 0, w = 0, h = 0;
  char reason[kResetReasonMax] = {0};  // NUL-padded fixed field (24 bytes)
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

// HOST_HELLO: legacy 20 bytes, then (M2-S3 Task 5) the displays block
// [u32 count]{ [u32 idx][s32 ox][s32 oy][u32 w][u32 h][u8 primary] }.
inline constexpr size_t kDisplayEntryBytes = 21;

inline std::vector<uint8_t> EncodeHostHello(const HostHelloPayload& h) {
  std::vector<uint8_t> p(20 + 4 + kDisplayEntryBytes * h.displays.size(), 0);
  rt_detail::PutU32(p.data(), h.gen);
  rt_detail::PutU32(p.data() + 4, h.w);
  rt_detail::PutU32(p.data() + 8, h.h);
  rt_detail::PutU32(p.data() + 12, h.fps);
  rt_detail::PutU32(p.data() + 16, h.max_subs);
  rt_detail::PutU32(p.data() + 20, static_cast<uint32_t>(h.displays.size()));
  for (size_t i = 0; i < h.displays.size(); ++i) {
    uint8_t* e = p.data() + 24 + kDisplayEntryBytes * i;
    const DisplayInfo& d = h.displays[i];
    rt_detail::PutU32(e, d.idx);
    rt_detail::PutU32(e + 4, static_cast<uint32_t>(d.origin_x));
    rt_detail::PutU32(e + 8, static_cast<uint32_t>(d.origin_y));
    rt_detail::PutU32(e + 12, d.w);
    rt_detail::PutU32(e + 16, d.h);
    e[20] = d.primary != 0 ? 1 : 0;
  }
  return p;
}
inline bool DecodeHostHello(const Frame& f, HostHelloPayload* out) {
  if (out == nullptr) return false;
  if (f.payload.size() == 20) {  // legacy server (displays unknown)
    out->displays.clear();
  } else {
    if (f.payload.size() < 24) return false;
    const uint32_t n = rt_detail::GetU32(f.payload.data() + 20);
    if (f.payload.size() != 24 + kDisplayEntryBytes * n) return false;
    out->displays.resize(n);
    for (uint32_t i = 0; i < n; ++i) {
      const uint8_t* e = f.payload.data() + 24 + kDisplayEntryBytes * i;
      DisplayInfo& d = out->displays[i];
      d.idx = rt_detail::GetU32(e);
      d.origin_x = rt_detail::GetS32(e + 4);
      d.origin_y = rt_detail::GetS32(e + 8);
      d.w = rt_detail::GetU32(e + 12);
      d.h = rt_detail::GetU32(e + 16);
      d.primary = e[20] != 0 ? 1 : 0;
    }
  }
  out->gen = rt_detail::GetU32(f.payload.data());
  out->w = rt_detail::GetU32(f.payload.data() + 4);
  out->h = rt_detail::GetU32(f.payload.data() + 8);
  out->fps = rt_detail::GetU32(f.payload.data() + 12);
  out->max_subs = rt_detail::GetU32(f.payload.data() + 16);
  return true;
}

// ---- 0x0128 MSG_SWITCH_DISPLAY req: [u32 idx] (M2-Slice3 Task 5) ----

inline std::vector<uint8_t> EncodeSwitchDisplay(uint32_t idx) {
  std::vector<uint8_t> p(4, 0);
  rt_detail::PutU32(p.data(), idx);
  return p;
}
inline bool DecodeSwitchDisplay(const Frame& f, uint32_t* idx) {
  if (idx == nullptr || f.payload.size() != 4) return false;
  *idx = rt_detail::GetU32(f.payload.data());
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

// ---- 0x010A MSG_DISPLAY_CHANGED event: [u32 gen][u32 w][u32 h]
// [char reason[24]] (M2-Slice1 Task 2). gen is the server's current
// generation (already incremented by the capture_rebuilt STATE that
// precedes this event); w/h the new stream geometry. ----

inline std::vector<uint8_t> EncodeDisplayChanged(const DisplayChangedPayload& d) {
  std::vector<uint8_t> p(12 + kResetReasonMax, 0);
  rt_detail::PutU32(p.data(), d.gen);
  rt_detail::PutU32(p.data() + 4, d.w);
  rt_detail::PutU32(p.data() + 8, d.h);
  CopyReason(reinterpret_cast<char*>(p.data()) + 12, kResetReasonMax, d.reason);
  return p;
}
inline bool DecodeDisplayChanged(const Frame& f, DisplayChangedPayload* out) {
  if (out == nullptr || f.payload.size() != 12 + kResetReasonMax) return false;
  out->gen = rt_detail::GetU32(f.payload.data());
  out->w = rt_detail::GetU32(f.payload.data() + 4);
  out->h = rt_detail::GetU32(f.payload.data() + 8);
  std::memcpy(out->reason, f.payload.data() + 12, kResetReasonMax);
  out->reason[kResetReasonMax - 1] = '\0';
  return true;
}

// ---- 0x0108 MSG_INPUT (plan M1-Slice3 Global Constraints; LE) ----
//
//   [u32 sub_id][u64 seq][u8 type][payload'], type:
//     1 MOVE   [s32 x][s32 y][u16 buttons]      buttons bit 1L 2R 4M 8X1 16X2
//     2 BUTTON [u8 btn][u8 down]                btn = the mask value 1/2/4/8/16
//     3 WHEEL  [s32 dx][s32 dy][u8 trackpad]    dy vertical, dx horizontal
//     4 KEY    [u16 scan][u8 down][u8 extended] scan = Windows scan code (set 1)
//     5 TEXT   [u16 len][utf16le units]         surrogate pairs ride as units
//     6 LOCK   [u8 caps][u8 num]                desired toggle states 0/1
//
// x/y are logical px in the HOST_HELLO w/h stream space (spec 11.3 maps them
// to the virtual desktop inside InputManager). MOVE is ABSOLUTE only -
// MOVE_RELATIVE (spec 11.3, delta clamp +-10000) is deferred to M2.
// Wire-shape/域 validation lives HERE (DecodeInputMsg); per-sub seq
// monotonicity and injection semantics live in InputManager::Inject.

inline constexpr uint8_t kInputMove = 1, kInputButton = 2, kInputWheel = 3,
                         kInputKey = 4, kInputText = 5, kInputLock = 6;
// Mouse button bitmask (spec 11.2 PointerMove.buttons).
inline constexpr uint16_t kBtnL = 1, kBtnR = 2, kBtnM = 4, kBtnX1 = 8,
                          kBtnX2 = 16, kBtnMaskAny = 31;
// TEXT unit bound: 512 UTF-16 units per message (desktop-side defensive cap;
// the agent enforces the <=2KiB message budget, spec 11.7).
inline constexpr uint16_t kMaxTextUnits = 512;

struct InputMsg {
  uint32_t sub_id = 0;
  uint64_t seq = 0;
  uint8_t type = 0;               // kInputMove..kInputLock
  int32_t x = 0, y = 0;           // MOVE logical px / WHEEL dx,dy
  uint16_t buttons = 0;           // MOVE button bitmask
  uint8_t btn = 0, down = 0;      // BUTTON (mask value / 0|1); down reused by KEY
  uint8_t trackpad = 0;           // WHEEL granularity selector
  uint16_t scan = 0;              // KEY scan code
  uint8_t extended = 0;           // KEY E0-prefix flag
  std::vector<uint16_t> text;     // TEXT UTF-16 code units
  uint8_t caps = 0, num = 0;      // LOCK desired states
};

// True when m is exactly one of the five valid button mask bits.
inline bool IsButtonMask(uint8_t mask) {
  return mask == kBtnL || mask == kBtnR || mask == kBtnM || mask == kBtnX1 ||
         mask == kBtnX2;
}

inline std::vector<uint8_t> EncodeInputMsg(const InputMsg& m) {
  std::vector<uint8_t> p;
  switch (m.type) {
    case kInputMove:
      p.resize(23, 0);
      rt_detail::PutU32(p.data() + 13, static_cast<uint32_t>(m.x));
      rt_detail::PutU32(p.data() + 17, static_cast<uint32_t>(m.y));
      rt_detail::PutU16(p.data() + 21, m.buttons);
      break;
    case kInputButton:
      p.resize(15, 0);
      p[13] = m.btn;
      p[14] = m.down;
      break;
    case kInputWheel:
      p.resize(22, 0);
      rt_detail::PutU32(p.data() + 13, static_cast<uint32_t>(m.x));
      rt_detail::PutU32(p.data() + 17, static_cast<uint32_t>(m.y));
      p[21] = m.trackpad;
      break;
    case kInputKey:
      p.resize(17, 0);
      rt_detail::PutU16(p.data() + 13, m.scan);
      p[15] = m.down;
      p[16] = m.extended;
      break;
    case kInputText:
      p.resize(size_t(15) + 2 * m.text.size(), 0);
      rt_detail::PutU16(p.data() + 13, static_cast<uint16_t>(m.text.size()));
      for (size_t i = 0; i < m.text.size(); ++i)
        rt_detail::PutU16(p.data() + 15 + 2 * i, m.text[i]);
      break;
    case kInputLock:
      p.resize(15, 0);
      p[13] = m.caps;
      p[14] = m.num;
      break;
    default:
      return {};
  }
  rt_detail::PutU32(p.data(), m.sub_id);
  rt_detail::PutU64(p.data() + 4, m.seq);
  p[12] = m.type;
  return p;
}

inline bool DecodeInputMsg(const Frame& f, InputMsg* out) {
  if (out == nullptr || f.payload.size() < 13) return false;
  const uint8_t* p = f.payload.data();
  const size_t rest = f.payload.size() - 13;
  out->sub_id = rt_detail::GetU32(p);
  out->seq = rt_detail::GetU64(p + 4);
  out->type = p[12];
  if (out->sub_id == 0) return false;
  switch (out->type) {
    case kInputMove:
      if (rest != 10) return false;
      out->x = rt_detail::GetS32(p + 13);
      out->y = rt_detail::GetS32(p + 17);
      out->buttons = rt_detail::GetU16(p + 21);
      return (out->buttons & ~kBtnMaskAny) == 0;
    case kInputButton:
      if (rest != 2) return false;
      out->btn = p[13];
      out->down = p[14];
      return IsButtonMask(out->btn) && out->down <= 1;
    case kInputWheel:
      if (rest != 9) return false;
      out->x = rt_detail::GetS32(p + 13);
      out->y = rt_detail::GetS32(p + 17);
      out->trackpad = p[21];
      return out->trackpad <= 1;
    case kInputKey:
      if (rest != 4) return false;
      out->scan = rt_detail::GetU16(p + 13);
      out->down = p[15];
      out->extended = p[16];
      return out->scan != 0 && out->down <= 1 && out->extended <= 1;
    case kInputText: {
      if (rest < 4) return false;
      const uint16_t len = rt_detail::GetU16(p + 13);
      if (len == 0 || len > kMaxTextUnits) return false;
      if (rest != size_t(2) + size_t(2) * len) return false;
      out->text.resize(len);
      for (uint16_t i = 0; i < len; ++i)
        out->text[i] = rt_detail::GetU16(p + 15 + 2 * i);
      return true;
    }
    case kInputLock:
      if (rest != 2) return false;
      out->caps = p[13];
      out->num = p[14];
      return out->caps <= 1 && out->num <= 1;
    default:
      return false;
  }
}

// ---- 0x0109 MSG_CURSOR event: [s32 x][s32 y][u8 visible] ----
// Coordinates are logical px in the HOST_HELLO w/h stream space (the
// InputManager's inverse mapping; see cursor_manager.h).

inline std::vector<uint8_t> EncodeCursorEvent(int32_t x, int32_t y, uint8_t visible) {
  std::vector<uint8_t> p(9, 0);
  rt_detail::PutU32(p.data(), static_cast<uint32_t>(x));
  rt_detail::PutU32(p.data() + 4, static_cast<uint32_t>(y));
  p[8] = visible != 0 ? 1 : 0;
  return p;
}
inline bool DecodeCursorEvent(const Frame& f, int32_t* x, int32_t* y,
                              uint8_t* visible) {
  if (x == nullptr || y == nullptr || visible == nullptr || f.payload.size() != 9)
    return false;
  *x = rt_detail::GetS32(f.payload.data());
  *y = rt_detail::GetS32(f.payload.data() + 4);
  *visible = f.payload[8];
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
    // M1-Slice3: externally-owned input/cursor managers (optional - null
    // leaves 0x0108 rejected-by-count and no cursor events, i.e. the pure
    // Slice2 video server). Both must outlive Start..Shutdown.
    InputManager* input = nullptr;
    CursorManager* cursor = nullptr;
    // M2-Slice1 Task 1: optional DesktopWatch name provider forwarded into
    // PipelineOpts (per-second diag_pipeline beat gains ` desktop=<name>`);
    // pure observation, no behavior change.
    const char* (*desktop_name_fn)(void*) = nullptr;
    void* desktop_name_ctx = nullptr;
    // M2-Slice1 Task 2: unified capture reset coordinator forwarded into
    // PipelineOpts (ACCESS_LOST / desktop-switch / resolution self-healing
    // with DISPLAY_CHANGED broadcasts). Optional - null keeps the legacy
    // fatal-on-error behavior.
    CaptureReset* reset = nullptr;
    // M2-Slice3 Task 5: displays-table provider (HOST_HELLO displays[] and
    // 0x0128 validation). Optional - null = empty table (every switch is
    // rejected as invalid_display). Returns a snapshot copy.
    std::vector<DisplayInfo> (*displays_fn)(void*) = nullptr;
    void* displays_ctx = nullptr;
    // 0x0128 handler: bind idx as the desired output (consumed at the next
    // rebuild). Returns false when the index is invalid (STATE
    // invalid_display; no reset). Optional - null = same as always-false.
    bool (*switch_display_fn)(void*, uint32_t idx) = nullptr;
    void* switch_display_ctx = nullptr;
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
    uint64_t input_received = 0;  // 0x0108 frames from attached connections
    uint64_t input_rejected = 0;  // decode/shape/sub_id-mismatch rejects
    uint64_t input_dropped = 0;   // stale seq or injection failure (Inject != ok)
    uint64_t cursor_events = 0;   // 0x0109 events broadcast
    uint64_t display_changes = 0; // 0x010A events broadcast (M2-S1 T2)
    uint64_t switch_accepted = 0; // 0x0128 accepted -> reset(reason=switch)
    uint64_t switch_invalid = 0;  // 0x0128 rejected idx / no handler (M2-S3 T5)
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
  void OnDisplayChanged(uint32_t w, uint32_t h, const char* reason) override;

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
  // Displays-table snapshot (opts_.displays_fn; empty when not wired).
  std::vector<DisplayInfo> CurrentDisplays();
  // Pushes a control frame to one subscriber (never dropped; backlog
  // overflow returns false = wedged connection, caller disconnects).
  bool PushControlTo(SubConn* c, const Frame& f);
  // CursorManager sink target: encodes 0x0109 and fans it out to every
  // attached subscriber (called on the cursor poll thread).
  void BroadcastCursor(int32_t x, int32_t y, uint8_t visible);
  // After a subscriber leaves the table: forgets its input seq baseline and,
  // when it was the LAST subscriber, stops the cursor poller and runs
  // InputManager::ReleaseAll (session-global key/button state). Callers must
  // NOT hold mu_.
  void OnSubscriberRemoved(uint32_t sub_id);

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
