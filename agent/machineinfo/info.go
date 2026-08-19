// Package machineinfo 采集节点环境信息（主机名、机器 ID、OS 版本、shell 类型）。
package machineinfo

import (
	"os"
	"os/exec"
	"runtime"
)

const Version = "0.1.0"

type Info struct {
	Hostname     string
	MachineID    string
	OSVersion    string
	AgentVersion string
	ShellType    string
}

func Collect() Info {
	i := Info{AgentVersion: Version}
	i.Hostname, _ = os.Hostname()
	i.MachineID = machineID()
	i.OSVersion = osVersion()
	i.ShellType = shellType()
	return i
}

func shellType() string {
	if _, err := exec.LookPath("pwsh"); err == nil {
		return "pwsh"
	}
	if runtime.GOOS == "windows" {
		return "windows-powershell"
	}
	return "bash"
}
