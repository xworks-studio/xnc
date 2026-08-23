// profile.go — shell profile 白名单解析(spec §8.2, M2-Slice2 Task 2)。
//
// xnc-shell 接受 --profile <name>(白名单:POWERSHELL/PWSH/CMD/BASH),
// 自己解析可执行文件路径,绝不接受路径参数。CMD/POWERSHELL 在 Windows
// 恒存在;PWSH/BASH 探测,缺失 → 明确报错 + exit 2(core 侧同理拒绝)。
//
// FindBash 探测链(复制自 agent/machineinfo,不 import agent 模块):
// PATH → Git Bash 常见路径 → WSL bash。
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Profiles 白名单大小写不敏感别名。
var profileAliases = map[string]string{
	"powershell": "POWERSHELL",
	"pwsh":       "PWSH",
	"cmd":        "CMD",
	"bash":       "BASH",
}

// normalizeProfile 白名单校验;非白名单名字返回错误(绝不当作路径)。
func normalizeProfile(name string) (string, error) {
	if p, ok := profileAliases[strings.ToLower(strings.TrimSpace(name))]; ok {
		return p, nil
	}
	return "", fmt.Errorf("unsupported profile %q (whitelist: POWERSHELL, PWSH, CMD, BASH)", name)
}

// lookPathProc 可注入的探测函数(单测用)。
var lookPathProc = exec.LookPath

// statProc 可注入的 stat(单测用)。
var statProc = os.Stat

// resolveProfile 解析 profile → exe 绝对/可执行路径。PWSH/BASH 缺失返回
// 错误(调用方 exit 2);CMD/POWERSHELL 缺失属系统损坏,同样报错。
func resolveProfile(profile string) (string, error) {
	switch profile {
	case "CMD":
		return lookPathProc("cmd.exe")
	case "POWERSHELL":
		return lookPathProc("powershell.exe")
	case "PWSH":
		p, err := lookPathProc("pwsh.exe")
		if err != nil {
			return "", fmt.Errorf("profile PWSH not available on this node (pwsh.exe not found)")
		}
		return p, nil
	case "BASH":
		p := findBash()
		if p == "" {
			return "", fmt.Errorf("profile BASH not available on this node (bash.exe not found)")
		}
		return p, nil
	}
	return "", fmt.Errorf("unsupported profile %q", profile)
}

// findBash 定位 bash.exe:PATH → Git Bash 常见路径 → WSL(复制自
// agent/machineinfo.FindBash)。
func findBash() string {
	if p, err := lookPathProc("bash"); err == nil {
		return p
	}
	if p, err := lookPathProc("bash.exe"); err == nil {
		return p
	}
	for _, p := range []string{
		`C:\Program Files\Git\bin\bash.exe`,
		`C:\Program Files (x86)\Git\bin\bash.exe`,
		os.Getenv("LOCALAPPDATA") + `\Programs\Git\bin\bash.exe`,
	} {
		if _, err := statProc(p); err == nil {
			return p
		}
	}
	if p := os.Getenv("SystemRoot") + `\System32\bash.exe`; p != `\System32\bash.exe` {
		if _, err := statProc(p); err == nil {
			return p
		}
	}
	return ""
}
