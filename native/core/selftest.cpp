// selftest.cpp - native/core selftest (Task 4 scope: frame + handshake).
// Shared vectors with the Go side (proto/ipc): V1 = XNIP wire frame,
// V2 = RFC 4231 HMAC-SHA256 test case 2. Byte-level equality between the
// C++ and Go implementations is enforced by these vectors.
// Fix-wave addition: an in-process LOOPBACK test drives the real server
// connection handler (ServeConnection -> overlapped TimedIo IO +
// ServerHandshake + ServeFrames) against a client using the real blocking
// ReadFrame/WriteFrame(HANDLE) path. The loopback pipe is created with a
// PERMISSIVE TEST-ONLY DACL (Everyone) so it runs non-elevated; production
// RunPipeServer keeps "D:P(A;;GA;;;SY)(A;;GA;;;BA)" (spec 9.1), untouched.
// Any failure prints "SELFTEST FAIL: <name>" and exits 1; all-pass prints
// "selftest ok". Entry point SelftestMain() is declared by xnc-core.cpp and
// reachable via `xnc-core.exe --selftest` / `build.bat selftest`.
#ifndef WIN32_LEAN_AND_MEAN
#define WIN32_LEAN_AND_MEAN
#endif
#include <windows.h>
#include <aclapi.h>
#include <sddl.h>

#include "../common/frame.h"
#include "../common/handshake.h"
#include "pipe_server.h"
#include "secret_file.h"
#include "spawn.h"
#include "token_manager.h"
#include "watchdog.h"

#include <atomic>
#include <cstdio>
#include <cstring>
#include <cwchar>
#include <thread>
#include <vector>
static int fails = 0;
#define CHECK(name, cond) do { if (!(cond)) { std::printf("SELFTEST FAIL: %s\n", name); fails++; } } while (0)

// ---- M2-Slice1 Task 4 fakes: scripted WTS session + fake capture spawn /
// terminate / SendSAS (loopback block 2 + the poll-mode monitor test). The
// fakes run on the SERVER thread (spawn/terminate) or monitor thread
// (session fn); the assertions below read them only after the matching
// response frame arrived, which orders the accesses.
static std::atomic<DWORD> s_script_session{0};
static DWORD WINAPI ScriptSessionFn() { return s_script_session.load(); }

static std::vector<HANDLE> s_spawn_events;       // in spawn order (server thread)
static std::atomic<int> s_spawn_count{0};
static std::vector<HANDLE> s_terminated_events;  // terminate order (server thread)
static std::atomic<int> s_sas_calls{0};
static std::atomic<int> s_sas_as_user{-1};
static std::atomic<bool> s_degraded_last{false};  // last fake capture spawn's degraded flag

// Fake child = manual-reset NON-signaled event: WaitForSingleObject(h,0)
// is WAIT_TIMEOUT (models "running"), and the detached WatchCaptureChild's
// INFINITE wait unblocks exactly when the fake terminate signals it.
static xnc::CaptureSpawnResult FakeCaptureSpawn(uint32_t, const wchar_t*,
                                                const uint8_t*,
                                                xnc::Watchdog*, bool) {
  HANDLE ev = CreateEventW(nullptr, TRUE, FALSE, nullptr);
  s_spawn_events.push_back(ev);
  s_spawn_count.fetch_add(1);
  xnc::CaptureSpawnResult r;
  r.ok = true;
  r.pid = 0x3000 + static_cast<DWORD>(s_spawn_count.load());
  r.child = ev;
  return r;
}
static BOOL WINAPI FakeTerminateProcess(HANDLE h, UINT) {
  s_terminated_events.push_back(h);
  SetEvent(h);  // "the process exited" -> the watcher reaps + closes
  return TRUE;
}
static void WINAPI FakeSendSas(BOOL as_user) {
  s_sas_as_user.store(static_cast<int>(as_user));
  s_sas_calls.fetch_add(1);
}
static xnc::SasSendFn NullResolveSendSas() { return nullptr; }

// ---- M2-Slice2 Task 3 fakes: token mint + shell spawn (loopback block) ----
static std::atomic<int> s_token_calls{0};
static bool s_token_succeeds = true;  // false -> NO_ACTIVE_SESSION path
static uint8_t s_token_kind_last = 0xFF;
static bool FakeShellToken(uint32_t, uint8_t kind, HANDLE* out) {
  s_token_calls.fetch_add(1);
  s_token_kind_last = kind;
  if (!s_token_succeeds) {
    *out = nullptr;
    return false;
  }
  *out = CreateEventW(nullptr, TRUE, FALSE, nullptr);  // closed by the handler
  return *out != nullptr;
}
static std::atomic<int> s_shell_spawns{0};
static xnc::ShellCreateReq s_shell_req_last{};
static std::atomic<uint32_t> s_shell_session_last{0};
static std::wstring s_shell_pipe_last;
static xnc::ShellSpawnResult FakeShellSpawn(const xnc::ShellCreateReq& req,
                                            uint32_t session, const wchar_t* pipe,
                                            const uint8_t*, HANDLE,
                                            xnc::Watchdog*) {
  s_shell_spawns.fetch_add(1);
  s_shell_req_last = req;
  s_shell_session_last.store(session);
  s_shell_pipe_last = pipe;
  xnc::ShellSpawnResult r;
  r.ok = true;
  r.pid = 0x4000 + static_cast<DWORD>(s_shell_spawns.load());
  r.child = CreateEventW(nullptr, TRUE, FALSE, nullptr);  // fake "process"
  return r;
}
// 0x0120 request payload builder (client-side encoder mirror, LE).
static std::vector<uint8_t> BuildShellPayload(uint32_t wts, uint8_t kind,
                                              uint8_t profile, uint8_t mode,
                                              uint16_t cols, uint16_t rows,
                                              const char* cwd,
                                              const char* env, const char* cmd,
                                              uint32_t timeout) {
  std::vector<uint8_t> p;
  auto put16 = [&p](uint16_t v) {
    p.push_back(static_cast<uint8_t>(v));
    p.push_back(static_cast<uint8_t>(v >> 8));
  };
  auto put32 = [&p](uint32_t v) {
    for (int i = 0; i < 4; i++) p.push_back(static_cast<uint8_t>(v >> (8 * i)));
  };
  auto blob = [&](const char* s) {
    size_t n = std::strlen(s);
    put16(static_cast<uint16_t>(n));
    p.insert(p.end(), s, s + n);
  };
  put32(wts);
  p.push_back(kind);
  p.push_back(profile);
  p.push_back(mode);
  put16(cols);
  put16(rows);
  blob(cwd);
  blob(env);
  blob(cmd);
  put32(timeout);
  return p;
}

// Loopback server for block 2 (same shape as block 1's inline Loop: real
// ServeConnection on a permissive TEST-ONLY DACL pipe).
struct Loop2 {
  const wchar_t* name;
  const uint8_t* secret;
  size_t secret_len;
  std::atomic<bool> created{false}, handshake_ok{false};
  uint32_t client_pid = 0;
  void Run() {
    PSECURITY_DESCRIPTOR sd = nullptr;
    if (!ConvertStringSecurityDescriptorToSecurityDescriptorW(
            L"D:P(A;;GA;;;WD)", SDDL_REVISION_1, &sd, nullptr))
      return;
    SECURITY_ATTRIBUTES sa{sizeof(sa), sd, FALSE};
    HANDLE pipe = CreateNamedPipeW(name,
        PIPE_ACCESS_DUPLEX | FILE_FLAG_FIRST_PIPE_INSTANCE | FILE_FLAG_OVERLAPPED,
        PIPE_TYPE_BYTE | PIPE_READMODE_BYTE | PIPE_WAIT, 1,
        64 * 1024, 64 * 1024, 0, &sa);
    LocalFree(sd);
    if (pipe == INVALID_HANDLE_VALUE) return;
    created = true;
    HANDLE ev = CreateEventW(nullptr, TRUE, FALSE, nullptr);
    OVERLAPPED ov{};
    ov.hEvent = ev;
    BOOL connected = ConnectNamedPipe(pipe, &ov);
    DWORD err = connected ? 0 : GetLastError();
    bool ok = connected || err == ERROR_PIPE_CONNECTED;
    if (!ok && err == ERROR_IO_PENDING && ev)
      ok = WaitForSingleObject(ev, 10000) == WAIT_OBJECT_0;
    if (ev) CloseHandle(ev);
    if (ok) {
      xnc::Watchdog wd;  // not Start()ed: Heartbeat target only
      handshake_ok = xnc::ServeConnection(pipe, &wd, secret, secret_len,
                                          &client_pid);
    }
    FlushFileBuffers(pipe);
    DisconnectNamedPipe(pipe);
    CloseHandle(pipe);
  }
};

