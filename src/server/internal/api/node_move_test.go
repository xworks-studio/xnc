package api

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

// mintToken 为指定 cluster 铸 enrollment token（TestEnv.CreateEnrollToken 只
// 对 default cluster）。
func mintToken(t *testing.T, srv *httptest.Server, adminTok, clusterID string) string {
	t.Helper()
	resp := doJSON(t, srv.URL, "POST", "/api/clusters/"+clusterID+"/enrollment-tokens",
		adminTok, `{}`)
	require.Equal(t, 201, resp.StatusCode)
	var out struct {
		Token string `json:"token"`
	}
	require.NoError(t, decodeJSON(resp.Body, &out))
	return out.Token
}

// enrollMachine 走 token 路径注册节点进任意 cluster，返回 nodeID。
func enrollMachine(t *testing.T, srv *httptest.Server, adminTok, clusterID,
	hostname, machineID string,
) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	tok := mintToken(t, srv, adminTok, clusterID)
	body := fmt.Sprintf(`{"token":%q,"hostname":%q,"machineId":%q,
		"osVersion":"Windows","agentVersion":"0.1.0","publicKey":%q}`,
		tok, hostname, machineID, base64.StdEncoding.EncodeToString(pub))
	resp, err := http.Post(srv.URL+"/api/agent/enroll", "application/json",
		strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 201, resp.StatusCode)
	var out struct {
		NodeID string `json:"nodeId"`
	}
	require.NoError(t, decodeJSON(resp.Body, &out))
	return out.NodeID
}

// createOwnedCluster：admin（或指定 token 的用户）建一个新 cluster 并返回 id。
func createOwnedCluster(t *testing.T, srv *httptest.Server, tok, name string) string {
	t.Helper()
	assert.Equal(t, 201, doJSON(t, srv.URL, "POST", "/api/clusters", tok,
		`{"name":"`+name+`"}`).StatusCode)
	var cl []map[string]any
	require.NoError(t, decodeJSON(doJSON(t, srv.URL, "GET", "/api/clusters", tok, "").Body, &cl))
	for _, c := range cl {
		if c["name"] == name {
			return c["id"].(string)
		}
	}
	t.Fatalf("cluster %s not found", name)
	return ""
}

