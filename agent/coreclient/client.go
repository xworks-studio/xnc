//go:build windows

// Package coreclient 实现 agent 侧 XNIP named pipe 客户端(spec §9):
// winio 拨号 + 双向认证握手(§9.3)+ 保活 Ping + START/STOP_CAPTURE
// RPC(M1-Slice2)+ SendSAS(M2-Slice1 Task 4/5;固定二进制 payload,
// protobuf 迁移 Slice3)。帧编解码与证明计算复用 xnc/proto/ipc;
// PID/映像路径校验(OpenProcess + Authenticode)属连接层,由 xnc-core
// 侧(M1)补全,本包不感知。
//
// 仅 Windows 构建(named pipe);与 agent/session 的 windows-only 测试
// 同风格。
//
// 并发边界(M1-Slice2):请求路径(Ping/StartCapture/StopCapture/Close)
// goroutine 安全——单一后台读泵按 RequestID 关联 pending map,写侧
// 互斥串行。剩余限制(ledger 项):Dial/ServerHandshake 仍为同步单发;
// 服务端主动事件帧(core→agent,M2)当前被泵丢弃;Close 不等待泵退出。
package coreclient

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"xnc/proto/ipc"
)

const (
	// handshakeTimeout 覆盖拨号等待与整个三步握手,防对端僵死挂起。
	handshakeTimeout = 5 * time.Second
	// pingTimeout 是 Ping 等待 Pong 的时限。
	pingTimeout = 2 * time.Second
	// rpcTimeout 覆盖 START/STOP_CAPTURE:核心侧含子进程 spawn +
	// pipe 就绪等待(≤2s)+ 会话令牌铸造,取充裕上界。
	rpcTimeout = 15 * time.Second
	// captureSecretLen 是 START_CAPTURE 响应携带的 desktop pipe secret
	// 固定长度(与 native/core 生成侧一致)。
	captureSecretLen = 32
)

// 应用 RPC 类型(0x0100 注册块;native/core/pipe_server.h 的 Go 镜像)。
const (
	MsgStartCapture uint16 = 0x0100
	MsgStopCapture  uint16 = 0x0101
	// MsgSas 是 SendSAS 请求(M2-Slice1 Task 4:capability 门控的
	// secure attention;Task 5 = 本侧调用方)。
	MsgSas uint16 = 0x0110
	// MsgCreateShell / MsgKillShell(M2-Slice2 Task 3/4:exec/shell 经
	// xnc-core 的令牌语义进程创建)。
	MsgCreateShell uint16 = 0x0120
	MsgKillShell   uint16 = 0x0121
)

// WTSActiveConsole 是 0x0120 wts 字段的哨兵值:调用方(agent)没有 wts
// 上下文,0xFFFFFFFF = "核心解析活动控制台会话"(agent<->core 文档化契约,
// native/core pipe_server.h 同注)。
const WTSActiveConsole uint32 = 0xFFFFFFFF

// Shell token kind(0x0120 token_kind 字段)。
const (
	TokenUser   uint8 = 0
	TokenSystem uint8 = 1
)

// Shell profile 白名单(0x0120 profile 字段枚举;spec §8.2——核心只认
// 枚举,xnc-shell 自解析路径)。
const (
	ProfilePowershell uint8 = 0
	ProfilePwsh       uint8 = 1
	ProfileCmd        uint8 = 2
	ProfileBash       uint8 = 3
)

// ShellMode 0x0120 mode 字段。
const (
	ModeInteractive uint8 = 0
	ModeOneshot     uint8 = 1
)

// sasReasonLen 镜像 native/core pipe_server.h kSasReasonLen:0x0110 请求
// payload 是 NUL 填充的定长 [char reason[24]] 字段(>23 字节截断)。
const sasReasonLen = 24

// Client 是一条已完成握手的 pipe 连接。请求经 roundTrip 走后台读泵;
// conn 的读侧仅由泵访问,写侧由 writeMu 串行。
type Client struct {
	conn    net.Conn
	writeMu sync.Mutex // 串行化请求帧写入

	mu      sync.Mutex // 守护以下字段
	reqID   uint32
	pending map[uint32]chan rpcResult
	readErr error // 置位后连接判死,后续请求立即失败
	started bool   // 读泵已启动
	closed  bool
	wg      sync.WaitGroup
}

