// log.h - thread-safe stderr logger for xnc binaries (core, desktop).
// One line per entry, local time, format (spec M0):
// [2026-08-22T12:00:00 core pid=1234 level=info] msg
// The process tag defaults to "core" so xnc-core's output is unchanged;
// xnc-desktop calls SetLogProcessName("desktop") at startup, before any
// thread logs.
// Callers must NEVER pass secret/nonce/proof bytes to these macros.
#ifndef XNC_NATIVE_CORE_LOG_H_
#define XNC_NATIVE_CORE_LOG_H_

#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>  // GetCurrentProcessId

#include <cstdarg>
#include <cstdio>
#include <ctime>
#include <mutex>

namespace xnc {

enum class LogLevel { kInfo, kError };

// Process tag in every log line (see header comment). Function-local static
// keeps this header-only with one shared instance across TUs (C++17 inline).
inline const char*& LogProcessName() {
  static const char* name = "core";
  return name;
}
inline void SetLogProcessName(const char* name) { LogProcessName() = name; }

// Optional file sink: when SetLogFile(path) is called, every log line also
// goes to that file. Production service spawns have no console and inherited
// stdio handles proved unreliable — the child opens its own log file via
// --log-file (2026-08-24 observability incident).
inline FILE*& LogFile() {
  static FILE* f = nullptr;
  return f;
}
inline void SetLogFile(const char* path) {
  if (LogFile()) std::fclose(LogFile());
  LogFile() = path ? std::fopen(path, "a") : nullptr;
}

// Inline with function-local static mutex: one instance across TUs (C++17).
inline void LogV(LogLevel level, const char* fmt, ...) {
  static std::mutex mu;
  std::lock_guard<std::mutex> lock(mu);
  time_t now = std::time(nullptr);
  tm lt{};
  localtime_s(&lt, &now);  // MSVC order (tm*, const time_t*)
  va_list ap;
  va_start(ap, fmt);
  std::fprintf(stderr, "[%04d-%02d-%02dT%02d:%02d:%02d %s pid=%lu level=%s] ",
               lt.tm_year + 1900, lt.tm_mon + 1, lt.tm_mday, lt.tm_hour,
               lt.tm_min, lt.tm_sec, LogProcessName(), GetCurrentProcessId(),
               level == LogLevel::kInfo ? "info" : "error");
  std::vfprintf(stderr, fmt, ap);
  va_end(ap);
  std::fputc('\n', stderr);
  std::fflush(stderr);
  if (FILE* f = LogFile()) {
    va_list ap2;
    va_start(ap2, fmt);
    std::fprintf(f, "[%04d-%02d-%02dT%02d:%02d:%02d %s pid=%lu level=%s] ",
                 lt.tm_year + 1900, lt.tm_mon + 1, lt.tm_mday, lt.tm_hour,
                 lt.tm_min, lt.tm_sec, LogProcessName(), GetCurrentProcessId(),
                 level == LogLevel::kInfo ? "info" : "error");
    std::vfprintf(f, fmt, ap2);
    va_end(ap2);
    std::fputc('\n', f);
    std::fflush(f);
  }
}

}  // namespace xnc

#define XNC_LOG_INFO(...) ::xnc::LogV(::xnc::LogLevel::kInfo, __VA_ARGS__)
#define XNC_LOG_ERROR(...) ::xnc::LogV(::xnc::LogLevel::kError, __VA_ARGS__)

#endif  // XNC_NATIVE_CORE_LOG_H_
