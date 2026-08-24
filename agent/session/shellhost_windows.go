//go:build windows

// shellhost_windows.go — ShellHost 的真实实现(M2-Slice2 Task 4):
// coreclient(core XNIP pipe)CreateShell(0x0120,wts=活动控制台哨兵,
// 令牌 kind 由 System 决定)→ shellpipe.Dial(xnc-shell pipe, secret)。
// 连接模型镜像 desktop/core_windows.go:进程内共享一条 core 连接
// (Ping 探活,死则重拨);凭据解析统一委托 desktop.ResolveCoreEndpoint
// (单一事实:env 链 XNC_CORE_* → XNC_DESKTOP_CORE_*,全缺回落 state-dir
// 服务约定),缺失 = CORE_UNAVAILABLE(dev 构建文档化行为:exec/shell
// 依赖运行中的 xnc-core)。
package session

import (
	"errors"
	"log/slog"
	"sync"

	"xnc/agent/coreclient"
	"xnc/agent/desktop"
	"xnc/agent/shellpipe"
)

// profileEnum 是白名单名 → 0x0120 profile 枚举(native/core 镜像)。
var profileEnum = map[string]uint8{
	"POWERSHELL": coreclient.ProfilePowershell,
	"PWSH":       coreclient.ProfilePwsh,
	"CMD":        coreclient.ProfileCmd,
	"BASH":       coreclient.ProfileBash,
}

// DefaultShellHost 按环境变量构造共享 core 连接的 ShellHost;凭据解析
// 委托 desktop.ResolveCoreEndpoint(stateDir="",不读盘)——env 链
// XNC_CORE_* 优先、回落 XNC_DESKTOP_CORE_*。缺失/损坏返回 nil(调用方
// 以 CORE_UNAVAILABLE 拒绝每次创建)。
func DefaultShellHost(log *slog.Logger) ShellHost {
	pipe, secret, err := desktop.ResolveCoreEndpoint("")
	if err != nil || len(secret) == 0 {
		return nil
	}
	return &coreShellHost{pipe: pipe, secret: secret, log: log}
}

// ShellHostFromStateDir 生产入口:env 链(同 DefaultShellHost)优先,
// 全缺则回落 XNCCore 服务约定(固定 pipe + <stateDir>/core-secret.hex,
// 与 desktop handler 同一凭据源)。失败返回 nil(CORE_UNAVAILABLE)。
// 2026-08-24 生产事故修复:此前缺 stateDir 回落,生产 exec/shell 全废。
func ShellHostFromStateDir(stateDir string, log *slog.Logger) ShellHost {
	pipe, secret, err := desktop.ResolveCoreEndpoint(stateDir)
	if err != nil || len(secret) == 0 {
		if log != nil {
			log.Warn("exec/shell: no core credentials (env unset, state-dir fallback failed)", "err", err)
		}
		return nil
	}
	return &coreShellHost{pipe: pipe, secret: secret, log: log}
}

// coreShellHost 镜像 desktop/coreStarter 的共享连接模型。
type coreShellHost struct {
	pipe   string
	secret []byte
	log    *slog.Logger

	mu     sync.Mutex
	client *coreclient.Client
}

func (h *coreShellHost) ensureClient() (*coreclient.Client, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.client != nil {
		if _, err := h.client.Ping(); err == nil {
			return h.client, nil
		}
		_ = h.client.Close()
		h.client = nil
	}
	c, err := coreclient.Dial(h.pipe, h.secret)
	if err != nil {
		return nil, err
	}
	h.client = c
	return c, nil
}

