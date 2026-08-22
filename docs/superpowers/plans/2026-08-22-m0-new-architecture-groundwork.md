# M0 新架构地基 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付 XNIP 本地 IPC 协议(Go + C++ 字节级一致)+ xnc-core C++ 骨架(pipe 服务端/双向握手/日志/watchdog)+ agent coreclient,并以自动化跨语言 smoke 证明两侧互通。

**Architecture:** 三层:① `xnc/proto/ipc`(帧编解码 + HMAC 握手原语,纯 Go、零 Windows 依赖、双平台可测);② `xnc/agent/coreclient`(go-winio 拨号 + 客户端握手状态机);③ `native/core`(MSVC C++17:帧/握手/pipe 服务端/日志/watchdog,`--selftest` 内置单元向量,`--console` 诊断运行)。C++ 与 Go 通过**共享测试向量**(固定 wire 帧 hex + RFC 4231 HMAC 向量)保证字节级一致,最终由跨语言 smoke(Go client ↔ 真 xnc-core.exe)闭环。

**Tech Stack:** Go 1.26(CGO_ENABLED=0;golang.org/x/sys、Microsoft/go-winio);C++17 / MSVC v143(静态 CRT /MT,仅依赖 Windows SDK + bcrypt);无 protobuf codegen(M1 接入,M0 仅定稿 .proto schema 文档)。

**Spec:** docs/superpowers/specs/2026-08-22-xnc-agent-refactor-spec.md(§6 xnc-core、§9 IPC、§19 工程结构、§20 M0;v1.3 已明确不在旧 helper 上修改)

## Global Constraints

- 不修改 `agent/screen-helper/**` 与任何 legacy screen 路径(owner 决策,spec §20 M0)
- XNIP 帧:magic `"XNIP"`、protocolVersion=1、16B 头小端、flags bit0=response/bit2=event/bit4=error、上限 9MiB(spec §9.2 逐字段)
- 握手:HELLO `[pid u32][nonce 16B]` → HELLO_PROOF `[pid u32][nonce 16B][hmac32]` → PROOF `[hmac32]`,HMAC-SHA256(pipe_secret, nonce) 双向证明;secret 不入日志/命令行(spec §9.3)
- Pipe 名 `\\.\pipe\xnc-core`;DACL 仅 SYSTEM + Administrators(service SID M1 补);`FILE_FLAG_FIRST_PIPE_INSTANCE`
- 命名:可执行文件 `xnc-core.exe`(服务名 XNCCore,SCM 集成 M2);日志结构化、无 secret
- C++ 构建走 `build.bat + vcvars64`(仓库先例 `agent/screen-helper/dda/build.bat`),产物 `bin/xnc-core.exe`
- Clean-room:不参考/复制 RustDesk 代码(spec §2 原则 9)
- 已存在草稿:`proto/ipc/frame.go`、`proto/ipc/handshake.go`(未提交)——以测试校验、按需修正,不推倒重写

## 共享测试向量(Go 测试与 C++ selftest 必须同时断言)

**V1(wire 帧):** `Frame{Flags: FlagResponse, MessageType: MsgPing, RequestID: 0xDEADBEEF, Payload: []byte("xnc")}` 编码后精确字节:

```text
58 4E 49 50 01 05 10 00 EF BE AD DE 03 00 00 00 78 6E 63
```

**V2(HMAC,RFC 4231 Test Case 2):** `Proof([]byte("Jefe"), []byte("what do ya want for nothing?"))` == hex `5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843`

---

### Task 1: proto/ipc 帧编解码(TDD)

**Files:**
- Verify/Modify: `proto/ipc/frame.go`(已存在草稿)
- Test: `proto/ipc/frame_test.go`

**Interfaces:**
- Produces: `ipc.Frame{Flags uint8; MessageType uint16; RequestID uint32; Payload []byte}`、`(*Frame).Encode() []byte`、`ipc.Decode([]byte) (*Frame, error)`、`ipc.ReadFrame(io.Reader) (*Frame, error)`、`ipc.WriteFrame(io.Writer, *Frame) error`、`ipc.DecodeHeader([]byte) (uint16, int, error)`;常量 `Magic/ProtocolVersion/HeaderSize/MaxFrameBytes/FlagResponse/FlagEvent/FlagError/MsgHello/MsgHelloProof/MsgProof/MsgBye/MsgPing/MsgPong`;错误 `ErrBadMagic/ErrBadVersion/ErrTooLarge/ErrTruncated`(Task 3/4 消费)

