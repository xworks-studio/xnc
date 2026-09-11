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
//
// **2026-09-11 用户决策：功能停用**——组件（驱动/安装器组件/agent 编排/
// CLI）全部保留，但缺省任何条件下都不创建显示器（会话/lid/无物理输出/
// 手动 on 一并被门禁，XNC_IDD_ENABLED=1 经注册表环境显式重新启用）。
package display

import (
	"errors"
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
	// featureEnabled IDD 功能总开关（2026-09-11 用户决策：放弃启用，
	// 组件全保留）。注册表环境 XNC_IDD_ENABLED=1 显式启用；缺省恒 false
	// ——任何条件下不创建显示器（会话/lid/无物理输出/手动 on 全部
	// 被门禁）。Windows 实现读注册表环境，非 Windows 恒 false。
	featureEnabled() bool
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
	// Enabled 功能总开关（缺省 false：组件在、永不建屏）。
	Enabled bool `json:"enabled"`
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

	// physicalAbsentStreak/physicalPresentStreak 连续"无/有物理输出"采样数
	// （双向触发滞回：物理屏 EDID 抖动的机器上避免随拓扑振荡反复建拆
	// ——2026-09-11 YOGA9 外接屏 4-8s 周期抖动实测）。
	physicalAbsentStreak  int
	physicalPresentStreak int

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
// （合盖→建屏的主路径，不等轮询）+ 1s 轮询兜底（物理输出变化；双向
// 跟随的切换延迟目标"立刻"，2026-09-11 从 3s 收紧）。
// 创建只在会话活跃时发生，移除只在触发解除时发生，见策略注释。
func (m *Manager) poll() {
	t := time.NewTicker(1 * time.Second)
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

// physicalHysteresis 物理输出采样的滞回门槛（连续 N 次同向才动作）：
// 物理屏 EDID 抖动的机器上避免拓扑振荡下反复建拆（2026-09-11 YOGA9
// 外接屏 4-8s 周期抖动实测；1s 轮询下 2 次 ≈ 2s 切换延迟）。
const physicalHysteresis = 2

// reconcile 重评估触发条件（lid 事件/轮询共用）。双向（2026-09-11）：
//   - 建屏：会话活跃 + 触发满足（盒盖/无物理输出，滞回后）；
//   - 拆屏：会话活跃 + 自动建的屏 + 触发解除（开盖事件即时 / 物理屏
//     回归滞回后）——视频流随 host 采集源立即切回物理屏。
func (m *Manager) reconcile() {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 总开关（2026-09-11 用户决策：缺省停用）：任何条件下不建屏——
	// 早退也避免每秒空跑探针进程。
	if !m.be.featureEnabled() {
		return
	}
	if m.sessionRefs == 0 {
		return
	}

	// 单次采样物理输出（探针进程 spawn 有开销，两个方向共用一次结果）。
	physicalActive := m.be.physicalOutputActive()
	m.updatePhysicalStreaksLocked(physicalActive)

	if !m.plugged && m.triggerWithPhysicalLocked(physicalActive) {
		if err := m.plugVirtualLocked(); err == nil {
			m.autoActive = true
			m.logger().Info("display: virtual display created for session")
		} else if err != errNotInstalled {
			m.logger().Warn("display: session-triggered create failed", "err", err)
		}
		return
	}
	if m.plugged && m.autoActive && !m.manualActive && m.releaseWithPhysicalLocked(physicalActive) {
		m.unplugLocked()
		m.autoActive = false
		m.logger().Info("display: virtual display removed (trigger released mid-session)")
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

// updatePhysicalStreaksLocked 更新双向滞回计数（连续 physicalHysteresis
// 次同向采样才动作）。
func (m *Manager) updatePhysicalStreaksLocked(physicalActive bool) {
	if physicalActive {
		m.physicalAbsentStreak = 0
		m.physicalPresentStreak++
	} else {
		m.physicalAbsentStreak++
		m.physicalPresentStreak = 0
	}
}

// physicalTriggerFromLocked 物理维度建屏判定（无物理输出且滞回满足）。
func (m *Manager) physicalTriggerFromLocked(physicalActive bool) bool {
	return !physicalActive && m.physicalAbsentStreak >= physicalHysteresis
}

// physicalReleaseFromLocked 物理维度拆屏判定（物理输出回归且滞回满足）。
func (m *Manager) physicalReleaseFromLocked(physicalActive bool) bool {
	return physicalActive && m.physicalPresentStreak >= physicalHysteresis
}

// triggerWithPhysicalLocked 建屏判定（物理状态已采样；lid/旋钮/物理 OR）。
func (m *Manager) triggerWithPhysicalLocked(physicalActive bool) bool {
	trigger := m.physicalTriggerFromLocked(physicalActive)
	switch m.be.forceLid() {
	case "closed":
		trigger = true
	case "open":
		// 仅物理维度（headless 机仍可用；盒盖信号被旋钮压制）
	default:
		if closed, known := m.be.lidClosed(); known && closed {
			trigger = true
		}
	}
	return trigger
}

// releaseWithPhysicalLocked 拆屏判定（物理状态已采样）：
//   - 真实 lid 已知：开盖即时拆、合盖保持（事件即事实，无滞回）；
//   - lid 未知（无 lid 事件的机器）：物理输出回归且滞回满足才拆；
//   - 旋钮 closed 强制保持，open 仅按物理维度。
func (m *Manager) releaseWithPhysicalLocked(physicalActive bool) bool {
	switch m.be.forceLid() {
	case "closed":
		return false // 旋钮强制合盖：保持插屏
	case "open":
		// 仅物理维度（headless 机仍可用；盒盖信号被旋钮压制）
	default:
		if closed, known := m.be.lidClosed(); known {
			return !closed // 真实 lid：开盖即拆、合盖保持
		}
	}
	return m.physicalReleaseFromLocked(physicalActive)
}

// triggerLocked SessionStarted 路径的建屏判定：自行采样一次物理输出
// （调用方持 mu）。
func (m *Manager) triggerLocked() bool {
	physicalActive := m.be.physicalOutputActive()
	m.updatePhysicalStreaksLocked(physicalActive)
	return m.triggerWithPhysicalLocked(physicalActive)
}

// ensureDeviceLocked 设备常驻（用户策略：设备可常驻后台）：进程生命周期
// 内创建一次、不随会话拆建（持有进程 EOF 生命周期，agent 退出即自动
// 移除）。驱动未装 = 静默 no-op（后续会话/触发/手动 on 时重试）。
func (m *Manager) ensureDeviceLocked() {
	if !m.be.featureEnabled() {
		return // 总开关关：不创建常驻设备（更不会插屏）
	}
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
	if !m.be.featureEnabled() {
		return errDisabled
	}
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
// 拔屏设 3s 上界（驱动侧异常可能让 IOCTL 长阻塞，2026-09-10 停机挂起
// 排查）——超时放弃拔屏继续拆设备；进程正在退出，滞留 goroutine 无碍。
func (m *Manager) teardownLocked() {
	if m.handle == 0 {
		m.plugged = false
		return
	}
	if m.plugged {
		done := make(chan struct{})
		go func() {
			defer close(done)
			if err := m.be.unplugMonitor(m.handle); err != nil {
				m.logger().Warn("display: unplug failed", "err", err)
			}
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			m.logger().Warn("display: unplug timed out during teardown; proceeding with device removal")
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
// 总开关关时同样被门禁（任何条件下不建屏）。
func (m *Manager) SetOn() error {
	if !m.be.featureEnabled() {
		return errDisabled
	}
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
		Enabled:         m.be.featureEnabled(),
	}
	st.LidClosed, st.LidKnown = m.be.lidClosed()
	return st
}

// errNotInstalled 驱动未入 store（idd 组件未装/装失败）——状态而非故障。
var errNotInstalled = &notInstalledError{}

// errDisabled 功能总开关关闭（2026-09-11 用户决策：组件保留、缺省
// 停用；XNC_IDD_ENABLED=1 显式重新启用）。
var errDisabled = errors.New("idd feature disabled (set XNC_IDD_ENABLED=1 to enable)")

type notInstalledError struct{}

func (e *notInstalledError) Error() string {
	return "not_installed: idd driver package not in store (component 'idd' not installed)"
}
