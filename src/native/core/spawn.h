// spawn.h - CreateProcessAsUserW into the console session, the M1 session
// bridge (spec 4.2: lpDesktop = "winsta0\default"; the child command line
// carries no secrets - the RTV host's endpoint/token config blob travels via
// the inherited stdin handle, never argv, spec 1.5). The spawnable exe is
// WHITELISTED to the plain relative name xnc-host.exe, resolved against
// xnc-core.exe's own directory (spec 6.4 spirit: core resolves to fixed
// executable paths and never accepts path parameters). The string helpers
// are pure and covered by the selftest; SpawnInSession itself needs the
// TokenManager token and is verified live on LABS-XIAOXIN.
#ifndef XNC_NATIVE_CORE_SPAWN_H_
#define XNC_NATIVE_CORE_SPAWN_H_

#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>

#include <string>

namespace xnc {

// Whitelist check (pure): the only accepted forms are "xnc-host.exe" /
// "xnc-shell.exe" (each optionally with a ".\" prefix, case-insensitive,
// like the filesystem). Absolute paths, UNC, "..", subdirectories, suffix
// tricks, embedded or trailing whitespace are all rejected by the
// exact-name comparison. xnc-host.exe is the RTV desktop worker (Rust);
// xnc-shell.exe serves the CreateShell RPC's worker spawn.
bool SpawnExeArgAllowed(const wchar_t* exe);

// dir + "\" + name, collapsing a single trailing slash on dir (pure; used
// to resolve the whitelisted exe next to xnc-core.exe).
std::wstring JoinSiblingPath(const std::wstring& dir, const std::wstring& name);

// Directory containing xnc-core.exe (own module path minus basename).
std::wstring OwnModuleDir();

// 运行日志目录(2026-09-15 规范 §3):%ProgramData%\XNC\logs,按需递归
// 创建;创建失败(极端 ACL/策略)回落安装目录(旧行为)——日志路径问题
// 绝不阻断服务。进程内缓存(agent 首启也会建 logs\,双保险)。
std::wstring ResolveLogDir();

// ResolveLogDir() + "\" + name:xnc-core-service/xnc-host/xnc-shell 三份
// 运行日志的统一路径来源。
std::wstring LogFilePath(const wchar_t* name);

// 大小轮转(2026-09-15 规范 §3.2):path 现存体积超过 maxBytes 时执行
// rename 链(keep-1→keep 覆盖、…、.1→.2、path→.1),随后由调用方重开
// 追加。任何失败静默返回(轮转问题不阻断日志);maxBytes<=0 取缺省
// 8MB,keep 固定 3。core 服务日志为启动期轮转(XNC_LOG 低频,启动
// 检查覆盖绝大多数场景)。
void RotateLogFileIfLarge(const std::wstring& path, unsigned long long maxBytes = 0);

// 目标令牌派生环境块(userenv CreateEnvironmentBlock,bInherit=FALSE;
// 纯包装,selftest 覆盖):APPDATA/TEMP/USERPROFILE 等随令牌用户走,与
// xnc-core 自身(生产为 SYSTEM 服务)环境无关 —— 修复用户态子进程继承
// systemprofile 环境的线上问题。产出 UTF-16 double-null 块,调用方
// DestroyEnvironmentBlock 回收;喂给 CreateProcessAsUserW 时必须置
// CREATE_UNICODE_ENVIRONMENT。失败返回 false(*env 置空,err 可选给
// "what err=N")。
bool BuildTokenEnvironment(HANDLE token, void** env,
                           std::string* err = nullptr);

// Join exe + argv[from..argc) into one child command line, quoting any
// argument that contains whitespace (pure; --diag-spawn forwards the rest
// of its own argv verbatim). Unsafe shapes are REJECTED, never escaped
// (spec 6.4 spirit: args come from program-constructed argv only, so no
// caller ever needs escaping): an argument containing an embedded '"'
// (breaks the argument boundary) or ending in '\' (would escape the
// closing quote) fails the build, as do null and empty arguments (an
// empty string cannot be represented in this simple quoting) and a bad
// exe/out/from. err (optional) receives the reason, e.g.
// "embedded quote in arg[2] at char 5" / "trailing backslash in arg[1]
// at char 7" / "empty argument at arg[1]" (positions are 0-based wchar
// indices into the offending argv element).
bool BuildChildCommandLine(const wchar_t* exe, int argc, wchar_t** argv,
                           int from, std::wstring* out,
                           std::string* err = nullptr);

// Spawn the whitelisted exe (validated and resolved next to xnc-core.exe)
// with cmdline on the token's session and desktop ("winsta0\default").
// token comes from TokenManager::SessionSystemToken. 环境来源:子进程环境
// 由目标令牌派生(BuildTokenEnvironment,含 CREATE_UNICODE_
// ENVIRONMENT);构造失败时打日志降级为继承 xnc-core 自身环境,不阻断
// spawn —— 生产 SYSTEM 服务的继承环境会让用户态子进程的 APPDATA/TEMP
// 指向 systemprofile(线上 PSReadLine 写历史 Access denied 根因)。
// The child's stdio is
// redirected to this process's stdout/stderr via duplicated inheritable
// handles (STARTF_USESTDHANDLES) so its XNC_LOG output reaches whatever
// captured xnc-core - the remote diag gate depends on this; without valid
// std handles the child gets a hidden console instead (not fatal).
// child_stdin (optional, default null = no stdin override): an INHERITABLE
// handle the child receives as its stdin (STARTF_USESTDHANDLES). This is
// the desktop pipe secret channel - the secret never appears in argv
// (spec 1.5). Null keeps the diag-spawn behavior untouched.
// On success *pid is set and, when child_process is non-null, *child_process
// receives the owned process handle (caller CloseHandle's it; otherwise it
// is closed here).
bool SpawnInSession(HANDLE token, const wchar_t* exe, const wchar_t* cmdline,
                    DWORD* pid, HANDLE* child_process, std::string* err,
                    HANDLE child_stdin = nullptr);

}  // namespace xnc

#endif  // XNC_NATIVE_CORE_SPAWN_H_
