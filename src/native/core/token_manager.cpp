// token_manager.cpp - see token_manager.h. The token ops here succeed only
// for a SYSTEM / SeTcbPrivilege-holding caller (the production xnc-core
// service context; verified live on LABS-XIAOXIN where exec runs as
// SYSTEM in session 0). The pure validation half (SessionTargetAllowed)
// is covered by the selftest without any privileges.
#include "token_manager.h"

#include "../common/log.h"

#include <cstdio>

namespace xnc {

bool SessionTargetAllowed(uint32_t want, uint32_t active_console) {
  if (active_console == 0xFFFFFFFFUL) return false;  // no physical console
  return want == active_console;
}

uint32_t ResolveRestartSession(uint32_t stored, uint32_t active_console) {
  if (active_console != 0xFFFFFFFFUL && stored != active_console)
    return active_console;
  return stored;
}

namespace {

// Best-effort enable of SeTcbPrivilege on our own token: SYSTEM processes
// hold it but possibly disabled; AdjustTokenPrivileges flips it. Failure is
// not fatal here - if the privilege is genuinely absent, the subsequent
// SetTokenInformation fails with ERROR_PRIVILEGE_NOT_HELD and that error is
// the useful one to report.
bool EnableTcbPrivilege() {
  HANDLE self = nullptr;
  if (!OpenProcessToken(GetCurrentProcess(),
                        TOKEN_QUERY | TOKEN_ADJUST_PRIVILEGES, &self))
    return false;
  LUID luid{};
  bool ok = false;
  if (LookupPrivilegeValueW(nullptr, L"SeTcbPrivilege", &luid)) {
    TOKEN_PRIVILEGES tp{};
    tp.PrivilegeCount = 1;
    tp.Privileges[0].Luid = luid;
    tp.Privileges[0].Attributes = SE_PRIVILEGE_ENABLED;
    AdjustTokenPrivileges(self, FALSE, &tp, sizeof(tp), nullptr, nullptr);
    ok = GetLastError() == ERROR_SUCCESS;
  }
  CloseHandle(self);
  return ok;
}

}  // namespace

bool TokenManager::SessionSystemToken(uint32_t session_id, HANDLE* out,
                                      std::string* err) {
  auto fail = [err](const char* what, DWORD e) {
    if (err)
      *err = std::string(what) + " err=" + std::to_string(e);
    return false;
  };
  if (!out || !err) return fail("null out param", ERROR_INVALID_PARAMETER);
  *out = nullptr;
  *err = "";

  // Session gate (spec 6.4): only the active console session (locked ok).
  const DWORD active = WTSGetActiveConsoleSessionId();
  if (!SessionTargetAllowed(session_id, active)) {
    char msg[96];
    _snprintf_s(msg, sizeof(msg), _TRUNCATE,
                "session %lu is not the active console session (%lu)",
                static_cast<unsigned long>(session_id),
                static_cast<unsigned long>(active));
    *err = msg;
    return false;
  }

  HANDLE self = nullptr;
  if (!OpenProcessToken(GetCurrentProcess(), TOKEN_DUPLICATE | TOKEN_QUERY,
                        &self))
    return fail("OpenProcessToken", GetLastError());

  // Primary duplicate carrying everything CreateProcessAsUserW and the
  // session retag need. SecurityImpersonation level on a TokenPrimary
  // duplicate is the documented combination for CreateProcessAsUser.
  HANDLE dup = nullptr;
  if (!DuplicateTokenEx(self,
                        TOKEN_QUERY | TOKEN_DUPLICATE | TOKEN_ASSIGN_PRIMARY |
                            TOKEN_ADJUST_SESSIONID | TOKEN_ADJUST_DEFAULT,
                        nullptr, SecurityImpersonation, TokenPrimary, &dup)) {
    const DWORD e = GetLastError();
    CloseHandle(self);
    return fail("DuplicateTokenEx", e);
  }
  CloseHandle(self);

  if (!EnableTcbPrivilege())
    XNC_LOG_INFO("SeTcbPrivilege not enabled (err=%lu); continuing - "
                 "non-SYSTEM callers will fail at SetTokenInformation",
                 GetLastError());

  DWORD sid = session_id;  // SetTokenInformation takes a non-const LPVOID
  if (!SetTokenInformation(dup, TokenSessionId, &sid, sizeof(sid))) {
    const DWORD e = GetLastError();
    CloseHandle(dup);
    return fail("SetTokenInformation(TokenSessionId)", e);
  }
  *out = dup;
  return true;
}

}  // namespace xnc
