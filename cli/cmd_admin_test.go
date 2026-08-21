package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

const (
	adminUserID = "11111111-1111-4111-8111-111111111111"
	opsUserID   = "22222222-2222-4222-8222-222222222222"
	nodeUUID    = "33333333-3333-4333-8333-333333333333"
)

func TestClusterMemberListJSONGolden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		assert.Equal(t, "/api/clusters/default/members", r.URL.Path)
		assert.Equal(t, "Bearer tk", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"user_id":"` + adminUserID + `","email":"admin@example.com","display_name":"Admin","role":"owner"},
			{"user_id":"` + opsUserID + `","email":"ops@example.com","display_name":"Ops","role":"operator"}]`))
	}))
	defer srv.Close()

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"cluster", "member", "list", "default",
			"--json", "--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 0, code)
	assert.JSONEq(t, `{"ok":true,"data":[
		{"user_id":"`+adminUserID+`","email":"admin@example.com","display_name":"Admin","role":"owner"},
		{"user_id":"`+opsUserID+`","email":"ops@example.com","display_name":"Ops","role":"operator"}],
		"error":null}`, out)

	// Table mode: EMAIL ROLE columns per the plan contract.
	tbl, code2 := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"cluster", "member", "list", "default",
			"--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 0, code2)
	assert.Contains(t, tbl, "EMAIL")
	assert.Contains(t, tbl, "ROLE")
	assert.Contains(t, tbl, "admin@example.com")
	assert.Contains(t, tbl, "operator")
}

func TestClusterMemberAddThenRemove(t *testing.T) {
	var addBody map[string]any
	var removedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "POST" && r.URL.Path == "/api/clusters/default/members":
			_ = readJSONBody(t, r, &addBody)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"user_id":"` + opsUserID + `","role":"operator"}`))
		case r.Method == "DELETE":
			removedPath = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"cluster", "member", "add", "default", opsUserID,
			"--role", "operator", "--json", "--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 0, code)
	assert.JSONEq(t, `{"ok":true,"data":{"user_id":"`+opsUserID+`","role":"operator"},"error":null}`, out)
	assert.Equal(t, map[string]any{"user_id": opsUserID, "role": "operator"}, addBody)

	out2, code2 := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"cluster", "member", "remove", "default", opsUserID,
			"--json", "--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 0, code2)
	assert.JSONEq(t, `{"ok":true,"data":null,"error":null}`, out2)
	assert.Equal(t, "/api/clusters/default/members/"+opsUserID, removedPath)
}

func TestClusterMemberAddInvalidRoleIsUsage(t *testing.T) {
	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"cluster", "member", "add", "default", opsUserID,
			"--role", "boss", "--json", "--server", "http://unused", "--token", "tk"})
	})
	require.Equal(t, 2, code)
	assert.JSONEq(t, `{"ok":false,"data":null,
		"error":{"code":"USAGE","message":"role must be owner, operator or viewer"}}`, out)
}

func TestNodeDisableJSONGolden(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "POST", r.Method)
		assert.Equal(t, "/api/nodes/"+nodeUUID+"/disable", r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"node", "disable", nodeUUID,
			"--json", "--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 0, code)
	assert.JSONEq(t, `{"ok":true,"data":null,"error":null}`, out)
}

// TestNodeDisableForbiddenExits241: a viewer hitting the owner-only disable
// endpoint gets exit 241 both in JSON envelope mode and table stderr mode.
func TestNodeDisableForbiddenExits241(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"FORBIDDEN","message":"owner role required"}}`))
	}))
	defer srv.Close()

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"node", "disable", nodeUUID,
			"--json", "--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 241, code)
	assert.JSONEq(t, `{"ok":false,"data":null,
		"error":{"code":"FORBIDDEN","message":"owner role required"}}`, out)

	stderr, code2 := captureStderr(t, func() int {
		return runCLI(t.Context(), []string{"node", "disable", nodeUUID,
			"--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 241, code2)
	assert.Contains(t, stderr, proto.CodeForbidden+": owner role required")
}

func TestNodeEnableResolvesName(t *testing.T) {
	var enabledPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/nodes":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":"` + nodeUUID + `","name":"web-01","cluster":"default",
				"status":"disabled","last_seen_at":null}]`))
		case r.Method == "POST":
			enabledPath = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"node", "enable", "web-01",
			"--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 0, code)
	assert.Equal(t, "/api/nodes/"+nodeUUID+"/enable", enabledPath)
	assert.Contains(t, out, "enabled web-01")
}

func TestClusterDeleteNoContent(t *testing.T) {
	var deleted bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "DELETE", r.Method)
		assert.Equal(t, "/api/clusters/lab", r.URL.Path)
		deleted = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"cluster", "delete", "lab",
			"--json", "--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 0, code)
	assert.JSONEq(t, `{"ok":true,"data":null,"error":null}`, out)
	assert.True(t, deleted)
}

func TestAuditListFiltersAndGolden(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "GET", r.Method)
		assert.Equal(t, "/api/audit", r.URL.Path)
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":7,"user_id":"` + adminUserID + `","cluster_id":null,
			"node_id":"` + nodeUUID + `","action":"node.disable","session_id":"",
			"metadata":{"name":"web-01"},"created_at":"2026-08-19T10:00:00Z"}]`))
	}))
	defer srv.Close()

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"audit", "list",
			"--node", nodeUUID, "--user", adminUserID, "--action", "node.disable",
			"--since", "7d", "--limit", "50", "--offset", "1",
			"--json", "--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 0, code)
	want := url.Values{}
	want.Set("nodeId", nodeUUID)
	want.Set("userId", adminUserID)
	want.Set("action", "node.disable")
	want.Set("since", "7d")
	want.Set("limit", "50")
	want.Set("offset", "1")
	assert.Equal(t, want.Encode(), gotQuery.Encode())
	assert.JSONEq(t, `{"ok":true,"data":[{"id":7,"user_id":"`+adminUserID+`","cluster_id":null,
		"node_id":"`+nodeUUID+`","action":"node.disable","session_id":"",
		"metadata":{"name":"web-01"},"created_at":"2026-08-19T10:00:00Z"}],"error":null}`, out)

	tbl, code2 := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"audit", "list", "--action", "node.disable",
			"--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 0, code2)
	assert.Contains(t, tbl, "ACTION")
	assert.Contains(t, tbl, "node.disable")
}

func readJSONBody(t *testing.T, r *http.Request, v *map[string]any) error {
	t.Helper()
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}
