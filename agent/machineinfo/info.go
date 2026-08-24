// Package machineinfo 采集节点环境信息（主机名、机器 ID、OS 版本、可用 shell 列表）。
package machineinfo

import (
	"os"
	"os/exec"
	"runtime"
)

const Version = "0.4.2" // bundle 版本（agent+helper 整体发布；自更新的比对单位）

type Info struct {
	Hostname     string
	MachineID    string
	OSVersion    string
	AgentVersion string
	ShellType    string   // 首选 shell（兼容旧字段）
	Shells       []string // 全部可用 shell（exec --shell 的合法值）
}

func Collect() Info {
	i := Info{AgentVersion: Version}
	i.Hostname, _ = os.Hostname()
	i.MachineID = machineID()
	i.OSVersion = osVersion()
	i.Shells = DetectShells()
	if len(i.Shells) > 0 {
		i.ShellType = i.Shells[0]
	}
	return i
}

// DetectShells 按优先级探测可用 shell：bash > pwsh > powershell > cmd。
// bash 覆盖 Git Bash（PATH）与 WSL（System32\bash.exe）。
func DetectShells() []string {
	var shells []string
	if FindBash() != "" {
		shells = append(shells, "bash")
	}
	if _, err := exec.LookPath("pwsh"); err == nil {
		shells = append(shells, "pwsh")
	}
	if _, err := exec.LookPath("powershell"); err == nil {
		shells = append(shells, "powershell")
	}
	if runtime.GOOS == "windows" {
		shells = append(shells, "cmd") // cmd.exe 永远存在
	}
	if len(shells) == 0 {
		shells = []string{"sh"} // 非 Windows 兜底
	}
	return shells
}

// FindBash 定位 bash.exe：PATH → Git Bash 常见路径 → WSL。
// 返回空串表示不可用。
func FindBash() string {
	// 1. PATH 里的 bash / bash.exe
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	if p, err := exec.LookPath("bash.exe"); err == nil {
		return p
	}
	// 2. Git for Windows 常见安装路径
	for _, p := range []string{
		`C:\Program Files\Git\bin\bash.exe`,
		`C:\Program Files (x86)\Git\bin\bash.exe`,
		os.Getenv("LOCALAPPDATA") + `\Programs\Git\bin\bash.exe`,
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// 3. WSL bash（System32\bash.exe）——仅探测，不引入 windows 包依赖
	if p := os.Getenv("SystemRoot") + `\System32\bash.exe`; p != `\System32\bash.exe` {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
