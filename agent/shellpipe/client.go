//go:build windows

// Package shellpipe 实现 agent 侧 xnc-shell pipe 客户端(M2-Slice2
// Task 4,镜像 desktoppipe 模式):winio 拨号 + M0 双向 HMAC 握手
// (proto/ipc,spec §9.3)+ SHELL_BEGIN 等待,随后 DATA 双向泵。消息集
// 与 shellhost/message.go 逐字节一致(payload 小端):
//
//	MSG_SHELL_BEGIN  0x0122 event [u16 cols][u16 rows][char profile[16]]
//	MSG_SHELL_DATA   0x0123 双向  [u8 stream 0=stdin,1=stdout,2=stderr]
//	                          [u32 len][bytes]
//	MSG_SHELL_RESIZE 0x0124 req   [u16 cols][u16 rows]
//	MSG_SHELL_EXIT   0x0125 event [u32 exit_code]
//	MSG_SHELL_KILL   0x0126 req   (无 payload)
//	MSG_SHELL_STATE  0x0127 event [char code[24]](NUL 填充)
//
// 消费模型:DataCh/StateCh 缓冲通道(满丢——oneshot 输出由会话层转发,
// 背压经 ws 写时限体现);ExitCh 在 SHELL_EXIT 后交付一次退出码并随泵
// 退出关闭。Kill 发 0x0126(xnc-shell 杀整棵进程树)。
package shellpipe

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
	"xnc/proto/ipc"
)

// 消息类型(shellhost/message.go 镜像)。
const (
	msgShellBegin  uint16 = 0x0122
	msgShellData   uint16 = 0x0123
	msgShellResize uint16 = 0x0124
	msgShellExit   uint16 = 0x0125
	msgShellKill   uint16 = 0x0126
	msgShellState  uint16 = 0x0127
)

// stream 字节值。
const (
	StreamStdin  uint8 = 0
	StreamStdout uint8 = 1
	StreamStderr uint8 = 2
)

const (
	handshakeTimeout = 5 * time.Second
	beginTimeout     = 10 * time.Second // 含 xnc-shell 解析 profile + 启动 pty
	ctrlWriteTimeout = 10 * time.Second
	dataChDepth      = 64
	stateChDepth     = 4
	profileFieldLen  = 16
	stateFieldLen    = 24
)

// dialPipe 是可注入的拨号函数(测试换 net.Pipe 适配器;生产 = winio)。
var dialPipe = func(ctx context.Context, pipe string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, pipe)
}

// Begin 是 SHELL_BEGIN 内容:xnc-shell 实际生效的几何与 profile。
type Begin struct {
	Cols, Rows uint16
	Profile    string // POWERSHELL / PWSH / CMD / BASH
}

// Data 是一条 SHELL_DATA(stdout/stderr 标记流)。
type Data struct {
	Stream uint8 // StreamStdout / StreamStderr(stdin 方向不经此通道)
	Bytes  []byte
}

// StateEvent 是 SHELL_STATE 事件(spawn_failed / profile_missing)。
type StateEvent struct{ Code string }

// Conn 是一条已握手且收到 SHELL_BEGIN 的 shell pipe 连接。读侧由泵
// goroutine 独占;控制帧写由 writeMu 串行;Close 后数据通道关闭。
type Conn struct {
	conn  net.Conn
	Begin Begin

	writeMu sync.Mutex

	dataCh   chan Data
	stateCh  chan StateEvent
	exitCh   chan uint32
	done     chan struct{}
	doneOnce sync.Once

	mu      sync.Mutex
	closed  bool
	exitVal *uint32
	pumpErr error
}

