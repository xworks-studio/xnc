//go:build windows

// apply_restart_windows_test.go — withCoreRestart 决策缝单测：停 →
// swap → 起 的顺序；swap 失败走回滚且仍拉起;EnsureStopped 失败则不
// swap。另有真实 SCM 容错探针（不存在的服务 = nil）。
package updater

import (
	"errors"
	"testing"
)

type fakeCore struct {
	calls     []string
	stopErr   error
	startErr  error
	stopCalls int
}

func (f *fakeCore) EnsureStopped() error {
	f.calls = append(f.calls, "stop")
	f.stopCalls++
	return f.stopErr
}

func (f *fakeCore) EnsureStarted() error {
	f.calls = append(f.calls, "start")
	return f.startErr
}

func TestWithCoreRestartOrderSuccess(t *testing.T) {
	f := &fakeCore{}
	var order []string
	err := withCoreRestart(f, func() error { order = append(order, "swap"); return nil },
		func() error { order = append(order, "rollback"); return nil })
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(order) != 1 || order[0] != "swap" {
		t.Fatalf("swap calls = %v, want exactly one swap", order)
	}
	if len(f.calls) != 2 || f.calls[0] != "stop" || f.calls[1] != "start" {
		t.Fatalf("svc calls = %v, want [stop start]", f.calls)
	}
}

func TestWithCoreRestartSwapFailureRollsBackAndRestarts(t *testing.T) {
	f := &fakeCore{}
	var rolled bool
	err := withCoreRestart(f, func() error { return errors.New("boom") },
		func() error { rolled = true; return nil })
	if err == nil || err.Error() != "boom" {
		t.Fatalf("err = %v, want boom", err)
	}
	if !rolled {
		t.Fatal("rollback must run on swap failure")
	}
	if len(f.calls) != 2 || f.calls[1] != "start" {
		t.Fatalf("svc calls = %v, core must still be restarted", f.calls)
	}
}

func TestWithCoreRestartStopFailureAborts(t *testing.T) {
	f := &fakeCore{stopErr: errors.New("scm down")}
	swapped := false
	err := withCoreRestart(f, func() error { swapped = true; return nil }, func() error { return nil })
	if err == nil || swapped {
		// EnsureStopped 容错实现永不报错；缝语义 = 报错则中止不动文件。
		t.Fatalf("stop failure must abort before swap: err=%v swapped=%v", err, swapped)
	}
}

// 真实 SCM 容错：XNCCore 未注册的机器上停/起都必须是 nil（不破坏
// 无 core 的既有部署）。
func TestScmCoreServiceTolerantWhenMissing(t *testing.T) {
	s := scmCoreService{name: "XNC-Definitely-Not-Registered-Test"}
	if err := s.EnsureStopped(); err != nil {
		t.Fatalf("EnsureStopped on missing service = %v, want nil", err)
	}
	if err := s.EnsureStarted(); err != nil {
		t.Fatalf("EnsureStarted on missing service = %v, want nil", err)
	}
}
