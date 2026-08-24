//go:build windows

// core_windows.go — Starter 的真实实现(M1-Slice2 Task 4):
//
//	Start  = coreclient(core XNIP pipe)StartCapture(活动控制台会话)
//	         → desktoppipe.Dial(rt pipe, secret, 随机 subID)
//	Stop   = 引用计数,最后一个 desktop 会话才 core StopCapture
//
// 连接模型:core pipe 是单实例串行服务(pipe_server.cpp 只有一个 accept
// 实例),因此进程内共享一条 coreclient 连接(Ping 保活探活,死则重拨);
// StartCapture 幂等(core 侧已运行则返回既有 pipe/secret)使多会话共享同
// 一采集 host、各自 ATTACH(max_subs 由 T2 host 限制)。
//
// dev 拓扑(M1):core 以 --console --smoke-secret 诊断模式跑;pipe 名与
// secret 经 XNC_DESKTOP_CORE_PIPE / XNC_DESKTOP_CORE_SECRET_HEX 环境变量
// 交给 agent(NewHandlerFromEnv;生产 SCM 模式的凭据通道 = M2)。secret
// 只进内存,绝无日志。
package desktop

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/windows"

	"xnc/agent/coreclient"
	"xnc/agent/desktoppipe"
)

// NewHandlerFromEnv 按 XNC_DESKTOP_CORE_PIPE + XNC_DESKTOP_CORE_SECRET_HEX
// (64 hex chars)构造 desktop 会话 handler;变量缺失/损坏返回 nil(agent
// 不注册 desktop kind,SESSION_OPEN 走既有 refused 路径)。
func NewHandlerFromEnv(log *slog.Logger) *Handler {
	pipe := os.Getenv("XNC_DESKTOP_CORE_PIPE")
	secHex := os.Getenv("XNC_DESKTOP_CORE_SECRET_HEX")
	if pipe == "" || secHex == "" {
		return nil
	}
	secret, err := hex.DecodeString(secHex)
	if err != nil {
		log.Warn("desktop: bad XNC_DESKTOP_CORE_SECRET_HEX, kind not registered")
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &Handler{Log: log, Starter: NewCoreStarter(pipe, secret, 0, log)}
}

// 生产缺省凭据(prod bootstrap):XNCCore 服务用 --secret-file 持久化
// secret,binPath 固定管道名;agent 无 env/flag 时按同约定回退。
const (
	// DefaultCorePipe XNCCore 服务的固定 XNIP 管道名。
	DefaultCorePipe = `\\.\pipe\xnc-core`
	// DefaultCoreSecretName agent state dir 下的 secret 文件名
	// (hex + 换行,与 xnc-core --secret-file 写盘格式一致)。
	DefaultCoreSecretName = "core-secret.hex"
)

// CoreSecretPath 返回 <stateDir>\core-secret.hex。
func CoreSecretPath(stateDir string) string {
	return filepath.Join(stateDir, DefaultCoreSecretName)
}

// ResolveCoreEndpoint 导出 resolveCoreEndpoint,供 exec/shell 的
// ShellHost 复用(同一凭据源,单一事实;含 legacy shellhost env 链)。
func ResolveCoreEndpoint(stateDir string) (string, []byte, error) {
	return resolveCoreEndpoint(stateDir)
}

// resolveCoreEndpoint 凭据解析(纯函数,便于单测):
//
//	env 链(逐条双全才可用,按优先级):
//	  XNC_CORE_PIPE + XNC_CORE_SECRET_HEX          (legacy shellhost 链,优先)
//	  XNC_DESKTOP_CORE_PIPE + XNC_DESKTOP_CORE_SECRET_HEX (canonical 链)
//	legacy 链半缺 → 回落 canonical 链(保持旧 DefaultShellHost 语义);
//	canonical 链半缺 / 任一链坏 hex → error(不回退弱配置);
//	env 全缺 → 生产缺省(DefaultCorePipe + CoreSecretPath(stateDir) 读盘,
//	trim 换行);stateDir 为空(DefaultShellHost 的 dev 形态)不读盘,直接 error。
func resolveCoreEndpoint(stateDir string) (pipe string, secret []byte, err error) {
	for i, chain := range [][2]string{
		{"XNC_CORE_PIPE", "XNC_CORE_SECRET_HEX"},
		{"XNC_DESKTOP_CORE_PIPE", "XNC_DESKTOP_CORE_SECRET_HEX"},
	} {
		envPipe := os.Getenv(chain[0])
		envHex := os.Getenv(chain[1])
		switch {
		case envPipe != "" && envHex != "":
			secret, err := hex.DecodeString(envHex)
			if err != nil {
				return "", nil, fmt.Errorf("desktop: bad %s: %w", chain[1], err)
			}
			return envPipe, secret, nil
		case envPipe != "" || envHex != "":
			if i == 0 {
				continue // legacy 链半缺:试 canonical 链
			}
			return "", nil, errors.New("desktop: XNC_DESKTOP_CORE_* partially set (need both or neither)")
		}
	}
	if stateDir == "" {
		return "", nil, errors.New("desktop: no core credentials in env (state-dir fallback needs a state dir)")
	}
	b, err := os.ReadFile(CoreSecretPath(stateDir))
	if err != nil {
		return "", nil, fmt.Errorf("desktop: no core credentials (env unset, %s unreadable): %w",
			CoreSecretPath(stateDir), err)
	}
	secret, err = hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return "", nil, fmt.Errorf("desktop: bad %s: %w", DefaultCoreSecretName, err)
	}
	return DefaultCorePipe, secret, nil
}

