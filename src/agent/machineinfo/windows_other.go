//go:build !windows && !linux

// windows_other.go — 其余平台（darwin 等开发调试形态）的桩：machineID
// 空串（注册侧可见拒绝，见 linux.go 注释），osVersion 退化为 GOOS。
// Linux 实现在 linux.go，Windows 在 windows_windows.go。
package machineinfo

import "runtime"

func machineID() string { return "" }

func osVersion() string { return runtime.GOOS }
