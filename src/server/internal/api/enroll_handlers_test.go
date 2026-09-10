package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/server/internal/tokens"
)

func TestEnrollment(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	tok := env.AdminToken(t)

	// 1. 创建 enrollment token
	req, _ := http.NewRequest("POST", srv.URL+"/api/clusters/default/enrollment-tokens",
		bytes.NewBufferString(`{}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 201, resp.StatusCode)
	var created struct {
		Token string `json:"token"`
	}
	require.NoError(t, decodeJSON(resp.Body, &created))
	require.NotEmpty(t, created.Token)

	pub, priv, _ := ed25519.GenerateKey(nil)
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	// （machineId 参数化：brief 原文 helper 固定 "mid-1"，但其 Step 3 与幂等规则矛盾——
	// 同 machineId+同 key 必须幂等 201；要测 max_uses 耗尽须换 machineId。）
	enrollBody := func(token, pub, machineID string) *bytes.Buffer {
		return bytes.NewBufferString(`{"token":"` + token +
			`","hostname":"WEB-01","machineId":"` + machineID + `","osVersion":"Windows Server 2022",
			"agentVersion":"0.1.0","publicKey":"` + pub + `"}`)
	}

	// 2. 注册成功
	resp2, err := http.Post(srv.URL+"/api/agent/enroll", "application/json",
		enrollBody(created.Token, pubB64, "mid-1"))
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.Equal(t, 201, resp2.StatusCode)
	var node struct {
		NodeID string `json:"nodeId"`
	}
	require.NoError(t, decodeJSON(resp2.Body, &node))
	require.NotEmpty(t, node.NodeID)

	// 3. 同 token 二次使用 → 401（max_uses=1 已耗尽；换 machineId 以避开幂等复用）
	resp3, err := http.Post(srv.URL+"/api/agent/enroll", "application/json",
		enrollBody(created.Token, pubB64, "mid-2"))
	require.NoError(t, err)
	defer resp3.Body.Close()
	assert.Equal(t, 401, resp3.StatusCode)

	// 4. 同 machineId + 同 key 幂等（新 token 下）
	req4, _ := http.NewRequest("POST", srv.URL+"/api/clusters/default/enrollment-tokens",
		bytes.NewBufferString(`{}`))
	req4.Header.Set("Authorization", "Bearer "+tok)
	resp4, err := http.DefaultClient.Do(req4)
	require.NoError(t, err)
	defer resp4.Body.Close()
	var t2 struct {
		Token string `json:"token"`
	}
	require.NoError(t, decodeJSON(resp4.Body, &t2))
	resp5, err := http.Post(srv.URL+"/api/agent/enroll", "application/json",
		enrollBody(t2.Token, pubB64, "mid-1"))
	require.NoError(t, err)
	defer resp5.Body.Close()
	assert.Equal(t, 201, resp5.StatusCode)
	var node2 struct {
		NodeID string `json:"nodeId"`
	}
	require.NoError(t, decodeJSON(resp5.Body, &node2))
	assert.Equal(t, node.NodeID, node2.NodeID)

	// 5. 同 machineId + 不同 key → 409
	// （brief 原文为 `_, pub2, _ :=`，那取到的是 64 字节私钥，会被公钥校验挡成 400；
	// 此处按测试意图改用新生成的公钥。）
	pub2, _, _ := ed25519.GenerateKey(nil)
	resp6, err := http.Post(srv.URL+"/api/agent/enroll", "application/json",
		enrollBody(t2.Token, base64.StdEncoding.EncodeToString(pub2), "mid-1"))
	require.NoError(t, err)
	defer resp6.Body.Close()
	assert.Equal(t, 409, resp6.StatusCode)

	// 6. 审计有 node.enroll
	var actions []string
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT array_agg(action ORDER BY id) FROM audit_logs`).Scan(&actions))
	assert.Contains(t, actions, "node.enroll")

	_ = priv
}

// TestAgentEnrollRegression 抽取共用落库函数（enrollNode）前的行为钉子：
// 覆盖 TestEnrollment 未钉住的分支——token 无效/过期、公钥格式、节点名去重
// 后缀、审计列（actor=token 创建者）。重构前后此测试必须逐字不变地通过。
func TestAgentEnrollRegression(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	admin := env.AdminToken(t)
	adminID := env.AdminUUID(t)
	cluster := defaultClusterID(t, srv.URL, admin)

	pub, _, _ := ed25519.GenerateKey(nil)
	pubB64 := base64.StdEncoding.EncodeToString(pub)
	enrollBody := func(token, pub, hostname, machineID string) *bytes.Buffer {
		return bytes.NewBufferString(`{"token":"` + token + `","hostname":"` + hostname +
			`","machineId":"` + machineID +
			`","osVersion":"Windows","agentVersion":"0.1.0","publicKey":"` + pub + `"}`)
	}
	enroll := func(token, pub, hostname, machineID string) int {
		t.Helper()
		resp, err := http.Post(srv.URL+"/api/agent/enroll", "application/json",
			enrollBody(token, pub, hostname, machineID))
		require.NoError(t, err)
		defer resp.Body.Close()
		return resp.StatusCode
	}
	var errBody struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	enrollCode := func(token, pub, hostname, machineID string) string {
		t.Helper()
		resp, err := http.Post(srv.URL+"/api/agent/enroll", "application/json",
			enrollBody(token, pub, hostname, machineID))
		require.NoError(t, err)
		defer resp.Body.Close()
		require.NoError(t, decodeJSON(resp.Body, &errBody))
		return errBody.Error.Code
	}

	// 1. token 不存在 → 401 ENROLLMENT_TOKEN_INVALID
	assert.Equal(t, 401, enroll("no-such-token", pubB64, "H", "m-reg-1"))
	assert.Equal(t, "ENROLLMENT_TOKEN_INVALID", enrollCode("no-such-token", pubB64, "H", "m-reg-1"))

	// 2. token 过期 → 410 ENROLLMENT_TOKEN_EXPIRED（DB 直改 expires_at）
	expTok := env.CreateEnrollToken(t)
	_, err := env.Store.Pool().Exec(t.Context(),
		`UPDATE enrollment_tokens SET expires_at = now() - interval '1 minute'
		 WHERE token_hash = $1`, tokens.Hash(expTok))
	require.NoError(t, err)
	assert.Equal(t, 410, enroll(expTok, pubB64, "H", "m-reg-2"))
	assert.Equal(t, "ENROLLMENT_TOKEN_EXPIRED", enrollCode(expTok, pubB64, "H", "m-reg-2"))

	// 3. 公钥格式非法 → 400（token 有效也不进入落库）
	assert.Equal(t, 400, enroll(env.CreateEnrollToken(t), "not-base64!!", "H", "m-reg-3"))

	// 4. 节点名冲突 → 后缀 -2（同 cluster 同 hostname 不同 machineId）
	tok4 := env.CreateEnrollToken(t)
	assert.Equal(t, 201, enroll(tok4, pubB64, "dup-host", "m-reg-4a"))
	resp5, err := http.Post(srv.URL+"/api/agent/enroll", "application/json",
		bytes.NewBufferString(`{"token":"`+env.CreateEnrollToken(t)+`","hostname":"dup-host",
			"machineId":"m-reg-4b","osVersion":"Windows","agentVersion":"0.1.0","publicKey":"`+pubB64+`"}`))
	require.NoError(t, err)
	defer resp5.Body.Close()
	require.Equal(t, 201, resp5.StatusCode)
	var n5 struct {
		NodeID string `json:"nodeId"`
		Name   string `json:"name"`
	}
	require.NoError(t, decodeJSON(resp5.Body, &n5))
	assert.Equal(t, "dup-host-2", n5.Name)

	// 5. audit node.enroll 列：user_id=token 创建者（admin）、cluster_id、node_id
	var actor, cid, nid string
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT user_id::text, cluster_id::text, node_id::text FROM audit_logs
		 WHERE action='node.enroll' AND node_id::text=$1`, n5.NodeID).Scan(&actor, &cid, &nid))
	assert.Equal(t, adminID.String(), actor)
	assert.Equal(t, cluster, cid)
	assert.Equal(t, n5.NodeID, nid)
}