- [ ] **Step 1: 写失败测试**

```go
package ipc

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"testing"
)

func TestFrameWireVectorV1(t *testing.T) {
	f := &Frame{Flags: FlagResponse, MessageType: MsgPing, RequestID: 0xDEADBEEF, Payload: []byte("xnc")}
	want, _ := hex.DecodeString("584E495001051000EFBEADDE03000000786E63")
	if got := f.Encode(); !bytes.Equal(got, want) {
		t.Fatalf("wire mismatch:\n got  %x\n want %x", got, want)
	}
	back, err := Decode(want)
	if err != nil {
		t.Fatal(err)
	}
	if back.Flags != FlagResponse || back.MessageType != MsgPing || back.RequestID != 0xDEADBEEF || string(back.Payload) != "xnc" {
		t.Fatalf("round-trip mismatch: %+v", back)
	}
}

func TestDecodeHeaderErrors(t *testing.T) {
	good := (&Frame{MessageType: MsgPing}).Encode()
	if _, _, err := DecodeHeader(good[:15]); !errors.Is(err, ErrTruncated) {
		t.Fatalf("short header: %v", err)
	}
	bad := append([]byte(nil), good...)
	bad[0] = 'X'
	if _, _, err := DecodeHeader(bad); !errors.Is(err, ErrBadMagic) {
		t.Fatalf("magic: %v", err)
	}
	bad = append([]byte(nil), good...)
	bad[4] = 2
	if _, _, err := DecodeHeader(bad); !errors.Is(err, ErrBadVersion) {
		t.Fatalf("version: %v", err)
	}
	bad = append([]byte(nil), good...)
	bad[12], bad[13], bad[14], bad[15] = 0xFF, 0xFF, 0xFF, 0x7F // >9MiB
	if _, _, err := DecodeHeader(bad); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too large: %v", err)
	}
	if _, err := Decode(good[:len(good)-1]); !errors.Is(err, ErrTruncated) {
		t.Fatalf("payload truncated: %v", err)
	}
}

func TestFrameStream(t *testing.T) {
	c1, c2 := net.Pipe()
	go func() {
		_ = WriteFrame(c1, &Frame{MessageType: MsgPong, RequestID: 7, Payload: []byte{1, 2}})
		_ = c1.Close()
	}()
	f, err := ReadFrame(c2)
	if err != nil {
		t.Fatal(err)
	}
	if f.MessageType != MsgPong || f.RequestID != 7 || !bytes.Equal(f.Payload, []byte{1, 2}) {
		t.Fatalf("stream frame mismatch: %+v", f)
	}
	if _, err := ReadFrame(c2); !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF, got %v", err)
	}
}

func TestMaxFrameEnforced(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Encode should panic on oversize payload (programmer error)")
		}
	}()
	_ = (&Frame{Payload: make([]byte, MaxFrameBytes+1)}).Encode()
}
```

- [ ] **Step 2: 运行确认结果**

Run: `cd proto && go test ./ipc/ -run 'TestFrame|TestDecode|TestMax' -v`
Expected: FAIL(测试文件不存在时先因无测试编译失败——创建文件后若草稿实现有偏差,按失败信息修 `frame.go`)

- [ ] **Step 3: 修正实现直至通过**

对照失败信息修 `proto/ipc/frame.go`(草稿按 spec §9.2 编写,预期已可过;重点核对:头部 16B 布局、LE、payload 副本语义、`DecodeHeader` 在 len<16 时返回 ErrTruncated)。

- [ ] **Step 4: 全量通过**

Run: `cd proto && go test ./ipc/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add proto/ipc/frame.go proto/ipc/frame_test.go
git commit -m "feat(proto/ipc): XNIP frame codec with shared wire vector test"
```

