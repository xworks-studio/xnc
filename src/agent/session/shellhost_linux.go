//go:build linux

// shellhost_linux.go — Linux 的 ShellHost 实现（direct 宿主）：agent(root)
// 直接 fork/exec + PTY，不经 xnc-core/xnc-shell 间接层——那是 Windows
// session-0/用户令牌问题的解法，Linux 服务进程的 root 身份即执行身份
// （对应 Windows system:true 语义；per-session 用户切换留作后续增强）。
// 杀树 = Setsid/Setpgid 独立进程组 + 负 PID 组杀；oneshot 与 Windows 版
// 同语义：stdout/stderr 分流、每流 4MB 输出预算（超限丢弃+marker+Truncated）、
// 宿主侧同参超时杀树。接口契约见 shellhost.go；ledger 裁决移除的是
// Windows 直连 spawn（令牌语义），Linux 直连不适用该裁决。
package session

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// ShellHostFromStateDir Linux 生产入口：direct 宿主无凭据（无 core 概念），
// stateDir 仅保持签名对齐（被忽略）。
func ShellHostFromStateDir(_ string, log *slog.Logger) ShellHost {
	return &directShellHost{log: log}
}

// DefaultShellHost Linux 与生产入口同源（无 dev core 凭据链）。
func DefaultShellHost(log *slog.Logger) ShellHost { return &directShellHost{log: log} }

// defaultSnapshotProvider Linux：快照通道已退役，恒 CORE_UNAVAILABLE
// （屏幕采集仅 Windows 形态）。
func defaultSnapshotProvider(_ string, _ *slog.Logger) SnapshotProvider {
	return unavailableSnapshot{}
}

type unavailableSnapshot struct{}

func (unavailableSnapshot) Snapshot(uint32) ([]byte, error) {
	return nil, &ShellHostError{Code: CodeCoreUnavailable}
}

// oneshot 每流输出预算（stdout/stderr 各自），与 Windows xnc-shell 对齐。
const oneshotBudgetPerStream = 4 * 1024 * 1024

// directShellHost 直接创建 shell 进程（无 core 往返）。
type directShellHost struct {
	log *slog.Logger
}

func (h *directShellHost) logger() *slog.Logger {
	if h.log != nil {
		return h.log
	}
	return slog.Default()
}

// profileExe 白名单 profile → 可执行名（Linux 词表；CMD 为 Windows 专属）。
func profileExe(profile string) (string, error) {
	switch profile {
	case "BASH":
		return "bash", nil
	case "SH":
		return "sh", nil
	case "ZSH":
		return "zsh", nil
	case "PWSH":
		return "pwsh", nil
	case "POWERSHELL":
		return "powershell", nil
	}
	return "", &ShellHostError{Code: "BAD_PAYLOAD"}
}

func (h *directShellHost) CreateShell(spec ShellSpec) (ShellProc, error) {
	exe, err := profileExe(spec.Profile)
	if err != nil {
		return nil, err
	}
	if _, err := exec.LookPath(exe); err != nil {
		h.logger().Warn("shellhost: profile exe unavailable", "profile", spec.Profile, "exe", exe)
		return nil, &ShellHostError{Code: "SPAWN_FAILED"}
	}
	if spec.Interactive {
		return newInteractiveProc(h.logger(), spec, exe)
	}
	if spec.Command == "" {
		return nil, &ShellHostError{Code: "BAD_PAYLOAD"}
	}
	return newOneshotProc(h.logger(), spec, exe)
}

// ---- 子进程环境 ----

// childEnv 构造子进程环境：服务最小环境 + 登录语义缺省（HOME/USER/TERM，
// systemd 服务默认不带）+ PATH 兜底 + spec.Env 显式覆盖（最后生效）。
func childEnv(extra []string) []string {
	env := os.Environ()
	home, name := "/root", "root"
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		home, name = u.HomeDir, u.Username
	}
	env = append(env,
		"HOME="+home,
		"USER="+name,
		"LOGNAME="+name,
		"TERM=xterm-256color",
	)
	if !hasEnvKey(env, "PATH") {
		env = append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	return append(env, extra...)
}

func hasEnvKey(env []string, key string) bool {
	for _, kv := range env {
		if strings.HasPrefix(kv, key+"=") {
			return true
		}
	}
	return false
}

// ---- ShellProc 实现（interactive 与 oneshot 共用载体）----