// CreateShell 经核心创建 shell 并接入其 pipe。
func (h *coreShellHost) CreateShell(spec ShellSpec) (ShellProc, error) {
	profile, ok := profileEnum[spec.Profile]
	if !ok {
		return nil, &ShellHostError{Code: "BAD_PAYLOAD"}
	}
	mode := coreclient.ModeOneshot
	if spec.Interactive {
		mode = coreclient.ModeInteractive
	}
	timeout := spec.TimeoutSec
	if timeout <= 0 {
		timeout = 300
	}
	kind := coreclient.TokenUser
	if spec.System {
		kind = coreclient.TokenSystem
	}
	c, err := h.ensureClient()
	if err != nil {
		if h.log != nil {
			h.log.Warn("shellhost: ensure core client failed", "err", err.Error())
		}
		return nil, &ShellHostError{Code: CodeCoreUnavailable}
	}
	pid, pipeName, shellSecret, err := c.CreateShell(coreclient.ShellCreateReq{
		WTS:        coreclient.WTSActiveConsole,
		TokenKind:  kind,
		Profile:    profile,
		Mode:       mode,
		Cols:       uint16(spec.Cols),
		Rows:       uint16(spec.Rows),
		Cwd:        spec.Cwd,
		Env:        spec.Env,
		Cmd:        spec.Command,
		TimeoutSec: uint32(timeout),
	})
	if err != nil {
		var rej *coreclient.RejectedError
		if errors.As(err, &rej) {
			return nil, &ShellHostError{Code: rej.Code}
		}
		// 连接级故障(T5 定向修):不无条件收线共享连接——单次超时
		// (连接仍健康)时硬 Close 会把并发在飞 RPC 打成伪
		// CORE_UNAVAILABLE。改为 Ping 探活:确实已死才丢弃引用
		// (在飞 RPC 已/将收到真实读错误),健康则保留,下次复用。
		if _, perr := c.Ping(); perr != nil {
			_ = c.Close()
			h.mu.Lock()
			if h.client == c {
				h.client = nil
			}
			h.mu.Unlock()
		}
		return nil, &ShellHostError{Code: CodeCoreUnavailable}
	}
	conn, err := shellpipe.Dial(pipeName, shellSecret)
	if err != nil {
		if h.log != nil {
			h.log.Warn("shellhost: dial shell pipe failed", "pipe", pipeName, "err", err.Error())
		}
		// 孤儿 shell:按存储 handle 让核心收尸(幂等)。
		if kerr := c.KillShell(pid); kerr != nil && h.log != nil {
			h.log.Warn("shellhost: orphan kill_shell failed", "pid", pid)
		}
		return nil, &ShellHostError{Code: CodeCoreUnavailable}
	}
	if !spec.Interactive {
		// oneshot(exec):启用每流 4MB 输出预算(T5 截断诚实化)。
		conn.SetExecBudget(0)
	}
	return &coreShellProc{host: h, conn: conn, pid: pid}, nil
}

// coreShellProc 适配 shellpipe.Conn → session.ShellProc。
type coreShellProc struct {
	host *coreShellHost
	conn *shellpipe.Conn
	pid  uint32
}

func (p *coreShellProc) Profile() string { return p.conn.Begin.Profile }

func (p *coreShellProc) WriteStdin(b []byte) error { return p.conn.WriteStdin(b) }

func (p *coreShellProc) Resize(cols, rows int) error { return p.conn.Resize(cols, rows) }

// Kill 双保险:0x0126(xnc-shell 杀树)+ 核心 KillShell(按存储
// handle,幂等)——pipe 断连时后者兜底。
func (p *coreShellProc) Kill() error {
	kerr := p.conn.Kill()
	if c, err := p.host.ensureClient(); err == nil {
		_ = c.KillShell(p.pid)
	}
	return kerr
}

func (p *coreShellProc) Close() error { return p.conn.Close() }

// Stream 转发 DataCh(stdout/stderr 标记)为 ShellStream。
func (p *coreShellProc) Stream() <-chan ShellStream {
	out := make(chan ShellStream)
	go func() {
		defer close(out)
		for d := range p.conn.DataCh() {
			out <- ShellStream{Stderr: d.Stream == shellpipe.StreamStderr, Bytes: d.Bytes}
		}
	}()
	return out
}

func (p *coreShellProc) Exit() <-chan uint32 { return p.conn.ExitCh() }

// Dropped 返回 oneshot 路径超预算丢弃字节数(interactive 恒 0)。
func (p *coreShellProc) Dropped() uint64 { return p.conn.DroppedBytes() }

var _ ShellProc = (*coreShellProc)(nil)
var _ ShellHost = (*coreShellHost)(nil)

// Snapshot 经共享 core 连接发 0x0111 快照请求(M2-Slice3 Task 3:screen
// 退役换轨;wts=活动控制台哨兵,核心解析)。拒绝码透传为 ShellHostError
// 形态,传输层错误并入 CORE_UNAVAILABLE。
func (h *coreShellHost) Snapshot(maxWidth uint32) ([]byte, error) {
	c, err := h.ensureClient()
	if err != nil {
		return nil, &ShellHostError{Code: CodeCoreUnavailable}
	}
	jpeg, err := c.Snapshot(coreclient.WTSActiveConsole, maxWidth)
	if err != nil {
		var rej *coreclient.RejectedError
		if errors.As(err, &rej) {
			return nil, &ShellHostError{Code: rej.Code}
		}
		return nil, &ShellHostError{Code: CodeCoreUnavailable}
	}
	return jpeg, nil
}

// defaultSnapshotProvider 构造默认快照通路(与 shellhost 同源的共享 core
// 连接模型);dev 凭据缺失返回 CORE_UNAVAILABLE 拒绝器。
func defaultSnapshotProvider(log *slog.Logger) SnapshotProvider {
	if h := DefaultShellHost(log); h != nil {
		if csh, ok := h.(*coreShellHost); ok {
			return csh
		}
	}
	return unavailableSnapshot{}
}

type unavailableSnapshot struct{}

func (unavailableSnapshot) Snapshot(uint32) ([]byte, error) {
	return nil, &ShellHostError{Code: CodeCoreUnavailable}
}
