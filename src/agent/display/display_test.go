package display

// Manager 状态机单测（假后端：不触真实驱动/显示系统；覆盖会话触发、
// 手动覆盖、拆除与轮询触发兜底）。

import (
	"sync"
	"testing"
	"time"
)

type fakeBackend struct {
	mu             sync.Mutex
	installed      bool
	physicalActive bool
	virtualActive  bool
	lidClosedV     bool
	lidKnownV      bool
	forceV         string

	created, closed, plugged, unplugged int
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{installed: true, physicalActive: true}
}

func (f *fakeBackend) driverInstalled() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.installed
}

func (f *fakeBackend) createDevice() (uintptr, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	return 1, nil
}

func (f *fakeBackend) closeDevice(uintptr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
}

func (f *fakeBackend) plugMonitor(uintptr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plugged++
	f.virtualActive = true // 假后端：插屏即活动（跳过 5s 等待）
	return nil
}

func (f *fakeBackend) unplugMonitor(uintptr) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unplugged++
	f.virtualActive = false
	return nil
}

func (f *fakeBackend) physicalOutputActive() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.physicalActive
}

func (f *fakeBackend) virtualDisplayActive() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.virtualActive
}

func (f *fakeBackend) lidClosed() (bool, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lidClosedV, f.lidKnownV
}

func (f *fakeBackend) forceLid() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.forceV
}

// newTestManager 构造 Manager 并注入假后端。
func newTestManager(f *fakeBackend) *Manager {
	m := &Manager{log: nil, be: f, stop: make(chan struct{})}
	go m.poll()
	return m
}

func TestSessionTriggerCreatesAndRemoves(t *testing.T) {
	f := newFakeBackend()
	f.lidClosedV, f.lidKnownV = true, true // 盒盖
	m := newTestManager(f)
	defer m.Close()

	m.SessionStarted()
	if f.created != 1 || f.plugged != 1 {
		t.Fatalf("expected create+plug on trigger, got created=%d plugged=%d", f.created, f.plugged)
	}
	st := m.Snapshot()
	if !st.AutoActive || !st.VirtualActive {
		t.Fatalf("expected autoActive+virtual, got %+v", st)
	}

	m.SessionEnded()
	if f.closed != 1 || f.unplugged != 1 {
		t.Fatalf("expected teardown on last session end, got closed=%d unplugged=%d", f.closed, f.unplugged)
	}
	st = m.Snapshot()
	if st.AutoActive || st.VirtualActive {
		t.Fatalf("expected teardown, got %+v", st)
	}
}

func TestNoTriggerNoCreate(t *testing.T) {
	f := newFakeBackend() // 开盖 + 物理屏在
	m := newTestManager(f)
	defer m.Close()

	m.SessionStarted()
	if f.created != 0 || f.plugged != 0 {
		t.Fatalf("expected no create without trigger, got created=%d plugged=%d", f.created, f.plugged)
	}
	m.SessionEnded()
	if f.closed != 0 {
		t.Fatalf("expected no teardown, got closed=%d", f.closed)
	}
}

func TestNoPhysicalOutputTriggers(t *testing.T) {
	f := newFakeBackend()
	f.physicalActive = false // headless：无物理输出
	m := newTestManager(f)
	defer m.Close()

	m.SessionStarted()
	if f.plugged != 1 {
		t.Fatalf("expected plug on headless trigger, got plugged=%d", f.plugged)
	}
}

func TestForceKnobOpenSuppressesLid(t *testing.T) {
	f := newFakeBackend()
	f.lidClosedV, f.lidKnownV = true, true
	f.forceV = "open" // 旋钮压制盒盖信号
	m := newTestManager(f)
	defer m.Close()

	m.SessionStarted()
	if f.created != 0 {
		t.Fatalf("expected no create with force=open, got created=%d", f.created)
	}
}

func TestForceKnobClosedTriggers(t *testing.T) {
	f := newFakeBackend() // 开盖 + 物理屏在，但旋钮强制盒盖
	f.forceV = "closed"
	m := newTestManager(f)
	defer m.Close()

	m.SessionStarted()
	if f.plugged != 1 {
		t.Fatalf("expected plug with force=closed, got plugged=%d", f.plugged)
	}
}

func TestManualOnSurvivesSessionEnd(t *testing.T) {
	f := newFakeBackend()
	m := newTestManager(f)
	defer m.Close()

	if err := m.SetOn(); err != nil {
		t.Fatalf("SetOn: %v", err)
	}
	m.SessionStarted()
	m.SessionEnded() // 手动 on：会话结束不移除
	if f.closed != 0 || f.unplugged != 0 {
		t.Fatalf("manual display must survive session end, got closed=%d unplugged=%d", f.closed, f.unplugged)
	}
	st := m.Snapshot()
	if !st.ManualActive || !st.VirtualActive {
		t.Fatalf("expected manualActive, got %+v", st)
	}
	m.SetOff()
	if f.closed != 1 || f.unplugged != 1 {
		t.Fatalf("SetOff must teardown, got closed=%d unplugged=%d", f.closed, f.unplugged)
	}
}

func TestSetOnWithoutDriver(t *testing.T) {
	f := newFakeBackend()
	f.installed = false
	m := newTestManager(f)
	defer m.Close()

	err := m.SetOn()
	if err == nil {
		t.Fatal("expected not_installed error")
	}
	if f.created != 0 {
		t.Fatalf("expected no create without driver, got created=%d", f.created)
	}
}

func TestPollerTriggersMidSession(t *testing.T) {
	f := newFakeBackend()
	m := newTestManager(f)
	defer m.Close()

	m.SessionStarted() // 无触发：不建
	if f.created != 0 {
		t.Fatalf("expected no create, got created=%d", f.created)
	}
	f.mu.Lock()
	f.lidClosedV, f.lidKnownV = true, true // 会话中途盒盖
	f.mu.Unlock()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		plugged := m.plugged
		m.mu.Unlock()
		if plugged {
			return // 轮询兜底建屏成功
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("poller did not create virtual display within 5s of mid-session lid close")
}
