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
	"fmt"
	"log/slog"
	"os"
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

func (p *pipeSource) Hello() *HelloInfo {
	h := p.sub.Hello()
	if h == nil {
		return nil
	}
	return &HelloInfo{Gen: h.Gen, W: h.W, H: h.H, Fps: h.Fps, MaxSubs: h.MaxSubs}
}

func (p *pipeSource) RequestKeyframe(reason string) error { return p.sub.RequestKeyframe(reason) }

func (p *pipeSource) Close() error { return p.sub.Close() }