// rpcResult 是一次挂起请求的交付物:响应帧或连接级错误。
type rpcResult struct {
	f   *ipc.Frame
	err error
}

var errClosed = errors.New("coreclient: connection closed")

// Dial 连接 named pipe 并完成三步握手:
//
//	写 HELLO(pid, nonce) → 读 HELLO_PROOF 并 VerifyProof(secret, 我方 nonce)
//	→ 写 PROOF(HMAC(secret, 对端 nonce))
//
// 任一步失败即关连接返回 err(spec §9.3:任一侧证明失败即断连)。
func Dial(pipeName string, secret []byte) (*Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout)
	defer cancel()
	conn, err := winio.DialPipeContext(ctx, pipeName)
	if err != nil {
		return nil, fmt.Errorf("coreclient: dial %s: %w", pipeName, err)
	}
	// 握手全程受 deadline 约束;成功后清除,后续读写由各方法自理。
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))

	myNonce := ipc.NewNonce()
	if err := ipc.WriteFrame(conn, &ipc.Frame{
		MessageType: ipc.MsgHello,
		Payload:     ipc.EncodeHello(uint32(os.Getpid()), myNonce),
	}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("coreclient: send hello: %w", err)
	}
	f, err := ipc.ReadFrame(conn)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("coreclient: read hello_proof: %w", err)
	}
	if f.MessageType != ipc.MsgHelloProof {
		conn.Close()
		return nil, fmt.Errorf("coreclient: handshake: got message type %#x, want HELLO_PROOF", f.MessageType)
	}
	_, peerNonce, proofC, err := ipc.DecodeHelloProof(f.Payload)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("coreclient: decode hello_proof: %w", err)
	}
	if !ipc.VerifyProof(secret, myNonce, proofC) {
		conn.Close()
		return nil, fmt.Errorf("coreclient: server proof rejected")
	}
	if err := ipc.WriteFrame(conn, &ipc.Frame{
		MessageType: ipc.MsgProof,
		Payload:     ipc.EncodeProof(ipc.Proof(secret, peerNonce)),
	}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("coreclient: send proof: %w", err)
	}
	_ = conn.SetDeadline(time.Time{})
	return &Client{conn: conn}, nil
}

// Ping 发送 MsgPing 并等待同 RequestID 的 MsgPong 响应,返回 RTT。
// 不相干帧(事件流等)由读泵丢弃。RTT 可为 0:Windows 单调钟粒度
// ~0.5ms,本地 pipe 往返常低于一个 tick;成功判据是 err==nil。
func (c *Client) Ping() (time.Duration, error) {
	start := time.Now()
	if _, err := c.roundTrip(pingTimeout, ipc.MsgPing, nil); err != nil {
		return 0, err
	}
	return time.Since(start), nil
}

// StartCapture 请求核心在指定 WTS 会话启动桌面采集(0x0100):
// 请求 payload `[u32 wts][u32 pad=0]`;成功响应 payload
// `[u32 pid][u16 nameLen][name utf8 字节][32B secret][u32 gen]`
// (nameLen = name 的 UTF-8 字节长度)。核心侧幂等:采集已运行时返回
// 既有 pipe/secret/gen,不重复 spawn。FlagError 响应以 payload 文本
// (ASCII 错误码)进错误。
func (c *Client) StartCapture(wtsSession uint32) (hostPid uint32, pipeName string, secret []byte, gen uint32, err error) {
	f, err := c.roundTrip(rpcTimeout, MsgStartCapture, encodeStartCaptureReq(wtsSession))
	if err != nil {
		return 0, "", nil, 0, err
	}
	return decodeStartCaptureResp(f.Payload)
}

// StopCapture 请求核心停止桌面采集(0x0101,空 payload)。核心侧
// Slice2 为 TerminateProcess 子进程(优雅排水在后续);空闲时同样回
// 成功(幂等)。
func (c *Client) StopCapture() error {
	_, err := c.roundTrip(rpcTimeout, MsgStopCapture, nil)
	return err
}

