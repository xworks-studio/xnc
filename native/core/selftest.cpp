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
#include "watchdog.h"

#include <atomic>
#include <cstdio>
#include <cstring>
#include <cwchar>
#include <thread>
static int fails = 0;
#define CHECK(name, cond) do { if (!(cond)) { std::printf("SELFTEST FAIL: %s\n", name); fails++; } } while (0)

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
        CHECK("loopback-bye", WriteFrame(c, Frame{0, kMsgBye, 0, {}}));
        CloseHandle(c);
        server_thread.join();
        CHECK("loopback-server-handshake", srv.handshake_ok.load());
        CHECK("loopback-server-saw-pid", srv.client_pid == GetCurrentProcessId());
      }
    }
    if (fails==0) std::printf("selftest ok\n");
    return fails==0 ? 0 : 1;
}
