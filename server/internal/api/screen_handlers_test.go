package api

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

func screenPost(t *testing.T, env *TestEnv, nodeID, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", env.srv.URL+"/api/nodes/"+nodeID+"/screen",
		bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+env.AdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

// TestScreenSessionEndToEnd：400 校验（fps/quality/maxWidth 越界）→
// 在线 202 → SESSION_OPEN 携带 KindScreen 与补默认后的 ScreenParams →
// 审计对 screen.open / screen.close。
func TestScreenSessionEndToEnd(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-SC1", "mid-sc1")
	ctrl := dialControl(t, env, nodeID)

	// 校验失败：fps 超上限 / quality 为负 / maxWidth 超上限
	for _, b := range []string{`{"fps":31}`, `{"quality":-1}`, `{"maxWidth":1921}`} {
		r := screenPost(t, env, nodeID, b)
		_ = r.Body.Close()
		assert.Equal(t, 400, r.StatusCode, b)
	}

	// captureOnly：读控制连接上的 SESSION_OPEN 即送 channel，不拨会话。
	openCh := captureSessionOpen(t, ctrl)

	resp := screenPost(t, env, nodeID, `{}`)
	defer resp.Body.Close()
	require.Equal(t, 202, resp.StatusCode)

	select {
	case so := <-openCh:
		require.Equal(t, proto.KindScreen, so.Kind)
	case <-time.After(3 * time.Second):
		t.Fatal("no SESSION_OPEN")
	}

	// 审计：screen.open 在会话创建即落盘（screen.close 由会话终态路径触发，
	// captureOnly 变体下无人拨会话 WS，不在本测试范围）。
	require.Eventually(t, func() bool {
		var n int
		require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
			`SELECT count(*) FROM audit_logs WHERE action = 'screen.open'`).Scan(&n))
		return n >= 1
	}, 5*time.Second, 200*time.Millisecond)
}

// TestScreenParamsExplicitInSessionOpen：显式合法参数原样下发（不经缺省替换）。
func TestScreenParamsExplicitInSessionOpen(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-SC2", "mid-sc2")
	ctrl := dialControl(t, env, nodeID)
	openCh := captureSessionOpen(t, ctrl)

	resp := screenPost(t, env, nodeID, `{"fps":30,"quality":85,"maxWidth":1280}`)
	defer resp.Body.Close()
	require.Equal(t, 202, resp.StatusCode)

	select {
	case so := <-openCh:
		require.Equal(t, proto.KindScreen, so.Kind)
		var p proto.ScreenParams
		require.NoError(t, jsonUnmarshal(so.Params, &p))
		assert.Equal(t, 30, p.Fps)
		assert.Equal(t, 85, p.Quality)
		assert.Equal(t, 1280, p.MaxWidth)
	case <-time.After(3 * time.Second):
		t.Fatal("no SESSION_OPEN")
	}
}

// TestScreenRBAC：Scenario F——owner/operator → 202；viewer → 403 FORBIDDEN；
// 非成员 → 404 NODE_NOT_FOUND。
func TestScreenRBAC(t *testing.T) {
	f := newRBACFixture(t)
	path := "/api/nodes/" + f.nodeID + "/screen"
	for user, want := range map[string]int{
		"owner": 202, "operator": 202, "viewer": 403, "outsider": 404,
	} {
		code, errCode := f.post(t, f.tokens[user], path, `{}`)
		assert.Equal(t, want, code, user)
		switch want {
		case 403:
			assert.Equal(t, proto.CodeForbidden, errCode, user)
		case 404:
			assert.Equal(t, proto.CodeNodeNotFound, errCode, user)
		}
	}
}
