package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoginSavesConfigAndEnvelope(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOME", dir) // os.UserHomeDir fallback on unix

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/auth/login", r.URL.Path)
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		assert.Contains(t, string(b), `"email":"a@b.c"`)
		assert.Contains(t, string(b), `"password":"s3cret"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"jwt-1","user":{"id":"u1","email":"a@b.c","display_name":"Admin"}}`))
	}))
	defer srv.Close()

	pr, pw, _ := os.Pipe()
	_, _ = pw.WriteString("s3cret\n")
	_ = pw.Close()
	oldStdin := os.Stdin
	os.Stdin = pr
	t.Cleanup(func() { os.Stdin = oldStdin })

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"login", "--json",
			"--server", srv.URL, "--email", "a@b.c"})
	})
	require.Equal(t, 0, code)
	// E2E extracts the token from the envelope with sed "token":"\([^"]*\)".
	assert.Contains(t, out, `"token":"jwt-1"`)
	assert.Contains(t, out, srv.URL)

	cfg, err := os.ReadFile(filepath.Join(dir, ".xnc", "config.json"))
	require.NoError(t, err)
	assert.Contains(t, string(cfg), `"token": "jwt-1"`)
}

func TestAPIErrorEnvelopeAndExitCodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"NODE_NOT_FOUND","message":"nope"}}`))
	}))
	defer srv.Close()

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"node", "show",
			"11111111-2222-4333-8444-555555555555", "--json",
			"--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 244, code)
	assert.JSONEq(t, `{"ok":false,"data":null,"error":{"code":"NODE_NOT_FOUND","message":"nope"}}`, out)

	// Network failure: client maps to code NETWORK, exit 245.
	out2, code2 := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"node", "list", "--json",
			"--server", "http://127.0.0.1:1", "--token", "tk"})
	})
	require.Equal(t, 245, code2)
	assert.Contains(t, out2, `"code":"NETWORK"`)
}

func TestUsageErrors(t *testing.T) {
	// Isolate the home dir: a developer's real ~/.xnc/config.json (written by
	// xnc login or an E2E run) would give node list a server+token, turning
	// these usage errors into network dials (exit 245/0) instead of exit 2.
	dir := t.TempDir()
	t.Setenv("USERPROFILE", dir)
	t.Setenv("HOME", dir) // os.UserHomeDir fallback on unix

	// Covering guard: a config with a server (but no token) in the isolated
	// home must still yield usage exit 2 (missing token, never a dial). If the
	// isolation above regresses, the real home's full config is read instead
	// and this test goes red on any machine that has logged in.
	err := os.MkdirAll(filepath.Join(dir, ".xnc"), 0o700)
	require.NoError(t, err)
	err = os.WriteFile(filepath.Join(dir, ".xnc", "config.json"),
		[]byte(`{"server": "http://127.0.0.1:1"}`), 0o600)
	require.NoError(t, err)

	// Missing server is a usage error (exit 2), not an API error.
	_, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"node", "list"})
	})
	require.Equal(t, exitUsage, code)

	// Unknown flag: cobra usage error -> exit 2.
	_, code2 := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"node", "list", "--bogus"})
	})
	require.Equal(t, exitUsage, code2)
}
