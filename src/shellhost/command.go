// command.go — oneshot 模式命令行构造(移植自 agent/session/exec.go 的
// buildCommand 命令模式分支;脚本临时文件流程不需要——oneshot 命令经
// --command 内联)。行为与 agent/session 逐参数对齐(单测矩阵互证):
//
//	bash       → <exe> -c <prefix+command>
//	cmd        → cmd /c <prefix+command>
//	pwsh/ps    → <exe> -NoLogo -NonInteractive [-ExecutionPolicy Bypass]
//	             -Command <prefix+command>
//
// 环境变量经 shell 语法前缀注入(envPrefix 同源复制)。
package main

import (
	"fmt"
	"os/exec"
	"strings"
)

// buildOneshotCommand 按已解析的 profile/exe 构造 oneshot 命令。
func buildOneshotCommand(profile, exe, command string, env []string) (*exec.Cmd, error) {
	prefix := envPrefix(profile, env)
	switch profile {
	case "BASH":
		return exec.Command(exe, "-c", prefix+command), nil
	case "CMD":
		return exec.Command(exe, "/c", prefix+command), nil
	case "PWSH", "POWERSHELL":
		// -ExecutionPolicy Bypass:oneshot 内联 -Command 亦受策略影响的
		// 边缘场景与 agent 行为保持一致(agent 脚本模式携带;此处随行)。
		return exec.Command(exe, "-NoLogo", "-NonInteractive", "-ExecutionPolicy", "Bypass",
			"-Command", prefix+command), nil
	}
	return nil, fmt.Errorf("unsupported profile %q", profile)
}

// envPrefix 按 shell 语法构建环境变量注入前缀(复制自 agent/session)。
func envPrefix(profile string, env []string) string {
	if len(env) == 0 {
		return ""
	}
	shell := map[string]string{
		"BASH": "bash", "CMD": "cmd", "PWSH": "pwsh", "POWERSHELL": "pwsh",
	}[profile]
	var sb strings.Builder
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch shell {
		case "bash":
			sb.WriteString(fmt.Sprintf("export %s='%s'; ", k, v))
		case "cmd":
			sb.WriteString(fmt.Sprintf("set %s=%s&& ", k, v))
		default: // pwsh / powershell
			sb.WriteString(fmt.Sprintf("$env:%s='%s'; ", k, v))
		}
	}
	return sb.String()
}
