//go:build !windows

package machineinfo

import "os/exec"

// DetectShells 按优先级探测可用 shell：bash > sh > zsh > pwsh。
// 兜底 sh（POSIX 恒有；exec 'auto' 取首选，与 Windows 兜底语义一致）。
func DetectShells() []string {
	var shells []string
	for _, name := range []string{"bash", "sh", "zsh", "pwsh"} {
		if _, err := exec.LookPath(name); err == nil {
			shells = append(shells, name)
		}
	}
	if len(shells) == 0 {
		return []string{"sh"}
	}
	return shells
}

// FindBash 非 Windows：PATH 上的 bash 即答案（无 Git Bash/WSL 探测链）。
func FindBash() string {
	if p, err := exec.LookPath("bash"); err == nil {
		return p
	}
	return ""
}