// NewHandler 生产入口:凭据 = env(优先,dev-console 保持既有语义)
// → 生产缺省(XNCCore 服务约定)。拿不到凭据返回 nil(desktop kind
// 不注册,零行为变化)。
func NewHandler(stateDir string, log *slog.Logger) *Handler {
	pipe, secret, err := resolveCoreEndpoint(stateDir)
	if err != nil {
		if log == nil {
			log = slog.Default()
		}
		log.Info("desktop: core credentials unavailable, kind not registered", "err", err)
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &Handler{Log: log, Starter: NewCoreStarter(pipe, secret, 0, log)}
}

// NewCoreStarter 构造共享 core 连接的 Starter。wts=0 = 每次取活动控制台会话。
func NewCoreStarter(pipe string, secret []byte, wts uint32, log *slog.Logger) Starter {
	if log == nil {
		log = slog.Default()
	}
	return &coreStarter{pipe: pipe, secret: secret, wts: wts, log: log}
}

type coreStarter struct {
	pipe   string
	secret []byte
	wts    uint32
	log    *slog.Logger

	mu       sync.Mutex
	client   *coreclient.Client
	sessions int // 活跃 desktop 会话数(ATTACH 成功计 1)
}

// ensureClientLocked 返回活的 core 连接(探活失败即重拨)。调用方持 mu。
func (s *coreStarter) ensureClientLocked() (*coreclient.Client, error) {
	if s.client != nil {
		if _, err := s.client.Ping(); err == nil {
			return s.client, nil
		}
		_ = s.client.Close()
		s.client = nil
	}
	c, err := coreclient.Dial(s.pipe, s.secret)
	if err != nil {
		return nil, fmt.Errorf("desktop: core dial: %w", err)
	}
	s.client = c
	return c, nil
}

// randSubID 产非 0 随机订阅 id(host 侧要求非 0、每连接唯一即可)。
func randSubID() (uint32, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	id := binary.LittleEndian.Uint32(b[:])
	if id == 0 {
		id = 1
	}
	return id, nil
}

// Start 启动(或复用)采集并 ATTACH(wts 覆盖顺序:会话 params >
// starter 构造值 > 活动控制台会话)。ATTACH 失败时若本 starter 无其他
// 活跃会话,顺手 StopCapture 防孤儿采集(仍有别的会话则不动)。
func (s *coreStarter) Start(_ context.Context, wts uint32) (Source, error) {
	s.mu.Lock()
	c, err := s.ensureClientLocked()
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if wts == 0 {
		wts = s.wts
	}
	if wts == 0 {
		wts = windows.WTSGetActiveConsoleSessionId()
	}
	hostPid, pipeName, capSecret, gen, err := c.StartCapture(wts)
	if err != nil {
		return nil, fmt.Errorf("desktop: core start_capture: %w", err)
	}
	subID, err := randSubID()
	if err != nil {
		return nil, fmt.Errorf("desktop: sub id: %w", err)
	}
	sub, err := desktoppipe.Dial(pipeName, string(capSecret), subID, desktoppipe.SubOpts{})
	if err != nil {
		s.mu.Lock()
		orphan := s.sessions == 0
		s.mu.Unlock()
		if orphan {
			if serr := c.StopCapture(); serr != nil {
				s.log.Warn("desktop: cleanup stop_capture failed", "err", serr)
			}
		}
		return nil, fmt.Errorf("desktop: rt pipe attach: %w", err)
	}
	s.mu.Lock()
	s.sessions++
	s.mu.Unlock()
	s.log.Info("desktop capture attached",
		"host_pid", hostPid, "gen", gen, "wts", wts) // 无 secret/pipe 凭据
	return &pipeSource{sub: sub}, nil
}

// Stop 会话终结:引用计数减一,最后一个会话 best-effort StopCapture
// (幂等;失败仅记日志——孤儿 child 由 core 侧 watchdog 收)。
func (s *coreStarter) Stop() error {
	s.mu.Lock()
	if s.sessions > 0 {
		s.sessions--
	}
	last := s.sessions == 0
	c := s.client
	s.mu.Unlock()
	if !last || c == nil {
		return nil
	}
	if err := c.StopCapture(); err != nil {
		s.log.Warn("desktop: stop_capture failed", "err", err)
		return nil
	}
	return nil
}

// SendSAS 触发 core 侧 secure attention(0x0110;M2-Slice1 Task 4/5)。
// 复用共享 core 连接(探活失败即重拨);门控(--allow-sas)与审计在
// core 侧。结果镜像见 SasResult:拒绝码透传(SAS_DENIED 等),传输层
// 失败归并为 core_unavailable / core_error(进 hr 的合成码无从谈起,
// viewer 只看 ok 与 code)。
func (s *coreStarter) SendSAS(reason string) SasResult {
	s.mu.Lock()
	c, err := s.ensureClientLocked()
	s.mu.Unlock()
	if err != nil {
		s.log.Warn("desktop: sas: core unavailable", "err", err)
		return SasResult{Code: "core_unavailable"}
	}
	hr, err := c.SendSAS(reason)
	if err != nil {
		var rej *coreclient.RejectedError
		if errors.As(err, &rej) {
			s.log.Info("desktop: sas rejected", "code", rej.Code, "reason", reason)
			return SasResult{Code: rej.Code}
		}
		s.log.Warn("desktop: sas failed", "err", err)
		return SasResult{Code: "core_error"}
	}
	return SasResult{OK: true, HR: hr}
}

// pipeSource 适配 desktoppipe.Sub → desktop.Source(就地 select 转换,
// 见 source.go 接口说明)。
type pipeSource struct {
	sub *desktoppipe.Sub
}

func (p *pipeSource) RecvFrame(ctx context.Context) (Frame, bool) {
	select {
	case f, ok := <-p.sub.FrameCh():
		if !ok {
			return Frame{}, false
		}
		return Frame{Key: f.Key, MonoUs: f.MonoUs, AU: f.AU}, true
	case <-ctx.Done():
		return Frame{}, false
	case <-p.sub.Done():
		return Frame{}, false
	}
}

func (p *pipeSource) RecvState(ctx context.Context) (StateEvent, bool) {
	select {
	case ev, ok := <-p.sub.StateCh():
		if !ok {
			return StateEvent{}, false
		}
		return StateEvent{Code: ev.Code, Recoverable: ev.Recoverable}, true
	case <-ctx.Done():
		return StateEvent{}, false
	case <-p.sub.Done():
		// StateCh 已关时上面的分支自然命中;Done 先到则终结。
		return StateEvent{}, false
	}
}

// RecvCursor 取 0x0109 光标事件(通道满时 desktoppipe 侧已丢弃,语义 =
// 最新即准;Slice3 Task 3)。
func (p *pipeSource) RecvCursor(ctx context.Context) (CursorEvent, bool) {
	select {
	case ev, ok := <-p.sub.CursorCh():
		if !ok {
			return CursorEvent{}, false
		}
		return CursorEvent{X: ev.X, Y: ev.Y, Visible: ev.Visible}, true
	case <-ctx.Done():
		return CursorEvent{}, false
	case <-p.sub.Done():
		return CursorEvent{}, false
	}
}

// RecvDisplay 取 0x010A 显示变化事件(M2-Slice1 Task 2);desktoppipe 侧
// 同步更新 Hello()(gen/w/h)。
func (p *pipeSource) RecvDisplay(ctx context.Context) (DisplayChangedEvent, bool) {
	select {
	case ev, ok := <-p.sub.DisplayCh():
		if !ok {
			return DisplayChangedEvent{}, false
		}
		return DisplayChangedEvent{Gen: ev.Gen, W: ev.W, H: ev.H, Reason: ev.Reason}, true
	case <-ctx.Done():
		return DisplayChangedEvent{}, false
	case <-p.sub.Done():
		return DisplayChangedEvent{}, false
	}
}

// SubID 返回 ATTACH 时选定的订阅 id(0x0108 消息必须携带)。
func (p *pipeSource) SubID() uint32 { return p.sub.SubID() }

// SendInput 发送 agent 已编码的 0x0108 payload(预校验在 input.go)。
func (p *pipeSource) SendInput(payload []byte) error { return p.sub.SendInputPayload(payload) }

func (p *pipeSource) Hello() *HelloInfo {
	h := p.sub.Hello()
	if h == nil {
		return nil
	}
	out := &HelloInfo{Gen: h.Gen, W: h.W, H: h.H, Fps: h.Fps, MaxSubs: h.MaxSubs}
	if len(h.Displays) > 0 {
		out.Displays = make([]Display, len(h.Displays))
		for i, d := range h.Displays {
			out.Displays[i] = Display{Index: d.Index, OriginX: d.OriginX,
				OriginY: d.OriginY, W: d.W, H: d.H, Primary: d.Primary}
		}
	}
	return out
}

// SwitchDisplay 发送 0x0128(M2-S3 Task 5;DisplaySwitcher 能力)。
func (p *pipeSource) SwitchDisplay(index uint32) error {
	return p.sub.SendSwitchDisplay(index)
}

func (p *pipeSource) RequestKeyframe(reason string) error { return p.sub.RequestKeyframe(reason) }

func (p *pipeSource) Close() error { return p.sub.Close() }
