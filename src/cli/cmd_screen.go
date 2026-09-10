// cmd_screen.go — `xnc screen` 退役桩。
//
// 2026-09-08 RTV 重构后 screen 会话整体退役：
// - 流式（--open）：已由 KindDesktop 实时桌面会话取代（web /desktop 或
//   `xnc rdp`），此前以 SCREEN_STREAM_RETIRED 稳定码拒绝。
// - 快照（--snapshot）：agent 侧 xnc-core 0x0111 --jpeg-single 通道随
//   C++ 栈删除，此前恒回 SCREEN_SNAPSHOT_UNSUPPORTED；恢复列入 RTV 设计
//   文档后续 PATCH 清单。
// 命令保留为桩（旧脚本/习惯命令得到清晰退役提示而非 "unknown command"），
// flags 亦保留以便旧调用形态（--snapshot f.jpg / --open）同样命中提示。
package main

import (
	"github.com/spf13/cobra"
)

func newScreenCmd() *cobra.Command {
	var snapshotPath string
	var openBrowser bool
	cmd := &cobra.Command{
		Use:   "screen <node> [--snapshot <file.jpg>|--open]",
		Short: "Retired: screen sessions were replaced by desktop sessions (RTV)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return failUsage(cmd,
				"screen sessions are retired (RTV rewrite, 2026-09-08); "+
					"use a desktop session instead: web /desktop/<node> or `xnc rdp <node>`")
		},
	}
	// flags 保留仅作兼容：任何组合都走同一退役提示（见 RunE）。
	cmd.Flags().StringVar(&snapshotPath, "snapshot", "",
		"retired: snapshot support removed with the RTV rewrite")
	cmd.Flags().BoolVar(&openBrowser, "open", false,
		"retired: live preview replaced by desktop sessions")
	addJSONFlag(cmd)
	return cmd
}
