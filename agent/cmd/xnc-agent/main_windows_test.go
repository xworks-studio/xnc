//go:build windows

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// 回归覆盖（评审 Critical）：服务参数必须是逐词的 argv 元素。
// 修复前是单个拼好的字符串，被 CreateService 整体引用成一个 argv 词，
// 服务启动后 os.Args[1] 为整串、cobra 报未知命令，服务启动失败。
func TestBuildServiceArgs(t *testing.T) {
	t.Run("no token", func(t *testing.T) {
		got := buildServiceArgs("http://127.0.0.1:8080", "", `C:\ProgramData\XNCAgent`, "", "", "")
		assert.Equal(t, []string{
			"run",
			"--server=http://127.0.0.1:8080",
			`--state-dir=C:\ProgramData\XNCAgent`,
		}, got)
	})

	t.Run("with token appended last", func(t *testing.T) {
		got := buildServiceArgs("https://xnc.example", "tok-123", `C:\ProgramData\XNCAgent`, "", "", "")
		assert.Equal(t, []string{
			"run",
			"--server=https://xnc.example",
			`--state-dir=C:\ProgramData\XNCAgent`,
			"--token=tok-123",
		}, got)
	})

	t.Run("every element is a single argv word", func(t *testing.T) {
		for _, el := range buildServiceArgs("http://s", "t", `D:\State Dir\XNCAgent`, "", "", "") {
			assert.NotContains(t, el, " --",
				"joined-string shape regressed: element %q contains multiple flags", el)
		}
	})

	// M2-Slice3 Task 2: dev profile(XNCAgentDev + 隔离 state 目录 + core
	// pipe 凭据)整体进 argv;生产名 XNCAgent 不追加 --service-name(默认)。
	t.Run("dev profile", func(t *testing.T) {
		got := buildServiceArgs("http://10.0.0.5:8080", "tok", `C:\ProgramData\XNCAgentDev`,
			"XNCAgentDev", `\\.\pipe\xnc-core-dev`, "746573")
		assert.Equal(t, []string{
			"run",
			"--server=http://10.0.0.5:8080",
			`--state-dir=C:\ProgramData\XNCAgentDev`,
			"--service-name=XNCAgentDev",
			`--desktop-core-pipe=\\.\pipe\xnc-core-dev`,
			"--desktop-core-secret-hex=746573",
			"--token=tok",
		}, got)
	})
}
