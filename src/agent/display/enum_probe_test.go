//go:build windows

package display

import "testing"

// 开发机探针：enumDisplays 应在有物理屏的机器上报 physicalActive=true。
func TestEnumDisplaysProbe(t *testing.T) {
	p, v := enumDisplays()
	t.Logf("physicalActive=%v virtualActive=%v", p, v)
}