// SendSAS 请求核心触发 secure attention 序列(0x0110,M2-Slice1
// Task 4/5)。请求 payload 为 NUL 填充的 [char reason[24]](reason 截断
// 到 23 字节);成功响应 payload [u32 hr] —— 合成 HRESULT:sas.dll 的
// SendSAS 返回 VOID,hr=0 只表示「调用未抛异常」,非 SAS 已送达的证明
// (T6 验收以安全桌面出现为准)。拒绝走 FlagError,稳定码
// SAS_DENIED(--allow-sas 门关)/ SAS_UNAVAILABLE(sas.dll 不可载)/
// BAD_PAYLOAD,以 RejectedError 形态返回(码可编程判别)。
func (c *Client) SendSAS(reason string) (hr uint32, err error) {
	f, err := c.roundTrip(rpcTimeout, MsgSas, encodeSasReason(reason))
	if err != nil {
		return 0, err
	}
	return decodeSasResp(f.Payload)
}

// ShellCreateReq 是 0x0120 的参数形态(native/core ShellCreateReq 的
// Go 镜像)。WTS 通常填 WTSActiveConsole 哨兵(核心解析活动控制台);
// Env 为 "K=V" 列表——值含 '\n' 不可表示,Encode 阶段即拒(BAD_PAYLOAD
// 语义前置;核心侧对裸段同样拒绝)。
type ShellCreateReq struct {
	WTS        uint32
	TokenKind  uint8 // TokenUser / TokenSystem
	Profile    uint8 // Profile* 白名单枚举
	Mode       uint8 // ModeInteractive / ModeOneshot
	Cols       uint16
	Rows       uint16
	Cwd        string
	Env        []string
	Cmd        string
	TimeoutSec uint32
}

// CreateShell 请求核心以指定令牌语义 spawn xnc-shell(0x0120)并回进程
// 描述符:成功响应 [u32 pid][u16 nameLen][pipeName utf8][32B secret]。
// 拒绝走 RejectedError(NO_ACTIVE_SESSION / SESSION_MISMATCH /
// BAD_PAYLOAD / SPAWN_FAILED / PIPE_TIMEOUT / TOKEN_FAILED / RNG_FAILED)。
func (c *Client) CreateShell(r ShellCreateReq) (pid uint32, pipeName string, secret []byte, err error) {
	p, err := EncodeShellCreateReq(r)
	if err != nil {
		return 0, "", nil, err
	}
	f, err := c.roundTrip(rpcTimeout, MsgCreateShell, p)
	if err != nil {
		return 0, "", nil, err
	}
	return decodeShellCreateResp(f.Payload)
}

// KillShell 请求核心终止 shell 子进程(0x0121 [u32 pid];核心按存储
// handle 限定,幂等)。
func (c *Client) KillShell(pid uint32) error {
	p := make([]byte, 4)
	binary.LittleEndian.PutUint32(p, pid)
	_, err := c.roundTrip(rpcTimeout, MsgKillShell, p)
	return err
}

