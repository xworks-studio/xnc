// main.go — xnc-shell.exe 入口(M2-Slice2 Task 2,spec §8)。
//
// xnc-core 以用户令牌或 SYSTEM@会话令牌 spawn 本进程;profile 白名单
// 自解析(PWSH/BASH 缺失 → exit 2);pipe secret 经 --secret-stdin 从
// stdin 读,oneshot 命令同样经 stdin(--command-stdin,secret 行之后的
// u32LE 长度前缀帧)——命令行零敏感字段,且 argv 无法无损承载任意用户
// 命令(内嵌引号/尾反斜杠会被 core 的 BuildChildCommandLine 拒绝,
// 2026-09-14 静默 243 事故)。进程以 spawn 时的令牌运行,子进程
// (ConPTY/oneshot)自然继承该令牌语义。
//
// 用法:
//
//	xnc-shell.exe --pipe \\.\pipe\xnc-shell-<pid> --secret-stdin
//	              --profile POWERSHELL|PWSH|CMD|BASH
//	              --mode interactive|oneshot
//	              [--cols N --rows N] [--cwd DIR] [--env K=V]...
//	              [--command-stdin] [--timeout SEC] [--log-file PATH]
//
// 退出码:0 正常终态;1 使用/运行错误;2 profile 缺失(PWSH/BASH 探测失败)。
package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
)

func main() {
	os.Exit(run())
}

func run() int {
	var (
		pipe       = flag.String("pipe", "", "pipe name to listen on (required)")
		secretIn   = flag.Bool("secret-stdin", false, "read 64-hex pipe secret from stdin")
		profileArg = flag.String("profile", "", "shell profile whitelist: POWERSHELL, PWSH, CMD, BASH")
		mode       = flag.String("mode", "", "interactive | oneshot")
		cols       = flag.Int("cols", 0, "initial pty columns (interactive)")
		rows       = flag.Int("rows", 0, "initial pty rows (interactive)")
		cwd        = flag.String("cwd", "", "working directory for the child")
		commandIn  = flag.Bool("command-stdin", false, "read oneshot command from stdin (u32LE length frame after the secret line)")
		timeout    = flag.Int("timeout", 0, "oneshot timeout seconds (0 = default 300)")
		logFile    = flag.String("log-file", "", "also append log output to this file (service spawns have no console)")
	)
	flag.Var(&envFlag, "env", "environment variable K=V (repeatable)")
	flag.Parse()

	if *logFile != "" {
		// 服务模式(无 console,继承 stdio 不可靠):xnc-core spawn 时传
		// --log-file,日志双写 stderr(console 模式可见)+ 文件(与 desktop
		// 同一单一日志通道,2026-08-24 可观测性事故跟进)。懒打开:首次写
		// 日志才创建文件——健康会话不产生 0 字节 xnc-shell.log;文件打不
		// 开不致命,回落 stderr 通道,console 模式日志不丢。
		lw := &logFileWriter{path: *logFile}
		defer lw.Close()
		h := slog.NewTextHandler(io.MultiWriter(os.Stderr, lw), nil)
		slog.SetDefault(slog.New(h))
	}

	if *pipe == "" || !*secretIn || *profileArg == "" || (*mode != "interactive" && *mode != "oneshot") {
		fmt.Fprintln(os.Stderr, "usage: xnc-shell.exe --pipe <name> --secret-stdin --profile <POWERSHELL|PWSH|CMD|BASH> --mode <interactive|oneshot> [...]")
		return 1
	}

	// secret 行与命令帧共用一个 bufio 读端:帧字节可能与 secret 行同批
	// 抵达,逐块裸读会吞掉换行之后的内容。
	br := bufio.NewReader(os.Stdin)
	secret, err := readSecretStdin(br)
	if err != nil {
		fmt.Fprintf(os.Stderr, "xnc-shell: %v\n", err)
		return 1
	}
	var command string
	if *commandIn {
		command, err = readCommandFrame(br)
		if err != nil {
			fmt.Fprintf(os.Stderr, "xnc-shell: %v\n", err)
			return 1
		}
	}
	if *mode == "oneshot" && command == "" {
		fmt.Fprintln(os.Stderr, "xnc-shell: --mode oneshot requires a command (--command-stdin frame empty)")
		return 1
	}

	profile, err := normalizeProfile(*profileArg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "xnc-shell: %v\n", err)
		return 1
	}
	exe, err := resolveProfile(profile)
	if err != nil {
		// PWSH/BASH 缺失:稳定 exit 2 + 明确信息(core/agent 侧可区分)。
		fmt.Fprintf(os.Stderr, "xnc-shell: %v\n", err)
		return 2
	}

	o := &serverOpts{
		pipe: *pipe, secret: secret, profile: profile, exe: exe, mode: *mode,
		cols: *cols, rows: *rows, cwd: *cwd, env: []string(envFlag), command: command,
		timeout: *timeout,
	}
	return serve(o)
}

