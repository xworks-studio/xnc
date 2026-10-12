//go:build !windows

// powercfg_other.go — 电源策略设置是 Windows 概念（DXGI 采集/合盖语义），
// Linux 节点无对应物，直接跳过（register 流程保持零噪音）。
package main

func applyPowerSettings() ([]string, []string) { return nil, nil }
