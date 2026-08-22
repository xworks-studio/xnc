// selftest.cpp - native/core selftest (Task 4 scope: frame + handshake).
// Shared vectors with the Go side (proto/ipc): V1 = XNIP wire frame,
// V2 = RFC 4231 HMAC-SHA256 test case 2. Byte-level equality between the
// C++ and Go implementations is enforced by these vectors.
// Any failure prints "SELFTEST FAIL: <name>" and exits 1; all-pass prints
// "selftest ok". Entry point SelftestMain() is linked by selftest_main.cpp
// until Task 5 provides the real main.
#include "frame.h"
#include "handshake.h"
#include <cstdio>
#include <cstring>
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
    if (fails==0) std::printf("selftest ok\n");
    return fails==0 ? 0 : 1;
}
