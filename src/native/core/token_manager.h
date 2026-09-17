// token_manager.h - session-bridge tokens for spawning workers into the
// interactive console session (spec 4.2 TokenManager, minimal M1 set).
// The xnc-desktop token is a duplicate of xnc-core's own SYSTEM primary
// token with TokenSessionId retagged to the target session, giving the
// child SYSTEM rights *and* access to the target session's winsta0
// desktops (locked console included, spec 6.4). WTSQueryUserToken probing
// and RDP-Active targets arrive with the real StartCapture RPC in
// M1-Slice3; this file only needs the console-session path.
#ifndef XNC_NATIVE_CORE_TOKEN_MANAGER_H_
#define XNC_NATIVE_CORE_TOKEN_MANAGER_H_

#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>

#include <cstdint>
#include <string>

namespace xnc {

// Pure validation (selftest-covered, needs no token privileges): the only
// legal spawn target in the M1 minimal set is the active console session.
// A LOCKED console session stays legal (spec 6.4) - that is a property of
// the caller-supplied active_console value (WTSGetActiveConsoleSessionId
// keeps reporting the locked console session), not of this comparison.
// active_console == 0xFFFFFFFF ("no physical console") rejects everything.
bool SessionTargetAllowed(uint32_t want, uint32_t active_console);

// 重启重解析（selftest 覆盖，纯函数）：控制台登录/注销会销毁并重建控制台
// 会话，会话 ID 随之改变。崩溃重启若复用 StartCapture 时刻的旧 ID，token
// 守卫必然 TOKEN_FAILED（旧 ID 已不是活动控制台）→ 退避滚满 + 降级锁，
// host 永远起不来（2026-09-17 真机事故：CAD 登录后视频流卡死）。重启的
// 意图是"把控制台采集拉回来"，应取当前活动控制台；无物理控制台
// （0xFFFFFFFF）时保底原值，保持既有失败语义不掩盖。
uint32_t ResolveRestartSession(uint32_t stored, uint32_t active_console);

class TokenManager {
 public:
  // Duplicate the calling process's primary token (SYSTEM; per spec 4.2
  // xnc-core runs as LocalSystem) and retag it via
  // SetTokenInformation(TokenSessionId = session_id) so a spawned child
  // lands in the target session with full SYSTEM rights. session_id must
  // pass SessionTargetAllowed against WTSGetActiveConsoleSessionId().
  // On success *out is owned by the caller (CloseHandle). Returns false
  // with *err set when the session check fails, when the caller lacks
  // SeTcbPrivilege (i.e. is not SYSTEM - the SetTokenInformation step
  // fails with ERROR_PRIVILEGE_NOT_HELD), or on any other Win32 error.
  static bool SessionSystemToken(uint32_t session_id, HANDLE* out,
                                 std::string* err);
};

}  // namespace xnc

#endif  // XNC_NATIVE_CORE_TOKEN_MANAGER_H_
