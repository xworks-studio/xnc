// service.h - SCM service mode glue (M2-Slice3 Task 1). xnc-core's
// --service <name> path: StartServiceCtrlDispatcher -> ServiceMain ->
// RunPipeServer on a worker thread; the Stop/Shutdown control handler
// flips the pipe server's stop flag (the same g_stop the console Ctrl+C
// handler sets), which drains: accept loop exits, capture/shell children
// are terminated SCOPED BY STORED HANDLE (never by image name), the WTS
// monitor stops, then the process reports STOPPED and exits.
//
// Credentials: the service is started by SCM (no stdin channel), so the
// pipe name/secret come from --pipe-name/--smoke-secret argv (dev
// topology; plaintext binPath precedent, see dev-topology.md warning) or
// the XNC_CORE_PIPE_NAME / XNC_CORE_SECRET_HEX environment fallback.
// SAS gate: service mode = OPEN (M2-Slice3 ledger ruling; the server
// capability ticket is the first gate, core --allow-sas is the
// second line). Console mode keeps the explicit --allow-sas flag.
#ifndef XNC_NATIVE_CORE_SERVICE_H_
#define XNC_NATIVE_CORE_SERVICE_H_

#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>

#include <cstddef>
#include <cstdint>

namespace xnc {

// Run as the Windows service <name> (must match the registered SCM name).
// Blocks until the service stops; returns the process exit code (0 on a
// clean SCM stop, 1 on dispatch/fatal errors - reported to SCM as the
// service-specific error code).
int RunService(const wchar_t* name, const wchar_t* pipe_name,
               const uint8_t* secret, size_t secret_len);

}  // namespace xnc

#endif  // XNC_NATIVE_CORE_SERVICE_H_