// Dial 连接 xnc-shell pipe,完成握手并同步等待 SHELL_BEGIN(即 shell
// 已实际启动;spawn 失败形态 = STATE{spawn_failed/profile_missing} 后
// 随即 EXIT)。失败即关连接。
func Dial(pipe string, secret []byte) (*Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), handshakeTimeout+beginTimeout)
	defer cancel()
	conn, err := dialPipe(ctx, pipe)
	if err != nil {
		return nil, fmt.Errorf("shellpipe: dial %s: %w", pipe, err)
	}
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout + beginTimeout))
	if err := clientHandshake(conn, secret); err != nil {
		conn.Close()
		return nil, err
	}
	c := &Conn{
		conn:    conn,
		dataCh:  make(chan Data, dataChDepth),
		stateCh: make(chan StateEvent, stateChDepth),
		exitCh:  make(chan uint32, 1),
		done:    make(chan struct{}),
	}
	// BEGIN 窗口内同步读;STATE/EXIT 先到 = 启动失败,升格为 Dial 错误。
	for {
		f, err := ipc.ReadFrame(conn)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("shellpipe: no shell_begin: %w", err)
		}
		switch f.MessageType {
		case msgShellBegin:
			b, err := decodeBegin(f.Payload)
			if err != nil {
				conn.Close()
				return nil, err
			}
			c.Begin = b
			_ = conn.SetDeadline(time.Time{})
			go c.pump()
			return c, nil
		case msgShellState:
			ev, err := decodeState(f.Payload)
			if err != nil {
				conn.Close()
				return nil, err
			}
			select {
			case c.stateCh <- ev:
			default:
			}
		case msgShellExit:
			code, err := decodeExit(f.Payload)
			if err != nil {
				conn.Close()
				return nil, err
			}
			conn.Close()
			return nil, fmt.Errorf("shellpipe: shell exited before begin (code %d)", code)
		default:
			// 握手后的 PONG 等噪声:忽略。
		}
	}
}

// DataCh 交付 stdout/stderr 数据;连接终结后关闭。满时丢弃(会话层
// 转发写时限构成端到端背压,此处不放大内存)。
func (c *Conn) DataCh() <-chan Data { return c.dataCh }

// StateCh 交付 STATE 事件;连接终结后关闭。
func (c *Conn) StateCh() <-chan StateEvent { return c.stateCh }

// ExitCh 在收到 SHELL_EXIT 后交付一次退出码;连接终结后关闭(未收到
// EXIT 即终结时交付 1——xnc-shell 异常终态语义)。
func (c *Conn) ExitCh() <-chan uint32 { return c.exitCh }

// Done 在泵退出(连接终结)时关闭;Err 返回终结原因(主动 Close 为
// 传输错误 EOF 类,调用方不视作故障)。
func (c *Conn) Done() <-chan struct{} { return c.done }

func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pumpErr
}

// WriteStdin 发送 stdin 数据(0x0123 stream=0;交互 shell 键入)。
func (c *Conn) WriteStdin(p []byte) error {
	return c.writeCtrl(&ipc.Frame{MessageType: msgShellData, Payload: EncodeData(StreamStdin, p)})
}

// Resize 发送 0x0124 [u16 cols][u16 rows](服务端自校验区间)。
func (c *Conn) Resize(cols, rows int) error {
	if cols < 1 || rows < 1 || cols > 0xFFFF || rows > 0xFFFF {
		return fmt.Errorf("shellpipe: resize out of range %dx%d", cols, rows)
	}
	p := make([]byte, 4)
	binary.LittleEndian.PutUint16(p, uint16(cols))
	binary.LittleEndian.PutUint16(p[2:], uint16(rows))
	return c.writeCtrl(&ipc.Frame{MessageType: msgShellResize, Payload: p})
}

// Kill 发送 0x0126:xnc-shell 杀整棵进程树(killTree)后回 EXIT 终态。
func (c *Conn) Kill() error {
	return c.writeCtrl(&ipc.Frame{MessageType: msgShellKill})
}

// Close 关闭连接(对端视作断开 → 杀树);幂等。数据通道由泵关闭。
func (c *Conn) Close() error {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	return c.conn.Close()
}

var errClosed = errors.New("shellpipe: connection closed")