---

### Task 2: proto/ipc 握手原语 + .proto schema(TDD)

**Files:**
- Verify/Modify: `proto/ipc/handshake.go`(已存在草稿)
- Test: `proto/ipc/handshake_test.go`
- Create: `proto/ipc/core.proto`、`proto/ipc/desktop.proto`、`proto/ipc/shell.proto`(schema 定稿文档,codegen M1)

**Interfaces:**
- Consumes: Task 1 的 Frame/消息常量
- Produces: `ipc.NewNonce() []byte`、`ipc.Proof(secret, nonce []byte) []byte`、`ipc.VerifyProof(secret, nonce, got []byte) bool`、`ipc.EncodeHello(pid uint32, nonce []byte) []byte`、`ipc.DecodeHello(p []byte) (uint32, []byte, error)`、`ipc.EncodeHelloProof(pid uint32, nonce, proof []byte) []byte`、`ipc.DecodeHelloProof(p []byte) (uint32, []byte, []byte, error)`、`ipc.EncodeProof/DecodeProof`;常量 `NonceSize=16`、`ProofSize=32`

- [ ] **Step 1: 写失败测试**

```go
package ipc

import (
	"bytes"
	"encoding/hex"
	"testing"
)

func TestProofRFC4231VectorV2(t *testing.T) {
	got := Proof([]byte("Jefe"), []byte("what do ya want for nothing?"))
	want := "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843"
	if hex.EncodeToString(got) != want {
		t.Fatalf("HMAC vector mismatch: %x", got)
	}
	if !VerifyProof([]byte("Jefe"), []byte("what do ya want for nothing?"), got) {
		t.Fatal("VerifyProof should accept correct proof")
	}
	bad := append([]byte(nil), got...)
	bad[0] ^= 1
	if VerifyProof([]byte("Jefe"), []byte("what do ya want for nothing?"), bad) {
		t.Fatal("VerifyProof must reject tampered proof")
	}
	if VerifyProof([]byte("wrong"), []byte("what do ya want for nothing?"), got) {
		t.Fatal("VerifyProof must reject wrong secret")
	}
}

func TestHelloPayloadRoundTrip(t *testing.T) {
	nonce := NewNonce()
	if len(nonce) != NonceSize {
		t.Fatalf("nonce size %d", len(nonce))
	}
	pid, back, err := DecodeHello(EncodeHello(1234, nonce))
	if err != nil || pid != 1234 || !bytes.Equal(back, nonce) {
		t.Fatalf("hello round-trip: %v %d", err, pid)
	}
	p2, n2, proof, err := DecodeHelloProof(EncodeHelloProof(5678, nonce, Proof(nonce, nonce)))
	if err != nil || p2 != 5678 || !bytes.Equal(n2, nonce) || len(proof) != ProofSize {
		t.Fatalf("hello_proof round-trip: %v", err)
	}
	prf, err := DecodeProof(EncodeProof(proof))
	if err != nil || !bytes.Equal(prf, proof) {
		t.Fatalf("proof round-trip: %v", err)
	}
	for _, tc := range []struct {
		name string
		f    func() error
	}{
		{"hello short", func() error { _, _, err := DecodeHello(make([]byte, 4)); return err }},
		{"hello_proof short", func() error { _, _, _, err := DecodeHelloProof(make([]byte, 20)); return err }},
		{"proof short", func() error { _, err := DecodeProof(make([]byte, 31)); return err }},
	} {
		if tc.f() == nil {
			t.Fatalf("%s: want error", tc.name)
		}
	}
}
```

- [ ] **Step 2: 运行确认失败/通过状态**

Run: `cd proto && go test ./ipc/ -run 'TestProof|TestHello' -v`
Expected: FAIL(新测试文件)→ 修 `handshake.go` 直至 PASS(草稿预期基本就绪,核对 RFC 4231 向量证明 HMAC-SHA256 实现与长度校验)

- [ ] **Step 3: 定稿三个 .proto schema 文档**

