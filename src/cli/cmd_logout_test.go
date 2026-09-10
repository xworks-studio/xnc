package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// isolateHome points ~/.xnc at a temp dir and clears auth env overrides so a
// developer's real session cannot leak into the test (same seam as
// cmd_auth_test.go).
func isolateHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOME", dir) // os.UserHomeDir fallback on unix
	t.Setenv("XNC_SERVER", "")
	t.Setenv("XNC_TOKEN", "")
	return dir
}

// writeConfig is shared with cmd_register_test.go.

// Case 1+4: token present → cleared; server, remembered_email and channel
// survive (spec §7: 删 JWT，保留 remembered_email 与 channel).
func TestLogoutClearsTokenKeepsRememberedEmailAndChannel(t *testing.T) {
	dir := isolateHome(t)
	writeConfig(t, dir, `{"server": "https://s.example",
		"token": "jwt-1", "remembered_email": "a@b.c", "channel": "dev"}`)

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"logout"})
	})
	require.Equal(t, exitOK, code)
	assert.Contains(t, out, "Logged out")
	assert.Contains(t, out, "a@b.c") // confirmation includes remembered email

	b, err := os.ReadFile(filepath.Join(dir, ".xnc", "config.json"))
	require.NoError(t, err)
	assert.NotContains(t, string(b), "jwt-1")
	assert.Contains(t, string(b), `"remembered_email": "a@b.c"`)
	assert.Contains(t, string(b), `"channel": "dev"`)
	assert.Contains(t, string(b), "https://s.example")
}

// Case 2: config exists without a token → "not logged in", exit 0, file
// untouched (idempotent).
func TestLogoutWithoutTokenIsIdempotent(t *testing.T) {
	dir := isolateHome(t)
	writeConfig(t, dir, `{"server": "https://s.example"}`)

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"logout"})
	})
	require.Equal(t, exitOK, code)
	assert.Contains(t, out, "not logged in")

	b, err := os.ReadFile(filepath.Join(dir, ".xnc", "config.json"))
	require.NoError(t, err)
	assert.JSONEq(t, `{"server": "https://s.example"}`, string(b))
}

// Case 3: no config file at all → same "not logged in", and logout must not
// create ~/.xnc (nothing to save).
func TestLogoutWithNoConfigFile(t *testing.T) {
	dir := isolateHome(t)

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"logout"})
	})
	require.Equal(t, exitOK, code)
	assert.Contains(t, out, "not logged in")

	_, err := os.Stat(filepath.Join(dir, ".xnc"))
	assert.True(t, os.IsNotExist(err), "logout must not create a config file")
}

// JSON mode: single envelope on stdout for both the logged-out and the
// not-logged-in (idempotent success) paths.
func TestLogoutJSONEnvelope(t *testing.T) {
	dir := isolateHome(t)
	writeConfig(t, dir, `{"server": "https://s.example",
		"token": "jwt-1", "remembered_email": "a@b.c"}`)

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"logout", "--json"})
	})
	require.Equal(t, exitOK, code)
	assert.JSONEq(t, `{"ok": true,
		"data": {"logged_out": true, "remembered_email": "a@b.c"},
		"error": null}`, out)

	out2, code2 := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"logout", "--json"})
	})
	require.Equal(t, exitOK, code2) // second run: still success (idempotent)
	assert.JSONEq(t, `{"ok": true, "data": {"logged_out": false}, "error": null}`, out2)
}

// XNC_TOKEN env override: the file token is still cleared, but the output
// notes that the env var keeps overriding until unset.
func TestLogoutNotesTokenEnvOverride(t *testing.T) {
	dir := isolateHome(t)
	writeConfig(t, dir, `{"server": "https://s.example", "token": "jwt-1"}`)
	t.Setenv("XNC_TOKEN", "env-jwt")

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"logout"})
	})
	require.Equal(t, exitOK, code)
	assert.Contains(t, out, "Logged out")
	assert.Contains(t, out, "XNC_TOKEN")

	b, err := os.ReadFile(filepath.Join(dir, ".xnc", "config.json"))
	require.NoError(t, err)
	assert.NotContains(t, string(b), "jwt-1")
}
