package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

const execNodeUUID = "11111111-1111-4111-8111-111111111111"

// jsonUnmarshalStr / mustJSONStr: string-form JSON helpers (no existing
// equivalents in main_test.go, which only has the capture* helpers).
func jsonUnmarshalStr(s string, v any) error { return json.Unmarshal([]byte(s), v) }

func mustJSONStr(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// fakeExecServer emulates the T4 contract: GET /api/nodes resolves names,
// POST /api/nodes/{id}/exec returns a 202 session, and the session WS streams
// binary [0x01|0x02] frames then a terminal EXEC_RESULT text frame (agent
// closes gracefully after it, so the CLI read loop ends via close error).
// exitCode nil = timed out ({"exitCode":null,"timedOut":true}).
func fakeExecServer(t *testing.T, exitCode *int, wantCommand, stdout, stderr string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/nodes" && r.Method == "GET":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":"` + execNodeUUID + `","name":"n1","cluster":"default",` +
				`"hostname":"N1","os_version":"Windows","agent_version":"0.1.0",` +
				`"shell_type":"pwsh","status":"online","last_seen_at":null}]`))
		case r.URL.Path == "/api/nodes/"+execNodeUUID+"/exec" && r.Method == "POST":
			assert.Equal(t, "Bearer tk", r.Header.Get("Authorization"))
			var req struct {
				Command    string `json:"command"`
				TimeoutSec int    `json:"timeoutSec"`
				Cwd        string `json:"cwd"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			assert.Equal(t, wantCommand, req.Command, "args after -- joined by space")
			assert.Equal(t, 300, req.TimeoutSec, "--timeout default 300")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"sessionId":"s1","token":"ct","expiresAt":"2026-01-01T00:00:00Z",` +
				`"websocketUrl":"/api/session/s1?token=ct"}`))
		case r.URL.Path == "/api/session/s1":
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			go func() {
				// r.Context() dies when the handler returns: write on a private
				// background ctx like the real agent does.
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				defer c.CloseNow()
				_ = c.Write(ctx, websocket.MessageBinary, append([]byte{0x01}, stdout...))
				_ = c.Write(ctx, websocket.MessageBinary, append([]byte{0x02}, stderr...))
				_ = c.Write(ctx, websocket.MessageText,
					[]byte(`{"type":"EXEC_RESULT","payload":`+mustJSONStr(proto.ExecResult{
						ExitCode: exitCode, TimedOut: exitCode == nil, DurationMs: 5,
					})+`}`))
				_ = c.Close(websocket.StatusNormalClosure, "")
			}()
		default:
			http.NotFound(w, r)
		}
	}))
	return srv
}

// lastJSONLine returns the final line of out: with --json the live stream is
// printed first and the envelope after it, so jsonl consumers see the envelope
// as its own trailing line.
func lastJSONLine(t *testing.T, out string) string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	require.GreaterOrEqual(t, len(lines), 1, "no envelope line in %q", out)
	return lines[len(lines)-1]
}

func TestExecStreamsAndPassthroughExit(t *testing.T) {
	seven := 7
	srv := fakeExecServer(t, &seven, "hostname", "out-line\n", "err-line\n")
	defer srv.Close()

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"exec", "n1", "--json",
			"--server", srv.URL, "--token", "tk", "--", "hostname"})
	})
	require.Equal(t, 7, code) // exit-code passthrough
	// Live stream lands on stdout before the envelope.
	assert.True(t, strings.HasPrefix(out, "out-line\n"), "stream first, got %q", out)
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Node      string `json:"node"`
			ExitCode  int    `json:"exitCode"`
			Stdout    string `json:"stdout"`
			Stderr    string `json:"stderr"`
			Duration  int64  `json:"durationMs"`
			TimedOut  bool   `json:"timedOut"`
		} `json:"data"`
	}
	require.NoError(t, jsonUnmarshalStr(lastJSONLine(t, out), &env))
	assert.True(t, env.OK)
	assert.Equal(t, "n1", env.Data.Node) // resolved name, not the raw arg
	assert.Equal(t, 7, env.Data.ExitCode)
	assert.Equal(t, "out-line\n", env.Data.Stdout)
	assert.Equal(t, "err-line\n", env.Data.Stderr)
	assert.False(t, env.Data.TimedOut)
	assert.Equal(t, int64(5), env.Data.Duration)
}

func TestExecTimedOutExits243(t *testing.T) {
	srv := fakeExecServer(t, nil, "slow", "", "") // exitCode null + timedOut true
	defer srv.Close()

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"exec", "n1", "--json",
			"--server", srv.URL, "--token", "tk", "--", "slow"})
	})
	assert.Equal(t, 243, code)
	var env struct {
		Data struct {
			ExitCode *int `json:"exitCode"`
			TimedOut bool `json:"timedOut"`
		} `json:"data"`
	}
	require.NoError(t, jsonUnmarshalStr(lastJSONLine(t, out), &env))
	assert.Nil(t, env.Data.ExitCode)
	assert.True(t, env.Data.TimedOut)
}

func TestExecNodeOfflineEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/nodes" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":"` + execNodeUUID + `","name":"n1","cluster":"default",` +
				`"hostname":"N1","os_version":"Windows","agent_version":"0.1.0",` +
				`"shell_type":"pwsh","status":"offline","last_seen_at":null}]`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(409)
		_, _ = w.Write([]byte(`{"error":{"code":"NODE_OFFLINE","message":"node is offline"}}`))
	}))
	defer srv.Close()

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"exec", "n1", "--json",
			"--server", srv.URL, "--token", "tk", "--", "hostname"})
	})
	require.Equal(t, 242, code)
	var env struct {
		OK    bool            `json:"ok"`
		Error *proto.APIError `json:"error"`
	}
	require.NoError(t, jsonUnmarshalStr(strings.TrimSpace(out), &env))
	assert.False(t, env.OK)
	require.NotNil(t, env.Error)
	assert.Equal(t, proto.CodeNodeOffline, env.Error.Code)
}