// directProc 一个直接创建的 shell 进程。interactive 持 ptmx；oneshot 持
// stdout/stderr 双管道。done 在 Close 时关闭，解阻塞所有内部 goroutine
// （消费方（会话泵）停止读取后的收线保障）。
type directProc struct {
	profile string
	log     *slog.Logger
	ptmx    *os.File // interactive 专用（oneshot 为 nil）
	cmd     *exec.Cmd

	streams chan ShellStream
	exits   chan uint32
	done    chan struct{}

	timer     *time.Timer // oneshot 宿主侧超时
	mu        sync.Mutex
	dropped   uint64
	closeOnce sync.Once
	strOnce   sync.Once
}

// newInteractiveProc：pty + 登录 shell。pty.Start* 自设 Setsid+Setctty，
// 子进程即新会话/进程组首——负 PID 组杀覆盖整棵树。
func newInteractiveProc(log *slog.Logger, spec ShellSpec, exe string) (ShellProc, error) {
	cmd := exec.Command(exe)
	// argv0 前导 '-' = 登录 shell 语义：加载 /etc/profile 与 ~/.bash_profile，
	// 远程终端贴近 ssh 登录体验。
	cmd.Args[0] = "-" + exe
	cmd.Env = childEnv(spec.Env)
	if spec.Cwd != "" {
		cmd.Dir = spec.Cwd
	}
	ws := &pty.Winsize{Cols: uint16(clampDim(spec.Cols)), Rows: uint16(clampDim(spec.Rows))}
	ptmx, err := pty.StartWithSize(cmd, ws)
	if err != nil {
		log.Warn("shellhost: pty start failed", "profile", spec.Profile, "err", err)
		return nil, &ShellHostError{Code: "SPAWN_FAILED"}
	}
	p := &directProc{
		profile: spec.Profile, log: log, ptmx: ptmx, cmd: cmd,
		streams: make(chan ShellStream, 64), exits: make(chan uint32, 1), done: make(chan struct{}),
	}
	var pumps sync.WaitGroup
	pumps.Add(1)
	go func() { defer pumps.Done(); p.pumpPty() }()
	go p.supervise(&pumps)
	return p, nil
}

// newOneshotProc：`<exe> -c <command>`，独立进程组（组杀不波及 agent），
// stdout/stderr 分流 + 每流预算 + 宿主侧同参超时杀树（exec 会话另有
// 等值定时器驱动 TimedOut 上报，宿主侧是纵深防御）。
func newOneshotProc(log *slog.Logger, spec ShellSpec, exe string) (ShellProc, error) {
	cmd := exec.Command(exe, "-c", spec.Command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = childEnv(spec.Env)
	if spec.Cwd != "" {
		cmd.Dir = spec.Cwd
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, &ShellHostError{Code: "SPAWN_FAILED"}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, &ShellHostError{Code: "SPAWN_FAILED"}
	}
	if err := cmd.Start(); err != nil {
		log.Warn("shellhost: oneshot start failed", "profile", spec.Profile, "err", err)
		return nil, &ShellHostError{Code: "SPAWN_FAILED"}
	}
	p := &directProc{
		profile: spec.Profile, log: log, cmd: cmd,
		streams: make(chan ShellStream, 64), exits: make(chan uint32, 1), done: make(chan struct{}),
	}
	var pumps sync.WaitGroup
	pumps.Add(2)
	go func() { defer pumps.Done(); p.pumpStream(stdout, "stdout", false) }()
	go func() { defer pumps.Done(); p.pumpStream(stderr, "stderr", true) }()
	go p.supervise(&pumps)
	if spec.TimeoutSec > 0 {
		p.timer = time.AfterFunc(time.Duration(spec.TimeoutSec)*time.Second, func() {
			p.killGroup()
		})
	}
	return p, nil
}

func (p *directProc) Profile() string { return p.profile }

// WriteStdin 仅 interactive 有意义（oneshot stdin 为 /dev/null）。
func (p *directProc) WriteStdin(b []byte) error {
	if p.ptmx == nil {
		return errors.New("shellhost: stdin unavailable in oneshot mode")
	}
	_, err := p.ptmx.Write(b)
	return err
}

// Resize 经 TIOCSWINSZ 调整 pty 尺寸；内核向前台进程组投递 SIGWINCH，
// 交互应用（vim/top）自行跟随。
func (p *directProc) Resize(cols, rows int) error {
	if p.ptmx == nil {
		return errors.New("shellhost: resize unavailable in oneshot mode")
	}
	return pty.Setsize(p.ptmx, &pty.Winsize{Cols: uint16(clampDim(cols)), Rows: uint16(clampDim(rows))})
}

// Kill 组杀（SIGKILL 到 -pid）：interactive 的 pty 会话树、oneshot 的
// 进程组一并终结；进程已退出时 ESRCH 静默（幂等 no-op 语义）。
func (p *directProc) Kill() error {
	p.killGroup()
	return nil
}

func (p *directProc) killGroup() {
	if p.cmd.Process == nil {
		return
	}
	// 负 PID = 整组；ESRCH（已退出）忽略。
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	_ = p.cmd.Process.Kill() // 兜底：子进程自身 setsid 逃组时的直接终结
}

func (p *directProc) Stream() <-chan ShellStream { return p.streams }
func (p *directProc) Exit() <-chan uint32        { return p.exits }

func (p *directProc) Dropped() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dropped
}