内容**逐条取自 spec**(不新增发明):`core.proto` = spec §6.3 的 `service Core` 全部 RPC + `CoreEvent` 事件消息;`desktop.proto` = spec §9.4 的 Agent→Host/Host→Agent 消息;`shell.proto` = spec §8.5(SHELL_BEGIN/SHELL_RESIZE/EXEC_RESULT + pty binary 流的帧外说明注释)。文件头注释统一:

```protobuf
// XNC local IPC schema (spec §9). Source of truth: this file.
// Codegen wiring lands in M1; M0 payloads on the wire are the fixed-binary
// handshake messages defined in proto/ipc/frame.go (MsgHello..MsgPong).
```

- [ ] **Step 4: Commit**

```bash
git add proto/ipc/
git commit -m "feat(proto/ipc): mutual-proof handshake primitives (RFC4231 vector) + core/desktop/shell schemas"
```

---

### Task 3: agent/coreclient 客户端(TDD)

**Files:**
- Create: `agent/coreclient/client.go`、`agent/coreclient/client_test.go`

**Interfaces:**
- Consumes: `xnc/proto/ipc`(Task 1/2 全部导出);`github.com/Microsoft/go-winio`(agent go.mod 已有)
- Produces:

```go
type Client struct{ /* conn net.Conn; 仅经方法访问 */ }
func Dial(pipeName string, secret []byte) (*Client, error) // winio.DialPipe + 3-step 握手,任一步失败即关连接返回 err
func (c *Client) Ping() (time.Duration, error)             // MsgPing→MsgPong,RequestID 关联
func (c *Client) Close() error
func ServerHandshake(conn net.Conn, secret []byte) error   // 测试用/未来 xnc-shell 复用的服务端握手半边
```

- [ ] **Step 1: 写失败测试(进程内 winio pipe 假服务端)**

```go
package coreclient

import (
	"io"
	"testing"
	"time"

	"github.com/Microsoft/go-winio"
	"xnc/proto/ipc"
)

func listen(t *testing.T) (string, *winio.PipeListener) {
	t.Helper()
	ln, err := winio.ListenPipe(`\\.\pipe\xnc-coreclient-test-`+t.Name(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln.Addr().String(), ln
}

func TestHandshakeAndPing(t *testing.T) {
	name, ln := listen(t)
	const secret = "test-pipe-secret"
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if err := ServerHandshake(conn, []byte(secret)); err != nil {
			t.Errorf("server handshake: %v", err)
			return
		}
		f, err := ipc.ReadFrame(conn) // Ping
		if err != nil || f.MessageType != ipc.MsgPing {
			t.Errorf("ping read: %v %+v", err, f)
			return
		}
		_ = ipc.WriteFrame(conn, &Frame{Flags: ipc.FlagResponse, MessageType: ipc.MsgPong, RequestID: f.RequestID})
	}()
	c, err := Dial(name, []byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Ping(); err != nil {
		t.Fatal(err)
	}
}

func TestHandshakeRejectsWrongSecret(t *testing.T) {
	name, ln := listen(t)
	go func() {
		conn, _ := ln.Accept()
		defer conn.Close()
		if err := ServerHandshake(conn, []byte("server-secret")); err != nil {
			return // 客户端证明失败即断
		}
	}()
	_, err := Dial(name, []byte("client-secret"))
	if err == nil {
		t.Fatal("Dial must fail when secrets differ")
	}
}

func TestPingTimeout(t *testing.T) {
	c := &Client{conn: nopDeadConn{}} // 注入不回包连接
	c.conn = pipeConn(t)
	// 用真实 pipe 但服务端不回 Pong:
	if _, err := c.Ping(); err == nil {
		t.Fatal("Ping must time out without Pong")
	}
}

type nopDeadConn struct{ io.Closer }
func (nopDeadConn) Read([]byte) (int, error)   { time.Sleep(10 * time.Minute); return 0, io.EOF }
func (nopDeadConn) Write([]byte) (int, error)  { return 0, io.ErrClosedPipe }

func pipeConn(t *testing.T) net.Conn { /* net.Pipe 服务端只读不写 */ ... }
```

