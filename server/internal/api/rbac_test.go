package api

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

// rbacFixture：Scenario F 布景——default cluster 上 owner/operator/viewer 三种
// 成员 + 一个非成员 outsider，一枚已注册且控制连接在线的节点。控制连接在线
// 使 owner/operator 的会话创建能走到 202（离线会先撞 409 NODE_OFFLINE，测不出
// role 断言）；SESSION_OPEN 发往无人消费的控制 WS 即可，无需完成会话。
type rbacFixture struct {
	env    *TestEnv
	srv    *httptest.Server
	nodeID string
	tokens map[string]string // role/identity → JWT
}

func newRBACFixture(t *testing.T) *rbacFixture {
	t.Helper()
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	t.Cleanup(srv.Close)
	admin := env.AdminToken(t)
	cluster := defaultClusterID(t, srv.URL, admin)

	// owner 用非 admin 用户（admin 本就是 owner），证明判的是 role 而非 is-admin。
	ownerID, ownerTok := createUserViaAPI(t, srv.URL, admin, "rbac-owner@t.local", "Owner")
	opID, opTok := createUserViaAPI(t, srv.URL, admin, "rbac-op@t.local", "Op")
	viewID, viewTok := createUserViaAPI(t, srv.URL, admin, "rbac-view@t.local", "View")
	_, noneTok := createUserViaAPI(t, srv.URL, admin, "rbac-none@t.local", "None")

	for _, m := range []struct{ id, role string }{
		{ownerID, "owner"}, {opID, "operator"}, {viewID, "viewer"},
	} {
		resp := doJSON(t, srv.URL, "POST", "/api/clusters/"+cluster+"/members", admin,
			`{"user_id":"`+m.id+`","role":"`+m.role+`"}`)
		require.Equal(t, 201, resp.StatusCode, "add member "+m.role)
	}

	nodeID := env.EnrollNode(t, "RBAC-F1", "mid-rbac-f1")
	_ = dialControl(t, env, nodeID)
	return &rbacFixture{
		env: env, srv: srv, nodeID: nodeID,
		tokens: map[string]string{
			"owner": ownerTok, "operator": opTok, "viewer": viewTok, "outsider": noneTok,
		},
	}
}

// post 返回 (status, error.code)。body 全部合法——viewer/outsider 必须先过
// 400 校验层才能撞上 RBAC 检查，断言 code 可区分 403/404 与校验错误。
func (f *rbacFixture) post(t *testing.T, tok, path, body string) (int, string) {
	t.Helper()
	resp := doJSON(t, f.srv.URL, "POST", path, tok, body)
	defer resp.Body.Close()
	var e struct {
		Error proto.APIError `json:"error"`
	}
	if resp.StatusCode == 202 {
		return resp.StatusCode, ""
	}
	_ = decodeJSON(resp.Body, &e)
	return resp.StatusCode, e.Error.Code
}

const rbacSHA = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// rbacEndpoints：五个会话端点（exec/shell/upload/download/tunnel）× 合法请求体。
func (f *rbacFixture) rbacEndpoints() map[string]struct {
	path string
	body string
} {
	base := "/api/nodes/" + f.nodeID
	return map[string]struct {
		path string
		body string
	}{
		"exec":      {base + "/exec", `{"command":"hostname"}`},
		"shell":     {base + "/shell", `{}`},
		"upload":    {base + "/files/upload", `{"path":"C:\\tmp\\f.bin","size":16,"sha256":"` + rbacSHA + `"}`},
		"download":  {base + "/files/download", `{"path":"C:\\tmp\\f.bin"}`},
		"tunnel":    {base + "/tunnel", `{"target":"rdp"}`},
	}
}

// TestRBACSessionMatrix：Scenario F——会话端点全量 RBAC。
// owner/operator → 202；viewer → 403 FORBIDDEN；非成员 → 404 NODE_NOT_FOUND
// （不泄漏节点存在性）。202 只断言状态码：会话停留在 Opening、SESSION_OPEN
// 发往无人接手的控制连接，与 RBAC 断言无关。
func TestRBACSessionMatrix(t *testing.T) {
	f := newRBACFixture(t)
	endpoints := f.rbacEndpoints()

	for user, want := range map[string]int{
		"owner": 202, "operator": 202, "viewer": 403, "outsider": 404,
	} {
		for name, ep := range endpoints {
			code, errCode := f.post(t, f.tokens[user], ep.path, ep.body)
			assert.Equal(t, want, code, user+" → "+name)
			switch want {
			case 403:
				assert.Equal(t, proto.CodeForbidden, errCode, user+" → "+name)
			case 404:
				assert.Equal(t, proto.CodeNodeNotFound, errCode, user+" → "+name)
			}
		}
	}
}

// TestRBACNodeVisibility：node list/show 保持 viewer 级（任一成员可见），
// 非成员被排除——列表不含该节点、单节点 GET 404（现有行为不变）。
func TestRBACNodeVisibility(t *testing.T) {
	f := newRBACFixture(t)

	for _, user := range []string{"owner", "operator", "viewer"} {
		resp := doJSON(t, f.srv.URL, "GET", "/api/nodes", f.tokens[user], "")
		require.Equal(t, 200, resp.StatusCode, user)
		var nodes []map[string]any
		require.NoError(t, decodeJSON(resp.Body, &nodes))
		ids := []string{}
		for _, n := range nodes {
			ids = append(ids, n["id"].(string))
		}
		assert.Contains(t, ids, f.nodeID, user+" sees enrolled node")
	}

	// outsider：列表 200 但不含节点（ListNodesForUser 按 membership 过滤），
	// 单节点 GET → 404 NODE_NOT_FOUND。
	resp := doJSON(t, f.srv.URL, "GET", "/api/nodes", f.tokens["outsider"], "")
	require.Equal(t, 200, resp.StatusCode)
	var nodes []map[string]any
	require.NoError(t, decodeJSON(resp.Body, &nodes))
	for _, n := range nodes {
		assert.NotEqual(t, f.nodeID, n["id"], "outsider must not see node")
	}
	resp2 := doJSON(t, f.srv.URL, "GET", "/api/nodes/"+f.nodeID, f.tokens["outsider"], "")
	require.Equal(t, 404, resp2.StatusCode)
	var e struct {
		Error proto.APIError `json:"error"`
	}
	require.NoError(t, decodeJSON(resp2.Body, &e))
	assert.Equal(t, proto.CodeNodeNotFound, e.Error.Code)
}

// TestRBACUnknownNode：不存在的 nodeID 对任何用户（含 owner）都 404，
// role 检查不得改变 404 语义。
func TestRBACUnknownNode(t *testing.T) {
	f := newRBACFixture(t)
	code, errCode := f.post(t, f.tokens["owner"],
		"/api/nodes/"+strings.Repeat("0", 8)+"-0000-4000-8000-000000000000/exec",
		`{"command":"hostname"}`)
	assert.Equal(t, 404, code)
	assert.Equal(t, proto.CodeNodeNotFound, errCode)
}