// TestNodeMoveBasics：双 owner move——nodeId/公钥/状态保留、列表换组、
// 同目标 400、未知目标 404、名字冲突自动递增。
func TestNodeMoveBasics(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)
	def := defaultClusterID(t, srv.URL, admin)
	target := createOwnedCluster(t, srv, admin, "target")

	nodeID := enrollMachine(t, srv, admin, def, "MOVE-1", "mid-move-1")

	// 目标里已有一台同名节点 → 搬入后名字自动 -2。
	assert.Equal(t, 201, doJSON(t, srv.URL, "POST", "/api/clusters/"+target+"/nodes/register",
		admin, registerBody("MOVE-1", "mid-target-dup",
			base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize)))).StatusCode)

	resp := doJSON(t, srv.URL, "POST", "/api/nodes/"+nodeID+"/move", admin,
		`{"target":"target"}`)
	require.Equal(t, 200, resp.StatusCode)
	var out struct {
		NodeID    string `json:"nodeId"`
		ClusterID string `json:"clusterId"`
		Name      string `json:"name"`
	}
	require.NoError(t, decodeJSON(resp.Body, &out))
	assert.Equal(t, nodeID, out.NodeID, "nodeId survives move")
	assert.Equal(t, target, out.ClusterID)
	assert.Equal(t, "MOVE-1-2", out.Name, "name auto-suffixed on conflict")

	// nodeId/公钥不变（DB 断言）；节点出现在目标 cluster（列表 cluster 名）。
	var pk, status string
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT public_key, status FROM nodes WHERE id=$1`, nodeID).Scan(&pk, &status))
	assert.NotEmpty(t, pk)
	nodes := env.ListNodes(t)
	var found map[string]any
	for _, n := range nodes {
		if n["id"] == nodeID {
			found = n
		}
	}
	require.NotNil(t, found, "moved node still visible to source owner (now target owner)")
	assert.Equal(t, "target", found["cluster"])

	// 同目标 → 400；未知目标 → 404。
	assert.Equal(t, 400, doJSON(t, srv.URL, "POST", "/api/nodes/"+nodeID+"/move", admin,
		`{"target":"target"}`).StatusCode)
	assert.Equal(t, 404, doJSON(t, srv.URL, "POST", "/api/nodes/"+nodeID+"/move", admin,
		`{"target":"no-such-cluster"}`).StatusCode)
}

// TestNodeMovePermissions：单边 owner 拒绝——源 owner 目标非 owner → 403；
// 源非 owner（operator）→ 403；机器被占 → 409 MACHINE_ID_CONFLICT。
func TestNodeMovePermissions(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)
	def := defaultClusterID(t, srv.URL, admin)

	// bob 自建 cluster（bob 是其 owner，但不是 default 的 owner）。
	_, bobTok := createUserViaAPI(t, srv.URL, admin, "bob@t.local", "Bob")
	createOwnedCluster(t, srv, bobTok, "bobs")

	// default 的 operator（非 owner）。
	opID, opTok := createUserViaAPI(t, srv.URL, admin, "mv-op@t.local", "Op")
	assert.Equal(t, 201, doJSON(t, srv.URL, "POST", "/api/clusters/"+def+"/members", admin,
		`{"user_id":"`+opID+`","role":"operator"}`).StatusCode)

	nodeID := env.EnrollNode(t, "PERM-1", "mid-perm-1")

	// operator（源非 owner）→ 403。
	assert.Equal(t, 403, doJSON(t, srv.URL, "POST", "/api/nodes/"+nodeID+"/move", opTok,
		`{"target":"bobs"}`).StatusCode)
	// bob（目标 owner 但源非成员）→ 403。
	assert.Equal(t, 403, doJSON(t, srv.URL, "POST", "/api/nodes/"+nodeID+"/move", bobTok,
		`{"target":"bobs"}`).StatusCode)

	// admin（双 owner：default + 自建的 admin-target）move 成功；随后另一台
	// 同 machineId 机器注册进 default → 409 MACHINE_ID_CONFLICT（机器单归属）。
	adminTarget := createOwnedCluster(t, srv, admin, "admin-target")
	assert.Equal(t, 200, doJSON(t, srv.URL, "POST", "/api/nodes/"+nodeID+"/move", admin,
		`{"target":"`+adminTarget+`"}`).StatusCode)
	resp := doJSON(t, srv.URL, "POST", "/api/clusters/"+def+"/nodes/register", admin,
		registerBody("OTHER", "mid-perm-1",
			base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize))))
	require.Equal(t, 409, resp.StatusCode)
	var e struct {
		Error proto.APIError `json:"error"`
	}
	require.NoError(t, decodeJSON(resp.Body, &e))
	assert.Equal(t, proto.CodeMachineIDConflict, e.Error.Code)
}

// TestNodeMoveOnlineKeepsConnection：在线节点 move 后控制连接不逐出、状态
// 仍 online（连接身份=公钥，与 cluster 无关——V1/V2 验证结论的行为断言）。
func TestNodeMoveOnlineKeepsConnection(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)
	createOwnedCluster(t, srv, admin, "online-target")

	nodeID := env.EnrollNode(t, "ONLINE-1", "mid-online-1")
	conn := dialControl(t, env, nodeID)
	defer conn.CloseNow()

	assert.Equal(t, 200, doJSON(t, srv.URL, "POST", "/api/nodes/"+nodeID+"/move", admin,
		`{"target":"online-target"}`).StatusCode)

	// 连接仍在 registry（状态由连接维持）：节点仍 online。
	var status string
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT status FROM nodes WHERE id=$1`, nodeID).Scan(&status))
	assert.Equal(t, "online", status)
}