(执行者注:`TestPingTimeout` 的辅助 conn 按最小实现补全——`net.Pipe()` 两侧,服务端 goroutine 只 `ReadFrame` 不回包,`Client.Ping` 内部对读设 2s deadline。类型引用 `net` 需补 import。)

- [ ] **Step 2: 运行确认失败**

Run: `cd agent && go test ./coreclient/ -v`
Expected: FAIL(包不存在 → 创建后按失败逐项实现)

- [ ] **Step 3: 实现 client.go**

要点(与测试对齐):`Dial` = `winio.DialPipe(ctx, name, nil)` → 写 HELLO(pid=uint32(os.Getpid()), nonce=NewNonce())→ 读 HELLO_PROOF 并 `VerifyProof(secret, myNonce, proofC)` 失败即断连 → 写 PROOF(HMAC 对端 nonce)→ 返回;`Ping` 设 2s deadline、RequestID 递增、只接受 FlagResponse+MsgPong+同 RequestID;`ServerHandshake` = 读 HELLO → 写 HELLO_PROOF(自 pid+nonce+Proof(secret, 客户端 nonce))→ 读 PROOF 并 Verify → 完成。所有错误路径 `conn.Close()`。

- [ ] **Step 4: 全量通过**

Run: `cd agent && go test ./coreclient/ -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add agent/coreclient/
git commit -m "feat(agent/coreclient): pipe dial + mutual-proof handshake + ping"
```

---

### Task 4: native/core C++ 帧与握手模块 + selftest(TDD)

**Files:**
- Create: `native/core/frame.h`、`native/core/frame.cpp`、`native/core/handshake.h`、`native/core/handshake.cpp`、`native/core/selftest.cpp`、`native/core/build.bat`(本任务先建最小集,pipe/log/main 在 Task 5)

**Interfaces(与 Go 侧镜像,字节级一致):**
- Produces:

```cpp
// frame.h
namespace xnc {
constexpr char kMagic[4] = {'X','N','C','I'}; // 注意:按 spec 为 "XNIP"
struct Frame { uint8_t flags; uint16_t message_type; uint32_t request_id; std::vector<uint8_t> payload; };
bool EncodeFrame(const Frame&, std::vector<uint8_t>& out);      // false 当 payload > 9MiB
enum class DecodeResult { Ok, BadMagic, BadVersion, TooLarge, Truncated };
DecodeResult DecodeFrame(const uint8_t* data, size_t len, Frame& out);
DecodeResult ReadFrame(HANDLE file, Frame& out);                // 命名管道同步读
bool WriteFrame(HANDLE file, const Frame&);
constexpr uint16_t kMsgHello = 0x0001, kMsgHelloProof = 0x0002, kMsgProof = 0x0003,
                   kMsgBye = 0x0004, kMsgPing = 0x0010, kMsgPong = 0x0011;
constexpr uint8_t kFlagResponse = 1, kFlagEvent = 4, kFlagError = 16;
}
```

```cpp
// handshake.h
namespace xnc {
constexpr size_t kNonceSize = 16, kProofSize = 32;
bool HmacSha256(const uint8_t* key, size_t key_len, const uint8_t* data, size_t data_len, uint8_t out[32]); // BCrypt
std::vector<uint8_t> EncodeHello(uint32_t pid, const uint8_t nonce[16]);
DecodeResult DecodeHello(const Frame&, uint32_t& pid, uint8_t nonce[16]);
std::vector<uint8_t> EncodeHelloProof(uint32_t pid, const uint8_t nonce[16], const uint8_t proof[32]);
DecodeResult DecodeHelloProof(const Frame&, uint32_t& pid, uint8_t nonce[16], uint8_t proof[32]);
std::vector<uint8_t> EncodeProof(const uint8_t proof[32]);
DecodeResult DecodeProof(const Frame&, uint8_t proof[32]);
}
```

(执行者注意:`kMagic` 注释里的笔误必须避免——实现一律 `{'X','N','I','P'}`;selftest 的 V1 向量会抓住任何此类错误,这正是向量的意义。)

- [ ] **Step 1: 写 selftest(= C++ 侧测试,先写后实现,main 在 Task 5 提供入口前以独立 exe 验证)**

