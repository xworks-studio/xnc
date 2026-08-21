package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"xnc/proto"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeBody(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// absoluteWSURL：http→ws / https→wss + 相对路径拼接。
func TestAbsoluteWSURL(t *testing.T) {
	assert.Equal(t, "ws://srv/api/session/abc?token=t",
		absoluteWSURL("http://srv", "/api/session/abc?token=t"))
	assert.Equal(t, "wss://srv:8443/api/session/abc",
		absoluteWSURL("https://srv:8443/", "/api/session/abc"))
}

// screenPreviewHTML：内嵌节点名与绝对 WS URL，含 WebCodecs 关键要素。
func TestScreenPreviewHTML(t *testing.T) {
	h := screenPreviewHTML("node-7", "wss://srv/api/session/abc?token=tk")
	assert.Contains(t, h, "XNC Screen — node-7")
	assert.Contains(t, h, `new WebSocket("wss://srv/api/session/abc?token=tk")`)
	assert.Contains(t, h, "VideoDecoder")
	assert.Contains(t, h, "avc1.4D4028") // 默认 codec 串兜底
	assert.Contains(t, h, "sub === 1")   // 子头协议：0x01 key
	assert.Contains(t, h, "SCREEN_BEGIN")
}

// fakeScreenServer：POST /screen 校验 body 中的 snapshot 标志，回 202 +
// websocketUrl；会话 WS 发一个 binary JPEG 帧（0xFF 0xD8 ...）后关闭。
func fakeScreenServer(t *testing.T, gotSnapshot *bool, jpeg []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/nodes" && r.Method == "GET":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":"` + execNodeUUID + `","name":"n1","cluster":"default",` +
				`"hostname":"N1","os_version":"Windows","agent_version":"0.1.0",` +
				`"shell_type":"pwsh","status":"online","last_seen_at":null}]`))
		case r.URL.Path == "/api/nodes/"+execNodeUUID+"/screen":
			// 升级为会话 WS（同路复用：CLI 拿到 202 后按 websocketUrl 回拨本 handler，GET）。
			if r.URL.Query().Get("token") != "" {
				c, err := websocket.Accept(w, r, nil)
				require.NoError(t, err)
				defer c.CloseNow()
				require.NoError(t, c.Write(context.Background(), websocket.MessageBinary, jpeg))
				return
			}
			var body struct {
				Snapshot bool `json:"snapshot"`
			}
			if r.Method != "POST" {
				http.NotFound(w, r)
				return
			}
			require.NoError(t, decodeBody(r, &body))
			*gotSnapshot = body.Snapshot
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"sessionId":"s1","token":"tk","websocketUrl":"/api/nodes/` + execNodeUUID + `/screen?token=tk"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestRunScreenSnapshot：--snapshot 全链路（fake server）→ JPEG 写入输出文件。
func TestRunScreenSnapshot(t *testing.T) {
	gotSnapshot := false
	jpeg := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("...jpeg bytes...")...)
	frame := append([]byte{proto.ScreenBinJPEG}, jpeg...)
	srv := fakeScreenServer(t, &gotSnapshot, frame)

	out := t.TempDir() + "/snap.jpg"
	cmd := newRootCmd()
	cmd.SetArgs([]string{"--server", srv.URL, "--token", "t",
		"screen", execNodeUUID, "--snapshot", out})
	err := cmd.Execute()
	require.NoError(t, err)
	assert.True(t, gotSnapshot, "snapshot flag must reach POST /screen")
	got, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, jpeg, got)
}
