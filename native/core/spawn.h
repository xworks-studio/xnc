// spawn.h - CreateProcessAsUserW into the console session, the M1 session
// bridge (spec 4.2: lpDesktop = "winsta0\default"; the child command line
// carries no secrets - the pipe_secret travels via the inherited stdin
// handle, never argv, spec 1.5). The spawnable exe is WHITELISTED to the
// plain relative name xnc-desktop.exe, resolved against xnc-core.exe's own
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
// token comes from TokenManager::SessionSystemToken. The child's stdio is
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
