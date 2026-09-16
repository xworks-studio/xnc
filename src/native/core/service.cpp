// service.cpp - SCM service mode implementation (M2-Slice3 Task 1).
// Status ladder: START_PENDING -> RUNNING -> (Stop/Shutdown ctrl)
// STOP_PENDING -> STOPPED. The serve loop is the SAME RunPipeServer the
// console mode uses (watchdog, WTS monitor, concurrent connections), so a
// service stop drains exactly like a console Ctrl+C: accept loop exits,
// capture/shell children are terminated scoped by their STORED HANDLES,
// the WTS monitor stops, then STOPPED is reported.
//
// Known limitation (ledger note, Slice-3 follow-up): there is no
// core->desktop DRAIN handshake on the wire (verified: rt_pipe_server's
// DRAIN is host-internal), so the desktop child is TerminateProcess'd and
// its ReleaseAll-at-exit input release does NOT run on a core stop.
// Shell children likewise (StopCapture semantics: scoped Terminate).
#include <windows.h>

#include <atomic>
#include <cstdio>
#include <cstring>
#include <string>
#include <thread>

#include "../common/log.h"
#include "../common/version.h"
#include "pipe_server.h"
#include "service.h"
#include "spawn.h"  // LogFilePath(ResolveLogDir)

namespace xnc {
namespace {

constexpr DWORD kSvcType = SERVICE_WIN32_OWN_PROCESS;
// Stop-drain grace: RunPipeServer's accept loop polls the stop flag at
// kWaitSliceMs granularity; children are terminated synchronously inside
// RunPipeServer before it returns, so seconds are ample.
constexpr DWORD kStopGraceMs = 15000;

// Set by RunService before dispatch - ServiceMain has no access to the
// parsed argv (SCM passes only the registered args).
const wchar_t* g_pipe_name = nullptr;
const uint8_t* g_secret = nullptr;
size_t g_secret_len = 0;

SERVICE_STATUS_HANDLE g_status = nullptr;
HANDLE g_stop_event = nullptr;
std::atomic<int> g_exit_code{1};

void Report(DWORD state, DWORD wait_hint_ms, DWORD exit_code, DWORD svc_code) {
  if (!g_status) return;
  SERVICE_STATUS s{};
  s.dwServiceType = kSvcType;
  s.dwCurrentState = state;
  s.dwControlsAccepted =
      (state == SERVICE_RUNNING) ? SERVICE_ACCEPT_STOP | SERVICE_ACCEPT_SHUTDOWN : 0;
  s.dwWin32ExitCode = exit_code;
  s.dwServiceSpecificExitCode = svc_code;
  s.dwCheckPoint = (state == SERVICE_START_PENDING || state == SERVICE_STOP_PENDING) ? 1 : 0;
  s.dwWaitHint = wait_hint_ms;
  SetServiceStatus(g_status, &s);
}

DWORD WINAPI HandlerEx(DWORD ctrl, DWORD, void*, void*) {
  switch (ctrl) {
    case SERVICE_CONTROL_STOP:
    case SERVICE_CONTROL_SHUTDOWN:
      XNC_LOG_INFO("service: stop requested (ctrl=%lu) - drain begin", ctrl);
      Report(SERVICE_STOP_PENDING, kStopGraceMs, 0, 0);
      // Same drain as console Ctrl+C: the pipe server's stop flag.
      RequestCoreStop();
      if (g_stop_event) SetEvent(g_stop_event);
      return NO_ERROR;
    case SERVICE_CONTROL_INTERROGATE:
      return NO_ERROR;  // SCM caches the last SetServiceStatus
    default:
      return NO_ERROR;
  }
}

// Under SCM, stderr goes nowhere: reopen it as <logs dir>\xnc-core-service.log
// (append) so the drain/stop gate has a log to read. Console mode's stderr
// is untouched (this is ServiceMain-only). 2026-09-15 规范 §3:运行日志归
// %ProgramData%\XNC\logs\(ResolveLogDir 带创建与安装目录兜底)。
void RedirectServiceLog() {
  const std::wstring path = xnc::LogFilePath(L"xnc-core-service.log");
  if (_wfreopen(path.c_str(), L"a", stderr)) setvbuf(stderr, nullptr, _IONBF, 0);
}

void WINAPI ServiceMain(DWORD argc, wchar_t** argv) {
  RedirectServiceLog();
  const wchar_t* name = (argc >= 1 && argv && argv[0]) ? argv[0] : L"xnc-core";
  g_status = RegisterServiceCtrlHandlerExW(name, HandlerEx, nullptr);
  if (!g_status) {
    XNC_LOG_ERROR("service: RegisterServiceCtrlHandlerEx failed err=%lu",
                  GetLastError());
    return;
  }
  Report(SERVICE_START_PENDING, 10000, 0, 0);
  XNC_LOG_INFO("service: starting name=%ls pipe=%ls version=%hs", name,
               g_pipe_name, XNC_VERSION_S);

  g_stop_event = CreateEventW(nullptr, TRUE, FALSE, nullptr);
  if (!g_stop_event) {
    Report(SERVICE_STOPPED, 0, ERROR_NOT_ENOUGH_MEMORY, 0);
    return;
  }

  int rc = 1;
  std::thread serve([]() {
    // Service mode SAS = OPEN (M2-Slice3 ledger ruling: the server
    // capability ticket is the first gate; this is the second line).
    SetSasAllowed(true);
    XNC_LOG_INFO("service: sas gate OPEN (service mode; capability "
                 "ticket gates upstream)");
    g_exit_code = RunPipeServer(g_pipe_name, g_secret, g_secret_len);
  });
  // RunPipeServer returns only on stop (0) or fatal (1). RUNNING is
  // reported first so the SCM start succeeds even while the pipe name is
  // being probed; a fatal create error surfaces as a service exit(1).
  Report(SERVICE_RUNNING, 0, 0, 0);

  HANDLE done = serve.native_handle();
  // Sliced wait (retry on spurious WAIT_FAILED - the first XIAOXIN run hit
  // one with err=0): exit on stop OR serve-loop exit; keep heartbeating
  // otherwise (no watchdog dependency here, the serve loop heartbeats its
  // own).
  for (;;) {
    HANDLE waits[2] = {g_stop_event, done};
    DWORD w = WaitForMultipleObjects(2, waits, FALSE, 5000);
    if (w == WAIT_OBJECT_0 || w == WAIT_OBJECT_0 + 1) break;
    if (w == WAIT_FAILED) {
      XNC_LOG_ERROR("service: wait failed err=%lu (retrying)", GetLastError());
      Sleep(1000);
    }
  }
  if (WaitForSingleObject(done, kStopGraceMs) != WAIT_OBJECT_0) {
    XNC_LOG_ERROR("service: drain exceeded %lums - exiting", kStopGraceMs);
  }
  if (serve.joinable()) serve.join();  // g_exit_code is final after join
  rc = g_exit_code.load();
  CloseHandle(g_stop_event);
  g_stop_event = nullptr;

  XNC_LOG_INFO("service: drain complete (children terminated scoped by "
               "handle; desktop input release is TerminateProcess'd - "
               "no DRAIN handshake yet, Slice-3 follow-up) rc=%d", rc);
  // SCM exit-code semantics: 0 = clean stop; anything else must go out as
  // a Win32 error, not the raw rc (SCM treats nonzero dwWin32ExitCode as
  // failure - intended here: unexpected serve-loop exit).
  Report(SERVICE_STOPPED, 0, rc == 0 ? 0 : ERROR_SERVICE_REQUEST_TIMEOUT, 0);
  XNC_LOG_INFO("service: stopped name=%ls", name);
}

}  // namespace

int RunService(const wchar_t* name, const wchar_t* pipe_name,
               const uint8_t* secret, size_t secret_len) {
  g_pipe_name = pipe_name;
  g_secret = secret;
  g_secret_len = secret_len;
  // The dispatch table name must match the SCM-registered name; it is
  // built from the --service argument so one binary serves any dev/prod
  // service name (XNCCoreDev / XNCCore).
  std::wstring name_storage(name);
  SERVICE_TABLE_ENTRYW table[2] = {};
  table[0].lpServiceName = name_storage.data();
  table[0].lpServiceProc = ServiceMain;
  XNC_LOG_INFO("service: dispatching as \"%ls\"", name);
  if (!StartServiceCtrlDispatcherW(table)) {
    DWORD err = GetLastError();
    // 1063 = not started by SCM (console run of --service): say so.
    XNC_LOG_ERROR("service: dispatch failed err=%lu (%hs)", err,
                  err == 1063 /* ERROR_FAILED_SERVICE_START (1063; the
                                 SDK header guard hides it at default
                                 WIN32_LEAN_AND_MEVH levels) */
                      ? "not launched by the service control manager"
                      : "StartServiceCtrlDispatcher");
    return 1;
  }
  return 0;
}

}  // namespace xnc
