// cursor_manager.cpp - GetCursorInfo poll loop (see cursor_manager.h).
#include "cursor_manager.h"

namespace xnc {

CursorManager::~CursorManager() { Stop(); }

void CursorManager::SetSink(Sink sink) {
  std::lock_guard<std::mutex> lk(mu_);
  sink_ = std::move(sink);
}

void CursorManager::Start(Sink sink) {
  {
    std::lock_guard<std::mutex> lk(mu_);
    sink_ = std::move(sink);
    has_last_ = false;  // fresh generation: first poll fires an initial event
  }
  if (running_.exchange(true)) return;  // already polling
  stop_.store(false);
  th_ = std::thread([this] {
    while (!stop_.load()) {
      PollOnce();
      Sleep(o_.poll_ms);  // 8ms cadence; Stop() waits at most one interval
    }
  });
}

void CursorManager::Stop() {
  if (!running_.exchange(false)) return;
  stop_.store(true);
  if (th_.joinable()) th_.join();
}

bool CursorManager::PollOnce() {
  CURSORINFO ci;
  ci.cbSize = sizeof(ci);
  ci.flags = 0;
  ci.hCursor = nullptr;
  ci.ptScreenPos.x = 0;
  ci.ptScreenPos.y = 0;
  if (!o_.get_cursor_info(&ci)) return false;  // no desktop access: skip
  const bool visible = (ci.flags & CURSOR_SHOWING) != 0;
  const int32_t sx = MapCursorToStream(ci.ptScreenPos.x, o_.hello_w,
                                       o_.get_system_metrics(SM_XVIRTUALSCREEN),
                                       o_.get_system_metrics(SM_CXVIRTUALSCREEN));
  const int32_t sy = MapCursorToStream(ci.ptScreenPos.y, o_.hello_h,
                                       o_.get_system_metrics(SM_YVIRTUALSCREEN),
                                       o_.get_system_metrics(SM_CYVIRTUALSCREEN));
  const uint8_t vis = visible ? 1 : 0;
  std::lock_guard<std::mutex> lk(mu_);
  if (has_last_ && sx == last_x_ && sy == last_y_ && vis == last_visible_)
    return false;
  has_last_ = true;
  last_x_ = sx;
  last_y_ = sy;
  last_visible_ = vis;
  if (sink_) sink_(sx, sy, vis);
  return true;
}

}  // namespace xnc
