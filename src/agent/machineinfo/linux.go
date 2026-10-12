//go:build linux

package machineinfo

import (
	"os"
	"strings"
)

// machineID 读 /etc/machine-id（systemd 惯例，重装/克隆才会变）；
// 回退 /var/lib/dbus/machine-id（二者通常互为符号链接）。取不到返回
// 空串——注册侧可见拒绝——绝不回落 hostname（改名即换 ID，孤儿化注册）。
func machineID() string {
	for _, p := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := os.ReadFile(p); err == nil {
			if s := strings.TrimSpace(string(b)); s != "" {
				return s
			}
		}
	}
	return ""
}

// osVersion 解析 /etc/os-release 的 PRETTY_NAME（如 "Ubuntu 24.04.4 LTS"）。
func osVersion() string {
	if s := osReleaseField("PRETTY_NAME"); s != "" {
		return s
	}
	return "Linux"
}

// osReleaseField 解析 /etc/os-release 的 KEY=VALUE 行（值可带引号）。
func osReleaseField(key string) string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	return parseOSRelease(string(b), key)
}

// parseOSRelease 纯解析（供测试）：前缀精确匹配 KEY=，值去引号；缺失返回空。
func parseOSRelease(content, key string) string {
	for _, line := range strings.Split(content, "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return ""
}
