// oneshot.go — --mode oneshot:按 profile 构造命令行(command.go,与
// agent/session exec 行为对齐)捕获 stdout/stderr 为 SHELL_DATA 标记流,
// 终态 SHELL_EXIT(exit code);超时(--timeout)与 SHELL_KILL 都杀整棵
// 进程树(killTree,taskkill /T /F 语义)。
package main

import (
	"net"
	"time"

	"xnc/proto/ipc"
)

const (
	oneshotDefaultTimeout = 300 * time.Second
	dataWriteTimeout      = 10 * time.Second
)

// runOneshot 在一条已握手的连接上执行单命令直至终态。
func runOneshot(conn net.Conn, o *serverOpts) error {
	defer conn.Close()

	// SHELL_BEGIN 先于任何输出帧(协议契约,镜像 interactive;T5 修:
	// 此前 oneshot 不发 BEGIN,等 BEGIN 的 agent/shellpipe.Dial 会把
	// 快命令的 EXIT 误判为「shell exited before begin」)。
	_ = writeFrame(conn, &ipc.Frame{MessageType: msgShellBegin, Payload: encodeBegin(0, 0, o.profile)})

	cmd, err := buildOneshotCommand(o.profile, o.exe, o.command, o.env)
	if err != nil {
		_ = writeFrame(conn, &ipc.Frame{MessageType: msgShellState, Payload: encodeState(stateProfileMissing)})
		return err
	}
	if o.cwd != "" {
		cmd.Dir = o.cwd
	}
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		o.logger().Warn("shellhost oneshot start failed", "err", err)
		_ = writeFrame(conn, &ipc.Frame{MessageType: msgShellState, Payload: encodeState(stateSpawnFailed)})
		_ = writeFrame(conn, &ipc.Frame{MessageType: msgShellExit, Payload: encodeExit(1)})
		return nil
	}

	// kill 请求 / 连接断开 → 杀树。读侧 goroutine 只消费 SHELL_KILL,
	// stdin 流在此模式无 stdin 可写(命令内联,无交互输入)。
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			f, err := ipc.ReadFrame(conn)
			if err != nil {
				killTree(cmd.Process.Pid)
				return
			}
			if f.MessageType == msgShellKill {
				killTree(cmd.Process.Pid)
				return
			}
		}
	}()

	// 超时 → 杀树(镜像 agent/session exec 的 timer 语义)。
	deadline := time.Duration(o.timeout) * time.Second
	if o.timeout <= 0 {
		deadline = oneshotDefaultTimeout
	}
	timer := time.AfterFunc(deadline, func() { killTree(cmd.Process.Pid) })
	defer timer.Stop()

	// 输出泵:StdoutPipe 语义要求先排空再 Wait(否则末段截断)。
	pumpDone := make(chan struct{}, 2)
	pump := func(r interface{ Read([]byte) (int, error) }, stream uint8) {
		go func() {
			defer func() { pumpDone <- struct{}{} }()
			buf := make([]byte, 32*1024)
			for {
				n, err := r.Read(buf)
				if n > 0 {
					_ = conn.SetWriteDeadline(time.Now().Add(dataWriteTimeout))
					if we := writeFrame(conn, &ipc.Frame{
						MessageType: msgShellData, Payload: encodeData(stream, buf[:n]),
					}); we != nil {
						killTree(cmd.Process.Pid)
						return
					}
				}
				if err != nil {
					return
				}
			}
		}()
	}
	pump(stdout, streamStdout)
	pump(stderr, streamStderr)

	// StdoutPipe 语义:先排空 pump 再 Wait(否则末段输出截断)。
	waitCh := make(chan error, 1)
	go func() {
		<-pumpDone
		<-pumpDone
		waitCh <- cmd.Wait()
	}()
	<-waitCh
	exitCode := uint32(1)
	if cmd.ProcessState != nil {
		exitCode = uint32(cmd.ProcessState.ExitCode())
	}
	_ = conn.SetWriteDeadline(time.Now().Add(dataWriteTimeout))
	_ = writeFrame(conn, &ipc.Frame{MessageType: msgShellExit, Payload: encodeExit(exitCode)})
	return nil
}
