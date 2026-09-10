// shellfake_test.go — session 包测试用 fake ShellHost/ShellProc:
// 跨平台纯内存实现(不依赖 winio),服务 canned 输出、记录 stdin/
// resize/kill、可注入拒绝码。
package session

import (
	"sync"
)

// fakeProc 记录交互并按脚本回放。
type fakeProc struct {
	spec    ShellSpec
	profile string

	mu      sync.Mutex
	stdin   []byte
	resizes [][2]int
	killed  int
	closed  int

	streamCh chan ShellStream
	exitCh   chan uint32
	// droppedVal 由用例注入(Dropped() 返回,Truncated 测试)。
	droppedVal uint64
	// onKill 由用例注入(默认:交付 exitCode)。
	onKill func(p *fakeProc)
}

func (p *fakeProc) Profile() string { return p.profile }

func (p *fakeProc) WriteStdin(b []byte) error {
	p.mu.Lock()
	p.stdin = append(p.stdin, b...)
	p.mu.Unlock()
	return nil
}

func (p *fakeProc) Stdin() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.stdin...)
}

func (p *fakeProc) Resize(c, r int) error {
	p.mu.Lock()
	p.resizes = append(p.resizes, [2]int{c, r})
	p.mu.Unlock()
	return nil
}

func (p *fakeProc) Kill() error {
	p.mu.Lock()
	p.killed++
	f := p.onKill
	p.mu.Unlock()
	if f != nil {
		f(p)
	} else {
		select {
		case p.exitCh <- 1:
		default:
		}
	}
	return nil
}

func (p *fakeProc) Killed() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killed
}

func (p *fakeProc) Resizes() [][2]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][2]int(nil), p.resizes...)
}

func (p *fakeProc) Stream() <-chan ShellStream { return p.streamCh }

func (p *fakeProc) Exit() <-chan uint32 { return p.exitCh }

// Dropped 返回注入的丢弃计数(T5 Truncated 测试用;缺省 0)。
func (p *fakeProc) Dropped() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.droppedVal
}

func (p *fakeProc) Close() error {
	p.mu.Lock()
	p.closed++
	p.mu.Unlock()
	return nil
}

func (p *fakeProc) emit(s ShellStream) { p.streamCh <- s }

// fakeHost 捕获 spec 并返回脚本化结果。
type fakeHost struct {
	mu      sync.Mutex
	spawned []ShellSpec
	reject  *ShellHostError
	procs   []*fakeProc
	// build 在每次 CreateShell 时构造 proc(nil = 默认)。
	build func(spec ShellSpec) *fakeProc
}

func (h *fakeHost) specs() []ShellSpec {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]ShellSpec(nil), h.spawned...)
}

func (h *fakeHost) lastProc() *fakeProc {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.procs) == 0 {
		return nil
	}
	return h.procs[len(h.procs)-1]
}

func (h *fakeHost) CreateShell(spec ShellSpec) (ShellProc, error) {
	h.mu.Lock()
	h.spawned = append(h.spawned, spec)
	rej := h.reject
	h.mu.Unlock()
	if rej != nil {
		return nil, rej
	}
	var p *fakeProc
	if h.build != nil {
		p = h.build(spec)
	} else {
		p = &fakeProc{
			spec:     spec,
			profile:  spec.Profile,
			streamCh: make(chan ShellStream, 16),
			exitCh:   make(chan uint32, 1),
		}
	}
	h.mu.Lock()
	h.procs = append(h.procs, p)
	h.mu.Unlock()
	return p, nil
}