// EncodeShellCreateReq 编码 0x0120 请求(布局见 pipe_server.h;小端):
// [u32 wts][u8 token_kind][u8 profile][u8 mode][u16 cols][u16 rows]
// [u16 cwdLen][cwd][u16 envLen][env "K=V\n" join][u16 cmdLen][cmd]
// [u32 timeoutSec]。域校验前置:profile/mode 枚举、oneshot 必带 cmd、
// env 值含 '\n' 或缺 '=' 即错(生产者契约,不静默拆分)。
func EncodeShellCreateReq(r ShellCreateReq) ([]byte, error) {
	if r.WTS == 0 {
		r.WTS = WTSActiveConsole // 缺省哨兵:核心解析活动控制台
	}
	if r.Profile > ProfileBash {
		return nil, fmt.Errorf("coreclient: create_shell: bad profile %d", r.Profile)
	}
	if r.TokenKind > TokenSystem {
		return nil, fmt.Errorf("coreclient: create_shell: bad token kind %d", r.TokenKind)
	}
	if r.Mode > ModeOneshot {
		return nil, fmt.Errorf("coreclient: create_shell: bad mode %d", r.Mode)
	}
	if r.Mode == ModeOneshot && r.Cmd == "" {
		return nil, errors.New("coreclient: create_shell: oneshot requires cmd")
	}
	for _, kv := range r.Env {
		if !strings.Contains(kv, "=") {
			return nil, fmt.Errorf("coreclient: create_shell: env entry %q missing '='", kv)
		}
		if strings.Contains(kv, "\n") {
			return nil, fmt.Errorf("coreclient: create_shell: env entry %q contains newline", kv)
		}
	}
	if len(r.Cwd) > 0xFFFF || len(r.Cmd) > 0xFFFF {
		return nil, errors.New("coreclient: create_shell: cwd/cmd too long")
	}
	env := strings.Join(r.Env, "\n")
	if len(env) > 0xFFFF {
		return nil, errors.New("coreclient: create_shell: env too long")
	}
	p := make([]byte, 0, 20+len(r.Cwd)+len(env)+len(r.Cmd))
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], r.WTS)
	p = append(p, b[:]...)
	p = append(p, r.TokenKind, r.Profile, r.Mode)
	var h [2]byte
	binary.LittleEndian.PutUint16(h[:], r.Cols)
	p = append(p, h[:]...)
	binary.LittleEndian.PutUint16(h[:], r.Rows)
	p = append(p, h[:]...)
	binary.LittleEndian.PutUint16(h[:], uint16(len(r.Cwd)))
	p = append(p, h[:]...)
	p = append(p, r.Cwd...)
	binary.LittleEndian.PutUint16(h[:], uint16(len(env)))
	p = append(p, h[:]...)
	p = append(p, env...)
	binary.LittleEndian.PutUint16(h[:], uint16(len(r.Cmd)))
	p = append(p, h[:]...)
	p = append(p, r.Cmd...)
	binary.LittleEndian.PutUint32(b[:], r.TimeoutSec)
	p = append(p, b[:]...)
	return p, nil
}

// decodeShellCreateResp 解码 ok 响应 [u32 pid][u16 nameLen][pipeName]
// [32B secret](与 start_capture 同形,少 gen)。
func decodeShellCreateResp(p []byte) (pid uint32, name string, secret []byte, err error) {
	const hdr = 6
	if len(p) < hdr+captureSecretLen {
		return 0, "", nil, fmt.Errorf("coreclient: create_shell response %d bytes, want >= %d", len(p), hdr+captureSecretLen)
	}
	pid = binary.LittleEndian.Uint32(p)
	nameLen := int(binary.LittleEndian.Uint16(p[4:hdr]))
	if len(p) != hdr+nameLen+captureSecretLen {
		return 0, "", nil, fmt.Errorf("coreclient: create_shell response length mismatch: nameLen=%d total=%d", nameLen, len(p))
	}
	name = string(p[hdr : hdr+nameLen])
	if name == "" {
		return 0, "", nil, errors.New("coreclient: create_shell response: empty pipe name")
	}
	secret = append([]byte(nil), p[hdr+nameLen:hdr+nameLen+captureSecretLen]...)
	return pid, name, secret, nil
}

// roundTrip 写一条请求帧并等待同 RequestID 的 FlagResponse(时限
// timeout)。FlagError 响应转为携带 payload 文本的错误。goroutine
// 安全:并发调用各自挂入 pending map,由单一读泵配对交付。
func (c *Client) roundTrip(timeout time.Duration, mt uint16, payload []byte) (*ipc.Frame, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errClosed
	}
	if c.readErr != nil {
		err := fmt.Errorf("coreclient: connection unusable: %w", c.readErr)
		c.mu.Unlock()
		return nil, err
	}
	if c.pending == nil {
		c.pending = make(map[uint32]chan rpcResult)
	}
	c.reqID++
	id := c.reqID
	ch := make(chan rpcResult, 1) // 缓冲 1:泵交付后无需等待接收方
	c.pending[id] = ch
	if !c.started {
		c.started = true
		c.wg.Add(1)
		go c.readPump()
	}
	c.mu.Unlock()

	c.writeMu.Lock()
	werr := ipc.WriteFrame(c.conn, &ipc.Frame{MessageType: mt, RequestID: id, Payload: payload})
	c.writeMu.Unlock()
	if werr != nil {
		c.failPending(id, nil)
		return nil, fmt.Errorf("coreclient: send %s: %w", msgName(mt), werr)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("coreclient: %s: %w", msgName(mt), r.err)
		}
		if r.f.Flags&ipc.FlagError != 0 {
			return nil, &RejectedError{RPC: msgName(mt), Code: respText(r.f)}
		}
		return r.f, nil
	case <-timer.C:
		c.failPending(id, nil)
		return nil, fmt.Errorf("coreclient: %s: no response within %s", msgName(mt), timeout)
	}
}

