package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"xnc/proto"
)

// TestShellRequiresTTY: go test's stdin is a pipe (not a TTY), so `xnc shell`
// must refuse with a usage error before any network traffic. failUsage prints
// the message on stderr (table mode), matching the exec table-mode pattern.
func TestShellRequiresTTY(t *testing.T) {
	stderr, code := captureStderr(t, func() int {
		return runCLI(t.Context(), []string{"shell", "n1",
			"--server", "http://127.0.0.1:1", "--token", "tk"})
	})
	assert.Equal(t, 2, code)
	assert.Contains(t, stderr, "interactive terminal")
	assert.Contains(t, stderr, "xnc exec")
}

// TestShellSessionLimitedMaps246: SESSION_LIMIT_EXCEEDED (per-node shell cap
// hit on POST /shell) maps to the quota exit code, same construction style as
// exit_test.go.
func TestShellSessionLimitedMaps246(t *testing.T) {
	assert.Equal(t, 246, ExitCode(proto.Err(409, proto.CodeSessionLimited, "")))
}
