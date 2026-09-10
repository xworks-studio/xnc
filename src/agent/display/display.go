// Package display 实现 XNC 虚拟显示器（IDD，"XWorks XNC Virtual Display"）
// 的生命周期编排（2026-09-10 引入；驱动本体 vendored 于
// src/third_party/xncidd，安装器 idd 组件 pnputil 入 store）。
//
// 策略（用户确认，比 2026-08-22 agent 重构 spec §13.3 更克制）：
//   - 默认一律不开启（开盖/盒盖均无虚拟屏）；
//   - 仅当 desktop（RTV）会话接入且（盒盖 ∨ 无活动物理输出 ∨
//     XNC_IDD_FORCE_LID 调试旋钮）时自动插入虚拟屏；
//   - 会话全部结束即移除（仅自动创建的）；手动 on 的保持到手动 off 或
//     agent 退出。
//
// 设备经 SwDeviceCreate 以 SWDeviceLifetimeHandle 由本进程持句柄——
// agent 崩溃句柄随进程关闭，设备与虚拟屏自动消失，无需兜底清理。
// 驱动未安装时全部操作退化为 no-op（状态经 agentctl display op 可见）。
package display

import (
	"log/slog"
	"sync"
	"time"
)

// backend 平台相关操作（Windows 实现见 idd_windows.go / lid_windows.go；
// 非 Windows 为全空 stub，保证 go-linux CI 可编译）。
type backend interface {
	// driverInstalled 驱动包是否在 store（DriverPackages 注册表键）。
	driverInstalled() bool
	// createDevice 经 SwDeviceCreate 创建软件设备（硬件 ID XncIdd），
	// 返回设备句柄（Handle 生命周期）。
	createDevice() (uintptr, error)
	// closeDevice 关闭句柄；Handle 生命周期下设备即刻被 PnP 移除。
	closeDevice(h uintptr)
	// plugMonitor/unplugMonitor 经设备接口 IOCTL 热插拔 0 号显示器
	// （EDID 0 = 1920×1080@60）。
	plugMonitor(h uintptr) error
	unplugMonitor(h uintptr) error
	// physicalOutputActive 存在活动的物理显示器（非本驱动、非镜像）。
	physicalOutputActive() bool
	// virtualDisplayActive 本驱动的虚拟显示器当前活动（插屏成功的判据）。
	virtualDisplayActive() bool
	// lidClosed 真实 lid 状态；known=false 表示尚无事件（状态未知，按开盖）。
	lidClosed() (closed, known bool)
	// forceLid 调试旋钮 XNC_IDD_FORCE_LID（"" | "open" | "closed"）。
	forceLid() string
}

// Status 是 agentctl display op 的应答载荷（展示/诊断用）。
type Status struct {
	DriverInstalled bool   `json:"driverInstalled"`
	DevicePresent   bool   `json:"devicePresent"`
	VirtualActive   bool   `json:"virtualActive"`
	PhysicalActive  bool   `json:"physicalActive"`
	LidClosed       bool   `json:"lidClosed"`
	LidKnown        bool   `json:"lidKnown"`
	ForceLid        string `json:"forceLid,omitempty"`
	AutoActive      bool   `json:"autoActive"`
	ManualActive    bool   `json:"manualActive"`
}

// Manager 虚拟显示器生命周期编排（会话引用计数 + 手动覆盖 + 3s 轮询
// 兜底会话中途的触发变化）。
type Manager struct {
	log *slog.Logger
	be  backend

	mu           sync.Mutex
	handle       uintptr
	plugged      bool
	sessionRefs  int
	autoActive   bool // 会话触发创建的（会话归零即移除）
	manualActive bool // 手动 on（保持到 off / agent 退出）

	stop     chan struct{}
	stopOnce sync.Once
}

// NewManager 构造并启动轮询 goroutine。Close 停止轮询并移除设备。
// lidNotify 盒盖电源事件即时信号源（平台实现：Windows = 电源通知窗口
// 的单播通道；非 Windows = nil）。测试可注入假通道。
var lidNotify = lidNotifyChan

func NewManager(log *slog.Logger) *Manager {
	m := &Manager{log: log, be: newBackend(), stop: make(chan struct{})}
	go m.poll()
	return m
}

func (m *Manager) logger() *slog.Logger {
	if m.log != nil {
		return m.log
	}
	return slog.Default()
}

// poll 会话期间的触发变化（盒盖/拔屏）。双源：lid 电源事件即时信号
// （合盖→建屏的主路径，不等轮询）+ 3s 轮询兜底（物理输出变化等）。
// 创建只在会话活跃时发生，移除只发生在会话归零，见策略注释。
func (m *Manager) poll() {
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-lidNotify():
			m.reconcile()
		case <-t.C:
			m.reconcile()
		}
	}
}

// reconcile 重评估触发条件（lid 事件/轮询共用）。
func (m *Manager) reconcile() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessionRefs > 0 && !m.plugged && m.triggerLocked() {
		if err := m.ensureVirtualLocked(); err == nil {
			m.autoActive = true
			m.logger().Info("display: virtual display created for session")
		} else if err != errNotInstalled {
			m.logger().Warn("display: session-triggered create failed", "err", err)
		}
	}
}

