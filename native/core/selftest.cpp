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
#include <sddl.h>

#include "../common/frame.h"
#include "../common/handshake.h"
#include "pipe_server.h"
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

// Fake child = manual-reset NON-signaled event: WaitForSingleObject(h,0)
// is WAIT_TIMEOUT (models "running"), and the detached WatchCaptureChild's
// INFINITE wait unblocks exactly when the fake terminate signals it.
static xnc::CaptureSpawnResult FakeCaptureSpawn(uint32_t, const wchar_t*,
                                                const uint8_t*,
                                                xnc::Watchdog*) {
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
    if (fails==0) std::printf("selftest ok\n");
    return fails==0 ? 0 : 1;
}
