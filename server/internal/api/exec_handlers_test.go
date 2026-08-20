package api

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

func execPost(t *testing.T, env *TestEnv, nodeID, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", env.srv.URL+"/api/nodes/"+nodeID+"/exec",
		bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+env.AdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestExecSessionEndToEnd(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-E1", "mid-e1")

	// 离线 → 409 NODE_OFFLINE（节点注册但未连控制连接）
	resp := execPost(t, env, nodeID, `{"command":"hostname"}`)
	defer resp.Body.Close()
	assert.Equal(t, 409, resp.StatusCode)
	var e1 struct {
		Error proto.APIError `json:"error"`
	}
	_ = decodeJSON(resp.Body, &e1)
	assert.Equal(t, proto.CodeNodeOffline, e1.Error.Code)

	// 在线：控制连接建立后创建 exec 会话。agent 侧并发等待 SESSION_OPEN。
	ctrl := dialControl(t, env, nodeID)
	agentDone := make(chan proto.SessionOpen, 1)
	go func() {
		agentDone <- fakeAgentSession(t, ctrl, func(aws *websocket.Conn) {
			wsWriteBinary(t, aws, append([]byte{0x01}, []byte("WEB-E1-OUT")...))
			wsWriteBinary(t, aws, append([]byte{0x02}, []byte("some-err")...))
			ec := 7
			wsWriteText(t, aws, mustMsg(t, proto.ExecResult{ExitCode: &ec, DurationMs: 12}))
			_ = aws.Close(websocket.StatusNormalClosure, "")
		})
	}()

	var created struct {
		SessionID    string    `json:"sessionId"`
		Token        string    `json:"token"`
		ExpiresAt    time.Time `json:"expiresAt"`
		WebsocketURL string    `json:"websocketUrl"`
	}
	resp2 := execPost(t, env, nodeID, `{"command":"hostname","timeoutSec":60}`)
	defer resp2.Body.Close()
	require.Equal(t, 202, resp2.StatusCode)
	require.NoError(t, decodeJSON(resp2.Body, &created))
	require.NotEmpty(t, created.SessionID)
	require.NotEmpty(t, created.Token)
	assert.False(t, created.ExpiresAt.IsZero())
	assert.Contains(t, created.WebsocketURL, "/api/session/"+created.SessionID)

	// client 立即拨号（sessionId/token 已随 202 到手）：与 agent goroutine 并发
	// attach，pump 双侧齐备即启动转发。绝不在此处等待 agentDone——agent 的
	// aws.Close 关闭握手要等 server 侧读到 close 帧（即 pump 已在跑），先等后拨
	// 会把握手卡满 coder/websocket 硬编码的 5s waitCloseHandshake。
	cl := dialClientSession(t, env.srv.URL, "/api/session/"+created.SessionID, created.Token)
	// 两条 binary 帧按序到达（stdout 前缀 0x01 / stderr 前缀 0x02）
	d1 := readBin(t, cl)
	require.Equal(t, byte(0x01), d1[0])
	assert.Equal(t, "WEB-E1-OUT", string(d1[1:]))
	d2 := readBin(t, cl)
	require.Equal(t, byte(0x02), d2[0])
	// 终态 text：EXEC_RESULT
	typ, data := readRaw(t, cl)
	require.Equal(t, "text", typ)
	var m proto.Message
	require.NoError(t, jsonUnmarshal(data, &m))
	var res proto.ExecResult
	require.NoError(t, m.Decode(&res))
	require.NotNil(t, res.ExitCode)
	assert.Equal(t, 7, *res.ExitCode)

	// agent 侧完成屏障：act（含优雅关闭握手）全部落地后才继续，并顺带校验
	// SESSION_OPEN 的 kind/AgentToken 与 agent 会话 WS URL 齐备。
	so := <-agentDone
	assert.Equal(t, proto.KindExec, so.Kind)
	assert.Equal(t, created.SessionID, so.SessionID)
	assert.NotEmpty(t, so.AgentToken)
	assert.Contains(t, so.WsURL, "/api/agent/session?token=")

	// 审计：exec.start 存在；等待 finish（会话关闭路径）
	require.Eventually(t, func() bool {
		var n int
		require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
			`SELECT count(*) FROM audit_logs WHERE action IN ('exec.start','exec.finish')`).Scan(&n))
		return n >= 2
	}, 5*time.Second, 200*time.Millisecond)
	// 审计 metadata 无命令内容
	var meta string
	require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
		`SELECT metadata::text FROM audit_logs WHERE action='exec.start'`).Scan(&meta))
	assert.NotContains(t, meta, "hostname")
}

func TestExecValidation(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-E2", "mid-e2")
	_ = dialControl(t, env, nodeID)

	cases := []struct {
		body string
		code int
	}{
		{`{}`, 400},                                                    // command/script 均空
		{`{"command":"a","script":"b"}`, 400},                          // 同时给
		{`{"command":"a","timeoutSec":0}`, 400},                        // 显式 0 越界（缺省须省略字段）
		{`{"command":"a","timeoutSec":100000}`, 400},                   // 越界
		{`{"script":"` + string(make([]byte, 256*1024+1)) + `"}`, 400}, // 超限（\x00 字节 JSON 合法）
	}
	for _, c := range cases {
		resp := execPost(t, env, nodeID, c.body)
		_ = resp.Body.Close()
		label := c.body
		if len(label) > 23 {
			label = label[:20] + "..."
		}
		assert.Equal(t, c.code, resp.StatusCode, label)
	}
}
