// spawn.h - CreateProcessAsUserW into the console session, the M1 session
// bridge (spec 4.2: lpDesktop = "winsta0\default"; the child command line
// carries no secrets - session ticket and pipe_secret travel via the pipe
// later, never argv). The spawnable exe is WHITELISTED to the plain
// relative name xnc-desktop.exe, resolved against xnc-core.exe's own
// directory (spec 6.4 spirit: core resolves to fixed executable paths and
// never accepts path parameters). The string helpers are pure and covered
// by the selftest; SpawnInSession itself needs the TokenManager token and
// is verified live on LABS-XIAOXIN.
#ifndef XNC_NATIVE_CORE_SPAWN_H_
#define XNC_NATIVE_CORE_SPAWN_H_

#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>

#include <string>

namespace xnc {

// Whitelist check (pure): the only accepted forms are "xnc-desktop.exe"
// and ".\xnc-desktop.exe" (case-insensitive, like the filesystem).
// Absolute paths, UNC, "..", subdirectories, suffix tricks, embedded or
// trailing whitespace are all rejected by the exact-name comparison.
bool SpawnExeArgAllowed(const wchar_t* exe);

// dir + "\" + name, collapsing a single trailing slash on dir (pure; used
// to resolve the whitelisted exe next to xnc-core.exe).
std::wstring JoinSiblingPath(const std::wstring& dir, const std::wstring& name);

// Directory containing xnc-core.exe (own module path minus basename).
std::wstring OwnModuleDir();

// Join exe + argv[from..argc) into one child command line, quoting any
// argument that contains whitespace (pure; --diag-spawn forwards the rest
// of its own argv verbatim). Returns false on null/empty arguments (an
// empty string argument cannot be represented in this simple quoting).
bool BuildChildCommandLine(const wchar_t* exe, int argc, wchar_t** argv,
                           int from, std::wstring* out);

// Spawn the whitelisted exe (validated and resolved next to xnc-core.exe)
// with cmdline on the token's session and desktop ("winsta0\default").
// token comes from TokenManager::SessionSystemToken. The child's stdio is
// redirected to this process's stdout/stderr via duplicated inheritable
// handles (STARTF_USESTDHANDLES) so its XNC_LOG output reaches whatever
// captured xnc-core - the remote diag gate depends on this; without valid
// std handles the child gets a hidden console instead (not fatal).
// On success *pid is set and, when child_process is non-null, *child_process
// receives the owned process handle (caller CloseHandle's it; otherwise it
// is closed here).
bool SpawnInSession(HANDLE token, const wchar_t* exe, const wchar_t* cmdline,
                    DWORD* pid, HANDLE* child_process, std::string* err);

}  // namespace xnc

#endif  // XNC_NATIVE_CORE_SPAWN_H_
