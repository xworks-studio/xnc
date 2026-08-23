// spawn.cpp - see spawn.h. Real spawn = CreateProcessAsUserW with the
// session-retagged SYSTEM token; verified live on LABS-XIAOXIN (--diag-spawn
// gate). Pure helpers are selftest-covered.
#include "spawn.h"

#include "../common/log.h"

#include <cstdint>
#include <cwchar>
#include <vector>

namespace xnc {

bool SpawnExeArgAllowed(const wchar_t* exe) {
  if (!exe) return false;
  const wchar_t* name = exe;
  if (exe[0] == L'.' && exe[1] == L'\\') name = exe + 2;  // ".\" prefix ok
  // lstrcmpiW: case-insensitive wide compare (filesystem-like). M2-Slice2
  // Task 3 adds xnc-shell.exe (CreateShell spawns it next to xnc-core.exe).
  return lstrcmpiW(name, L"xnc-desktop.exe") == 0 ||
         lstrcmpiW(name, L"xnc-shell.exe") == 0;
}

std::wstring JoinSiblingPath(const std::wstring& dir, const std::wstring& name) {
  if (dir.empty()) return name;
  if (dir.back() == L'\\' || dir.back() == L'/') return dir + name;
  return dir + L"\\" + name;
}

std::wstring OwnModuleDir() {
  wchar_t path[MAX_PATH];
  const DWORD n = GetModuleFileNameW(nullptr, path, MAX_PATH);
  if (n == 0 || n >= MAX_PATH) return L".";  // degenerate; never expected
  const std::wstring p(path, n);
  const size_t slash = p.find_last_of(L"\\/");
  if (slash == std::wstring::npos) return L".";
  return p.substr(0, slash);
}

bool BuildChildCommandLine(const wchar_t* exe, int argc, wchar_t** argv,
                           int from, std::wstring* out, std::string* err) {
  auto reject = [err](const char* what, int argi, int pos) {
    if (err)
      *err = pos < 0
                 ? std::string(what) + " at arg[" + std::to_string(argi) + "]"
                 : std::string(what) + " in arg[" + std::to_string(argi) +
                       "] at char " + std::to_string(pos);
    return false;
  };
  if (!exe || !*exe || !out || from < 0 || from > argc) {
    if (err) *err = "invalid exe/out/from";
    return false;
  }
  std::wstring cmd = exe;
  for (int i = from; i < argc; i++) {
    const wchar_t* a = argv[i];
    if (!a) return reject("null argument", i, -1);
    if (!*a) return reject("empty argument", i, -1);  // cannot be represented
    // Simple quoting: whitespace-bearing args get wrapped in double
    // quotes. Unsafe shapes are REJECTED, never escaped (spec 6.4 spirit:
    // args come from program-constructed argv only, so no caller needs
    // escaping): an embedded '"' breaks the argument boundary, and a
    // trailing '\' would escape the closing quote. Resolved in M1-Slice3
    // ("fix(native/core): reject quotes and trailing backslash in child
    // args"); was the M1-Slice1 final-review must-fix.
    const wchar_t* q = std::wcschr(a, L'"');
    if (q) return reject("embedded quote", i, static_cast<int>(q - a));
    const size_t len = std::wcslen(a);
    if (a[len - 1] == L'\\')
      return reject("trailing backslash", i, static_cast<int>(len - 1));
    cmd += L" ";
    if (std::wcspbrk(a, L" \t")) {
      cmd += L"\"";
      cmd += a;
      cmd += L"\"";
    } else {
      cmd += a;
    }
  }
  *out = std::move(cmd);
  return true;
}

namespace {

std::string Narrow(const wchar_t* s) {
  std::string out;
  for (const wchar_t* p = s; p && *p; ++p) out.push_back(*p < 128 ? (char)*p : '?');
  return out;
}

}  // namespace

bool SpawnInSession(HANDLE token, const wchar_t* exe, const wchar_t* cmdline,
                    DWORD* pid, HANDLE* child_process, std::string* err,
                    HANDLE child_stdin) {
  auto fail = [err](const char* what, DWORD e) {
    if (err) *err = std::string(what) + " err=" + std::to_string(e);
    return false;
  };
  if (!token || !exe || !cmdline || !pid)
    return fail("null argument", ERROR_INVALID_PARAMETER);
  *pid = 0;
  if (child_process) *child_process = nullptr;

  if (!SpawnExeArgAllowed(exe)) {
    if (err)
      *err = "exe rejected by whitelist (only xnc-desktop.exe / xnc-shell.exe "
             "next to xnc-core.exe): " + Narrow(exe);
    return false;
  }
  // The whitelisted name (whitespace-free validated) resolves next to
  // xnc-core.exe's own module dir - never a caller-supplied path.
  const std::wstring name = exe[0] == L'.' && exe[1] == L'\\'
                                ? std::wstring(exe + 2)
                                : std::wstring(exe);
  const std::wstring full = JoinSiblingPath(OwnModuleDir(), name);

  STARTUPINFOW si{};
  si.cb = sizeof(si);
  si.lpDesktop = const_cast<LPWSTR>(L"winsta0\\default");  // spec 4.2

  // Redirect the child's stdio to ours via duplicated inheritable handles
  // so its stderr (XNC_LOG) reaches the console/pipe that captured
  // xnc-core. The child keeps the SYSTEM retagged token, so inheriting
  // these handles passes nothing to a lesser principal. When our own stdio
  // is unusable the child just gets a hidden console and its logs are
  // lost - not fatal.
  HANDLE hout = GetStdHandle(STD_OUTPUT_HANDLE);
  HANDLE herr = GetStdHandle(STD_ERROR_HANDLE);
  HANDLE out_dup = nullptr, err_dup = nullptr;
  bool redirect = false;
  if (hout != nullptr && hout != INVALID_HANDLE_VALUE &&
      herr != nullptr && herr != INVALID_HANDLE_VALUE &&
      DuplicateHandle(GetCurrentProcess(), hout, GetCurrentProcess(),
                      &out_dup, 0, TRUE /*inheritable*/, DUPLICATE_SAME_ACCESS) &&
      DuplicateHandle(GetCurrentProcess(), herr, GetCurrentProcess(),
                      &err_dup, 0, TRUE, DUPLICATE_SAME_ACCESS)) {
    redirect = true;
  } else {
    if (out_dup) { CloseHandle(out_dup); out_dup = nullptr; }
    if (err_dup) { CloseHandle(err_dup); err_dup = nullptr; }
    XNC_LOG_INFO("stdio redirect unavailable (err=%lu); child logs go nowhere",
                 GetLastError());
  }
  // Caller-supplied stdin (the pipe-secret channel, spec 1.5): an already
  // INHERITABLE handle passed through as the child's hStdInput.
  const bool stdin_redirect =
      child_stdin != nullptr && child_stdin != INVALID_HANDLE_VALUE;
  if (redirect || stdin_redirect) {
    si.dwFlags = STARTF_USESTDHANDLES;
    si.hStdOutput = out_dup;
    si.hStdError = err_dup;
    si.hStdInput = stdin_redirect ? child_stdin : nullptr;
  }

  std::vector<wchar_t> cmd(cmdline, cmdline + std::wcslen(cmdline) + 1);
  PROCESS_INFORMATION pi{};
  // CREATE_NO_WINDOW: the child gets a hidden console instead of flashing
  // a window on the target session's desktop (logs travel via the
  // redirected handles above).
  const BOOL ok = CreateProcessAsUserW(
      token, full.c_str(), cmd.data(), nullptr, nullptr,
      redirect || stdin_redirect /*bInheritHandles*/, CREATE_NO_WINDOW,
      nullptr, nullptr, &si, &pi);
  const DWORD e = GetLastError();
  if (out_dup) CloseHandle(out_dup);
  if (err_dup) CloseHandle(err_dup);
  if (!ok) return fail("CreateProcessAsUserW", e);

  CloseHandle(pi.hThread);
  *pid = pi.dwProcessId;
  if (child_process) {
    *child_process = pi.hProcess;
  } else {
    CloseHandle(pi.hProcess);
  }
  return true;
}

}  // namespace xnc
