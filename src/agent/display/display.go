// Package display 实现 XNC 虚拟显示器（IDD，"XWorks XNC Virtual Display"）
// 的生命周期编排（2026-09-10 引入；驱动本体 vendored 于
// src/third_party/xncidd，安装器 idd 组件 pnputil 入 store）。
//
// 策略（用户确认，比 2026-08-22 agent 重构 spec §13.3 更克制）：
//   - **设备常驻后台**：SW 设备经活动控制台会话里的持有进程创建一次
//     （xnc idd-hold，见 holder_windows.go——会话 0 创建设备会把虚拟屏
//     绑到不可见会话，2026-09-10 XIAOXIN 实测修复）、不随会话拆建；
//     agent 退出/崩溃 → 持有进程 EOF 释放句柄 → 设备自动移除；
//   - **屏幕仅在有控制请求接入后才创建**：desktop（RTV）会话活跃 且
//     （盒盖 ∨ 无活动物理输出 ∨ XNC_IDD_FORCE_LID 调试旋钮）时插屏；
//     会话全部结束即移除（仅自动创建的）；手动 `xnc display on` 保持
//     到 off。无会话时盒盖不产生任何屏幕。
//
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
	// startDevice 在活动控制台会话拉起设备持有进程（xnc idd-hold，
	// holder_windows.go），等设备接口就绪后返回持有进程句柄。无控制台
	// 会话/就绪超时返回错误——可重试的延迟条件。会话亲和：会话 0 创建
	// 设备会把虚拟屏绑到不可见会话（2026-09-10 实测）。
	startDevice() (uintptr, error)
	// stopDevice 结束持有进程（优雅 EOF → 超时强杀），设备随句柄释放
	// 被 PnP 移除。
	stopDevice(h uintptr)
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
	// 设备常驻：agent 启动即建（屏幕仍严格会话触发）。
	m.mu.Lock()
	m.ensureDeviceLocked()
	m.mu.Unlock()
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

// reconcile 重评估触发条件（lid 事件/轮询共用）。屏幕只在会话活跃时
// 创建（用户策略：设备可常驻后台，屏幕仅控制请求接入后才新建）。
func (m *Manager) reconcile() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessionRefs > 0 && !m.plugged && m.triggerLocked() {
		if err := m.plugVirtualLocked(); err == nil {
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

// ensureDeviceLocked 设备常驻（用户策略：设备可常驻后台）：进程生命周期
// 内创建一次、不随会话拆建（持有进程 EOF 生命周期，agent 退出即自动
// 移除）。驱动未装 = 静默 no-op（后续会话/触发/手动 on 时重试）。
func (m *Manager) ensureDeviceLocked() {
	if m.handle != 0 || !m.be.driverInstalled() {
		return
	}
	h, err := m.be.startDevice()
	if err != nil {
		m.logger().Warn("display: device start failed", "err", err)
		return
	}
	m.handle = h
	m.logger().Info("display: device resident (background)")
}

// plugVirtualLocked 设备就绪 → 插屏 → 等 ≤5s 显示器活动。调用方持 mu。
// 驱动未安装是常态（组件未选/装失败），返回错误不视为故障。
func (m *Manager) plugVirtualLocked() error {
	if !m.be.driverInstalled() {
		return errNotInstalled
	}
	if m.handle == 0 {
		h, err := m.be.startDevice()
		if err != nil {
			return err
		}
		m.handle = h
	}
	if err := m.be.plugMonitor(m.handle); err != nil {
		// 常驻设备可能已失效（驱动重装/外部移除/用户注销）：弃句柄，
		// 下次重建。
		m.be.stopDevice(m.handle)
		m.handle = 0
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

// unplugLocked 仅移除屏幕（设备保持常驻——下一会话插屏即达）。
func (m *Manager) unplugLocked() {
	if m.handle == 0 || !m.plugged {
		m.plugged = false
		return
	}
	if err := m.be.unplugMonitor(m.handle); err != nil {
		m.logger().Warn("display: unplug failed", "err", err)
	}
	m.plugged = false
}

// teardownLocked 拆除屏幕与设备（仅 agent 退出路径）。调用方持 mu。幂等。
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
	m.be.stopDevice(m.handle)
	m.handle = 0
}

// SessionStarted desktop 会话接入：引用计数 +1；设备常驻保证（agent
// 启动即建，此处幂等兜底驱动后装场景）；**屏幕仅此刻（会话活跃 + 触发
// 满足）才创建**——先于 host spawn（host 枚举时虚拟屏须已在位）。
func (m *Manager) SessionStarted() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessionRefs++
	m.ensureDeviceLocked()
	if !m.plugged && m.triggerLocked() {
		if err := m.plugVirtualLocked(); err == nil {
			m.autoActive = true
			m.logger().Info("display: virtual display created for session")
		} else if err != errNotInstalled {
			m.logger().Warn("display: session-triggered create failed", "err", err)
		}
	}
}

// SessionEnded 最后一个会话结束后移除自动创建的屏幕（手动 on 的保留）；
// 设备保持常驻（下一会话插屏即达，不重建设备）。
func (m *Manager) SessionEnded() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessionRefs > 0 {
		m.sessionRefs--
	}
	if m.sessionRefs == 0 && m.autoActive && !m.manualActive {
		m.unplugLocked()
		m.autoActive = false
		m.logger().Info("display: virtual display removed (last session closed)")
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
	if err := m.plugVirtualLocked(); err != nil {
		return err
	}
	m.autoActive = false
	m.manualActive = true
	m.logger().Info("display: virtual display turned on manually")
	return nil
}

// SetOff 手动关：仅移除屏幕（设备常驻），清除手动标记（会话仍活跃时
// 不建回——策略一致：屏幕只随控制请求/显式 on 出现）。
func (m *Manager) SetOff() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unplugLocked()
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
