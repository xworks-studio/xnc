package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

// TestNodeSas：POST /api/nodes/{id}/desktop/sas 的四条路径——离线 409、
// 回执成功 200(ok)、core 拒绝 200(ok=false, code)、超时 504。
// 假 agent = dialControl 控制连接 + 回显 goroutine（仅 SAS_REQUEST 有应答）。
func TestNodeSas(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "SAS-01", "mid-sas")

	post := func(t *testing.T) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequest("POST",
			env.srv.URL+"/api/nodes/"+nodeID+"/desktop/sas",
			strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+env.AdminToken(t))
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		var body map[string]any
		require.NoError(t, decodeJSON(resp.Body, &body))
		return resp.StatusCode, body
	}

	// echoAgent 起"收到 SAS_REQUEST 即按 policy 应答"的假 agent。
	echoAgent := func(t *testing.T, reply func(sr proto.SasRequest) proto.SasResult) *websocket.Conn {
		t.Helper()
		c := dialControl(t, env, nodeID)
		go func() {
			for {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				_, data, err := c.Read(ctx)
				cancel()
				if err != nil {
					return
				}
				var m proto.Message
				if json.Unmarshal(data, &m) != nil || m.Type != proto.TypeSasRequest {
					continue
				}
				var sr proto.SasRequest
				if m.Decode(&sr) != nil || reply == nil {
					continue
				}
				res, _ := proto.NewMsg(proto.TypeSasResult, reply(sr))
				b, _ := json.Marshal(res)
				wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = c.Write(wctx, websocket.MessageText, b)
				wcancel()
			}
		}()
		require.Eventually(t, func() bool {
			for _, n := range env.ListNodes(t) {
				if n["status"] == "online" {
					return true
				}
			}
			return false
		}, 5*time.Second, 100*time.Millisecond, "node should be online after dial")
		return c
	}

	t.Run("offline 409", func(t *testing.T) {
		code, body := post(t)
		assert.Equal(t, 409, code)
		errObj, _ := body["error"].(map[string]any)
		require.NotNil(t, errObj, "error envelope: %v", body)
		assert.Equal(t, proto.CodeNodeOffline, errObj["code"])
	})

	t.Run("ok roundtrip", func(t *testing.T) {
		echoAgent(t, func(sr proto.SasRequest) proto.SasResult {
			assert.NotEmpty(t, sr.ReqID)
			assert.NotEmpty(t, sr.Reason) // 触发者标识随请求下发
			return proto.SasResult{ReqID: sr.ReqID, OK: true}
		})
		code, body := post(t)
		require.Equal(t, 200, code)
		assert.Equal(t, true, body["ok"])
	})

	t.Run("core rejected", func(t *testing.T) {
		echoAgent(t, func(sr proto.SasRequest) proto.SasResult {
			return proto.SasResult{ReqID: sr.ReqID, OK: false, Code: "SAS_DENIED"}
		})
		code, body := post(t)
		require.Equal(t, 200, code) // 节点已处理；结果为拒绝——非传输错误
		assert.Equal(t, false, body["ok"])
		assert.Equal(t, "SAS_DENIED", body["code"])
	})

	t.Run("timeout 504", func(t *testing.T) {
		old := sasAckTimeout
		sasAckTimeout = 300 * time.Millisecond
		defer func() { sasAckTimeout = old }()
		echoAgent(t, nil) // nil = 收到也不应答（nil 回调在 goroutine 里跳过）
		code, _ := post(t)
		assert.Equal(t, 504, code)
	})
}

// TestNodeSasRBAC：随机 node id → 404（不泄漏存在性；角色链路由
// requireMinRole 自身的测试矩阵覆盖）。
func TestNodeSasRBAC(t *testing.T) {
	env := NewTestEnv(t)
	req, _ := http.NewRequest("POST",
		env.srv.URL+"/api/nodes/00000000-0000-0000-0000-000000000000/desktop/sas",
		strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+env.AdminToken(t))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, 404, resp.StatusCode)
}
