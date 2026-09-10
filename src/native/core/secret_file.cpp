// secret_file.cpp - see secret_file.h. The hex parser mirrors the
// --smoke-secret argv parser (even count, 2..2*kMaxPipeSecretBytes chars);
// a trailing '\r'/'\n' (or any trailing whitespace) written by the file
// round-trip or an editor is tolerated.
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>
#include <sddl.h>

#include <bcrypt.h>

#include <aclapi.h>

#include <cstdio>
#include <cstring>
#include <cwctype>
#include <string>
#include <utility>

#include "../common/handshake.h"
#include "../common/log.h"
#include "pipe_server.h"
#include "secret_file.h"

namespace xnc {
namespace {

int HexValW(wchar_t c) {
  if (c >= L'0' && c <= L'9') return c - L'0';
  if (c >= L'a' && c <= L'f') return c - L'a' + 10;
  if (c >= L'A' && c <= L'F') return c - L'A' + 10;
  return -1;
}

// Trim trailing whitespace (covers "\r\n" and friends); leading whitespace
// is a parse error (same strictness as the argv parser).
std::wstring TrimTrailing(const std::wstring& s) {
  size_t end = s.size();
  while (end > 0 && iswspace(s[end - 1])) end--;
  return s.substr(0, end);
}

bool ParseSecretHexW(const std::wstring& hex, std::string& out) {
  size_t n = hex.size();
  // Min 16 bytes: a short secret is a hard error, never a weak fallback.
  // (Generation always writes 32 bytes.)
  if (n < 2 * 16 || n > 2 * kMaxPipeSecretBytes || n % 2 != 0) return false;
  out.resize(n / 2, 0);
  for (size_t i = 0; i < n / 2; i++) {
    int hi = HexValW(hex[2 * i]), lo = HexValW(hex[2 * i + 1]);
    if (hi < 0 || lo < 0) return false;
    out[i] = static_cast<char>((hi << 4) | lo);
  }
  return true;
}

// PROTECTED_DACL + SYSTEM/Admins full access, everyone else nothing.
// SetFileSecurity + a hand-built SD (SetNamedSecurityInfo rejects the
// ownerless SDDL-converted descriptor with ERROR_NO_TOKEN here).
bool LockSecretFileDacl(const wchar_t* path) {
  PSECURITY_DESCRIPTOR sddl_sd = nullptr;
  if (!ConvertStringSecurityDescriptorToSecurityDescriptorW(
          L"D:P(A;;FA;;;SY)(A;;FA;;;BA)", SDDL_REVISION_1, &sddl_sd,
          nullptr)) {
    return false;
  }
  BOOL present = FALSE, def = FALSE;
  PACL sddl_dacl = nullptr;
  BOOL got = GetSecurityDescriptorDacl(sddl_sd, &present, &sddl_dacl,
                                       &def) &&
             present && sddl_dacl;
  if (!got) {
    LocalFree(sddl_sd);
    SetLastError(ERROR_INVALID_SECURITY_DESCR);
    return false;
  }
  SECURITY_DESCRIPTOR sd;
  BOOL ok = InitializeSecurityDescriptor(&sd,
                                         SECURITY_DESCRIPTOR_REVISION) &&
            SetSecurityDescriptorDacl(&sd, TRUE, sddl_dacl, FALSE);
  if (ok) {
    // SE_DACL_PROTECTED = do not inherit parent-dir ACEs.
    sd.Control |= SE_DACL_PROTECTED;
    ok = SetFileSecurityW(path, DACL_SECURITY_INFORMATION, &sd);
  }
  DWORD gle = GetLastError();
  LocalFree(sddl_sd);
  if (!ok) SetLastError(gle);
  return ok != FALSE;
}

}  // namespace

bool LoadOrCreateSecretFile(const wchar_t* path, std::string& secret,
                            bool& generated, std::string& err) {
  generated = false;
  secret.clear();

  HANDLE f = CreateFileW(path, GENERIC_READ, FILE_SHARE_READ, nullptr,
                         OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL, nullptr);
  if (f != INVALID_HANDLE_VALUE) {
    char buf[2 * kMaxPipeSecretBytes + 16];  // hex + newline margin
    DWORD read_n = 0;
    BOOL ok = ReadFile(f, buf, sizeof(buf), &read_n, nullptr);
    CloseHandle(f);
    if (!ok) {
      err = "secret file read failed";
      return false;
    }
    // Reject if the file is larger than what we read (truncation risk).
    if (read_n == sizeof(buf)) {
      err = "secret file too large";
      return false;
    }
    int wlen = MultiByteToWideChar(CP_UTF8, 0, buf,
                                   static_cast<int>(read_n), nullptr, 0);
    std::wstring w(static_cast<size_t>(wlen), L'\0');
    MultiByteToWideChar(CP_UTF8, 0, buf, static_cast<int>(read_n),
                        &w[0], wlen);
    if (!ParseSecretHexW(TrimTrailing(w), secret)) {
      err = "secret file is not a valid hex secret";
      return false;
    }
    return true;
  }
  DWORD gle = GetLastError();
  if (gle != ERROR_FILE_NOT_FOUND) {
    err = "secret file open failed (err=" + std::to_string(gle) + ")";
    return false;
  }

  // Missing: generate 32 bytes; create the file EMPTY and DACL-lock it
  // BEFORE writing the secret (no inherit-ACL window where the hex content
  // is readable), then write hex + newline.
  uint8_t raw[32];
  NTSTATUS rng = BCryptGenRandom(nullptr, raw, sizeof(raw),
                                 BCRYPT_USE_SYSTEM_PREFERRED_RNG);
  if (!BCRYPT_SUCCESS(rng)) {
    err = "BCryptGenRandom failed";
    return false;
  }
  static const char kHex[] = "0123456789abcdef";
  std::string hex_text;
  hex_text.reserve(sizeof(raw) * 2 + 1);
  for (uint8_t b : raw) {
    hex_text.push_back(kHex[b >> 4]);
    hex_text.push_back(kHex[b & 0xF]);
  }
  hex_text.push_back('\n');

  f = CreateFileW(path, GENERIC_WRITE, 0, nullptr, CREATE_NEW,
                  FILE_ATTRIBUTE_NORMAL, nullptr);
  if (f == INVALID_HANDLE_VALUE) {
    gle = GetLastError();
    if (gle == ERROR_FILE_EXISTS) {  // lost a create race: reread
      return LoadOrCreateSecretFile(path, secret, generated, err);
    }
    err = "secret file create failed (err=" + std::to_string(gle) + ")";
    return false;
  }
  // Lock the (still empty) file's DACL first: between CREATE_NEW and this
  // SetFileSecurity the file exists but contains no secret material, so the
  // inherited parent-dir ACL cannot leak anything.
  if (!LockSecretFileDacl(path)) {
    gle = GetLastError();
    CloseHandle(f);
    DeleteFileW(path);  // never leave a half-initialized secret file behind
    SetLastError(gle);
    err = "secret file DACL lock failed (err=" + std::to_string(gle) + ")";
    return false;
  }
  DWORD written = 0;
  BOOL ok = WriteFile(f, hex_text.data(),
                      static_cast<DWORD>(hex_text.size()), &written,
                      nullptr) &&
            written == hex_text.size() && FlushFileBuffers(f);
  CloseHandle(f);
  if (!ok) {
    err = "secret file write failed";
    return false;
  }
  secret.assign(reinterpret_cast<const char*>(raw), sizeof(raw));
  generated = true;
  return true;
}

}  // namespace xnc