int SelftestMain() {
    using namespace xnc;
    { // V1 wire vector
        Frame f{kFlagResponse, kMsgPing, 0xDEADBEEF, {0x78,0x6E,0x63}};
        std::vector<uint8_t> wire;
        CHECK("encode", EncodeFrame(f, wire));
        const uint8_t want[19] = {0x58,0x4E,0x49,0x50,0x01,0x01,0x10,0x00,0xEF,0xBE,0xAD,0xDE,0x03,0x00,0x00,0x00,0x78,0x6E,0x63};
        CHECK("v1-bytes", wire.size()==19 && std::memcmp(wire.data(), want, 19)==0);
        Frame back;
        CHECK("v1-decode", DecodeFrame(wire.data(), wire.size(), back)==DecodeResult::Ok
              && back.flags==kFlagResponse && back.message_type==kMsgPing && back.request_id==0xDEADBEEF);
    }
    { // 错误路径
        Frame back; std::vector<uint8_t> w(16, 0);
        CHECK("bad-magic", DecodeFrame(w.data(), w.size(), back)==DecodeResult::BadMagic);
        auto f = Frame{kFlagResponse, kMsgPing, 0, {}};
        std::vector<uint8_t> wire; EncodeFrame(f, wire); wire[4]=2;
        CHECK("bad-version", DecodeFrame(wire.data(), wire.size(), back)==DecodeResult::BadVersion);
        CHECK("truncated", DecodeFrame(wire.data(), 3, back)==DecodeResult::Truncated);
        wire[4]=1; wire[12]=0x01; wire[13]=0x00; wire[14]=0x90; wire[15]=0x00;  // payloadLength = 9MiB+1
        CHECK("too-large", DecodeFrame(wire.data(), wire.size(), back)==DecodeResult::TooLarge);
    }
    { // V2 RFC4231
        const char* k="Jefe"; const char* d="what do ya want for nothing?";
        uint8_t mac[32];
        // data is 28 bytes (brief typo'd 30, which would read past the
        // literal and never match the vector); strlen is the authority.
        CHECK("hmac", HmacSha256((const uint8_t*)k,4,(const uint8_t*)d,std::strlen(d),mac));
        const uint8_t want[32]={0x5b,0xdc,0xc1,0x46,0xbf,0x60,0x75,0x4e,0x6a,0x04,0x24,0x26,0x08,0x95,0x75,0xc7,
                                0x5a,0x00,0x3f,0x08,0x9d,0x27,0x39,0x83,0x9d,0xec,0x58,0xb9,0x64,0xec,0x38,0x43};
        CHECK("rfc4231", std::memcmp(mac, want, 32)==0);
    }
    { // 握手 payload 往返
        uint8_t nonce[16]; for (int i=0;i<16;i++) nonce[i]=(uint8_t)i;
        Frame h{0, kMsgHello, 0, EncodeHello(4242, nonce)};
        uint32_t pid; uint8_t n2[16];
        CHECK("hello-rt", DecodeHello(h, pid, n2)==DecodeResult::Ok && pid==4242 && std::memcmp(n2,nonce,16)==0);
        uint8_t proof[32]; HmacSha256(nonce,16,nonce,16,proof);
        Frame hp{0, kMsgHelloProof, 0, EncodeHelloProof(7, nonce, proof)};
        uint8_t n3[16], p3[32];
        CHECK("helloproof-rt", DecodeHelloProof(hp, pid, n3, p3)==DecodeResult::Ok && pid==7 && std::memcmp(p3,proof,32)==0);
    }
    { // Task 6: 纯校验路径(不触真实令牌操作 —— 那需要 SYSTEM/SeTcb)
      // 会话目标:仅活动 console 会话合法;锁屏 console 同样合法由
      // WTSGetActiveConsoleSessionId 语义保证(纯函数只比较值,spec §6.4)。
      CHECK("sess-allowed", SessionTargetAllowed(1, 1));
      CHECK("sess-allowed-0", SessionTargetAllowed(0, 0));
      CHECK("sess-wrong-id", !SessionTargetAllowed(2, 1));          // 非活动会话
      CHECK("sess-swapped", !SessionTargetAllowed(1, 2));
      CHECK("sess-no-console", !SessionTargetAllowed(1, 0xFFFFFFFF)); // 无物理 console
      CHECK("sess-invalid-target", !SessionTargetAllowed(0xFFFFFFFF, 1));
      // exe 白名单:仅接受同目录相对名 xnc-desktop.exe(可带 .\ 前缀,
      // 大小写不敏感);绝对路径/穿越/子目录/多余后缀一律拒绝。
      CHECK("exe-allowed", SpawnExeArgAllowed(L"xnc-desktop.exe"));
      CHECK("exe-allowed-dotslash", SpawnExeArgAllowed(L".\\xnc-desktop.exe"));
      CHECK("exe-allowed-case", SpawnExeArgAllowed(L"XNC-Desktop.Exe"));
      CHECK("exe-wrong-name", !SpawnExeArgAllowed(L"cmd.exe"));
      CHECK("exe-double-ext", !SpawnExeArgAllowed(L"xnc-desktop.exe.exe"));
      CHECK("exe-absolute", !SpawnExeArgAllowed(L"C:\\xnc-diag\\xnc-desktop.exe"));
      CHECK("exe-unc", !SpawnExeArgAllowed(L"\\\\srv\\share\\xnc-desktop.exe"));
      CHECK("exe-traversal", !SpawnExeArgAllowed(L"..\\xnc-desktop.exe"));
      CHECK("exe-subdir", !SpawnExeArgAllowed(L"bin\\xnc-desktop.exe"));
      CHECK("exe-double-dotslash", !SpawnExeArgAllowed(L".\\.\\xnc-desktop.exe"));
      CHECK("exe-empty", !SpawnExeArgAllowed(L""));
      CHECK("exe-null", !SpawnExeArgAllowed(nullptr));
      CHECK("exe-leading-space", !SpawnExeArgAllowed(L" xnc-desktop.exe"));
      CHECK("exe-trailing-space", !SpawnExeArgAllowed(L"xnc-desktop.exe "));
      CHECK("exe-only-dotslash", !SpawnExeArgAllowed(L".\\"));
      // 同目录解析:dir + "\" + name(容忍 dir 尾斜杠,不产生双斜杠)。
      CHECK("join-plain", JoinSiblingPath(L"C:\\xnc-diag", L"xnc-desktop.exe")
                             == L"C:\\xnc-diag\\xnc-desktop.exe");
      CHECK("join-trailing-slash", JoinSiblingPath(L"C:\\xnc-diag\\", L"xnc-desktop.exe")
                             == L"C:\\xnc-diag\\xnc-desktop.exe");
      CHECK("join-empty-dir", JoinSiblingPath(L"", L"xnc-desktop.exe")
                             == L"xnc-desktop.exe");
      // 子命令行拼装(--diag-spawn 之后原样转发,含空格参数加引号)。
      {
        wchar_t a0[] = L"prog", a1[] = L"--console-diag", a2[] = L"--duration",
                a3[] = L"60", sp[] = L"C:\\my dir\\out.h264";
        wchar_t* av[] = {a0, a1, a2, a3};
        std::wstring cmd;
        CHECK("cmd-simple", BuildChildCommandLine(L"xnc-desktop.exe", 4, av, 1, &cmd)
                             && cmd == L"xnc-desktop.exe --console-diag --duration 60");
        wchar_t* av2[] = {a0, a1, sp};
        CHECK("cmd-quotes-spaces", BuildChildCommandLine(L"xnc-desktop.exe", 3, av2, 1, &cmd)
                             && cmd == L"xnc-desktop.exe --console-diag \"C:\\my dir\\out.h264\"");
        CHECK("cmd-exe-only", BuildChildCommandLine(L"xnc-desktop.exe", 1, av, 1, &cmd)
                             && cmd == L"xnc-desktop.exe");
        CHECK("cmd-bad-from", !BuildChildCommandLine(L"xnc-desktop.exe", 2, av, 3, &cmd));
        wchar_t* av3[] = {a0, nullptr};
        CHECK("cmd-null-arg", !BuildChildCommandLine(L"xnc-desktop.exe", 2, av3, 1, &cmd));
        wchar_t* av4[] = {a0, (wchar_t*)L""};
        CHECK("cmd-empty-arg", !BuildChildCommandLine(L"xnc-desktop.exe", 2, av4, 1, &cmd));
        // M1-Slice3 must-fix matrix: embedded quotes (incl. \" shapes) and
        // trailing backslash are rejected, never escaped; err names the
        // argv index and 0-based wchar position. Interior backslashes,
        // whitespace paths and unicode args still pass; empty/null args
        // stay rejected (pre-slice3 behavior, unchanged).
        std::string cerr;
        wchar_t bad_q[] = L"--title=bad\"name";
        const wchar_t* qp = std::wcschr(bad_q, L'"');
        wchar_t* avq[] = {a0, bad_q};
        CHECK("cmd-quote-reject",
              !BuildChildCommandLine(L"xnc-desktop.exe", 2, avq, 1, &cmd, &cerr));
        CHECK("cmd-quote-err",
              cerr.find("embedded quote in arg[1] at char " +
                        std::to_string((int)(qp - bad_q))) != std::string::npos);
        wchar_t bad_eq[] = L"he said \\\"hi\\\"";  // arg text: he said \"hi\"
        wchar_t* avq2[] = {a0, bad_eq};
        CHECK("cmd-escaped-quote-reject",
              !BuildChildCommandLine(L"xnc-desktop.exe", 2, avq2, 1, &cmd, &cerr));
        CHECK("cmd-escaped-quote-err",
              cerr.find("embedded quote in arg[1] at char 9") != std::string::npos);
        wchar_t bad_bs[] = L"C:\\dir\\";
        wchar_t* avb[] = {a0, bad_bs};
        CHECK("cmd-trailing-backslash-reject",
              !BuildChildCommandLine(L"xnc-desktop.exe", 2, avb, 1, &cmd, &cerr));
        CHECK("cmd-trailing-backslash-err",
              cerr.find("trailing backslash in arg[1] at char 6") != std::string::npos);
        // 合法混合路径 + unicode(内部反斜杠、空格、项目名)照常通过。
        wchar_t uni1[] = L"C:\\proj 项目\\out file.h264", uni2[] = L"项目";
        wchar_t* avu[] = {a0, uni1, uni2};
        CHECK("cmd-unicode-path-ok",
              BuildChildCommandLine(L"xnc-desktop.exe", 3, avu, 1, &cmd) &&
              cmd == L"xnc-desktop.exe \"C:\\proj 项目\\out file.h264\" 项目");
        // unicode 内嵌引号:位置按 wchar 计(项0 目1 \2 "3)。
        wchar_t uq[] = L"项目\\\"";
        wchar_t* avu2[] = {a0, uq};
        CHECK("cmd-unicode-quote-reject",
              !BuildChildCommandLine(L"xnc-desktop.exe", 2, avu2, 1, &cmd, &cerr));
        CHECK("cmd-unicode-quote-err",
              cerr.find("embedded quote in arg[1] at char 3") != std::string::npos);
        // 空参 err 信息(行为与 slice3 之前一致,仅补上原因)。
        CHECK("cmd-empty-arg-err",
              !BuildChildCommandLine(L"xnc-desktop.exe", 2, av4, 1, &cmd, &cerr) &&
              cerr.find("empty argument at arg[1]") != std::string::npos);
      }
    }
    { // M1-Slice2 Task 3 fix wave: EncodeStartCaptureOk success layout,
      // byte-level (test side builds the expected payload independently).
      uint8_t secret[32];
      for (int i = 0; i < 32; i++) secret[i] = (uint8_t)(i + 1);
      Frame ok = EncodeStartCaptureOk(Frame{0, kMsgStartCapture, 0xABCD, {}},
                                      0x11223344,
                                      L"\\\\.\\pipe\\xnc-desktop-rt-77", secret, 9);
      const char* name = "\\\\.\\pipe\\xnc-desktop-rt-77";  // 26 chars
      std::vector<uint8_t> want;
      auto put32 = [&want](uint32_t v) {
        for (int i = 0; i < 4; i++) want.push_back((uint8_t)(v >> (8 * i)));
      };
      put32(0x11223344);                       // pid u32 LE
      want.push_back(26); want.push_back(0);   // name_len u16 LE
      for (const char* p = name; *p; ++p) want.push_back((uint8_t)*p);
      for (int i = 0; i < 32; i++) want.push_back(secret[i]);
      put32(9);                                // gen u32 LE
      CHECK("sc-ok-frame-meta",
            ok.message_type == kMsgStartCapture && ok.flags == kFlagResponse &&
            ok.request_id == 0xABCD);
      CHECK("sc-ok-layout-bytes",
            ok.payload.size() == want.size() &&
            std::memcmp(ok.payload.data(), want.data(), want.size()) == 0);
    }
    { // 真实 IO 路径 in-process loopback(修复波新增):
      // 服务端线程跑生产 ServeConnection(overlapped TimedIo + 握手 +
      // PING/PONG 帧循环);客户端用阻塞 ReadFrame/WriteFrame(HANDLE)。
      // 修复前该测试稳定复现 elevated gate 失败:BCryptGenRandom(NULL,..,0)
      // 返回 STATUS_INVALID_HANDLE,服务端握手立即断连。
      using namespace xnc;
      const uint8_t secret[16] = {'t','e','s','t','-','p','i','p','e','-','s','e','c','r','e','t'};
      wchar_t name[96];
      std::swprintf(name, 96, L"\\\\.\\pipe\\xnc-core-selftest-%lu", GetCurrentProcessId());

      struct Loop {
        const wchar_t* name;
        const uint8_t* secret; size_t secret_len;
        std::atomic<bool> created{false}, handshake_ok{false};
        uint32_t client_pid = 0;
        void Run() {  // 服务端半边:建管(测试专用宽松 DACL)-> accept -> ServeConnection
          // TEST-ONLY permissive DACL(Everyone full);生产 DACL 见 pipe_server.cpp kSddl
          PSECURITY_DESCRIPTOR sd = nullptr;
          if (!ConvertStringSecurityDescriptorToSecurityDescriptorW(
                  L"D:P(A;;GA;;;WD)", SDDL_REVISION_1, &sd, nullptr))
            return;
          SECURITY_ATTRIBUTES sa{sizeof(sa), sd, FALSE};
          HANDLE pipe = CreateNamedPipeW(name,
              PIPE_ACCESS_DUPLEX | FILE_FLAG_FIRST_PIPE_INSTANCE | FILE_FLAG_OVERLAPPED,
              PIPE_TYPE_BYTE | PIPE_READMODE_BYTE | PIPE_WAIT, 1,
              64 * 1024, 64 * 1024, 0, &sa);
          LocalFree(sd);
          if (pipe == INVALID_HANDLE_VALUE) return;
          created = true;
          HANDLE ev = CreateEventW(nullptr, TRUE, FALSE, nullptr);  // accept(镜像生产路径)
          OVERLAPPED ov{}; ov.hEvent = ev;
          BOOL connected = ConnectNamedPipe(pipe, &ov);
          DWORD err = connected ? 0 : GetLastError();
          bool ok = connected || err == ERROR_PIPE_CONNECTED;  // 客户端先到的竞态
          if (!ok && err == ERROR_IO_PENDING && ev)
            ok = WaitForSingleObject(ev, 10000) == WAIT_OBJECT_0;
          if (ev) CloseHandle(ev);
          if (ok) {
            Watchdog wd;  // 不 Start:仅作 Heartbeat 目标,避免 selftest 内杀进程线程
            handshake_ok = ServeConnection(pipe, &wd, secret, secret_len, &client_pid);
          }
          FlushFileBuffers(pipe);
          DisconnectNamedPipe(pipe);
          CloseHandle(pipe);
        }
      } srv{name, secret, sizeof(secret)};

      std::thread server_thread([&srv] { srv.Run(); });
      for (int i = 0; i < 200 && !srv.created.load(); i++) Sleep(10);  // 等管建立(≤2s)
      CHECK("loopback-pipe-created", srv.created.load());

      HANDLE c = CreateFileW(name, GENERIC_READ | GENERIC_WRITE, 0, nullptr,
                             OPEN_EXISTING, 0, nullptr);  // 同步句柄 -> 走阻塞 ReadFrame/WriteFrame
      if (c == INVALID_HANDLE_VALUE) {
        std::printf("SELFTEST FAIL: %s (err=%lu)\n", "loopback-client-open", GetLastError());
        fails++;
        server_thread.join();
      } else {
        // 客户端半边 = Go coreclient.Dial 的 C++ 镜像
        uint8_t nonce[16]; for (int i = 0; i < 16; i++) nonce[i] = (uint8_t)(i * 7 + 1);
        CHECK("loopback-hello", WriteFrame(c, Frame{0, kMsgHello, 0, EncodeHello(GetCurrentProcessId(), nonce)}));
        Frame hp;
        CHECK("loopback-read-proof", ReadFrame(c, hp) == DecodeResult::Ok && hp.message_type == kMsgHelloProof);
        uint32_t spid = 0; uint8_t snonce[16], sproof[32], want[32];
        CHECK("loopback-decode-proof", DecodeHelloProof(hp, spid, snonce, sproof) == DecodeResult::Ok);
        CHECK("loopback-verify-hmac", HmacSha256(secret, sizeof(secret), nonce, 16, want) && std::memcmp(want, sproof, 32) == 0);
        uint8_t myproof[32];
        CHECK("loopback-send-proof", HmacSha256(secret, sizeof(secret), snonce, 16, myproof) &&
                                    WriteFrame(c, Frame{0, kMsgProof, 0, EncodeProof(myproof)}));
        CHECK("loopback-ping", WriteFrame(c, Frame{0, kMsgPing, 7, {}}));
        Frame pong;
        CHECK("loopback-pong", ReadFrame(c, pong) == DecodeResult::Ok && pong.message_type == kMsgPong &&
                              (pong.flags & kFlagResponse) != 0 && pong.request_id == 7);
        // Task 3 (M1-Slice2): START/STOP_CAPTURE implemented. selftest walks the
        // closed paths (no real spawn): bad payload (pad!=0) → BAD_PAYLOAD;
        // wrong session (0x00ABCDEF never equals the active console session;
        // headless boxes active=0xFFFFFFFF likewise) → SESSION_MISMATCH;
        // idle STOP → idempotent empty response (no FlagError). The real
        // spawn path (secret now via inherited stdin, never argv - spec 1.5)
        // stays behind the elevated gate coreclient.TestStartCaptureCross.
        CHECK("loopback-start-capture-badpayload-send",
              WriteFrame(c, Frame{0, kMsgStartCapture, 8,
                                  {0x01,0,0,0, 0x09,0,0,0}}));
        Frame sc;
        CHECK("loopback-start-capture-badpayload",
              ReadFrame(c, sc) == DecodeResult::Ok && sc.message_type == kMsgStartCapture &&
              (sc.flags & (kFlagResponse | kFlagError)) == (kFlagResponse | kFlagError) &&
              sc.request_id == 8 && sc.payload.size() == 11 &&
              std::memcmp(sc.payload.data(), "BAD_PAYLOAD", 11) == 0);
        CHECK("loopback-start-capture-badsession-send",
              WriteFrame(c, Frame{0, kMsgStartCapture, 11,
                                  {0xEF,0xBC,0xAB,0x00, 0,0,0,0}}));
        Frame sc2;
        CHECK("loopback-start-capture-badsession",
              ReadFrame(c, sc2) == DecodeResult::Ok && sc2.message_type == kMsgStartCapture &&
              (sc2.flags & (kFlagResponse | kFlagError)) == (kFlagResponse | kFlagError) &&
              sc2.request_id == 11 && sc2.payload.size() == 16 &&
              std::memcmp(sc2.payload.data(), "SESSION_MISMATCH", 16) == 0);
        CHECK("loopback-stop-capture-send",
              WriteFrame(c, Frame{0, kMsgStopCapture, 9, {}}));
        Frame st;
        CHECK("loopback-stop-capture-idle-ok",
              ReadFrame(c, st) == DecodeResult::Ok && st.message_type == kMsgStopCapture &&
              (st.flags & kFlagResponse) != 0 && (st.flags & kFlagError) == 0 &&
              st.request_id == 9 && st.payload.empty());
        // RPC 之后 PING 仍工作(握手/ping 不受影响)。
        CHECK("loopback-ping-after-stub", WriteFrame(c, Frame{0, kMsgPing, 10, {}}));
        Frame pong2;
        CHECK("loopback-pong-after-stub",
              ReadFrame(c, pong2) == DecodeResult::Ok && pong2.message_type == kMsgPong &&
              pong2.request_id == 10 && (pong2.flags & kFlagError) == 0);
        CHECK("loopback-bye", WriteFrame(c, Frame{0, kMsgBye, 0, {}}));
        CloseHandle(c);
        server_thread.join();
        CHECK("loopback-server-handshake", srv.handshake_ok.load());
        CHECK("loopback-server-saw-pid", srv.client_pid == GetCurrentProcessId());
      }
    }
    { // M2-Slice1 Task 4, part 1: WTS reason table, reuse decision table,
      // live getter parity, and the poll-mode monitor with a scripted
      // session source (notification leg = real message window + WTS
      // registration: compiles here, live-verified on XIAOXIN in T6).
      CHECK("wts-reason-map",
            std::strcmp(WtsChangeReasonForCode(WTS_CONSOLE_CONNECT), "console_connect") == 0 &&
            std::strcmp(WtsChangeReasonForCode(WTS_CONSOLE_DISCONNECT), "console_disconnect") == 0 &&
            std::strcmp(WtsChangeReasonForCode(WTS_REMOTE_CONNECT), "remote_connect") == 0 &&
            std::strcmp(WtsChangeReasonForCode(WTS_REMOTE_DISCONNECT), "remote_disconnect") == 0 &&
            std::strcmp(WtsChangeReasonForCode(WTS_SESSION_LOGON), "session_logon") == 0 &&
            std::strcmp(WtsChangeReasonForCode(WTS_SESSION_LOGOFF), "session_logoff") == 0 &&
            std::strcmp(WtsChangeReasonForCode(WTS_SESSION_LOCK), "session_lock") == 0 &&
            std::strcmp(WtsChangeReasonForCode(WTS_SESSION_UNLOCK), "session_unlock") == 0 &&
            std::strcmp(WtsChangeReasonForCode(WTS_SESSION_REMOTE_CONTROL), "remote_control") == 0);
      CHECK("wts-reason-unknown",
            std::strcmp(WtsChangeReasonForCode(9999), "unknown") == 0);
      // Reuse decision (pure table): no live child -> fresh; live child in
      // the CURRENT session -> reuse; live child elsewhere (console moved,
      // incl. "no console" 0xFFFFFFFF) -> terminate + respawn.
      CHECK("reuse-fresh-invalid",
            DecideCaptureReuse(false, false, 1, 1) == CaptureReuse::kFresh);
      CHECK("reuse-fresh-exited",
            DecideCaptureReuse(true, false, 1, 1) == CaptureReuse::kFresh);
      CHECK("reuse-same-session",
            DecideCaptureReuse(true, true, 1, 1) == CaptureReuse::kReuse);
      CHECK("reuse-stale-1-2",
            DecideCaptureReuse(true, true, 1, 2) == CaptureReuse::kRespawnStaleSession);
      CHECK("reuse-stale-2-1",
            DecideCaptureReuse(true, true, 2, 1) == CaptureReuse::kRespawnStaleSession);
      CHECK("reuse-stale-no-console",
            DecideCaptureReuse(true, true, 1, 0xFFFFFFFFu) == CaptureReuse::kRespawnStaleSession);
      // Not-started monitor: the getter falls back to the live API value
      // (StartCapture keeps pre-Task4 semantics when no monitor runs).
      CHECK("wts-getter-live",
            CoreWts().console_session() == WTSGetActiveConsoleSessionId());

      // Poll-mode monitor: scripted session source, 10ms poll.
      s_script_session.store(7);
      WtsMonitor::Opts wo;
      wo.use_notifications = false;
      wo.poll_ms = 10;
      wo.session_fn = &ScriptSessionFn;
      std::atomic<int> fired{0};
      WtsSessionChange got{};
      wo.on_change = [&fired, &got](const WtsSessionChange& c) { got = c; fired++; };
      WtsMonitor mon(wo);
      CHECK("wts-start", mon.Start());
      for (int i = 0; i < 300 && mon.console_session() != 7; i++) Sleep(10);
      CHECK("wts-prime", mon.console_session() == 7);
      Sleep(80);  // several polls on the same value: baseline never fires
      CHECK("wts-baseline-no-fire", fired.load() == 0);
      s_script_session.store(9);
      for (int i = 0; i < 300 && fired.load() == 0; i++) Sleep(10);
      CHECK("wts-poll-detect",
            fired.load() == 1 && got.from == 7 && got.to == 9 &&
            std::strcmp(got.reason, "poll") == 0);
      CHECK("wts-changes-count", mon.changes() == 1);
      Sleep(80);  // steady value: still exactly one change
      CHECK("wts-steady-no-fire", fired.load() == 1);
      CHECK("wts-getter-cached", mon.console_session() == 9);
      mon.Stop();
      mon.Stop();  // idempotent
      CHECK("wts-restart", mon.Start() && (mon.Stop(), true));
      // Shutdown-race pin: Stop immediately after Start with the
      // NOTIFICATION leg enabled must terminate within the sliced wait
      // (a blocking GetMessage loop would hang this test - the window may
      // not exist yet when Stop's PostMessage is skipped). Real window on
      // this box; live leg behavior (registration lines) verified in T4/T6.
      {
        WtsMonitor m2;  // defaults: 500ms poll, notifications on
        CHECK("wts-start-notify", m2.Start());
        m2.Stop();
        CHECK("wts-stop-race-no-hang", !m2.running());
      }
    }
    { // M2-Slice1 Task 4, part 2: SAS gate matrix + StartCapture session
      // unification / respawn on a SECOND loopback against the REAL
      // ServeConnection, with the fake spawn/terminate/session/SAS seams
      // injected (no real token mint, no real xnc-desktop, no real sas.dll).
      using namespace xnc;
      const uint8_t secret[16] = {'t','e','s','t','-','p','i','p','e','-','s','e','c','r','e','t'};
      wchar_t name[96];
      std::swprintf(name, 96, L"\\\\.\\pipe\\xnc-core-selftest2-%lu", GetCurrentProcessId());
      Loop2 srv{name, secret, sizeof(secret)};
      std::thread server_thread([&srv] { srv.Run(); });
      for (int i = 0; i < 200 && !srv.created.load(); i++) Sleep(10);
      CHECK("loop2-pipe-created", srv.created.load());
      HANDLE c = CreateFileW(name, GENERIC_READ | GENERIC_WRITE, 0, nullptr,
                             OPEN_EXISTING, 0, nullptr);
      if (c == INVALID_HANDLE_VALUE) {
        std::printf("SELFTEST FAIL: %s (err=%lu)\n", "loop2-client-open", GetLastError());
        fails++;
        server_thread.join();
      } else {
        auto get32 = [](const std::vector<uint8_t>& p, size_t off) {
          return static_cast<uint32_t>(p[off]) |
                 static_cast<uint32_t>(p[off + 1]) << 8 |
                 static_cast<uint32_t>(p[off + 2]) << 16 |
                 static_cast<uint32_t>(p[off + 3]) << 24;
        };
        // client handshake half (same as block 1)
        uint8_t nonce[16]; for (int i = 0; i < 16; i++) nonce[i] = (uint8_t)(i * 5 + 3);
        CHECK("loop2-hello", WriteFrame(c, Frame{0, kMsgHello, 0, EncodeHello(GetCurrentProcessId(), nonce)}));
        Frame hp;
        CHECK("loop2-read-proof", ReadFrame(c, hp) == DecodeResult::Ok && hp.message_type == kMsgHelloProof);
        uint32_t spid = 0; uint8_t snonce[16], sproof[32], want[32];
        CHECK("loop2-verify-server",
              DecodeHelloProof(hp, spid, snonce, sproof) == DecodeResult::Ok &&
              HmacSha256(secret, sizeof(secret), nonce, 16, want) &&
              std::memcmp(want, sproof, 32) == 0);
        uint8_t myproof[32];
        CHECK("loop2-send-proof", HmacSha256(secret, sizeof(secret), snonce, 16, myproof) &&
                                  WriteFrame(c, Frame{0, kMsgProof, 0, EncodeProof(myproof)}));

        // ---- 0x0110 SAS gate matrix ----
        auto sas_payload = [](const char* r) {
          std::vector<uint8_t> p(kSasReasonLen, 0);
          for (size_t i = 0; i + 1 < kSasReasonLen && r[i] != '\0'; i++)
            p[i] = static_cast<uint8_t>(r[i]);
          return p;
        };
        SetSasAllowed(false);           // default gate state
        SetSasSendForTest(nullptr);
        SetSasResolveForTest(nullptr);

        CHECK("sas-badpayload-send",
              WriteFrame(c, Frame{0, kMsgSas, 21, std::vector<uint8_t>(23, 'x')}));
        Frame s1;
        CHECK("sas-badpayload",
              ReadFrame(c, s1) == DecodeResult::Ok && s1.message_type == kMsgSas &&
              (s1.flags & (kFlagResponse | kFlagError)) == (kFlagResponse | kFlagError) &&
              s1.request_id == 21 && s1.payload.size() == 11 &&
              std::memcmp(s1.payload.data(), "BAD_PAYLOAD", 11) == 0);

        const uint32_t audit0 = SasAuditCount();
        SasAuditEvent ae;
        CHECK("sas-denied-send", WriteFrame(c, Frame{0, kMsgSas, 22, sas_payload("lock-screen")}));
        Frame s2;
        CHECK("sas-denied",
              ReadFrame(c, s2) == DecodeResult::Ok && s2.message_type == kMsgSas &&
              (s2.flags & kFlagError) != 0 && s2.request_id == 22 &&
              s2.payload.size() == 10 &&
              std::memcmp(s2.payload.data(), "SAS_DENIED", 10) == 0);
        CHECK("sas-denied-audit",
              SasAuditCount() == audit0 + 1 && SasAuditLast(&ae) &&
              !ae.allowed && !ae.attempted &&
              std::strcmp(ae.action, "sas_denied") == 0 &&
              std::strcmp(ae.reason, "lock-screen") == 0);

        SetSasAllowed(true);            // --allow-sas
        SetSasSendForTest(&FakeSendSas);
        s_sas_calls.store(0); s_sas_as_user.store(-1);
        CHECK("sas-allowed-send", WriteFrame(c, Frame{0, kMsgSas, 23, sas_payload("e2e-viewer")}));
        Frame s3;
        CHECK("sas-allowed-ok",
              ReadFrame(c, s3) == DecodeResult::Ok && s3.message_type == kMsgSas &&
              (s3.flags & kFlagResponse) != 0 && (s3.flags & kFlagError) == 0 &&
              s3.request_id == 23 && s3.payload.size() == 4 && get32(s3.payload, 0) == 0);
        CHECK("sas-stub-called-once",
              s_sas_calls.load() == 1 && s_sas_as_user.load() == 0 /*AsUser=FALSE*/);
        CHECK("sas-sent-audit-reason-passthrough",
              SasAuditCount() == audit0 + 2 && SasAuditLast(&ae) &&
              ae.allowed && ae.attempted && ae.hr == 0 &&
              std::strcmp(ae.action, "sas_sent") == 0 &&
              std::strcmp(ae.reason, "e2e-viewer") == 0);

        SetSasSendForTest(nullptr);
        SetSasResolveForTest(&NullResolveSendSas);   // sas.dll unresolvable
        CHECK("sas-unavailable-send", WriteFrame(c, Frame{0, kMsgSas, 24, sas_payload("no-dll")}));
        Frame s4;
        CHECK("sas-unavailable",
              ReadFrame(c, s4) == DecodeResult::Ok &&
              (s4.flags & kFlagError) != 0 && s4.request_id == 24 &&
              s4.payload.size() == 15 &&
              std::memcmp(s4.payload.data(), "SAS_UNAVAILABLE", 15) == 0);
        CHECK("sas-unavailable-audit",
              SasAuditCount() == audit0 + 3 && SasAuditLast(&ae) &&
              std::strcmp(ae.action, "sas_unavailable") == 0);
        SetSasAllowed(false);
        SetSasResolveForTest(nullptr);

        // ---- StartCapture session unification + respawn (fake spawn) ----
        s_script_session.store(1);
        CoreWts().SetSessionFnForTest(&ScriptSessionFn);
        SetCaptureSpawnForTest(&FakeCaptureSpawn);
        SetTerminateForTest(&FakeTerminateProcess);
        s_spawn_events.clear();
        s_terminated_events.clear();
        s_spawn_count.store(0);

        CHECK("sc-fake-start-send",
              WriteFrame(c, Frame{0, kMsgStartCapture, 30, {0x01,0,0,0, 0,0,0,0}}));
        Frame r30;
        CHECK("sc-fake-start",
              ReadFrame(c, r30) == DecodeResult::Ok && r30.message_type == kMsgStartCapture &&
              (r30.flags & kFlagError) == 0 && r30.payload.size() > 36 &&
              get32(r30.payload, r30.payload.size() - 4) == 1 &&   // gen 1
              s_spawn_count.load() == 1);
        const uint32_t pidA = get32(r30.payload, 0);

        CHECK("sc-fake-reuse-send",
              WriteFrame(c, Frame{0, kMsgStartCapture, 31, {0x01,0,0,0, 0,0,0,0}}));
        Frame r31;
        CHECK("sc-fake-reuse",
              ReadFrame(c, r31) == DecodeResult::Ok &&
              get32(r31.payload, r31.payload.size() - 4) == 1 &&   // still gen 1
              get32(r31.payload, 0) == pidA &&                     // same child
              s_spawn_count.load() == 1);                          // no respawn

        s_script_session.store(2);  // console session moved under the child
        CHECK("sc-respawn-send",
              WriteFrame(c, Frame{0, kMsgStartCapture, 32, {0x02,0,0,0, 0,0,0,0}}));
        Frame r32;
        CHECK("sc-respawn-on-session-change",
              ReadFrame(c, r32) == DecodeResult::Ok &&
              get32(r32.payload, r32.payload.size() - 4) == 2 &&   // gen 2
              get32(r32.payload, 0) != pidA &&                     // new child
              s_spawn_count.load() == 2);
        CHECK("sc-respawn-terminated-old-scoped",
              s_terminated_events.size() == 1 &&
              s_terminated_events[0] == s_spawn_events[0]);        // old HANDLE

        // Monitor-driven termination (production wiring calls this from the
        // wts monitor thread; direct call = deterministic coverage).
        WtsSessionChange chg{2, 3, "poll"};
        OnActiveConsoleSessionChanged(chg);
        CHECK("sc-monitor-terminated",
              s_terminated_events.size() == 2 &&
              s_terminated_events[1] == s_spawn_events[1]);
        s_script_session.store(3);
        CHECK("sc-post-monitor-send",
              WriteFrame(c, Frame{0, kMsgStartCapture, 33, {0x03,0,0,0, 0,0,0,0}}));
        Frame r33;
        CHECK("sc-post-monitor-respawn",
              ReadFrame(c, r33) == DecodeResult::Ok &&
              get32(r33.payload, r33.payload.size() - 4) == 3 &&   // gen 3
              s_spawn_count.load() == 3);

        CHECK("sc-stop-send", WriteFrame(c, Frame{0, kMsgStopCapture, 34, {}}));
        Frame st2;
        CHECK("sc-stop-terminates",
              ReadFrame(c, st2) == DecodeResult::Ok &&
              (st2.flags & kFlagError) == 0 &&
              s_terminated_events.size() == 3 &&
              s_terminated_events[2] == s_spawn_events[2]);

        // ---- 0x0120/0x0121 CreateShell / KillShell (fake token + spawn) ----
        s_script_session.store(1);  // live console = 1 (stubbed wts getter)
        SetShellTokenForTest(&FakeShellToken);
        SetShellSpawnForTest(&FakeShellSpawn);

        // user kind, no live user token -> NO_ACTIVE_SESSION (spec 8.4:
        // never implicit elevation). No spawn may happen.
        s_token_succeeds = false;
        s_token_calls.store(0);
        CHECK("cs-no-session-send",
              WriteFrame(c, Frame{0, kMsgCreateShell, 50,
                                  BuildShellPayload(1, 0, 2, 1, 120, 30, "", "",
                                                    "whoami", 60)}));
        Frame c1;
        CHECK("cs-no-session",
              ReadFrame(c, c1) == DecodeResult::Ok &&
              c1.message_type == kMsgCreateShell &&
              (c1.flags & kFlagError) != 0 && c1.request_id == 50 &&
              c1.payload.size() == 17 &&
              std::memcmp(c1.payload.data(), "NO_ACTIVE_SESSION", 17) == 0);
        CHECK("cs-no-session-no-spawn",
              s_token_calls.load() == 1 && s_shell_spawns.load() == 0);

        s_token_succeeds = true;
        // profile enum out of whitelist -> BAD_PAYLOAD.
        CHECK("cs-bad-profile-send",
              WriteFrame(c, Frame{0, kMsgCreateShell, 51,
                                  BuildShellPayload(1, 0, 9, 1, 80, 25, "", "",
                                                    "whoami", 30)}));
        Frame c2;
        CHECK("cs-bad-profile",
              ReadFrame(c, c2) == DecodeResult::Ok &&
              (c2.flags & kFlagError) != 0 && c2.request_id == 51 &&
              c2.payload.size() == 11 &&
              std::memcmp(c2.payload.data(), "BAD_PAYLOAD", 11) == 0);
        // oneshot without a command -> BAD_PAYLOAD.
        CHECK("cs-oneshot-no-cmd",
              WriteFrame(c, Frame{0, kMsgCreateShell, 52,
                                  BuildShellPayload(1, 0, 0, 1, 80, 25, "", "",
                                                    "", 30)}) &&
              (ReadFrame(c, c2) == DecodeResult::Ok &&
               (c2.flags & kFlagError) != 0 && c2.request_id == 52 &&
               std::memcmp(c2.payload.data(), "BAD_PAYLOAD", 11) == 0));
        // wts not the live console session -> SESSION_MISMATCH.
        CHECK("cs-session-mismatch",
              WriteFrame(c, Frame{0, kMsgCreateShell, 53,
                                  BuildShellPayload(5, 0, 2, 1, 80, 25, "", "",
                                                    "whoami", 30)}) &&
              (ReadFrame(c, c2) == DecodeResult::Ok &&
               (c2.flags & kFlagError) != 0 && c2.request_id == 53 &&
               c2.payload.size() == 16 &&
               std::memcmp(c2.payload.data(), "SESSION_MISMATCH", 16) == 0));
        // truncated payload -> BAD_PAYLOAD.
        {
          auto trunc = BuildShellPayload(1, 0, 2, 1, 80, 25, "x", "", "y", 1);
          trunc.resize(trunc.size() - 1);
          CHECK("cs-truncated",
                WriteFrame(c, Frame{0, kMsgCreateShell, 54, std::move(trunc)}) &&
                (ReadFrame(c, c2) == DecodeResult::Ok &&
                 (c2.flags & kFlagError) != 0 && c2.request_id == 54 &&
                 std::memcmp(c2.payload.data(), "BAD_PAYLOAD", 11) == 0));
        }
        // mode >= 2 -> BAD_PAYLOAD (T3 review).
        CHECK("cs-bad-mode",
              WriteFrame(c, Frame{0, kMsgCreateShell, 60,
                                  BuildShellPayload(1, 0, 2, 2, 80, 25, "", "",
                                                    "whoami", 30)}) &&
              (ReadFrame(c, c2) == DecodeResult::Ok &&
               (c2.flags & kFlagError) != 0 && c2.request_id == 60 &&
               std::memcmp(c2.payload.data(), "BAD_PAYLOAD", 11) == 0));
        // env segment without '=' (value with embedded '\n' split through)
        // -> BAD_PAYLOAD, never a silently-split entry (T3 review).
        CHECK("cs-env-bare-segment",
              WriteFrame(c, Frame{0, kMsgCreateShell, 61,
                                  BuildShellPayload(1, 0, 2, 1, 80, 25, "",
                                                    "A=1\nsmuggled", "whoami",
                                                    30)}) &&
              (ReadFrame(c, c2) == DecodeResult::Ok &&
               (c2.flags & kFlagError) != 0 && c2.request_id == 61 &&
               std::memcmp(c2.payload.data(), "BAD_PAYLOAD", 11) == 0));
        // 0xFFFFFFFF sentinel resolves to the live active console (agent
        // contract) -> success with wts=1 at the spawn seam.
        {
          const int spawnsS = s_shell_spawns.load();
          CHECK("cs-sentinel-ok",
                WriteFrame(c, Frame{0, kMsgCreateShell, 62,
                                    BuildShellPayload(0xFFFFFFFFu, 0, 2, 1, 80,
                                                      25, "", "", "whoami",
                                                      30)}) &&
                (ReadFrame(c, c2) == DecodeResult::Ok &&
                 (c2.flags & kFlagError) == 0 && c2.request_id == 62 &&
                 s_shell_spawns.load() == spawnsS + 1 &&
                 s_shell_session_last.load() == 1));
        }
        // success: user token, CMD oneshot -> ok descriptor; request fields
        // reach the spawn seam verbatim.
        const int spawns0 = s_shell_spawns.load();
        CHECK("cs-ok-send",
              WriteFrame(c, Frame{0, kMsgCreateShell, 55,
                                  BuildShellPayload(1, 0, 2, 1, 120, 30,
                                                    "C:\\tmp x", "A=1\nB=2\n",
                                                    "whoami", 60)}));
        Frame c3;
        CHECK("cs-ok",
              ReadFrame(c, c3) == DecodeResult::Ok &&
              c3.message_type == kMsgCreateShell &&
              (c3.flags & kFlagError) == 0 && c3.request_id == 55 &&
              s_shell_spawns.load() == spawns0 + 1);
        {
          const uint32_t pid = get32(c3.payload, 0);
          const uint16_t nlen = static_cast<uint16_t>(
              c3.payload[4] | static_cast<unsigned>(c3.payload[5]) << 8);
          CHECK("cs-ok-layout",
                c3.payload.size() == 6 + nlen + 32 && nlen > 0 &&
                pid == 0x4000 + static_cast<uint32_t>(spawns0 + 1));
          std::wstring pipe(c3.payload.begin() + 6,
                            c3.payload.begin() + 6 + nlen);
          CHECK("cs-ok-pipe-prefix",
                pipe.rfind(L"\\\\.\\pipe\\xnc-shell-", 0) == 0);
          CHECK("cs-ok-req-fields",
                s_shell_req_last.wts == 1 && s_shell_req_last.token_kind == 0 &&
                s_shell_req_last.profile == 2 && s_shell_req_last.mode == 1 &&
                s_shell_req_last.cols == 120 && s_shell_req_last.rows == 30 &&
                s_shell_req_last.cwd == "C:\\tmp x" &&
                s_shell_req_last.env == "A=1\nB=2\n" &&
                s_shell_req_last.cmd == "whoami" &&
                s_shell_req_last.timeout_sec == 60 &&
                s_shell_pipe_last == pipe);
          // kill the shell we created -> empty ok (scoped by stored handle).
          std::vector<uint8_t> kp(4);
          for (int i = 0; i < 4; i++)
            kp[i] = static_cast<uint8_t>(pid >> (8 * i));
          CHECK("cs-kill-send",
                WriteFrame(c, Frame{0, kMsgKillShell, 56, kp}));
          Frame k1;
          CHECK("cs-kill-ok",
                ReadFrame(c, k1) == DecodeResult::Ok &&
                k1.message_type == kMsgKillShell &&
                (k1.flags & kFlagResponse) != 0 &&
                (k1.flags & kFlagError) == 0 && k1.request_id == 56 &&
                k1.payload.empty());
          // unknown pid: idempotent ok.
          std::vector<uint8_t> kp2 = {0x99, 0x99, 0, 0};
          CHECK("cs-kill-unknown-ok",
                WriteFrame(c, Frame{0, kMsgKillShell, 57, kp2}) &&
                (ReadFrame(c, k1) == DecodeResult::Ok &&
                 (k1.flags & kFlagError) == 0 && k1.request_id == 57));
        }
        // success: system token -> same ok shape, kind=1 reached the seam.
        CHECK("cs-system-ok",
              WriteFrame(c, Frame{0, kMsgCreateShell, 58,
                                  BuildShellPayload(1, 1, 0, 0, 100, 30, "", "",
                                                    "", 0)}) &&
              (ReadFrame(c, c3) == DecodeResult::Ok &&
               (c3.flags & kFlagError) == 0 && c3.request_id == 58 &&
               s_token_kind_last == 1 && get32(c3.payload, 0) != 0));
        // bad KillShell payload -> BAD_PAYLOAD.
        CHECK("cs-kill-badpayload",
              WriteFrame(c, Frame{0, kMsgKillShell, 59, {1, 2, 3}}) &&
              (ReadFrame(c, c3) == DecodeResult::Ok &&
               (c3.flags & kFlagError) != 0 && c3.request_id == 59 &&
               std::memcmp(c3.payload.data(), "BAD_PAYLOAD", 11) == 0));
        SetShellTokenForTest(nullptr);
        SetShellSpawnForTest(nullptr);

        // restore all seams (production defaults)
        SetCaptureSpawnForTest(nullptr);
        SetTerminateForTest(nullptr);
        CoreWts().SetSessionFnForTest(nullptr);
        SetSasAllowed(false);

        CHECK("loop2-ping-after", WriteFrame(c, Frame{0, kMsgPing, 40, {}}));
        Frame pong3;
        CHECK("loop2-pong-after",
              ReadFrame(c, pong3) == DecodeResult::Ok && pong3.message_type == kMsgPong &&
              pong3.request_id == 40 && (pong3.flags & kFlagError) == 0);
        CHECK("loop2-bye", WriteFrame(c, Frame{0, kMsgBye, 0, {}}));
        CloseHandle(c);
        server_thread.join();
        CHECK("loop2-server-handshake", srv.handshake_ok.load());
        CHECK("loop2-server-saw-pid", srv.client_pid == GetCurrentProcessId());
      }
    }
    { // M2-Slice2 Task 3 pure units: profile/token-kind whitelist, 0x0120
      // payload decode round-trip + golden header bytes, env splitter,
      // EncodeCreateShellOk golden bytes, backoff table, crash-loop window.
      CHECK("shell-profile-enum",
            ShellProfileAllowed(0) && ShellProfileAllowed(1) &&
            ShellProfileAllowed(2) && ShellProfileAllowed(3));
      CHECK("shell-profile-reject",
            !ShellProfileAllowed(4) && !ShellProfileAllowed(255));
      CHECK("shell-profile-names",
            std::strcmp(ShellProfileName(0), "POWERSHELL") == 0 &&
            std::strcmp(ShellProfileName(1), "PWSH") == 0 &&
            std::strcmp(ShellProfileName(2), "CMD") == 0 &&
            std::strcmp(ShellProfileName(3), "BASH") == 0 &&
            std::strcmp(ShellProfileName(4), "?") == 0);
      CHECK("shell-token-kind", ShellTokenKindAllowed(0) &&
                                ShellTokenKindAllowed(1) &&
                                !ShellTokenKindAllowed(2));

      // round-trip + golden header (wts=1 user CMD oneshot 120x30).
      auto p = BuildShellPayload(1, 0, 2, 1, 120, 30, "C:\\tmp x",
                                 "A=1\nB=2\n", "whoami", 60);
      const uint8_t head[13] = {0x01, 0x00, 0x00, 0x00, 0x00, 0x02, 0x01,
                                0x78, 0x00, 0x1E, 0x00, 0x08, 0x00};
      CHECK("cs-golden-head",
            p.size() >= 13 && std::memcmp(p.data(), head, 13) == 0);
      ShellCreateReq sr;
      CHECK("cs-decode-rt", DecodeShellCreatePayload(p.data(), p.size(), &sr));
      CHECK("cs-decode-fields",
            sr.wts == 1 && sr.token_kind == 0 && sr.profile == 2 &&
            sr.mode == 1 && sr.cols == 120 && sr.rows == 30 &&
            sr.cwd == "C:\\tmp x" && sr.env == "A=1\nB=2\n" &&
            sr.cmd == "whoami" && sr.timeout_sec == 60);
      // minimal all-empty payload = 21 bytes, still decodes.
      auto mini = BuildShellPayload(7, 1, 0, 0, 0, 0, "", "", "", 0);
      CHECK("cs-minimal-21", mini.size() == 21 &&
            DecodeShellCreatePayload(mini.data(), mini.size(), &sr) &&
            sr.wts == 7 && sr.token_kind == 1 && sr.cwd.empty());
      // trailing byte / truncation rejected.
      auto trail = p;
      trail.push_back(0);
      CHECK("cs-trailing-reject",
            !DecodeShellCreatePayload(trail.data(), trail.size(), &sr));
      CHECK("cs-trunc-reject",
            !DecodeShellCreatePayload(p.data(), p.size() - 1, &sr));
      CHECK("cs-null-reject",
            !DecodeShellCreatePayload(nullptr, p.size(), &sr) &&
            !DecodeShellCreatePayload(p.data(), p.size(), nullptr));

      // env splitter: "K=V\n"-joined -> entries, empties skipped.
      {
        auto v = SplitShellEnv("A=1\nB=2\n");
        CHECK("cs-env-split-2", v.size() == 2 && v[0] == "A=1" && v[1] == "B=2");
        v = SplitShellEnv("A=1");
        CHECK("cs-env-split-1", v.size() == 1 && v[0] == "A=1");
        v = SplitShellEnv("A=1\nB=2");
        CHECK("cs-env-split-no-nl", v.size() == 2 && v[1] == "B=2");
        v = SplitShellEnv("\n\n");
        CHECK("cs-env-split-empty", v.empty());
        v = SplitShellEnv("");
        CHECK("cs-env-split-blank", v.empty());
      }

      // EncodeCreateShellOk golden bytes: [pid u32][nameLen u16][name][32B].
      {
        uint8_t secret[32];
        for (int i = 0; i < 32; i++) secret[i] = (uint8_t)(i + 1);
        Frame ok = EncodeCreateShellOk(Frame{0, kMsgCreateShell, 0xABCE, {}},
                                       0x11223344,
                                       L"\\\\.\\pipe\\xnc-shell-77-1", secret);
        const char* name = "\\\\.\\pipe\\xnc-shell-77-1";  // 23 chars
        std::vector<uint8_t> want;
        auto put32 = [&want](uint32_t v) {
          for (int i = 0; i < 4; i++)
            want.push_back((uint8_t)(v >> (8 * i)));
        };
        put32(0x11223344);
        want.push_back(23);
        want.push_back(0);
        for (const char* q = name; *q; ++q) want.push_back((uint8_t)*q);
        for (int i = 0; i < 32; i++) want.push_back(secret[i]);
        CHECK("cs-ok-frame-meta",
              ok.message_type == kMsgCreateShell && ok.flags == kFlagResponse &&
              ok.request_id == 0xABCE);
        CHECK("cs-ok-layout-bytes",
              ok.payload.size() == want.size() &&
              std::memcmp(ok.payload.data(), want.data(), want.size()) == 0);
      }

      // backoff table (spec 15.2): 1s,2s,4s,...,cap 60s; index 0 == 1.
      CHECK("cs-backoff-table",
            WorkerBackoffMs(0) == 1000 && WorkerBackoffMs(1) == 1000 &&
            WorkerBackoffMs(2) == 2000 && WorkerBackoffMs(3) == 4000 &&
            WorkerBackoffMs(4) == 8000 && WorkerBackoffMs(5) == 16000 &&
            WorkerBackoffMs(6) == 32000 && WorkerBackoffMs(7) == 60000 &&
            WorkerBackoffMs(8) == 60000 && WorkerBackoffMs(100) == 60000);

      // crash-loop window: >=5 exits inside trailing 60s (injected clock).
      const uint64_t now = 1000000;
      CHECK("cs-crashloop-4", !CrashLoopReached(
            {now - 1, now - 2, now - 3, now - 4, now - 60000}, now));
      CHECK("cs-crashloop-5", CrashLoopReached(
            {now - 1, now - 2, now - 3, now - 4, now - 5}, now));
      CHECK("cs-crashloop-old-out",
            !CrashLoopReached(
                {now - 1, now - 2, now - 3, now - 4, now - 70000}, now));
      CHECK("cs-crashloop-empty", !CrashLoopReached({}, now));
    }
    { // M2-Slice3 Task 3: 0x0111 snapshot payload codec + response golden.
      SnapshotReq sr;
      const uint8_t req8[8] = {0xFF, 0xFF, 0xFF, 0xFF, 0x80, 0x0B, 0x00, 0x00};
      CHECK("sn-decode-ok",
            DecodeSnapshotPayload(req8, sizeof(req8), &sr) &&
            sr.wts == 0xFFFFFFFFu && sr.max_w == 0x0B80);  // 1920
      const uint8_t zero8[8] = {0};
      CHECK("sn-decode-zero",
            DecodeSnapshotPayload(zero8, sizeof(zero8), &sr) &&
            sr.wts == 0 && sr.max_w == 0);
      CHECK("sn-decode-trailing-reject",
            !DecodeSnapshotPayload(req8, sizeof(req8) + 1, &sr));
      CHECK("sn-decode-trunc-reject",
            !DecodeSnapshotPayload(req8, sizeof(req8) - 1, &sr));
      CHECK("sn-decode-null-reject",
            !DecodeSnapshotPayload(nullptr, sizeof(req8), &sr) &&
            !DecodeSnapshotPayload(req8, sizeof(req8), nullptr));
      // Response golden: [u32 len][bytes], frame meta preserved.
      const uint8_t jpeg[3] = {0xFF, 0xD8, 0xFF};
      Frame ok = EncodeSnapshotResp(Frame{0, kMsgSnapshot, 0xABCF, {}}, jpeg, 3);
      CHECK("sn-resp-meta",
            ok.message_type == kMsgSnapshot && ok.flags == kFlagResponse &&
            ok.request_id == 0xABCF);
      CHECK("sn-resp-bytes",
            ok.payload.size() == 7 && ok.payload[0] == 3 && ok.payload[1] == 0 &&
            ok.payload[2] == 0 && ok.payload[3] == 0 &&
            ok.payload[4] == 0xFF && ok.payload[5] == 0xD8 &&
            ok.payload[6] == 0xFF);
      Frame empty = EncodeSnapshotResp(Frame{0, kMsgSnapshot, 1, {}}, nullptr, 0);
      CHECK("sn-resp-empty", empty.payload.size() == 4);
    }
    { // prod bootstrap: --secret-file round-trip + DACL lock.
      wchar_t tmp[MAX_PATH];
      DWORD tn = GetTempPathW(MAX_PATH, tmp);
      CHECK("sf-tempdir", tn > 0 && tn < MAX_PATH);
      std::wstring path = std::wstring(tmp) + L"xnc-sf-selftest-" +
                          std::to_wstring(GetCurrentProcessId()) + L".hex";
      DeleteFileW(path.c_str());
      std::string s1, s2, err;
      bool gen1 = false, gen2 = false;
      if (!(LoadOrCreateSecretFile(path.c_str(), s1, gen1, err) &&
            gen1 && s1.size() == 32)) {
        std::printf("SELFTEST FAIL: sf-create err=%s\n", err.c_str());
        ++fails;
      }
      // ACL: DACL must be exactly the 2 aces (SYSTEM+Admins), protected.
      PACL dacl = nullptr;
      PSECURITY_DESCRIPTOR sd = nullptr;
      if (GetNamedSecurityInfoW(const_cast<LPWSTR>(path.c_str()),
                                SE_FILE_OBJECT, DACL_SECURITY_INFORMATION,
                                nullptr, nullptr, &dacl, nullptr,
                                &sd) != ERROR_SUCCESS) {
        std::printf("SELFTEST FAIL: sf-acl read failed (skipping counts, "
                    "note: non-elevated context)\n");
        ++fails;
      } else {
        ACL_SIZE_INFORMATION asi{};
        DWORD asi_len = sizeof(asi);
        CHECK("sf-acl-count",
              GetAclInformation(dacl, &asi, asi_len,
                                AclSizeInformation) &&
              asi.AceCount == 2);
        // No allow ace for anyone other than SYSTEM/BA: both aces must be
        // the SDDL pair we set (trustees SID start S-1-5-18 / S-1-5-32-544).
        int known = 0;
        for (DWORD i = 0; i < asi.AceCount; i++) {
          void* ace = nullptr;
          if (!GetAce(dacl, i, &ace)) continue;
          PSID sid = reinterpret_cast<PSID>(
              &reinterpret_cast<ACCESS_ALLOWED_ACE*>(ace)->SidStart);
          if (!IsValidSid(sid)) continue;
          // NT authority = S-1-5 (6-byte big-endian, low byte last).
          if (GetSidIdentifierAuthority(sid)->Value[5] != 5) continue;
          if (*GetSidSubAuthorityCount(sid) == 1 &&
              *GetSidSubAuthority(sid, 0) == 18) {  // S-1-5-18 SYSTEM
            ++known;
          } else if (*GetSidSubAuthorityCount(sid) == 2 &&
                     *GetSidSubAuthority(sid, 0) == 32 &&
                     *GetSidSubAuthority(sid, 1) == 544) {  // ...-32-544 BA
            ++known;
          }
        }
        CHECK("sf-acl-trustees", known == 2);
        if (sd) LocalFree(sd);
      }
      // Round-trip reread: the locked DACL (SY+BA only) correctly denies a
      // non-elevated reader, so the owner (implicit WRITE_DAC) relaxes the
      // test file's DACL first — in prod the SYSTEM core/agent read it fine.
      {
        PSECURITY_DESCRIPTOR relaxed = nullptr;
        if (ConvertStringSecurityDescriptorToSecurityDescriptorW(
                L"D:P(A;;FA;;;WD)", SDDL_REVISION_1, &relaxed, nullptr)) {
          SetFileSecurityW(path.c_str(), DACL_SECURITY_INFORMATION, relaxed);
          LocalFree(relaxed);
        }
      }
      if (!(LoadOrCreateSecretFile(path.c_str(), s2, gen2, err) &&
            !gen2 && s1 == s2)) {
        std::printf("SELFTEST FAIL: sf-reread err=%s gen2=%d\n",
                    err.c_str(), gen2 ? 1 : 0);
        ++fails;
      }
      // Garbage file content must be a hard error (never a weak fallback).
      {
        HANDLE g = CreateFileW(path.c_str(), GENERIC_WRITE, 0, nullptr,
                               CREATE_ALWAYS, FILE_ATTRIBUTE_NORMAL,
                               nullptr);
        if (g != INVALID_HANDLE_VALUE) {
          DWORD w2;
          WriteFile(g, "zz\n", 3, &w2, nullptr);
          CloseHandle(g);
        }
        std::string s3, e3;
        bool g3 = false;
        CHECK("sf-garbage",
              !LoadOrCreateSecretFile(path.c_str(), s3, g3, e3));
      }
      // A valid-hex but too-short secret (<16 bytes) must also be rejected.
      {
        HANDLE h = CreateFileW(path.c_str(), GENERIC_WRITE, 0, nullptr,
                               CREATE_ALWAYS, FILE_ATTRIBUTE_NORMAL,
                               nullptr);
        if (h != INVALID_HANDLE_VALUE) {
          DWORD w3;
          WriteFile(h, "0011223344556677\n", 17, &w3, nullptr);
          CloseHandle(h);
        }
        std::string s4, e4;
        bool g4 = false;
        CHECK("sf-short",
              !LoadOrCreateSecretFile(path.c_str(), s4, g4, e4));
      }
      DeleteFileW(path.c_str());
    }
    if (fails==0) std::printf("selftest ok\n");
    return fails==0 ? 0 : 1;
}