// readPump 是唯一读方:读到响应帧按 RequestID 交付,无挂起方的帧
// (服务端主动事件,M2 接线)当前丢弃;读错误置死连接并唤醒全部
// 挂起请求。
func (c *Client) readPump() {
	defer c.wg.Done()
	for {
		f, err := ipc.ReadFrame(c.conn)
		c.mu.Lock()
		if err != nil {
			c.readErr = err
			pend := c.pending
			c.pending = nil
			c.mu.Unlock()
			for _, ch := range pend {
				ch <- rpcResult{err: err}
			}
			return
		}
		ch, ok := c.pending[f.RequestID]
		if ok {
			delete(c.pending, f.RequestID)
		}
		c.mu.Unlock()
		if ok {
			ch <- rpcResult{f: f}
		}
	}
}

// failPending 摘除一条挂起请求;err 非 nil 时向其交付连接级错误。
func (c *Client) failPending(id uint32, err error) {
	c.mu.Lock()
	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()
	if ok && err != nil {
		ch <- rpcResult{err: err}
	}
}

// Close 关闭底层连接并唤醒全部挂起请求(以 errClosed)。幂等;不等待
// 读泵退出(连接关闭即令其退出)。
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	pend := c.pending
	c.pending = nil
	c.mu.Unlock()
	err := c.conn.Close()
	for _, ch := range pend {
		ch <- rpcResult{err: errClosed}
	}
	return err
}

// ---- 0x0100/0x0101 payload 编解码(布局见方法注释;小端) ----

// RejectedError 标记一条 FlagError 响应(payload = 核心侧稳定 ASCII 码,
// 如 SAS_DENIED / SPAWN_FAILED / SESSION_MISMATCH)。错误文本与旧
// fmt.Errorf 形态一致;类型化让调用方(如 SAS 路径)能按码编程分派。
type RejectedError struct {
	RPC  string // 请求名(msgName)
	Code string // 核心侧稳定码
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("coreclient: %s rejected: %s", e.RPC, e.Code)
}

// encodeStartCaptureReq 编码请求 `[u32 wts][u32 pad=0]`。
func encodeStartCaptureReq(wts uint32) []byte {
	p := make([]byte, 8)
	binary.LittleEndian.PutUint32(p, wts)
	binary.LittleEndian.PutUint32(p[4:], 0) // 保留位,发送侧恒 0
	return p
}

// decodeStartCaptureResp 解码响应 `[u32 pid][u16 nameLen][name utf8]
// [32B secret][u32 gen]`;长度不自洽或 name 为空即协议错误。
func decodeStartCaptureResp(p []byte) (pid uint32, name string, secret []byte, gen uint32, err error) {
	const hdr = 6 // u32 pid + u16 nameLen
	if len(p) < hdr+captureSecretLen+4 {
		return 0, "", nil, 0, fmt.Errorf("coreclient: start_capture response %d bytes, want >= %d", len(p), hdr+captureSecretLen+4)
	}
	pid = binary.LittleEndian.Uint32(p)
	nameLen := int(binary.LittleEndian.Uint16(p[4:hdr]))
	if len(p) != hdr+nameLen+captureSecretLen+4 {
		return 0, "", nil, 0, fmt.Errorf("coreclient: start_capture response length mismatch: nameLen=%d total=%d", nameLen, len(p))
	}
	off := hdr
	name = string(p[off : off+nameLen])
	off += nameLen
	if name == "" {
		return 0, "", nil, 0, errors.New("coreclient: start_capture response: empty pipe name")
	}
	secret = append([]byte(nil), p[off:off+captureSecretLen]...)
	off += captureSecretLen
	gen = binary.LittleEndian.Uint32(p[off:])
	return pid, name, secret, gen, nil
}

