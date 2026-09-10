package main

// xnc display on|off|status（2026-09-10 IDD 虚拟显示器本地控制）：
// 经 agent 控制管道 display op 驱动（注册态无关——本机显示器控制）。
// 自动策略见 agent/display 包：desktop 会话接入且（盒盖 ∨ 无物理输出 ∨
// 调试旋钮）时自动建屏，会话结束自动移除；本命令是手动覆盖。

import (
	"fmt"

	"github.com/spf13/cobra"
)

// agentctlOpDisplay 镜像 agent/agentctl.OpDisplay。
const agentctlOpDisplay = "display"

func newDisplayCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "display on|off|status",
		Short: "Control the virtual display (XWorks XNC Virtual Display) on this machine",
		Long: `Control the IDD virtual display on this machine (component "idd").

  xnc display on      create the virtual display (1920x1080@60) and keep it
                      until turned off or the agent restarts
  xnc display off     remove the virtual display
  xnc display status  show driver / virtual-display / lid state

The virtual display is normally created automatically while a remote desktop
session is active and the lid is closed (or no physical display is attached)
and removed when the session ends; these commands are the manual override.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			action := args[0]
			switch action {
			case "on", "off", "status":
			default:
				return failUsage(cmd, "argument must be on, off or status")
			}
			resp, err := agentctlCall(agentctlReq{Op: agentctlOpDisplay, Action: action})
			if err != nil {
				return failAPI(cmd, errPipeUnreachable(err))
			}
			if !resp.OK {
				return failPipeOp(cmd, resp)
			}
			if jsonOut(cmd) {
				PrintJSON(true, resp.Display, nil)
				return nil
			}
			switch action {
			case "on":
				fmt.Println("virtual display on")
			case "off":
				fmt.Println("virtual display off")
			}
			printDisplayStatus(resp.Display)
			return nil
		},
	}
	addJSONFlag(cmd)
	return cmd
}

// printDisplayStatus 状态表格（lid 未知 = "-"）。
func printDisplayStatus(d *agentctlDisplay) {
	if d == nil {
		fmt.Println("display: no status returned")
		return
	}
	lid := "-"
	if d.LidKnown {
		lid = map[bool]string{true: "closed", false: "open"}[d.LidClosed]
	}
	pairs := [][2]string{
		{"driver installed", yesNo(d.DriverInstalled)},
		{"virtual active", yesNo(d.VirtualActive)},
		{"physical active", yesNo(d.PhysicalActive)},
		{"lid", lid},
		{"auto (session)", yesNo(d.AutoActive)},
		{"manual", yesNo(d.ManualActive)},
	}
	if d.ForceLid != "" {
		pairs = append(pairs, [2]string{"force lid (debug)", d.ForceLid})
	}
	printKV(pairs)
}

// yesNo 渲染布尔为 yes/no（状态表格惯例）。
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
