//go:build windows

// cmd_idd_probe.go — 隐藏命令 `xnc idd-probe`：从当前会话枚举显示设备，
// stdout 输出单行 JSON {"total":N,"physicalActive":bool,"virtualActive":bool}。
//
// 背景（2026-09-10 XIAOXIN 实测）：agent 是 session 0 服务，GDI/DXGI
// 枚举在服务会话恒空——"物理输出是否在场"无法自测。且该机盒盖不产生
// 任何可观测事件（无 GUID_LIDSWITCH_STATE_CHANGE 电源通知、无 LIDACTION
// 设置项、桌面拓扑不变）。"无物理输出"触发改由 agent 在活动控制台会话
// 拉起本命令读取真实拓扑（见 src/agent/display/probe_windows.go）。
// 无参数、无网络、无凭据；虚拟屏自身不计入物理输出。
package main

import (
	"fmt"
	"unsafe"

	"github.com/spf13/cobra"

	"golang.org/x/sys/windows"
)

const (
	iddDisplayDeviceActive    = 0x1
	iddDisplayDeviceMirroring = 0x8
	// virtualDisplayName 与驱动 EDID 显示器名一致（镜像 agent 侧
	// display.virtualDisplayName），用于把虚拟屏从物理输出中排除。
	virtualDisplayName = "XWorks XNC Virtual Display"
)

type iddDisplayDevice struct {
	cb    uint32
	name  [32]uint16
	str   [128]uint16
	flags uint32
	id    [128]uint16
	key   [128]uint16
}

func newIddProbeCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "idd-probe",
		Short:  "report display topology of this session (internal, spawned by the agent)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			total, physical, virtual := probeDisplays()
			fmt.Printf("{\"total\":%d,\"physicalActive\":%v,\"virtualActive\":%v}\n", total, physical, virtual)
			return nil
		},
	}
}

// probeDisplays 两级枚举：NULL 级 = 适配器视图（合盖时其标志不变，
// 2026-09-11 YOGA9 70 样本实测恒 true——面板 EDID 掉线不反映到这一层）；
// 监视器级（lpDevice = \\.\DISPLAYn）才反映面板在位——合盖时监视器条目
// 从枚举消失（用户 watch-display.ps1 实证 = 这类无 lid 事件机器上的
// 合盖信号）。total/physicalActive/virtualActive 均按监视器计。
func probeDisplays() (total int, physicalActive, virtualActive bool) {
	procEnum := windows.NewLazySystemDLL("user32.dll").NewProc("EnumDisplayDevicesW")
	for i := uint32(0); ; i++ {
		dev := iddDisplayDevice{cb: uint32(unsafe.Sizeof(iddDisplayDevice{}))}
		r1, _, _ := procEnum.Call(0, uintptr(i), uintptr(unsafe.Pointer(&dev)), 0)
		if r1 == 0 {
			return
		}
		// lpDevice 指向本适配器的 \\.\DISPLAYn 名（本地数组，调用期间有效）。
		for j := uint32(0); ; j++ {
			mon := iddDisplayDevice{cb: uint32(unsafe.Sizeof(iddDisplayDevice{}))}
			r2, _, _ := procEnum.Call(uintptr(unsafe.Pointer(&dev.name[0])), uintptr(j),
				uintptr(unsafe.Pointer(&mon)), 0)
			if r2 == 0 {
				break
			}
			total++
			if mon.flags&iddDisplayDeviceActive == 0 {
				continue
			}
			if windows.UTF16ToString(mon.str[:]) == virtualDisplayName {
				virtualActive = true
				continue
			}
			if mon.flags&iddDisplayDeviceMirroring != 0 {
				continue
			}
			physicalActive = true
		}
	}
}
