//go:build windows

// interactive.go — --mode interactive:ConPTY 承载交互 shell(移植自
// agent/session/shell.go 的交互循环),SHELL_BEGIN 先于任何输出;
// pty 输出 → SHELL_DATA(stdout);SHELL_DATA(stdin) → pty 输入;
// SHELL_RESIZE 实时生效;SHELL_KILL / 对端断开 / shell 退出 →
// KillAndClose(杀树)+ SHELL_EXIT 终态。
package main

import (
	"net"
	"time"

	"xnc/proto/ipc"
)

const (
	interactiveDefaultCols = 120
	interactiveDefaultRows = 30
	interactiveMaxDim      = 1000
)

// runInteractive 在一条已握手的连接上承载交互 shell 直至终态。
func runInteractive(conn net.Conn, o *serverOpts) error {
	defer conn.Close()

	cols, rows := o.cols, o.rows
	if cols < 1 || cols > interactiveMaxDim {
		cols = interactiveDefaultCols
	}
	if rows < 1 || rows > interactiveMaxDim {
		rows = interactiveDefaultRows
	}

	// 启动参数镜像 agent/session/shell.go:-NoLogo;pwsh/powershell 加
	// -NoExit -Command 主机名提示符(不经交互行编辑器,无回显)。
	args := []string{"-NoLogo"}
	if o.profile == "PWSH" || o.profile == "POWERSHELL" {
		args = append(args, "-NoExit", "-Command",
			"function global:prompt { '[' + $env:COMPUTERNAME + '] PS ' + $executionContext.SessionState.Path.CurrentLocation + '> ' }")
	}
	pty, err := startConPTY(cols, rows, o.exe, args...)
	if err != nil {
		o.logger().Warn("shellhost conpty start failed", "err", err)
		_ = writeFrame(conn, &ipc.Frame{MessageType: msgShellState, Payload: encodeState(stateSpawnFailed)})
		_ = writeFrame(conn, &ipc.Frame{MessageType: msgShellExit, Payload: encodeExit(1)})
		return nil
	}
	defer pty.KillAndClose()

	// SHELL_BEGIN 先于任何输出帧。
	_ = conn.SetWriteDeadline(time.Now().Add(dataWriteTimeout))
	if err := writeFrame(conn, &ipc.Frame{
		MessageType: msgShellBegin, Payload: encodeBegin(uint16(cols), uint16(rows), o.profile),
	}); err != nil {
		return nil
	}

	done := make(chan struct{}) // pty 输出泵退出
	go func() { // pty → SHELL_DATA(stdout)
		defer close(done)
		buf := make([]byte, 32*1024)
		for {
			n, err := pty.outR.Read(buf)
			if n > 0 {
				_ = conn.SetWriteDeadline(time.Now().Add(dataWriteTimeout))
				if we := writeFrame(conn, &ipc.Frame{
					MessageType: msgShellData, Payload: encodeData(streamStdout, buf[:n]),
				}); we != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	gone := make(chan struct{}) // 读侧(对端断开/kill)
	go func() {
		defer close(gone)
		for {
			f, err := ipc.ReadFrame(conn)
			if err != nil {
				return
			}
			switch f.MessageType {
			case msgShellData:
				stream, data, err := decodeData(f.Payload)
				if err != nil || stream != streamStdin || len(data) == 0 {
					continue
				}
				if _, err := pty.inW.Write(data); err != nil {
					return
				}
			case msgShellResize:
				c, r, err := decodeResize(f.Payload)
				if err == nil && c > 0 && c <= interactiveMaxDim && r > 0 && r <= interactiveMaxDim {
					_ = pty.Resize(int(c), int(r))
				}
			case msgShellKill:
				return // KillAndClose(杀树)由 defer 收尾
			}
		}
	}()

	waitCh := make(chan error, 1)
	go func() { waitCh <- pty.Wait() }()
	killed := false
	select {
	case <-waitCh: // shell 退出(exit 命令):捕获退出码
	case <-done: // 输出流结束(pty 关闭;Wait/KillAndClose 兜底收割)
	case <-gone: // 对端断开 / KILL:杀树语义
		killed = true
	}
	// 先收尾(KillAndClose 幂等,内含 5s 进程句柄等待),再上报终态:
	// 杀树后的 GetExitCodeProcess 才可见。被杀路径(killed)落定的退出码
	// 为 0 时不可信 —— force 非零;自然退出(含 0)不受影响(T2 评审修复:
	// 被杀的 shell 绝不上报 exit 0)。
	pty.KillAndClose()
	var exitCode uint32
	if pty.exitCode != nil {
		exitCode = *pty.exitCode
	} else {
		exitCode = 1 // 未捕获到退出码:视为异常终态
	}
	if killed && exitCode == 0 {
		exitCode = 1
	}
	_ = conn.SetWriteDeadline(time.Now().Add(dataWriteTimeout))
	_ = writeFrame(conn, &ipc.Frame{MessageType: msgShellExit, Payload: encodeExit(exitCode)})
	return nil
}
