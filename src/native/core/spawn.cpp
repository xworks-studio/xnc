// spawn.cpp - see spawn.h. Real spawn = CreateProcessAsUserW with the
// session-retagged SYSTEM token; verified live on LABS-XIAOXIN (--diag-spawn
// gate). Pure helpers are selftest-covered.
// 环境来源(env provenance):子进程环境由目标令牌派生(userenv
// CreateEnvironmentBlock,bInherit=FALSE),而非继承 xnc-core 自身的
// SYSTEM 服务环境 —— 修复线上用户态子进程(APPDATA/TEMP 指向
// systemprofile,ConPTY PSReadLine 写历史 Access denied)问题。构造
// 失败时降级为继承环境(现状行为,打 error 日志保证可见),不阻断
// spawn:可用的终端胜过死终端。
#include "spawn.h"

#include "../common/log.h"

#include <cstdint>
#include <cwchar>
#include <shlobj.h>  // SHGetKnownFolderPath / SHCreateDirectory (ResolveLogDir)
#include <userenv.h>  // CreateEnvironmentBlock / DestroyEnvironmentBlock
#include <vector>

namespace xnc {

bool SpawnExeArgAllowed(const wchar_t* exe) {
  if (!exe) return false;
  const wchar_t* name = exe;
  if (exe[0] == L'.' && exe[1] == L'\\') name = exe + 2;  // ".\" prefix ok
  // lstrcmpiW: case-insensitive wide compare (filesystem-like). RTV 重构后
  // 桌面工作进程为 xnc-host.exe（Rust），xnc-shell.exe 不变（CreateShell
  // spawns it next to xnc-core.exe）。
  return lstrcmpiW(name, L"xnc-host.exe") == 0 ||
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

std::wstring ResolveLogDir() {
  // 进程内缓存:调用点(service 启动重开 stderr、host/shell 的 --log-file
  // 拼路径)每会话多次触发,目录解析/创建只做一次。
  static const std::wstring cached = []() -> std::wstring {
    PWSTR base = nullptr;
    std::wstring dir;
    if (SUCCEEDED(SHGetKnownFolderPath(FOLDERID_ProgramData, 0, nullptr,
                                       &base)) &&
        base) {
      dir = std::wstring(base) + L"\\XNC\\logs";
      CoTaskMemFree(base);
      // SHCreateDirectory 递归建中间目录;已存在不算失败。
      const HRESULT hr = SHCreateDirectory(nullptr, dir.c_str());
      if (SUCCEEDED(hr) || hr == HRESULT_FROM_WIN32(ERROR_ALREADY_EXISTS)) {
        return dir;
      }
      XNC_LOG_ERROR("ResolveLogDir: create %ls failed hr=0x%08lX, falling "
                    "back to exe dir",
                    dir.c_str(), static_cast<unsigned long>(hr));
    } else {
      if (base) CoTaskMemFree(base);
      XNC_LOG_ERROR("ResolveLogDir: ProgramData resolve failed, falling back "
                    "to exe dir");
    }
    return OwnModuleDir();  // 极端 ACL 兜底:回到旧安装目录行为
  }();
  return cached;
}

std::wstring LogFilePath(const wchar_t* name) {
  return JoinSiblingPath(ResolveLogDir(), name);
}

void RotateLogFileIfLarge(const std::wstring& path, unsigned long long maxBytes) {
  if (maxBytes == 0) maxBytes = 8ull << 20;  // 8MB 缺省(规范 §3.2)
  WIN32_FILE_ATTRIBUTE_DATA st;
  if (!GetFileAttributesExW(path.c_str(), GetFileExInfoStandard, &st)) return;
  const unsigned long long size =
      (static_cast<unsigned long long>(st.nFileSizeHigh) << 32) | st.nFileSizeLow;
  if (size < maxBytes) return;
  // rename 链:.2→.3(MoveFileEx 覆盖)、.1→.2、path→.1;任一步失败
  // 静默中止(下次启动重试),绝不影响随后的追加打开。
  const int keep = 3;
  for (int i = keep - 1; i >= 1; i--) {
    std::wstring from = path + L"." + std::to_wstring(i);
    std::wstring to = path + L"." + std::to_wstring(i + 1);
    if (!MoveFileExW(from.c_str(), to.c_str(), MOVEFILE_REPLACE_EXISTING)) {
      // 源不存在是正常链尾;真失败(占用等)也直接中止。
      if (GetLastError() != ERROR_FILE_NOT_FOUND) return;
    }
  }
  MoveFileExW(path.c_str(), (path + L".1").c_str(), MOVEFILE_REPLACE_EXISTING);
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

// 目标令牌派生环境块(纯包装,userenv):以 token 所属用户的配置为源
// (APPDATA/TEMP/USERPROFILE 等随令牌走),bInherit=FALSE —— 与 xnc-core
// 自身(可能是 SYSTEM 服务)环境无关。产出 UTF-16 double-null 结尾块,
// 调用方须 DestroyEnvironmentBlock 回收;CreateProcessAsUserW 消费时必须
// 置 CREATE_UNICODE_ENVIRONMENT(否则按 ANSI 块解析损坏环境)。失败返回
// false 且 *env 置空(err 给出 "what err=N"),不抛异常不改动调用方状态。
bool BuildTokenEnvironment(HANDLE token, void** env, std::string* err) {
  auto fail = [err](const char* what, DWORD e) {
    if (err) *err = std::string(what) + " err=" + std::to_string(e);
    return false;
  };
  if (!env) return fail("null argument", ERROR_INVALID_PARAMETER);
  *env = nullptr;
  if (!token) return fail("null argument", ERROR_INVALID_PARAMETER);
  LPVOID block = nullptr;
  if (CreateEnvironmentBlock(&block, token, FALSE) == FALSE || !block) {
    const DWORD e = GetLastError();
    if (block) DestroyEnvironmentBlock(block);
    return fail("CreateEnvironmentBlock", e);
  }
  *env = block;
  return true;
}

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
      *err = "exe rejected by whitelist (only xnc-host.exe / xnc-shell.exe "
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
  // is unusable (service context) there is no fallback file: the child
  // opens its own log via --log-file (core passes it at spawn time), so
  // the stderr channel and the file channel stay single-sourced and no
  // 0-byte xnc-xnc-*.log placeholder is ever created (2026-08-24
  // observability incident follow-up).
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
    // 服务模式(无 console 句柄):不再做文件回退——子进程经 --log-file 自开
    // 日志(desktop/shell 的 XNC_LOG 双写 stderr+文件);此处 redirect=false
    // 意味着子进程日志仅走其自持通道,不产生额外的空文件。
    XNC_LOG_INFO("stdio redirect unavailable; child logs via its own --log-file");
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
  // 子进程环境 = 目标令牌派生(见文件头注释):CreateProcessAsUserW 若
  // 传 nullptr 则继承 xnc-core(生产为 SYSTEM 服务,session 0)环境,
  // 用户态子进程的 APPDATA/TEMP 会指向 systemprofile。构造失败降级为
  // nullptr(继承,修复前行为)并打日志 —— log.h 只有 info/error 两级,
  // 无 warn,降级用 error 保证可见。
  LPVOID env = nullptr;
  std::string env_err;
  if (!BuildTokenEnvironment(token, &env, &env_err)) {
    XNC_LOG_ERROR("spawn: token env block failed err=\"%s\"; child falls "
                  "back to inherited (core) environment - per-user vars "
                  "like APPDATA/TEMP may be wrong",
                  env_err.c_str());
    env = nullptr;
  }
  // 环境块为 UTF-16:必须加 CREATE_UNICODE_ENVIRONMENT,否则被按 ANSI
  // 块解析而损坏(经典陷阱)。
  const DWORD creation =
      env ? (CREATE_NO_WINDOW | CREATE_UNICODE_ENVIRONMENT) : CREATE_NO_WINDOW;
  PROCESS_INFORMATION pi{};
  // CREATE_NO_WINDOW: the child gets a hidden console instead of flashing
  // a window on the target session's desktop (logs travel via the
  // redirected handles above).
  const BOOL ok = CreateProcessAsUserW(
      token, full.c_str(), cmd.data(), nullptr, nullptr,
      redirect || stdin_redirect /*bInheritHandles*/, creation,
      env, nullptr, &si, &pi);
  const DWORD e = GetLastError();
  // 环境块在 CreateProcess 内部已被复制,调用返回即可回收 —— 成功/失败
  // 两条路径都经过这里,单一出口销毁。
  if (env) DestroyEnvironmentBlock(env);
  if (err_dup != nullptr && err_dup != out_dup) CloseHandle(err_dup);
  if (out_dup) CloseHandle(out_dup);
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