// TestTokenEnrollCrossClusterMachineConflict：token 路径此前不查跨 cluster
// machineId 冲突——全局唯一索引 + 23505 分流后 409（修复回归）。
func TestTokenEnrollCrossClusterMachineConflict(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)
	def := defaultClusterID(t, srv.URL, admin)
	other := createOwnedCluster(t, srv, admin, "other")

	enrollMachine(t, srv, admin, def, "T-1", "mid-tok-dup")

	pub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	tok := mintToken(t, srv, admin, other)
	body := fmt.Sprintf(`{"token":%q,"hostname":"T-2","machineId":"mid-tok-dup",
		"osVersion":"Windows","agentVersion":"0.1.0","publicKey":%q}`,
		tok, base64.StdEncoding.EncodeToString(pub))
	resp, err := http.Post(srv.URL+"/api/agent/enroll", "application/json",
		strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 409, resp.StatusCode)
	var e struct {
		Error proto.APIError `json:"error"`
	}
	require.NoError(t, decodeJSON(resp.Body, &e))
	assert.Equal(t, proto.CodeMachineIDConflict, e.Error.Code)
}

// TestTokenEnrollDeletedCluster：cluster 软删除后其未过期 token 视作失效
// （410），不再能把节点注册进"幽灵" cluster（0005 存量 bug 修复）。
func TestTokenEnrollDeletedCluster(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)

	gone := createOwnedCluster(t, srv, admin, "gone")
	tok := mintToken(t, srv, admin, gone)
	assert.Equal(t, 204, doJSON(t, srv.URL, "DELETE", "/api/clusters/"+gone, admin, "").StatusCode)

	pub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	body := fmt.Sprintf(`{"token":%q,"hostname":"G-1","machineId":"mid-gone",
		"osVersion":"Windows","agentVersion":"0.1.0","publicKey":%q}`,
		tok, base64.StdEncoding.EncodeToString(pub))
	resp, err := http.Post(srv.URL+"/api/agent/enroll", "application/json",
		strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 410, resp.StatusCode)
}

// TestNodeRename：owner 改名 200 + 审计；cluster 内重名 409；operator 403；
// 未知节点 404。
func TestNodeRename(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)

	a := env.EnrollNode(t, "REN-1", "mid-ren-1")
	b := env.EnrollNode(t, "REN-2", "mid-ren-2")

	// owner 改名 → 200，name 生效。
	assert.Equal(t, 200, doJSON(t, srv.URL, "PATCH", "/api/nodes/"+a, admin,
		`{"name":"web-01"}`).StatusCode)
	// cluster 内重名 → 409。
	resp := doJSON(t, srv.URL, "PATCH", "/api/nodes/"+b, admin, `{"name":"web-01"}`)
	assert.Equal(t, 409, resp.StatusCode)
	// operator（非 owner）→ 403；未知节点 → 404。
	opID, opTok := createUserViaAPI(t, srv.URL, admin, "ren-op@t.local", "Op")
	assert.Equal(t, 201, doJSON(t, srv.URL, "POST", "/api/clusters/default/members", admin,
		`{"user_id":"`+opID+`","role":"operator"}`).StatusCode)
	assert.Equal(t, 403, doJSON(t, srv.URL, "PATCH", "/api/nodes/"+a, opTok,
		`{"name":"nope"}`).StatusCode)
	assert.Equal(t, 404, doJSON(t, srv.URL, "PATCH",
		"/api/nodes/00000000-0000-0000-0000-000000000000", admin,
		`{"name":"x"}`).StatusCode)
	// 审计 node.rename。
	var n int
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM audit_logs WHERE action='node.rename'`).Scan(&n))
	assert.Equal(t, 1, n)
}
