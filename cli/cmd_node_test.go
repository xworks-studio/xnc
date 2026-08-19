package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNodeListJSONGolden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer tk", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"n1","name":"web-01","cluster":"default",
			"hostname":"WEB-01","os_version":"Windows","agent_version":"0.1.0",
			"shell_type":"pwsh","status":"online","last_seen_at":null}]`))
	}))
	defer srv.Close()

	// NOTE(brief erratum): the brief's `out := captureStdout(...)` +
	// `require.Equal(t, 0, out)` compares an int to a string; captureStdout
	// returns (stdout, exitCode) so both can be asserted.
	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"node", "list", "--json",
			"--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 0, code)
	assert.JSONEq(t, `{"ok":true,"data":[{"name":"web-01","cluster":"default",
		"status":"online","id":"n1","hostname":"WEB-01","os_version":"Windows",
		"agent_version":"0.1.0","shell_type":"pwsh","last_seen_at":null}],"error":null}`, out)
}
