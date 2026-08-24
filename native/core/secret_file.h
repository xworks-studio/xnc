// secret_file.h - service-mode pipe secret persistence (prod bootstrap).
// xnc-core --service <name> --secret-file <path>: at service start the file
// is read (hex + optional trailing newline, same parser family as
// --smoke-secret); when missing, a 32-byte secret is generated
// (BCryptGenRandom), written as hex + newline, and the file DACL is locked
// to SYSTEM + Administrators only (PROTECTED_DACL, SDDL
// "D:P(A;;FA;;;SY)(A;;FA;;;BA)"). The secret value itself is NEVER logged.
#ifndef XNC_CORE_SECRET_FILE_H_
#define XNC_CORE_SECRET_FILE_H_

#include <windows.h>

#include <string>

namespace xnc {

// Reads <path> (hex + optional trailing whitespace/newline; nominal 64 hex
// chars = 32 bytes) into `secret`. If the file does not exist, generates 32
// random bytes, writes hex + newline, locks the DACL, and returns the
// secret with generated=true. Returns false + a human-readable `err` on any
// I/O, parse, RNG or ACL failure (caller must refuse to serve rather than
// fall back to a weak default).
bool LoadOrCreateSecretFile(const wchar_t* path, std::string& secret,
                            bool& generated, std::string& err);

}  // namespace xnc

#endif  // XNC_CORE_SECRET_FILE_H_
