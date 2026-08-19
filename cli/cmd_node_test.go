package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
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

func TestNodeShowAmbiguousListsCandidates(t *testing.T) {
	// production first on purpose: candidates must be sorted by cluster then id.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"id":"11111111-1111-4111-8111-111111111111","name":"web-01","cluster":"production",
			 "hostname":"WEB-01","os_version":"Windows","agent_version":"0.1.0",
			 "shell_type":"pwsh","status":"online","last_seen_at":null},
			{"id":"22222222-2222-4222-8222-222222222222","name":"web-01","cluster":"lab",
			 "hostname":"WEB-01","os_version":"Linux","agent_version":"0.1.0",
			 "shell_type":"bash","status":"offline","last_seen_at":null}]`))
	}))
	defer srv.Close()

	wantMsg := `ambiguous node "web-01": web-01 (lab, 22222222-2222-4222-8222-222222222222), ` +
		`web-01 (production, 11111111-1111-4111-8111-111111111111); use a UUID or cluster/name`

	// JSON mode: exit 244, both candidates in the envelope error.message.
	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"node", "show", "web-01", "--json",
			"--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 244, code)
	want, err := json.Marshal(map[string]any{"ok": false, "data": nil,
		"error": map[string]string{"code": proto.CodeNodeNotFound, "message": wantMsg}})
	require.NoError(t, err)
	assert.JSONEq(t, string(want), out)

	// Table mode: same message (with both candidate ids) on stderr.
	stderr, code2 := captureStderr(t, func() int {
		return runCLI(t.Context(), []string{"node", "show", "web-01",
			"--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 244, code2)
	assert.Contains(t, stderr, wantMsg)
	assert.Contains(t, stderr, "22222222-2222-4222-8222-222222222222")
	assert.Contains(t, stderr, "11111111-1111-4111-8111-111111111111")
}