func (c *Conn) writeCtrl(f *ipc.Frame) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return errClosed
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(ctrlWriteTimeout))
	defer func() { _ = c.conn.SetWriteDeadline(time.Time{}) }()
	if err := ipc.WriteFrame(c.conn, f); err != nil {
		return fmt.Errorf("shellpipe: write %#04x: %w", f.MessageType, err)
	}
	return nil
}

// pump 是唯一读方:DATA → DataCh(满丢)、STATE → StateCh、EXIT →
// ExitCh(一次)后终结;读错误置 done 并关闭全部通道。
func (c *Conn) pump() {
	var exit *uint32
	for {
		f, err := ipc.ReadFrame(c.conn)
		if err != nil {
			c.mu.Lock()
			if !c.closed {
				c.pumpErr = err
			}
			c.mu.Unlock()
			c.teardown(exit)
			return
		}
		switch f.MessageType {
		case msgShellData:
			stream, data, err := decodeData(f.Payload)
			if err != nil {
				c.mu.Lock()
				c.pumpErr = err
				c.mu.Unlock()
				c.teardown(exit)
				return
			}
			if stream == StreamStdin {
				continue
			}
			select {
			case c.dataCh <- Data{Stream: stream, Bytes: data}:
			case <-c.done:
				c.teardown(exit)
				return
			default: // 满丢(见 DataCh 注释)
			}
		case msgShellState:
			ev, err := decodeState(f.Payload)
			if err != nil {
				continue
			}
			select {
			case c.stateCh <- ev:
			default:
			}
		case msgShellExit:
			code, err := decodeExit(f.Payload)
			if err == nil {
				exit = &code
				c.mu.Lock()
				c.exitVal = &code
				c.mu.Unlock()
				// EXIT 是终态:xnc-shell 随即关连接,读循环自然终结。
			}
		default:
			// PONG 等:忽略。
		}
	}
}

// teardown 是泵的下线路径:投递退出码(未见 EXIT 视作异常终态 1,
// 镜像 shellhost interactive 语义)、关闭 done 与全部数据通道。
func (c *Conn) teardown(exit *uint32) {
	code := uint32(1)
	if exit != nil {
		code = *exit
	}
	c.doneOnce.Do(func() { close(c.done) })
	select {
	case c.exitCh <- code:
	default:
	}
	close(c.dataCh)
	close(c.stateCh)
	close(c.exitCh)
}

// ---- 握手与 payload 编解码(shellhost/message.go 镜像) ----

// clientHandshake 是 M0 三步握手的发起方半边(镜像 coreclient.Dial)。
func clientHandshake(conn net.Conn, secret []byte) error {
	myNonce := ipc.NewNonce()
	if err := ipc.WriteFrame(conn, &ipc.Frame{
		MessageType: ipc.MsgHello,
		Payload:     ipc.EncodeHello(uint32(os.Getpid()), myNonce),
	}); err != nil {
		return fmt.Errorf("shellpipe: send hello: %w", err)
	}
	f, err := ipc.ReadFrame(conn)
	if err != nil {
		return fmt.Errorf("shellpipe: read hello_proof: %w", err)
	}
	if f.MessageType != ipc.MsgHelloProof {
		return fmt.Errorf("shellpipe: handshake: got message type %#x, want HELLO_PROOF", f.MessageType)
	}
	_, peerNonce, proofC, err := ipc.DecodeHelloProof(f.Payload)
	if err != nil {
		return fmt.Errorf("shellpipe: decode hello_proof: %w", err)
	}
	if !ipc.VerifyProof(secret, myNonce, proofC) {
		return errors.New("shellpipe: server proof rejected")
	}
	if err := ipc.WriteFrame(conn, &ipc.Frame{
		MessageType: ipc.MsgProof,
		Payload:     ipc.EncodeProof(ipc.Proof(secret, peerNonce)),
	}); err != nil {
		return fmt.Errorf("shellpipe: send proof: %w", err)
	}
	return nil
}