// Close 停止轮询并拆除设备（agent 退出路径；Handle 生命周期下句柄关闭
// 即设备消失，这里只做进程内收线）。
func (m *Manager) Close() {
	m.stopOnce.Do(func() { close(m.stop) })
	m.mu.Lock()
	defer m.mu.Unlock()
	m.teardownLocked()
}

// triggerLocked 判定本次是否应创建虚拟屏。调用方持 mu。
func (m *Manager) triggerLocked() bool {
	trigger := !m.be.physicalOutputActive()
	switch m.be.forceLid() {
	case "closed":
		trigger = true
	case "open":
		// 仅无物理输出触发（headless 机仍可用；盒盖信号被旋钮压制）
	default:
		if closed, known := m.be.lidClosed(); known && closed {
			trigger = true
		}
	}
	return trigger
}

// ensureVirtualLocked 驱动就绪 → 设备就绪 → 插屏 → 等 ≤5s 显示器活动。
// 调用方持 mu。驱动未安装是常态（组件未选/装失败），返回错误不视为故障。
func (m *Manager) ensureVirtualLocked() error {
	if !m.be.driverInstalled() {
		return errNotInstalled
	}
	if m.handle == 0 {
		h, err := m.be.createDevice()
		if err != nil {
			return err
		}
		m.handle = h
	}
	if err := m.be.plugMonitor(m.handle); err != nil {
		return err
	}
	m.plugged = true
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if m.be.virtualDisplayActive() {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	// PLUG_IN 已成功但显示器尚未活动（慢机器/会话切换）：不视为失败，
	// host 侧 1s 拓扑轮询会在其出现后自动采上。
	m.logger().Warn("display: virtual display not active after 5s (arrival delayed)")
	return nil
}

// teardownLocked 拆除设备与显示器。调用方持 mu。幂等。
func (m *Manager) teardownLocked() {
	if m.handle == 0 {
		m.plugged = false
		return
	}
	if m.plugged {
		if err := m.be.unplugMonitor(m.handle); err != nil {
			m.logger().Warn("display: unplug failed", "err", err)
		}
		m.plugged = false
	}
	m.be.closeDevice(m.handle)
	m.handle = 0
}

// SessionStarted desktop 会话接入：引用计数 +1；**预创建设备**（无显示
// 器——盒盖触发时只剩插屏 + OS 模式提交，省掉设备创建/接口就绪的秒级
// 延迟，设备生命周期随会话）；触发满足即建屏（先于 host spawn——host
// 枚举时虚拟屏须已在位）。
func (m *Manager) SessionStarted() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionRefs++
	if m.handle == 0 && m.be.driverInstalled() {
		if h, err := m.be.createDevice(); err == nil {
			m.handle = h
			m.logger().Info("display: device pre-created for session")
		}
	}
	if !m.plugged && m.triggerLocked() {
		if err := m.ensureVirtualLocked(); err == nil {
			m.autoActive = true
			m.logger().Info("display: virtual display created for session")
		} else if err != errNotInstalled {
			m.logger().Warn("display: session-triggered create failed", "err", err)
		}
	}
}

// SessionEnded 最后一个会话结束后移除自动创建的虚拟屏（手动 on 的保留），
// 并关闭仅预创建（未插屏）的设备。
func (m *Manager) SessionEnded() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessionRefs > 0 {
		m.sessionRefs--
	}
	if m.sessionRefs == 0 && !m.manualActive {
		if m.autoActive {
			m.teardownLocked()
			m.autoActive = false
			m.logger().Info("display: virtual display removed (last session closed)")
		} else if m.handle != 0 && !m.plugged {
			// 仅预创建设备（无显示器）：随会话关闭。
			m.teardownLocked()
		}
	}
}

// SetOn 手动开（agentctl display on）：无视触发条件，保持到 off/退出。
func (m *Manager) SetOn() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.plugged {
		// 已活动：所有权转手动（会话结束不再移除）。
		m.autoActive = false
		m.manualActive = true
		return nil
	}
	if err := m.ensureVirtualLocked(); err != nil {
		return err
	}
	m.autoActive = false
	m.manualActive = true
	m.logger().Info("display: virtual display turned on manually")
	return nil
}

// SetOff 手动关：拆除设备与显示器，清除手动标记（会话仍活跃时不建回）。
func (m *Manager) SetOff() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.teardownLocked()
	m.manualActive = false
	m.autoActive = false
	m.logger().Info("display: virtual display turned off")
}

// Snapshot 状态快照（agentctl display status）。
func (m *Manager) Snapshot() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{
		DriverInstalled: m.be.driverInstalled(),
		DevicePresent:   m.handle != 0,
		VirtualActive:   m.be.virtualDisplayActive(),
		PhysicalActive:  m.be.physicalOutputActive(),
		AutoActive:      m.autoActive,
		ManualActive:    m.manualActive,
		ForceLid:        m.be.forceLid(),
	}
	st.LidClosed, st.LidKnown = m.be.lidClosed()
	return st
}

// errNotInstalled 驱动未入 store（idd 组件未装/装失败）——状态而非故障。
var errNotInstalled = &notInstalledError{}

type notInstalledError struct{}

func (e *notInstalledError) Error() string {
	return "not_installed: idd driver package not in store (component 'idd' not installed)"
}
