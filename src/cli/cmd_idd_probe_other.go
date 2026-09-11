//go:build !windows

// cmd_idd_probe_other.go — 非 Windows：IDD 是 Windows 专属，命令保留但
// 恒失败（保证 go-linux CI 可编译）。
package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newIddProbeCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "idd-probe",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("idd-probe: windows only")
		},
	}
}