// envListFlag 可重复的 --env K=V。
type envListFlag []string

func (e *envListFlag) String() string { return strings.Join(*e, ";") }
func (e *envListFlag) Set(v string) error {
	*e = append(*e, v)
	return nil
}

var envFlag envListFlag

// serve 监听并逐连接服务(串行单实例,镜像 core console pipe 模型)。
func serve(o *serverOpts) int {
	ln, err := listenNetPipe(o.pipe, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "xnc-shell: %v\n", err)
		return 1
	}
	defer ln.Close()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return 1
		}
		if err := serveConn(conn, o); err != nil {
			o.logger().Warn("shellhost connection ended", "err", err)
		}
		// oneshot 是一次性契约:命令终态(EXIT / 断连 / KILL)即使命结束,
		// 继续回到 accept 只会变成无人认领的僵尸(真机踩坑:正常完成的
		// exec 都泄漏一个 xnc-shell——agent 正常路径只 Close 断管不杀
		// 进程,Kill 路径才有 core KillShell 兜底)。interactive 维持
		// 单连接串行模型(agent 四条收线路径均显式 Kill)。
		if o.mode == "oneshot" {
			return 0
		}
	}
}

// readSecretStdin 镜像 xnc-desktop 的 --secret-stdin 语义:恰好 64 hex
// 字符(32 字节),可选尾部 "\n"/"\r\n"(或无换行)。首行经 bufio 读取:
// secret 行之后 stdin 还可能跟随 oneshot 命令帧,换行后的字节必须留在
// 缓冲里交给 readCommandFrame(旧实现按块裸读,同批抵达的帧字节会被
// 连带吞掉)。
func readSecretStdin(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("--secret-stdin: reading stdin failed: %w", err)
	}
	// 剥一个可选尾部换行对;EOF 无换行同视,交给长度校验。
	if n := len(line); n >= 2 && line[n-2] == '\r' && line[n-1] == '\n' {
		line = line[:n-2]
	} else if n := len(line); n >= 1 && (line[n-1] == '\n' || line[n-1] == '\r') {
		line = line[:n-1]
	}
	if len(line) != 64 {
		return nil, fmt.Errorf("--secret-stdin: expected exactly 64 hex chars (32 bytes), optional trailing newline")
	}
	out := make([]byte, 32)
	for i := 0; i < 64; i += 2 {
		hi, ok1 := hexNibble(line[i])
		lo, ok2 := hexNibble(line[i+1])
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("--secret-stdin: expected exactly 64 hex chars (32 bytes), optional trailing newline")
		}
		out[i/2] = hi<<4 | lo
	}
	return out, nil
}

// readCommandFrame 读 oneshot 命令帧:u32 LE 字节长度 + UTF-8 命令。
// 长度上界 0xFFFF 对齐 core 解码器(0x0120 payload 的 u16 长度域);
// 超界或半截帧即报错退出——绝不猜着执行残缺命令(引号事故的教训:
// 静默改坏命令比失败更危险)。
func readCommandFrame(r io.Reader) (string, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return "", fmt.Errorf("--command-stdin: reading length: %w", err)
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n > 0xFFFF {
		return "", fmt.Errorf("--command-stdin: frame length %d exceeds 64KB bound", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("--command-stdin: reading %d command bytes: %w", n, err)
	}
	return string(buf), nil
}

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// logFileWriter 懒打开 --log-file:首次写日志时才创建文件
// (健康会话不产生 0 字节 xnc-shell.log)。打开失败:一次性提示到
// stderr,之后的记录照常经 MultiWriter 落到 stderr(与启动即开的
// 既有降级语义一致)。
type logFileWriter struct {
	mu     sync.Mutex
	path   string
	f      *os.File
	failed bool
}

func (w *logFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil && !w.failed {
		f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			w.failed = true
			fmt.Fprintf(os.Stderr, "xnc-shell: --log-file %s: %v (continuing on stderr only)\n", w.path, err)
			return len(p), nil // 吞掉本记录:stderr 已被 MultiWriter 写入
		}
		w.f = f
	}
	if w.f != nil {
		return w.f.Write(p)
	}
	return len(p), nil
}

// Close 关闭已打开的日志文件(未打开过则无操作)。
func (w *logFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		err := w.f.Close()
		w.f = nil
		return err
	}
	return nil
}