`selftest.cpp` 断言(任一失败输出 `SELFTEST FAIL: <name>` 并 exit 1,全过输出 `selftest ok`):

```cpp
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
        const uint8_t want[19] = {0x58,0x4E,0x49,0x50,0x01,0x05,0x10,0x00,0xEF,0xBE,0xAD,0xDE,0x03,0x00,0x00,0x00,0x78,0x6E,0x63};
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
        CHECK("hmac", HmacSha256((const uint8_t*)k,4,(const uint8_t*)d,30,mac));
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
```

- [ ] **Step 2: 临时驱动验证失败**

`native/core/build.bat` 先编译 `selftest.cpp + frame.cpp + handshake.cpp`(临时 `selftest_main.cpp` 提供 `int main(){return SelftestMain();}`,Task 5 移除)并运行。
Run: `native/core/build.bat && bin/xnc-core-selftest.exe`
Expected: 链接失败/断言 FAIL(实现未写)

- [ ] **Step 3: 实现 frame.cpp / handshake.cpp**

`frame.cpp`:纯内存小端编解码(`memcpy` + 手工 LE,不依赖 host 字序假设的字节逐拼);`ReadFrame(HANDLE)`:先 `ReadFile` 16B(循环至齐/错)→ 校验 → 按 payloadLength `ReadFile`;`WriteFrame(HANDLE)`:`WriteFile` 全量。`handshake.cpp`:`HmacSha256` 用 `BCryptOpenAlgorithmProvider(BCRYPT_SHA256_ALGORITHM)` + `BCryptCreateHash(..., pbSecret, cbSecret, ...)`(HMAC 形态)+ `BCryptHashData`/`BCryptFinishHash`/`BCryptDestroyHash`/`BCryptCloseAlgorithmProvider`;链接 `bcrypt.lib`。

- [ ] **Step 4: selftest 通过**

Run: `native/core/build.bat && bin/xnc-core-selftest.exe`
Expected: 输出 `selftest ok`,exit 0

- [ ] **Step 5: Commit**

```bash
git add native/core/ bin/xnc-core-selftest.exe
git commit -m "feat(native/core): XNIP frame + HMAC handshake (byte-identical to Go, shared vectors)"
```

---

### Task 5: native/core pipe 服务端 + 日志 + watchdog + main

**Files:**
- Create: `native/core/log.h`、`native/core/pipe_server.h`、`native/core/pipe_server.cpp`、`native/core/watchdog.h`、`native/core/xnc-core.cpp`
- Modify: `native/core/build.bat`(编入全部源文件产出 `bin/xnc-core.exe`,保留 selftest 目标)

**Interfaces:**
- Consumes: Task 4 的 frame/handshake
- Produces: `xnc-core.exe [--console [--pipe-name <name>] [--smoke-secret <hex>]] [--selftest]`;`--selftest` 跑 SelftestMain;`--console` 前台运行:创建 `\\.\pipe\xnc-core`(或指定名,DACL: SYSTEM+Administrators,`FILE_FLAG_FIRST_PIPE_INSTANCE`),循环 accept,每连接:服务端握手(`--smoke-secret` 指定 32B hex secret,**仅 --console 模式允许,服务模式 M1 从 spawn 通道取**)→ 进入消息循环(PING→PONG 回显,未知类型记日志并回 FlagError)→ 断开继续 accept;watchdog 线程每 5s 检查主循环心跳 atomic,>30s 无进展 log error 并 exit(1)(自杀重启语义,spec §15)

- [ ] **Step 1: 实现(纯 C++ 侧无单测框架,行为由 Task 6 跨语言 smoke 验收;log/watchdog 保持 ≤60 行/文件)**