// ServerHandshake 应答方半边(测试 fake shell pipe 用;时序与
// shellhost/server.go 相同)。
func ServerHandshake(conn net.Conn, secret []byte) error {
	f, err := ipc.ReadFrame(conn)
	if err != nil {
		return fmt.Errorf("shellpipe: server handshake: read hello: %w", err)
	}
	if f.MessageType != ipc.MsgHello {
		return fmt.Errorf("shellpipe: server handshake: got %#x, want HELLO", f.MessageType)
	}
	_, clientNonce, err := ipc.DecodeHello(f.Payload)
	if err != nil {
		return err
	}
	myNonce := ipc.NewNonce()
	if err := ipc.WriteFrame(conn, &ipc.Frame{
		MessageType: ipc.MsgHelloProof,
		Payload: ipc.EncodeHelloProof(uint32(os.Getpid()), myNonce,
			ipc.Proof(secret, clientNonce)),
	}); err != nil {
		return err
	}
	f, err = ipc.ReadFrame(conn)
	if err != nil {
		return err
	}
	if f.MessageType != ipc.MsgProof {
		return fmt.Errorf("shellpipe: server handshake: got %#x, want PROOF", f.MessageType)
	}
	proof, err := ipc.DecodeProof(f.Payload)
	if err != nil {
		return err
	}
	if !ipc.VerifyProof(secret, myNonce, proof) {
		return errors.New("shellpipe: server handshake: client proof rejected")
	}
	return nil
}

// EncodeData 暴露 DATA 编码(fake 服务端测试用)。
func EncodeData(stream uint8, data []byte) []byte {
	p := make([]byte, 5+len(data))
	p[0] = stream
	binary.LittleEndian.PutUint32(p[1:], uint32(len(data)))
	copy(p[5:], data)
	return p
}

// EncodeBegin 暴露 BEGIN 编码(fake 服务端测试用)。
func EncodeBegin(cols, rows uint16, profile string) []byte {
	p := make([]byte, 4+profileFieldLen)
	binary.LittleEndian.PutUint16(p, cols)
	binary.LittleEndian.PutUint16(p[2:], rows)
	copy(p[4:], profile)
	return p
}

// EncodeExit 暴露 EXIT 编码(fake 服务端测试用)。
func EncodeExit(code uint32) []byte {
	p := make([]byte, 4)
	binary.LittleEndian.PutUint32(p, code)
	return p
}

func decodeBegin(p []byte) (Begin, error) {
	if len(p) != 4+profileFieldLen {
		return Begin{}, fmt.Errorf("shellpipe: begin payload %d bytes, want %d", len(p), 4+profileFieldLen)
	}
	return Begin{
		Cols:    binary.LittleEndian.Uint16(p),
		Rows:    binary.LittleEndian.Uint16(p[2:]),
		Profile: nulString(p[4:]),
	}, nil
}

func decodeData(p []byte) (uint8, []byte, error) {
	if len(p) < 5 {
		return 0, nil, fmt.Errorf("shellpipe: data payload %d bytes, want >= 5", len(p))
	}
	n := binary.LittleEndian.Uint32(p[1:])
	if uint64(len(p)) != 5+uint64(n) {
		return 0, nil, fmt.Errorf("shellpipe: data length mismatch: len field %d, payload %d", n, len(p))
	}
	return p[0], p[5:], nil
}

func decodeExit(p []byte) (uint32, error) {
	if len(p) != 4 {
		return 0, fmt.Errorf("shellpipe: exit payload %d bytes, want 4", len(p))
	}
	return binary.LittleEndian.Uint32(p), nil
}

func decodeState(p []byte) (StateEvent, error) {
	if len(p) != stateFieldLen {
		return StateEvent{}, fmt.Errorf("shellpipe: state payload %d bytes, want %d", len(p), stateFieldLen)
	}
	return StateEvent{Code: nulString(p)}, nil
}

func nulString(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}
