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

func shellPost(t *testing.T, env *TestEnv, nodeID, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", env.srv.URL+"/api/nodes/"+nodeID+"/shell",
		bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+env.AdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestShellSessionEndToEnd(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-SH1", "mid-sh1")

	// 离线 → 409
	resp := shellPost(t, env, nodeID, `{}`)
	defer resp.Body.Close()
	assert.Equal(t, 409, resp.StatusCode)

	ctrl := dialControl(t, env, nodeID)

	// 校验失败：cols 越界 / rows 为负 / shell 名非法
	for _, b := range []string{`{"cols":1001}`, `{"rows":-1}`, `{"shell":"cmd"}`} {
		r := shellPost(t, env, nodeID, b)
		_ = r.Body.Close()
		assert.Equal(t, 400, r.StatusCode, b)
	}

	// 在线：假 agent 走 SHELL_BEGIN → VT 帧 → 断开
	var created struct {
		SessionID    string `json:"sessionId"`
		Token        string `json:"token"`
		WebsocketURL string `json:"websocketUrl"`
	}
	agentDone := make(chan struct{})
	go func() {
		defer close(agentDone)
		fakeAgentSession(t, ctrl, func(aws *websocket.Conn) {
			// "SHELL_BEGIN" 是 shell kind 的会话帧私有词汇（server 不解析会话帧），
			// 与 mustMsg 的 "EXEC_RESULT" 同理，仅允许出现在测试与 agent 实现中。
			beginMsg, err := proto.NewMsg("SHELL_BEGIN", proto.ShellBegin{Shell: "powershell"})
			require.NoError(t, err)
			wsWriteText(t, aws, beginMsg)
			wsWriteBinary(t, aws, []byte("\x1b[2JPS> "))
			_ = aws.Close(websocket.StatusNormalClosure, "")
		})
	}()

	resp2 := shellPost(t, env, nodeID, `{"cols":100,"rows":40,"shell":"powershell"}`)
	defer resp2.Body.Close()
	require.Equal(t, 202, resp2.StatusCode)
	require.NoError(t, decodeJSON(resp2.Body, &created))
	assert.Contains(t, created.WebsocketURL, "/api/session/"+created.SessionID)

	cl := dialClientSession(t, env.srv.URL, "/api/session/"+created.SessionID, created.Token)
	begin := readMsg(t, cl)
	require.Equal(t, "SHELL_BEGIN", begin.Type)
	var sb proto.ShellBegin
	require.NoError(t, begin.Decode(&sb))
	assert.Equal(t, "powershell", sb.Shell)
	vt := readBin(t, cl)
	assert.Contains(t, string(vt), "PS>")

	select {
	case <-agentDone:
	case <-time.After(5 * time.Second):
		t.Fatal("agent goroutine stuck")
	}

	// 审计对（shell.open / shell.close，metadata 无 VT 内容）
	require.Eventually(t, func() bool {
		var n int
		require.NoError(t, env.Store.Pool().QueryRow(t.Context(),
			`SELECT count(*) FROM audit_logs WHERE action IN ('shell.open','shell.close')`).Scan(&n))
		return n >= 2
	}, 5*time.Second, 200*time.Millisecond)
}

func TestShellParamsDefaultsInSessionOpen(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-SH2", "mid-sh2")
	ctrl := dialControl(t, env, nodeID)

	// captureOnly 变体：读控制连接上的 SESSION_OPEN 即送 channel，不拨会话。
	openCh := captureSessionOpen(t, ctrl)

	resp := shellPost(t, env, nodeID, `{}`)
	defer resp.Body.Close()
	require.Equal(t, 202, resp.StatusCode)

	select {
	case so := <-openCh:
		require.Equal(t, proto.KindShell, so.Kind)
		var p proto.ShellParams
		require.NoError(t, jsonUnmarshal(so.Params, &p))
		assert.Equal(t, 120, p.Cols)
		assert.Equal(t, 30, p.Rows)
	case <-time.After(3 * time.Second):
		t.Fatal("no SESSION_OPEN")
	}
}
