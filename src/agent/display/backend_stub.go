//go:build !windows

// backend_stub.go — 非 Windows：全空后端（保证 go-linux CI 可编译；
// 虚拟显示器是 Windows IDD 专属特性）。
package display

type stubBackend struct{}

func newBackend() backend { return stubBackend{} }

func (stubBackend) driverInstalled() bool         { return false }
func (stubBackend) startDevice() (uintptr, error) { return 0, errNotInstalled }
func (stubBackend) stopDevice(uintptr)            {}
func (stubBackend) plugMonitor(uintptr) error     { return errNotInstalled }
func (stubBackend) unplugMonitor(uintptr) error   { return nil }
func (stubBackend) physicalOutputActive() bool    { return true }
func (stubBackend) virtualDisplayActive() bool    { return false }
func (stubBackend) lidClosed() (bool, bool)       { return false, false }
func (stubBackend) forceLid() string              { return "" }

// lidNotifyChan 非 Windows：无 lid 事件源（Manager 只走轮询）。
func lidNotifyChan() <-chan struct{} { return nil }
