package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

func postJSON(t *testing.T, env *TestEnv, path, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", env.srv.URL+path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+env.AdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestFileUploadValidation(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-F1", "mid-f1")
	// 202 用例要求节点在线（Create 对无控制连接节点 409）；会话 WS 无人拨号，
	// 由 Opening TTL 60s 兜底清理——测试只断言状态码。
	dialControl(t, env, nodeID)

	cases := []struct {
		body string
		code int
	}{
		{`{"path":"C:\\t\\f.zip","size":100,"sha256":"` + strings.Repeat("a", 64) + `"}`, 202},
		{`{"path":"rel\\path","size":100,"sha256":"abc"}`, 400},          // 相对路径
		{`{"path":"C:\\t\\f.zip","size":0,"sha256":"abc"}`, 400},         // size ≤ 0
		{`{"path":"C:\\t\\f.zip","size":268435457,"sha256":"abc"}`, 400}, // >256MB
		{`{"path":"C:\\t\\f.zip","size":100,"sha256":"zz"}`, 400},        // 非 hex
	}
	for _, c := range cases {
		resp := postJSON(t, env, "/api/nodes/"+nodeID+"/files/upload", c.body)
		_ = resp.Body.Close()
		assert.Equal(t, c.code, resp.StatusCode, c.body)
	}
}

func TestFileDownloadEndpoint(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-F2", "mid-f2")
	dialControl(t, env, nodeID)

	resp := postJSON(t, env, "/api/nodes/"+nodeID+"/files/download", `{"path":"C:\\t\\f.zip"}`)
	_ = resp.Body.Close()
	assert.Equal(t, 202, resp.StatusCode)
}

func TestTunnelTargetWhitelist(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-T1", "mid-t1")
	ctrl := dialControl(t, env, nodeID)

	gotOpen := captureSessionOpen(t, ctrl)
	resp := postJSON(t, env, "/api/nodes/"+nodeID+"/tunnel", `{"target":"rdp"}`)
	defer resp.Body.Close()
	require.Equal(t, 202, resp.StatusCode)

	select {
	case so := <-gotOpen:
		assert.Equal(t, proto.KindTunnel, so.Kind)
		var p map[string]any
		require.NoError(t, json.Unmarshal(so.Params, &p))
		assert.Equal(t, "rdp", p["target"])
		assert.Equal(t, "127.0.0.1", p["host"])
		assert.Equal(t, float64(3389), p["port"])
	case <-time.After(3 * time.Second):
		t.Fatal("no SESSION_OPEN")
	}

	// 未知 target → 400
	resp2 := postJSON(t, env, "/api/nodes/"+nodeID+"/tunnel", `{"target":"ssh"}`)
	_ = resp2.Body.Close()
	assert.Equal(t, 400, resp2.StatusCode)
}
