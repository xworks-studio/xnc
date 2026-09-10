package api

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
	"xnc/server/internal/db/sqlc"
)

// registerBody 构造 POST /api/clusters/{id}/nodes/register 请求体（与
// /api/agent/enroll 同形、无 token 字段）。
func registerBody(hostname, machineID, pub string) string {
	return `{"hostname":"` + hostname + `","machineId":"` + machineID +
		`","osVersion":"Windows Server 2022","agentVersion":"0.1.0","publicKey":"` + pub + `"}`
}

// TestUserNodeRegister 覆盖 spec §6.4：成员注册成功 + audit register
// {userId, clusterId}；viewer 亦可（任一成员，非 admin-only）；非成员 403；
// 未知 cluster 404；未认证 401；token 字段 400；缺参/坏公钥 400；machineId
// 已属其他 cluster → 409 MACHINE_ID_CONFLICT（响应含冲突 cluster 名）；
// 同 cluster 异 key → adopt（201 复用既有节点行，node_adopt 审计；细节语义
// 见 TestUserNodeRegisterAdopt）。
func TestUserNodeRegister(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)
	adminID := env.AdminUUID(t)
	cluster := defaultClusterID(t, srv.URL, admin)
	register := func(token, clusterID, body string) *http.Response {
		t.Helper()
		return doJSON(t, srv.URL, "POST", "/api/clusters/"+clusterID+"/nodes/register", token, body)
	}

	pub, _, _ := ed25519.GenerateKey(nil)
	pubB64 := base64.StdEncoding.EncodeToString(pub)

	// 1. 未认证 → 401（JWT 中间件）
	assert.Equal(t, 401, register("", cluster, registerBody("H", "reg-mid-1", pubB64)).StatusCode)

	// 2. 未知 cluster → 404 CLUSTER_NOT_FOUND
	resp := register(admin, uuid.New().String(), registerBody("H", "reg-mid-1", pubB64))
	require.Equal(t, 404, resp.StatusCode)
	var nf struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, decodeJSON(resp.Body, &nf))
	assert.Equal(t, "CLUSTER_NOT_FOUND", nf.Error.Code)

	// 3. 非成员 → 403 FORBIDDEN（spec §6.4 明示 403，不走 404 隐藏）
	_, outsiderTok := createUserViaAPI(t, srv.URL, admin, "outsider@t.local", "Outsider")
	resp = register(outsiderTok, cluster, registerBody("OUT-01", "reg-mid-out", pubB64))
	require.Equal(t, 403, resp.StatusCode)
	var fb struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, decodeJSON(resp.Body, &fb))
	assert.Equal(t, "FORBIDDEN", fb.Error.Code)

	// 4. 成员注册成功 → 201 {nodeId, clusterId, name}
	resp = register(admin, cluster, registerBody("WEB-01", "reg-mid-1", pubB64))
	require.Equal(t, 201, resp.StatusCode)
	var node struct {
		NodeID    string `json:"nodeId"`
		ClusterID string `json:"clusterId"`
		Name      string `json:"name"`
	}
	require.NoError(t, decodeJSON(resp.Body, &node))
	require.NotEmpty(t, node.NodeID)
	assert.Equal(t, cluster, node.ClusterID)
	assert.Equal(t, "WEB-01", node.Name)

	// 5. audit register：user_id=调用者、cluster_id、node_id（spec：取代
	// token 创建+使用两条审计的拼接）
	var actor, cid, nid string
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT user_id::text, cluster_id::text, node_id::text FROM audit_logs
		 WHERE action='register' AND node_id::text=$1`, node.NodeID).Scan(&actor, &cid, &nid))
	assert.Equal(t, adminID.String(), actor)
	assert.Equal(t, cluster, cid)
	assert.Equal(t, node.NodeID, nid)

	// 6. 幂等：同 machineId + 同 key → 同 nodeId（不新增 audit）
	resp = register(admin, cluster, registerBody("WEB-01", "reg-mid-1", pubB64))
	require.Equal(t, 201, resp.StatusCode)
	var node2 struct {
		NodeID string `json:"nodeId"`
	}
	require.NoError(t, decodeJSON(resp.Body, &node2))
	assert.Equal(t, node.NodeID, node2.NodeID)
	var nReg int
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM audit_logs WHERE action='register'`).Scan(&nReg))
	assert.Equal(t, 1, nReg)

	// 7. viewer 成员亦可注册（任一成员，非 admin-only）
	viewID, viewTok := createUserViaAPI(t, srv.URL, admin, "viewer@t.local", "Viewer")
	require.Equal(t, 201, doJSON(t, srv.URL, "POST", "/api/clusters/"+cluster+"/members",
		admin, `{"user_id":"`+viewID+`","role":"viewer"}`).StatusCode)
	assert.Equal(t, 201, register(viewTok, cluster,
		registerBody("VIEW-01", "reg-mid-view", pubB64)).StatusCode)

	// 8. 请求体含 token 字段 → 400（两条注册路径不得混用；空 token 同拒）
	assert.Equal(t, 400, register(admin, cluster,
		`{"token":"anything","hostname":"H","machineId":"reg-mid-tok",
			"osVersion":"Windows","agentVersion":"0.1.0","publicKey":"`+pubB64+`"}`).StatusCode)
	assert.Equal(t, 400, register(admin, cluster,
		`{"token":"","hostname":"H","machineId":"reg-mid-tok",
			"osVersion":"Windows","agentVersion":"0.1.0","publicKey":"`+pubB64+`"}`).StatusCode)

	// 9. 参数缺失 / 公钥非法 → 400
	assert.Equal(t, 400, register(admin, cluster,
		registerBody("H", "", pubB64)).StatusCode)
	assert.Equal(t, 400, register(admin, cluster,
		registerBody("H", "reg-mid-bad", "not-base64!!")).StatusCode)

	// 10. machineId 已属其他 cluster → 409 MACHINE_ID_CONFLICT，响应含冲突
	// cluster 名（CLI 据此提示 --force 或管理端处理）
	resp = doJSON(t, srv.URL, "POST", "/api/clusters", admin, `{"name":"second"}`)
	require.Equal(t, 201, resp.StatusCode)
	var second struct {
		ID string `json:"id"`
	}
	require.NoError(t, decodeJSON(resp.Body, &second))
	pub2, _, _ := ed25519.GenerateKey(nil)
	resp = register(admin, second.ID, registerBody("WEB-02", "reg-mid-1",
		base64.StdEncoding.EncodeToString(pub2)))
	require.Equal(t, 409, resp.StatusCode)
	var conflict struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, decodeJSON(resp.Body, &conflict))
	assert.Equal(t, "MACHINE_ID_CONFLICT", conflict.Error.Code)
	assert.Contains(t, conflict.Error.Message, `"default"`)

	// 11. 同 cluster 同 machineId 异 key → adopt（controller 批准设计）：201 复用
	// 既有 nodeId（不新增节点行）、name 不变，审计含 node_adopt；与跨 cluster
	// 冲突（409 MACHINE_ID_CONFLICT）可区分。字段刷新/连接接管等细节语义见
	// TestUserNodeRegisterAdopt。
	resp = register(admin, cluster, registerBody("WEB-03", "reg-mid-1",
		base64.StdEncoding.EncodeToString(pub2)))
	require.Equal(t, 201, resp.StatusCode)
	var adopted struct {
		NodeID string `json:"nodeId"`
		Name   string `json:"name"`
	}
	require.NoError(t, decodeJSON(resp.Body, &adopted))
	assert.Equal(t, node.NodeID, adopted.NodeID, "adopt reuses the existing node row")
	assert.Equal(t, "WEB-01", adopted.Name, "adopt must not rename the node")
	var nAdopt int
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM audit_logs WHERE node_id=$1 AND action='node_adopt'`,
		node.NodeID).Scan(&nAdopt))
	assert.Equal(t, 1, nAdopt)
}

// TestUserNodeRegisterAdopt：同 cluster 同 machineId 异 key 的 adopt 全语义
// （controller 批准设计：同 cluster 成员可接管节点身份——与注册新节点同一信任
// 域；跨 cluster 冲突仍 409，须管理端先删）：201 复用既有 nodeId，公钥重绑为
// 新 key，hostname/osVersion/agentVersion 刷新，shell_type 清空（连接时 HELLO
// 重新探测），last_seen_at 置空（新 key 尚未连接），用户可见 name 不变；审计
// register（如常）+ node_adopt（各一条）；旧 key 控制连接挑战验签失败，新 key
// 握手成功。
func TestUserNodeRegisterAdopt(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)
	cluster := defaultClusterID(t, srv.URL, admin)

	register := func(body string) *http.Response {
		t.Helper()
		return doJSON(t, srv.URL, "POST", "/api/clusters/"+cluster+"/nodes/register", admin, body)
	}

	pubA, privA, _ := ed25519.GenerateKey(nil)
	pubB, privB, _ := ed25519.GenerateKey(nil)
	keyA := base64.StdEncoding.EncodeToString(pubA)
	keyB := base64.StdEncoding.EncodeToString(pubB)

	// key A 首次注册 → 201 {nodeId, name=OLD-HOST}
	resp := register(registerBody("OLD-HOST", "adopt-mid", keyA))
	require.Equal(t, 201, resp.StatusCode)
	var node struct {
		NodeID string `json:"nodeId"`
		Name   string `json:"name"`
	}
	require.NoError(t, decodeJSON(resp.Body, &node))
	require.Equal(t, "OLD-HOST", node.Name)

	// 预置"旧身份已连接过"的痕迹（shell_type 探测结果 + last_seen）：adopt 应
	// 重置身份相关字段——shell_type 清空、last_seen_at 置空（新 key 未见过）。
	nid := mustUUID(node.NodeID)
	require.NoError(t, env.Store.Q().UpdateNodeMeta(t.Context(),
		sqlc.UpdateNodeMetaParams{Hostname: "OLD-HOST", ShellType: "pwsh",
			AgentVersion: "0.1.0", ID: nid}))
	require.NoError(t, env.Store.Q().TouchNode(t.Context(), nid))

	// 同 machineId 同 cluster 换 key B + 新机器字段 → 201 同 nodeId（adopt）
	resp = register(fmt.Sprintf(
		`{"hostname":"NEW-HOST","machineId":"adopt-mid","osVersion":"Windows 11",
		 "agentVersion":"0.2.0","publicKey":%q}`, keyB))
	require.Equal(t, 201, resp.StatusCode)
	var adopted struct {
		NodeID string `json:"nodeId"`
		Name   string `json:"name"`
	}
	require.NoError(t, decodeJSON(resp.Body, &adopted))
	assert.Equal(t, node.NodeID, adopted.NodeID, "adopt must reuse the existing node id")
	assert.Equal(t, "OLD-HOST", adopted.Name, "adopt must not rename the node")

	// 落库断言：公钥重绑、机器字段刷新、shell_type/last_seen 重置、name 不变。
	var hostname, osVer, agentVer, shell, pub, name string
	var lastSeen *time.Time
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT hostname, os_version, agent_version, shell_type, public_key, name, last_seen_at
		 FROM nodes WHERE id=$1`, node.NodeID).
		Scan(&hostname, &osVer, &agentVer, &shell, &pub, &name, &lastSeen))
	assert.Equal(t, "NEW-HOST", hostname)
	assert.Equal(t, "Windows 11", osVer)
	assert.Equal(t, "0.2.0", agentVer)
	assert.Equal(t, "", shell, "shell_type cleared; re-probed on next connect")
	assert.Equal(t, keyB, pub, "public key must be rebound to the new key")
	assert.Equal(t, "OLD-HOST", name)
	assert.Nil(t, lastSeen, "last_seen_at must not survive adopt (new key has not connected)")

	// 审计：register（如常，共两条——首次创建 + adopt）+ node_adopt（显式可查，
	// actor=调用用户、target=被接管节点）。
	var nReg, nAdoptRows int
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM audit_logs WHERE node_id=$1 AND action='register'`,
		node.NodeID).Scan(&nReg))
	assert.Equal(t, 2, nReg)
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT count(*) FROM audit_logs WHERE node_id=$1 AND action='node_adopt'`,
		node.NodeID).Scan(&nAdoptRows))
	assert.Equal(t, 1, nAdoptRows)
	var adoptActor string
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT user_id::text FROM audit_logs
		 WHERE action='node_adopt' AND node_id=$1`, node.NodeID).Scan(&adoptActor))
	assert.Equal(t, env.AdminUUID(t).String(), adoptActor)

	// 旧 key A 挑战验签失败：服务端在校验后直接关闭连接（无 HELLO_ACK）。
	c := dialAgentWS(t, "ws"+env.srv.URL[4:]+"/api/agent/connect")
	m := readMsg(t, c)
	require.Equal(t, proto.TypeChallenge, m.Type)
	var ch proto.Challenge
	require.NoError(t, m.Decode(&ch))
	sigMsg, _ := proto.NewMsg(proto.TypeChallengeResponse, proto.ChallengeResponse{
		NodeID: node.NodeID, Signature: ed25519.Sign(privA, []byte(ch.Nonce))})
	writeMsg(t, c, sigMsg)
	require.Error(t, readErr(t, c), "old key must fail the challenge after adopt")

	// 新 key B 握手成功（dialControl 内完成挑战→验签→HELLO→HELLO_ACK）。
	env.nodes[node.NodeID] = privB
	_ = dialControl(t, env, node.NodeID)
}

// TestUserNodeRegisterAdoptEvictsOldConn：adopt 必须逐出旧 key 的在线控制连
// 接（评审 fix round 1）。旧连接若继续存活：(a) 会照常收到 SESSION_OPEN；
// (b) 其断开清理（RemoveIf==true 的正常路径）会回写 offline + last_seen_at，
// 覆盖 adopt 刚置空的 last_seen_at，破坏"新身份未见过"语义。逐出（先 Remove
// 再 Cancel）后被逐连接的清理走 RemoveIf==false，跳过状态回写——断言连接被
// 服务端关闭、registry 表项清除、last_seen_at 在清理跑完后仍为 NULL。
func TestUserNodeRegisterAdoptEvictsOldConn(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)
	cluster := defaultClusterID(t, srv.URL, admin)

	// key A 注册并建立在线控制连接（旧身份在线；HELLO 已置 online + last_seen）。
	pubA, privA, _ := ed25519.GenerateKey(nil)
	keyA := base64.StdEncoding.EncodeToString(pubA)
	resp := doJSON(t, srv.URL, "POST", "/api/clusters/"+cluster+"/nodes/register",
		admin, registerBody("OLD-HOST", "adopt-evict-mid", keyA))
	require.Equal(t, 201, resp.StatusCode)
	var node struct {
		NodeID string `json:"nodeId"`
	}
	require.NoError(t, decodeJSON(resp.Body, &node))
	env.nodes[node.NodeID] = privA
	live := dialControl(t, env, node.NodeID)
	require.NotNil(t, env.reg.Get(node.NodeID), "old-key conn must be registered")

	// adopt：同 machineId 换 key B → 201
	pubB, _, _ := ed25519.GenerateKey(nil)
	resp = doJSON(t, srv.URL, "POST", "/api/clusters/"+cluster+"/nodes/register",
		admin, registerBody("NEW-HOST", "adopt-evict-mid",
			base64.StdEncoding.EncodeToString(pubB)))
	require.Equal(t, 201, resp.StatusCode)

	// 逐出是同步的：201 返回时 registry 表项已移除、旧连接 ctx 已 Cancel。
	assert.Nil(t, env.reg.Get(node.NodeID),
		"old-key live conn must be evicted from the registry on adopt")

	// 被逐连接读到关闭（服务端 Cancel → 读错误）。
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		_, _, err := live.Read(ctx)
		return err != nil
	}, 5*time.Second, 100*time.Millisecond, "old-key live conn must be closed after adopt")

	// 断开清理已在此刻的 defer 链上执行（同一 goroutine）：留一个短窗口让
	// 错误的回写（若存在）可见，再断言 last_seen_at 仍为 NULL（RemoveIf==
	// false 跳过 TouchNode）。
	time.Sleep(500 * time.Millisecond)
	var lastSeen *time.Time
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT last_seen_at FROM nodes WHERE id=$1`, node.NodeID).Scan(&lastSeen))
	assert.Nil(t, lastSeen,
		"evicted conn cleanup must not touch last_seen_at (RemoveIf==false skips write-back)")
}
