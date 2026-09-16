// powercfg.go — xnc register 附带的电源策略设置(2026-09-16 设计)。
//
// 远程管理节点的要求:显示器必须保持可见(DXGI 采集依赖),合盖不能触发
// 睡眠/关机。在注册时以管理员权限写入三条 powercfg 策略(幂等,每次
// register 重跑无害):
//
//   1. 合盖动作 = 不做任何事(LIDACTION,AC+DC 都设 0)
//   2. 插电时关闭显示器 = 从不(VIDEOIDLE AC=0)
//   3. 电池时关闭显示器 = 从不(VIDEOIDLE DC=0)
//
// 设置失败(非管理员/策略被组策略锁)仅告警不阻断注册——电源策略是体验
// 优化而非注册前置;失败时提示用户手动配置。
package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// applyPowerSettings 幂等地设置远程管理友好的电源策略。
// 返回 (成功项列表, 失败项列表)。
func applyPowerSettings() ([]string, []string) {
	var applied, failed []string

	// powercfg /setacvalueindex SCHEME_CURRENT SUB_BUTTONS LIDACTION 0
	// powercfg /setdcvalueindex SCHEME_CURRENT SUB_BUTTONS LIDACTION 0
	// powercfg /setacvalueindex SCHEME_CURRENT SUB_VIDEO VIDEOIDLE 0
	// powercfg /setdcvalueindex SCHEME_CURRENT SUB_VIDEO VIDEOIDLE 0
	// powercfg /setactive SCHEME_CURRENT
	settings := []struct {
		desc string
		args []string
	}{
		{"lid close = do nothing (AC)", []string{"/setacvalueindex", "SCHEME_CURRENT", "SUB_BUTTONS", "LIDACTION", "0"}},
		{"lid close = do nothing (DC)", []string{"/setdcvalueindex", "SCHEME_CURRENT", "SUB_BUTTONS", "LIDACTION", "0"}},
		{"display off = never (plugged in)", []string{"/setacvalueindex", "SCHEME_CURRENT", "SUB_VIDEO", "VIDEOIDLE", "0"}},
		{"display off = never (on battery)", []string{"/setdcvalueindex", "SCHEME_CURRENT", "SUB_VIDEO", "VIDEOIDLE", "0"}},
		{"activate scheme", []string{"/setactive", "SCHEME_CURRENT"}},
	}

	for _, s := range settings {
		cmd := exec.Command("powercfg", s.args...)
		var sb strings.Builder
		cmd.Stderr = &sb
		if err := cmd.Run(); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", s.desc, strings.TrimSpace(sb.String())))
		} else {
			applied = append(applied, s.desc)
		}
	}
	return applied, failed
}
