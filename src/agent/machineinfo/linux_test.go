//go:build linux

package machineinfo

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseOSRelease(t *testing.T) {
	fixture := "NAME=\"Ubuntu\"\nVERSION=\"24.04.4 LTS (Noble Numbat)\"\nPRETTY_NAME=\"Ubuntu 24.04.4 LTS\"\nID=ubuntu\nHOME_URL=\"https://www.ubuntu.com/\"\n"
	assert.Equal(t, "Ubuntu 24.04.4 LTS", parseOSRelease(fixture, "PRETTY_NAME"))
	assert.Equal(t, "ubuntu", parseOSRelease(fixture, "ID"))
	assert.Equal(t, "", parseOSRelease(fixture, "MISSING"))
	// 前缀精确匹配：KEY0= 不得命中 KEY=。
	assert.Equal(t, "", parseOSRelease("KEY0=v\n", "KEY"))
}

func TestLinuxCollect(t *testing.T) {
	i := Collect()
	if i.MachineID == "" {
		t.Skip("/etc/machine-id 与 dbus 回退均不可用（容器最小环境）")
	}
	assert.NotContains(t, i.MachineID, "\n", "machine-id 须去除尾部换行")
	assert.NotContains(t, i.MachineID, "nonwindows-", "不得回落 hostname 拼接")
	assert.NotEmpty(t, i.Shells, "POSIX 兜底 sh")
	assert.Contains(t, i.Shells[:1], i.ShellType)
}
