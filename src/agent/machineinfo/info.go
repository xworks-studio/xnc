// Package machineinfo 采集节点环境信息（主机名、机器 ID、OS 版本、可用 shell 列表）。
package machineinfo

import "os"

// Version 是 bundle 版本（agent 整体发布；自更新的比对单位），构建时经
// ldflags 注入（版本单一来源，与发布 bundle 版本保持一致）：
//
//	go build -ldflags "-X xnc/agent/machineinfo.Version=0.4.6" ./cmd/xnc-agent
//
// 未注入（本地开发构建）回落 0.0.0-dev。
var Version = "0.0.0-dev"

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