// Close 收线：解阻塞内部 goroutine、停超时定时器、关 ptmx。幂等。
func (p *directProc) Close() error {
	p.closeOnce.Do(func() {
		close(p.done)
		if p.timer != nil {
			p.timer.Stop()
		}
		if p.ptmx != nil {
			_ = p.ptmx.Close()
		}
		p.closeStreams()
	})
	return nil
}

func (p *directProc) closeStreams() {
	p.strOnce.Do(func() { close(p.streams) })
}

// pumpPty interactive：pty 主设备 → streams（EOF/EIO = 子进程退出）。
func (p *directProc) pumpPty() {
	defer p.closeStreams()
	buf := make([]byte, 32*1024)
	for {
		n, err := p.ptmx.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			select {
			case p.streams <- ShellStream{Bytes: chunk}:
			case <-p.done:
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// pumpStream oneshot：单流读入 streams，超预算丢弃计数（读端持续排空，
// 不让子进程阻塞在写管道上），EOF 时按需补 stderr marker 行。
func (p *directProc) pumpStream(r io.Reader, name string, isStderr bool) {
	buf := make([]byte, 32*1024)
	remaining := int64(oneshotBudgetPerStream)
	var dropped int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			take := min(int64(n), remaining)
			if take > 0 {
				remaining -= take
				chunk := make([]byte, take)
				copy(chunk, buf[:take])
				select {
				case p.streams <- ShellStream{Stderr: isStderr, Bytes: chunk}:
				case <-p.done:
					return
				}
			}
			if int64(n) > take {
				dropped += int64(n) - take
			}
		}
		if err != nil {
			break
		}
	}
	if dropped > 0 {
		p.mu.Lock()
		p.dropped += uint64(dropped)
		p.mu.Unlock()
		marker := fmt.Sprintf("\n[xnc] %s output over budget, %d bytes dropped\n", name, dropped)
		select {
		case p.streams <- ShellStream{Stderr: true, Bytes: []byte(marker)}:
		case <-p.done:
		}
	}
}

// supervise 输出泵的监督收尾：先等全部泵排空（cmd.Wait 会关闭 StdoutPipe
// 读端，未读完的数据会随 fd 关闭丢失——必须先读完再收割），再 Wait 收割
// 子进程、关流、投递退出码。泵在消费方停读且未 Close 时可能阻塞在发送：
// 会话两侧（exec/shell）保证终态调用 Close 解锁。
func (p *directProc) supervise(pumps *sync.WaitGroup) {
	pumps.Wait()
	code := exitCodeOf(p.cmd.Wait())
	p.closeStreams()
	select {
	case p.exits <- code:
	case <-p.done:
	}
}

func exitCodeOf(err error) uint32 {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if st, ok := ee.Sys().(syscall.WaitStatus); ok && st.Signaled() {
			return uint32(128 + int(st.Signal()))
		}
		if c := ee.ExitCode(); c >= 0 {
			return uint32(c)
		}
	}
	return 1
}

// clampDim 会话侧几何校验的兜底（shell.go 已限 1..1000，此处再夹一次防
// 直接构造 ShellSpec 的调用方传 0/越界）。
func clampDim(v int) int {
	if v < 1 {
		return 1
	}
	if v > shellMaxDim {
		return shellMaxDim
	}
	return v
}
