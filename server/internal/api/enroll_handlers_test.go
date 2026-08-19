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
