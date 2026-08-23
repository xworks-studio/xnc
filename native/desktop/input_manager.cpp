// input_manager.cpp - SendInput injection engine (see input_manager.h).
// Thread map: rt_pipe_server reader threads call Inject (serialized on
// mu_), the janitor thread calls JanitorSweep on the same lock, the owner
// calls ReleaseAll/ForgetSub/StopJanitor at teardown. Every SendInput
// batch is built as plain INPUT structs (zero-initialized, dwExtraInfo
// marker set) so the injectable seam records exactly what the OS would
// receive.
#include "input_manager.h"

#include "../common/log.h"

namespace xnc {
namespace {

constexpr int32_t kWheelClamp = 10000;  // defensive clamp on raw wheel deltas

INPUT MouseInput(DWORD flags, DWORD mouse_data, LONG dx, LONG dy) {
  INPUT i{};
  i.type = INPUT_MOUSE;
  i.mi.dx = dx;
  i.mi.dy = dy;
  i.mi.mouseData = mouse_data;
  i.mi.dwFlags = flags;
  i.mi.dwExtraInfo = kInputExtraInfoMarker;
  return i;
}

INPUT KeyInput(WORD scan, DWORD flags) {
  INPUT i{};
  i.type = INPUT_KEYBOARD;
  i.ki.wScan = scan;
  i.ki.wVk = 0;
  i.ki.dwFlags = flags;
  i.ki.dwExtraInfo = kInputExtraInfoMarker;
  return i;
}

// Button mask bit -> matching SendInput flags (+ XBUTTON mouseData).
DWORD ButtonFlags(uint16_t mask, bool down, DWORD* mouse_data) {
  if (mouse_data != nullptr) *mouse_data = 0;
  switch (mask) {
    case kBtnL: return down ? MOUSEEVENTF_LEFTDOWN : MOUSEEVENTF_LEFTUP;
    case kBtnR: return down ? MOUSEEVENTF_RIGHTDOWN : MOUSEEVENTF_RIGHTUP;
    case kBtnM: return down ? MOUSEEVENTF_MIDDLEDOWN : MOUSEEVENTF_MIDDLEUP;
    case kBtnX1:
    case kBtnX2:
      if (mouse_data != nullptr)
        *mouse_data = mask == kBtnX1 ? XBUTTON1 : XBUTTON2;
      return down ? MOUSEEVENTF_XDOWN : MOUSEEVENTF_XUP;
    default: return 0;
  }
}

// Wheel value: trackpad values are already fine-grained wheel units;
// notch values scale by WHEEL_DELTA. Signed value rides in the DWORD
// mouseData field (wheel convention).
DWORD ScaleWheel(int32_t d, bool trackpad) {
  int64_t v = trackpad ? static_cast<int64_t>(d)
                       : static_cast<int64_t>(d) * WHEEL_DELTA;
  if (v > 0x7FFFFFFFll) v = 0x7FFFFFFFll;
  if (v < -0x7FFFFFFFll) v = -0x7FFFFFFFll;
  return static_cast<DWORD>(static_cast<int32_t>(v));
}

}  // namespace

InputManager::~InputManager() {
  StopJanitor();
  ReleaseAll();
}

InputManager::Result InputManager::Inject(const InputMsg& m) {
  std::lock_guard<std::mutex> lk(mu_);
  // Per-sub strictly-increasing seq (replay/backward = stale). The seq is
  // recorded even when the injection below fails: the message is consumed.
  {
    const auto it = seq_.find(m.sub_id);
    if (it != seq_.end() && m.seq <= it->second) {
      stats_.stale_seq++;
      XNC_LOG_INFO("input_stale_seq sub=%u seq=%llu last=%llu", m.sub_id,
                   static_cast<unsigned long long>(m.seq),
                   static_cast<unsigned long long>(it->second));
      return Result::kStaleSeq;
    }
    seq_[m.sub_id] = m.seq;
  }

  switch (m.type) {
    case kInputMove: {
      if (o_.hello_w == 0 || o_.hello_h == 0) {
        stats_.invalid++;
        return Result::kInvalid;
      }
      uint16_t old_mask = 0;
      for (const auto& kv : buttons_down_) old_mask |= kv.first;
      const uint16_t new_mask = m.buttons & kBtnMaskAny;
      std::vector<INPUT> batch;
      // 1) released buttons: UP first, at the old position.
      for (int b = 0; b < 5; ++b) {
        const uint16_t bit = static_cast<uint16_t>(1u << b);
        if ((old_mask & bit) != 0 && (new_mask & bit) == 0) {
          DWORD data = 0;
          const DWORD fl = ButtonFlags(bit, false, &data);
          if (fl != 0) batch.push_back(MouseInput(fl, data, 0, 0));
        }
      }
      // 2) the absolute move over the real virtual desktop metrics.
      const LONG ax = MapMoveToAbs(m.x, o_.hello_w,
                                   o_.get_system_metrics(SM_XVIRTUALSCREEN),
                                   o_.get_system_metrics(SM_CXVIRTUALSCREEN));
      const LONG ay = MapMoveToAbs(m.y, o_.hello_h,
                                   o_.get_system_metrics(SM_YVIRTUALSCREEN),
                                   o_.get_system_metrics(SM_CYVIRTUALSCREEN));
      batch.push_back(MouseInput(MOUSEEVENTF_MOVE | MOUSEEVENTF_ABSOLUTE |
                                     MOUSEEVENTF_VIRTUALDESK,
                                 0, ax, ay));
      // 3) newly pressed buttons: DOWN last, at the target position.
      const uint64_t now = o_.clock_ms();
      for (int b = 0; b < 5; ++b) {
        const uint16_t bit = static_cast<uint16_t>(1u << b);
        if ((new_mask & bit) != 0 && (old_mask & bit) == 0) {
          DWORD data = 0;
          const DWORD fl = ButtonFlags(bit, true, &data);
          if (fl != 0) {
            batch.push_back(MouseInput(fl, data, 0, 0));
            buttons_down_[bit] = now;
          }
        } else if ((old_mask & bit) != 0 && (new_mask & bit) == 0) {
          buttons_down_.erase(bit);
        }
      }
      if (!SendBatchLocked(batch.data(), static_cast<UINT>(batch.size())))
        return Result::kSendFailed;
      stats_.injected++;
      return Result::kInjected;
    }
    case kInputButton: {
      if (!IsButtonMask(m.btn)) {
        stats_.invalid++;
        return Result::kInvalid;
      }
      const auto it = buttons_down_.find(m.btn);
      if (m.down != 0) {
        if (it != buttons_down_.end()) {
          stats_.injected++;  // already held: no event, keep original press time
          return Result::kInjected;
        }
        DWORD data = 0;
        const DWORD fl = ButtonFlags(m.btn, true, &data);
        INPUT e = MouseInput(fl, data, 0, 0);
        buttons_down_[m.btn] = o_.clock_ms();
        if (!SendBatchLocked(&e, 1)) return Result::kSendFailed;
      } else {
        if (it == buttons_down_.end()) {
          stats_.injected++;  // release of a button we never saw pressed
          return Result::kInjected;
        }
        DWORD data = 0;
        const DWORD fl = ButtonFlags(m.btn, false, &data);
        INPUT e = MouseInput(fl, data, 0, 0);
        buttons_down_.erase(it);
        if (!SendBatchLocked(&e, 1)) return Result::kSendFailed;
      }
      stats_.injected++;
      return Result::kInjected;
    }
    case kInputWheel: {
      std::vector<INPUT> batch;
      const int32_t dy = m.y < -kWheelClamp  ? -kWheelClamp
                         : m.y > kWheelClamp ? kWheelClamp
                                             : m.y;
      const int32_t dx = m.x < -kWheelClamp  ? -kWheelClamp
                         : m.x > kWheelClamp ? kWheelClamp
                                             : m.x;
      const bool tp = m.trackpad != 0;
      if (dy != 0)
        batch.push_back(MouseInput(MOUSEEVENTF_WHEEL, ScaleWheel(dy, tp), 0, 0));
      if (dx != 0)
        batch.push_back(MouseInput(MOUSEEVENTF_HWHEEL, ScaleWheel(dx, tp), 0, 0));
      if (batch.empty()) {
        stats_.injected++;  // zero wheel = no-op
        return Result::kInjected;
      }
      if (!SendBatchLocked(batch.data(), static_cast<UINT>(batch.size())))
        return Result::kSendFailed;
      stats_.injected++;
      return Result::kInjected;
    }
    case kInputKey: {
      if (m.scan == 0 || m.down > 1 || m.extended > 1) {
        stats_.invalid++;
        return Result::kInvalid;
      }
      const uint32_t kid =
          m.scan | (m.extended != 0 ? kKeyExtendedBit : 0u);
      const DWORD ext = m.extended != 0 ? KEYEVENTF_EXTENDEDKEY : 0;
      if (m.down != 0) {
        if (keys_down_.count(kid) != 0) {
          stats_.injected++;  // already held: keep the original press time
          return Result::kInjected;
        }
        INPUT e = KeyInput(m.scan, KEYEVENTF_SCANCODE | ext);
        keys_down_[kid] = o_.clock_ms();
        if (!SendBatchLocked(&e, 1)) return Result::kSendFailed;
      } else {
        const auto it = keys_down_.find(kid);
        if (it == keys_down_.end()) {
          stats_.injected++;  // up for a key we never saw pressed
          return Result::kInjected;
        }
        INPUT e = KeyInput(m.scan, KEYEVENTF_SCANCODE | KEYEVENTF_KEYUP | ext);
        keys_down_.erase(it);
        if (!SendBatchLocked(&e, 1)) return Result::kSendFailed;
      }
      stats_.injected++;
      return Result::kInjected;
    }
    case kInputText: {
      if (m.text.empty() || m.text.size() > kMaxTextUnits) {
        stats_.invalid++;
        return Result::kInvalid;
      }
      std::vector<INPUT> batch;
      batch.reserve(m.text.size() * 2);
      for (const uint16_t unit : m.text) {  // surrogate pairs ride as units
        batch.push_back(KeyInput(unit, KEYEVENTF_UNICODE));
        batch.push_back(KeyInput(unit, KEYEVENTF_UNICODE | KEYEVENTF_KEYUP));
      }
      if (!SendBatchLocked(batch.data(), static_cast<UINT>(batch.size())))
        return Result::kSendFailed;
      stats_.injected++;
      return Result::kInjected;
    }
    case kInputLock: {
      std::vector<INPUT> batch;
      const bool caps_on = (o_.get_key_state(VK_CAPITAL) & 1) != 0;
      const bool num_on = (o_.get_key_state(VK_NUMLOCK) & 1) != 0;
      if (static_cast<bool>(m.caps) != caps_on) {
        batch.push_back(KeyInput(kScanCapsLock, KEYEVENTF_SCANCODE));
        batch.push_back(KeyInput(kScanCapsLock, KEYEVENTF_SCANCODE | KEYEVENTF_KEYUP));
      }
      if (static_cast<bool>(m.num) != num_on) {
        // Plain 0x45, NOT E0-prefixed: T6 live evidence (slice3 gate 1)
        // showed E0 0x45 does not toggle VK_NUMLOCK — two full E0 down/up
        // pairs rode through with the toggle state unchanged. The web /
        // e2eviewer KEY path already sends plain 0x45 and toggles fine;
        // lock sync now matches it.
        batch.push_back(KeyInput(kScanNumLock, KEYEVENTF_SCANCODE));
        batch.push_back(KeyInput(kScanNumLock, KEYEVENTF_SCANCODE |
                                                   KEYEVENTF_KEYUP));
      }
      if (batch.empty()) {
        stats_.injected++;  // already in the desired state
        return Result::kInjected;
      }
      if (!SendBatchLocked(batch.data(), static_cast<UINT>(batch.size())))
        return Result::kSendFailed;
      stats_.injected++;
      return Result::kInjected;
    }
    default:
      stats_.invalid++;
      return Result::kInvalid;
  }
}

void InputManager::ReleaseAll() {
  std::vector<INPUT> batch;
  {
    std::lock_guard<std::mutex> lk(mu_);
    if (keys_down_.empty() && buttons_down_.empty()) return;
    for (const auto& kv : keys_down_) {
      const bool ext = (kv.first & kKeyExtendedBit) != 0;
      batch.push_back(KeyInput(static_cast<WORD>(kv.first & 0xFFFF),
                               KEYEVENTF_SCANCODE | KEYEVENTF_KEYUP |
                                   (ext ? KEYEVENTF_EXTENDEDKEY : 0u)));
    }
    for (const auto& kv : buttons_down_) {
      DWORD data = 0;
      const DWORD fl = ButtonFlags(kv.first, false, &data);
      if (fl != 0) batch.push_back(MouseInput(fl, data, 0, 0));
    }
    const size_t keys = keys_down_.size(), buttons = buttons_down_.size();
    keys_down_.clear();
    buttons_down_.clear();
    stats_.release_all++;
    XNC_LOG_INFO("input_release_all keys=%zu buttons=%zu", keys, buttons);
  }
  // Send outside mu_: nothing else can race us for these inputs (the
  // tables are already cleared), and SendBatchLocked requires the lock.
  if (!batch.empty() &&
      o_.send_input(static_cast<UINT>(batch.size()), const_cast<INPUT*>(batch.data()),
                    static_cast<int>(sizeof(INPUT))) != batch.size()) {
    XNC_LOG_ERROR("input_release_all send failed (batch=%zu)",
                  static_cast<size_t>(batch.size()));
  }
}

void InputManager::ForgetSub(uint32_t sub_id) {
  std::lock_guard<std::mutex> lk(mu_);
  seq_.erase(sub_id);
}

void InputManager::StartJanitor() {
  if (janitor_.joinable()) return;
  janitor_stop_.store(false);
  janitor_ = std::thread([this] {
    for (;;) {
      for (uint32_t slept = 0; slept < o_.janitor_scan_ms; slept += 100) {
        if (janitor_stop_.load()) return;
        Sleep(100);  // sliced so StopJanitor never waits a full interval
      }
      JanitorSweep(o_.clock_ms());
    }
  });
}

void InputManager::StopJanitor() {
  janitor_stop_.store(true);
  if (janitor_.joinable()) janitor_.join();
}

void InputManager::JanitorSweep(uint64_t now_ms) {
  std::vector<INPUT> batch;
  {
    std::lock_guard<std::mutex> lk(mu_);
    for (auto it = keys_down_.begin(); it != keys_down_.end();) {
      if (it->second <= now_ms && now_ms - it->second > o_.stuck_release_ms) {
        const bool ext = (it->first & kKeyExtendedBit) != 0;
        batch.push_back(KeyInput(static_cast<WORD>(it->first & 0xFFFF),
                                 KEYEVENTF_SCANCODE | KEYEVENTF_KEYUP |
                                     (ext ? KEYEVENTF_EXTENDEDKEY : 0u)));
        it = keys_down_.erase(it);
      } else {
        ++it;
      }
    }
    for (auto it = buttons_down_.begin(); it != buttons_down_.end();) {
      if (it->second <= now_ms && now_ms - it->second > o_.stuck_release_ms) {
        DWORD data = 0;
        const DWORD fl = ButtonFlags(it->first, false, &data);
        if (fl != 0) batch.push_back(MouseInput(fl, data, 0, 0));
        it = buttons_down_.erase(it);
      } else {
        ++it;
      }
    }
    if (!batch.empty()) {
      stats_.janitor_released += batch.size();
      XNC_LOG_INFO("input_janitor_released n=%zu", static_cast<size_t>(batch.size()));
    }
  }
  if (!batch.empty() &&
      o_.send_input(static_cast<UINT>(batch.size()), const_cast<INPUT*>(batch.data()),
                    static_cast<int>(sizeof(INPUT))) != batch.size()) {
    XNC_LOG_ERROR("input_janitor send failed (batch=%zu)",
                  static_cast<size_t>(batch.size()));
  }
}

InputManager::Stats InputManager::stats() {
  std::lock_guard<std::mutex> lk(mu_);
  return stats_;
}

size_t InputManager::HeldKeys() {
  std::lock_guard<std::mutex> lk(mu_);
  return keys_down_.size();
}

size_t InputManager::HeldButtons() {
  std::lock_guard<std::mutex> lk(mu_);
  return buttons_down_.size();
}

bool InputManager::SendBatchLocked(const INPUT* in, UINT n) {
  if (n == 0 || in == nullptr) return true;
  INPUT* inputs = const_cast<INPUT*>(in);  // SendInput's LPINPUT signature
  if (o_.send_input(n, inputs, static_cast<int>(sizeof(INPUT))) == n) return true;
  // One desktop rebind, one retry (spec 7.3/11.5).
  if (RebindInputDesktopLocked()) {
    stats_.desktop_rebinds++;
    if (o_.send_input(n, inputs, static_cast<int>(sizeof(INPUT))) == n) return true;
  }
  stats_.send_failures++;
  stats_.desktop_mismatch++;  // INPUT_DESKTOP_MISMATCH
  XNC_LOG_ERROR("input_desktop_mismatch batch=%u", n);
  return false;
}

bool InputManager::RebindInputDesktopLocked() {
  HDESK desk = o_.open_input_desktop(0, FALSE, GENERIC_ALL);
  if (desk == nullptr) {
    XNC_LOG_INFO("input_rebind open_input_desktop failed err=%lu", GetLastError());
    return false;
  }
  const BOOL ok = o_.set_thread_desktop(desk);
  CloseDesktop(desk);
  return ok != FALSE;
}

}  // namespace xnc