`log.h`:`XNC_LOG(level, fmt, ...)` → `[2026-08-22T12:00:00Z core pid=1234 level=info] msg`(本地时间即可,mutex+stderr;绝不打印 secret/nonce 后的 proof)。
`pipe_server.cpp`:`CreateNamedPipeW(name, PIPE_ACCESS_DUPLEX|FILE_FLAG_FIRST_PIPE_INSTANCE, PIPE_TYPE_BYTE|PIPE_READMODE_BYTE|PIPE_WAIT, 1, 64*1024, 64*1024, 0, &sa)`;DACL 用 `ConvertStringSecurityDescriptorToSecurityDescriptorW`:`D:P(A;;GA;;;SY)(A;;GA;;;BA)`;`SECURITY_ATTRIBUTES` 传 bInheritHandle=FALSE。
`xnc-core.cpp`:参数解析 → selftest 分支 → console 分支(日志起、watchdog 线程起、serve 循环,Ctrl+C 退出码 0);无参数时打印用法并提示「服务模式 M2 接入 SCM」。

- [ ] **Step 2: 手动 smoke**

Run: `native/core/build.bat && (./bin/xnc-core.exe --console --smoke-secret 746573742d706970652d736563726574 &)`(secret hex = "test-pipe-secret")
Expected: 日志显示 pipe 监听就绪、无崩溃

- [ ] **Step 3: Commit**

```bash
git add native/core/
git commit -m "feat(native/core): console pipe server with auth handshake, logging, watchdog"
```

---

### Task 6: 跨语言 smoke(自动化验收)

**Files:**
- Create: `agent/coreclient/cross_test.go`

**Interfaces:**
- Consumes: Task 3 `Dial/Ping/Close`;Task 5 `bin/xnc-core.exe --console --smoke-secret <hex>`

- [ ] **Step 1: 写 gated 测试**

```go
package coreclient

import (
	"encoding/hex"
	"os"
	"os/exec"
	"testing"
	"time"
)

// XNC_CORE_EXE 指向 xnc-core.exe 时启用(默认跳过);CI 与本地验收:
//   XNC_CORE_EXE=../../bin/xnc-core.exe go test ./coreclient/ -run Cross -v
func TestCrossLanguageHandshake(t *testing.T) {
	exe := os.Getenv("XNC_CORE_EXE")
	if exe == "" {
		t.Skip("set XNC_CORE_EXE to run cross-language smoke")
	}
	const secret = "test-pipe-secret"
	cmd := exec.Command(exe, "--console", "--pipe-name", `\\.\pipe\xnc-core-smoke`,
		"--smoke-secret", hex.EncodeToString([]byte(secret)))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	time.Sleep(500 * time.Millisecond) // pipe 就绪
	c, err := Dial(`\\.\pipe\xnc-core-smoke`, []byte(secret))
	if err != nil {
		t.Fatalf("handshake with xnc-core failed: %v", err)
	}
	defer c.Close()
	rtt, err := c.Ping()
	if err != nil || rtt <= 0 {
		t.Fatalf("ping: %v %v", err, rtt)
	}
}
```

- [ ] **Step 2: 运行跨语言验收**

Run: `cd agent && XNC_CORE_EXE=../bin/xnc-core.exe go test ./coreclient/ -run Cross -v`
Expected: PASS(Go 完成握手 + PING/PONG 往返 —— 两种语言的帧/握手实现字节级一致的最终证明)

- [ ] **Step 3: 全仓回归 + 提交**

Run: `cd proto && go test ./... && cd ../agent && go test ./... && cd ../server && go test ./...`(server/cli 无改动,编译回归即可)
Expected: 全 PASS

```bash
git add agent/coreclient/cross_test.go
git commit -m "test(coreclient): cross-language smoke vs xnc-core.exe (CI gate)"
```

---

## Self-Review 记录

- Spec 覆盖:M0 四条(proto/ipc ✓ Task1/2;xnc-core 骨架 ✓ Task4/5;coreclient ✓ Task3;跨语言 smoke ✓ Task6);SCM 服务化、protobuf codegen、pipe_secret 正式发放通道均为 M1+,不在本计划
- 占位符扫描:Task 3 测试中 `pipeConn` 辅助函数留有执行者注记(明确到实现方式),其余无 TBD
- 类型一致性:`Frame.Flags/MessageType/RequestID/Payload`、`MsgHello..MsgPong`、`Dial(pipeName, secret)`、`ServerHandshake(conn, secret)` 各任务引用一致;C++ `kMagic` 特意标注防笔误并由 V1 向量兜底
