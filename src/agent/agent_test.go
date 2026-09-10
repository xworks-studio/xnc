package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"xnc/agent/machineinfo"
)

// dev 覆盖只影响显式设置的字段；空覆盖 = 生产行为不变。
func TestInfoOverrides(t *testing.T) {
	a := &Agent{HostnameOverride: "XIAOXIN-DEV", MachineIDOverride: "dev-abc-XIAOXIN-DEV"}
	i := a.info()
	assert.Equal(t, "XIAOXIN-DEV", i.Hostname)
	assert.Equal(t, "dev-abc-XIAOXIN-DEV", i.MachineID)

	prod := (&Agent{}).info()
	raw := machineinfo.Collect()
	assert.Equal(t, raw.Hostname, prod.Hostname)
	assert.Equal(t, raw.MachineID, prod.MachineID)
}