// encodeSasReason 编码 0x0110 请求 [char reason[24]]:NUL 填充定长,
// reason 超过 23 字节截断(核心侧按定长字段读,不另行协商长度)。
func encodeSasReason(reason string) []byte {
	b := make([]byte, sasReasonLen)
	r := []byte(reason)
	if len(r) > sasReasonLen-1 {
		r = r[:sasReasonLen-1]
	}
	copy(b, r)
	return b
}

// decodeSasResp 解码 ok 响应 [u32 hr];长度 != 4 即协议错误。
func decodeSasResp(p []byte) (uint32, error) {
	if len(p) != 4 {
		return 0, fmt.Errorf("coreclient: sas response %d bytes, want 4", len(p))
	}
	return binary.LittleEndian.Uint32(p), nil
}

func msgName(mt uint16) string {
	switch mt {
	case ipc.MsgPing:
		return "ping"
	case MsgStartCapture:
		return "start_capture"
	case MsgStopCapture:
		return "stop_capture"
	case MsgSas:
		return "sas"
	case MsgCreateShell:
		return "create_shell"
	case MsgKillShell:
		return "kill_shell"
	}
	return fmt.Sprintf("msg %#04x", mt)
}

// respText 提取 FlagError 帧的 ASCII 错误码(不可打印则给字节数)。
func respText(f *ipc.Frame) string {
	if len(f.Payload) == 0 {
		return "<empty>"
	}
	for _, b := range f.Payload {
		if b < 0x20 || b > 0x7E {
			return fmt.Sprintf("<%d binary bytes>", len(f.Payload))
		}
	}
	return string(f.Payload)
}

// ServerHandshake 在已接受的连接上执行服务端握手半边:
//
//	读 HELLO → 写 HELLO_PROOF(自 pid + 自 nonce + Proof(secret, 发起方 nonce))
//	→ 读 PROOF 并 VerifyProof
//
// 任一步失败即断连返回 err;成功后清除 deadline,连接交还调用方。
// 测试用;xnc-core(C++ 侧 M1)与 xnc-shell 复用同一时序。
func ServerHandshake(conn net.Conn, secret []byte) error {
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	fail := func(err error) error {
		conn.Close()
		return err
	}

	f, err := ipc.ReadFrame(conn)
	if err != nil {
		return fail(fmt.Errorf("coreclient: server handshake: read hello: %w", err))
	}
	if f.MessageType != ipc.MsgHello {
		return fail(fmt.Errorf("coreclient: server handshake: got message type %#x, want HELLO", f.MessageType))
	}
	_, clientNonce, err := ipc.DecodeHello(f.Payload)
	if err != nil {
		return fail(fmt.Errorf("coreclient: server handshake: decode hello: %w", err))
	}
	myNonce := ipc.NewNonce()
	if err := ipc.WriteFrame(conn, &ipc.Frame{
		MessageType: ipc.MsgHelloProof,
		Payload:     ipc.EncodeHelloProof(uint32(os.Getpid()), myNonce, ipc.Proof(secret, clientNonce)),
	}); err != nil {
		return fail(fmt.Errorf("coreclient: server handshake: send hello_proof: %w", err))
	}
	f, err = ipc.ReadFrame(conn)
	if err != nil {
		return fail(fmt.Errorf("coreclient: server handshake: read proof: %w", err))
	}
	if f.MessageType != ipc.MsgProof {
		return fail(fmt.Errorf("coreclient: server handshake: got message type %#x, want PROOF", f.MessageType))
	}
	proof, err := ipc.DecodeProof(f.Payload)
	if err != nil {
		return fail(fmt.Errorf("coreclient: server handshake: decode proof: %w", err))
	}
	if !ipc.VerifyProof(secret, myNonce, proof) {
		return fail(fmt.Errorf("coreclient: server handshake: client proof rejected"))
	}
	_ = conn.SetDeadline(time.Time{})
	return nil
}
