package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

// sha256Hex returns the lowercase hex sha256 of b (frame-vocabulary helper).
func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// fileNodesJSON: the one-node list the file fakes serve for name resolution.
// The node's id equals its name so the POST paths below match either form.
const fileNodesJSON = `[{"id":"n1","name":"n1","cluster":"default",` +
	`"hostname":"N1","os_version":"Windows","agent_version":"0.1.0",` +
	`"shell_type":"pwsh","status":"online","last_seen_at":null}]`

// fileSession202: the 202 session handshake both endpoints answer with.
const fileSession202 = `{"sessionId":"fs1","token":"ct",` +
	`"websocketUrl":"/api/session/fs1?token=ct","expiresAt":"2026-01-01T00:00:00Z"}`

// fakeFileServer: POST /files/upload or /files/download → 202 + session; the
// session WS runs a fake agent speaking the file frame vocabulary. Upload mode
// (downloadContent == nil) reads exactly the size declared in the POST body
// and answers FILE_RESULT echoing the declared hash; download mode sends
// FILE_BEGIN + binary + FILE_RESULT with the real hash, then closes.
func fakeFileServer(t *testing.T, downloadContent []byte) *httptest.Server {
	t.Helper()
	var uploadSize int64
	var uploadSha string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/nodes" && r.Method == "GET":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fileNodesJSON))
		case r.URL.Path == "/api/nodes/n1/files/upload" && r.Method == "POST":
			assert.Equal(t, "Bearer tk", r.Header.Get("Authorization"))
			var req struct {
				Path   string `json:"path"`
				Size   int64  `json:"size"`
				Sha256 string `json:"sha256"`
			}
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			uploadSize, uploadSha = req.Size, req.Sha256
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(202)
			_, _ = w.Write([]byte(fileSession202))
		case r.URL.Path == "/api/nodes/n1/files/download" && r.Method == "POST":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(202)
			_, _ = w.Write([]byte(fileSession202))
		case r.URL.Path == "/api/session/fs1":
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			go func() {
				// r.Context() dies when the handler returns: write on a
				// private background ctx like the real agent does.
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				defer c.CloseNow()
				if downloadContent != nil { // download mode
					_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"FILE_BEGIN","payload":{"direction":"download","path":"p","size":`+strconv.Itoa(len(downloadContent))+`,"sha256":""}}`))
					_ = c.Write(ctx, websocket.MessageBinary, downloadContent)
					_ = c.Write(ctx, websocket.MessageText,
						[]byte(`{"type":"FILE_RESULT","payload":`+mustJSONStr(proto.FileResult{
							Bytes: int64(len(downloadContent)), Sha256: sha256Hex(downloadContent), Ok: true,
						})+`}`))
					_ = c.Close(websocket.StatusNormalClosure, "")
					return
				}
				// upload mode: read exactly the declared byte count, then reply.
				var total int64
				for total < uploadSize {
					typ, data, err := c.Read(ctx)
					if err != nil {
						return
					}
					if typ == websocket.MessageBinary {
						total += int64(len(data))
					}
				}
				_ = c.Write(ctx, websocket.MessageText,
					[]byte(`{"type":"FILE_RESULT","payload":`+mustJSONStr(proto.FileResult{
						Bytes: total, Sha256: uploadSha, Ok: true,
					})+`}`))
				_ = c.Close(websocket.StatusNormalClosure, "")
			}()
		default:
			http.NotFound(w, r)
		}
	}))
	return srv
}

// fakeFileServerMismatch: download whose verify fails on the writing side —
// FILE_RESULT ok=false followed by FILE_ERROR HASH_MISMATCH (both terminal
// shapes; the CLI must exit 246 on whichever arrives first).
func fakeFileServerMismatch(t *testing.T) *httptest.Server {
	t.Helper()
	content := []byte("corrupted-in-flight")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/nodes" && r.Method == "GET":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fileNodesJSON))
		case r.URL.Path == "/api/nodes/n1/files/download" && r.Method == "POST":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(202)
			_, _ = w.Write([]byte(fileSession202))
		case r.URL.Path == "/api/session/fs1":
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				defer c.CloseNow()
				_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"FILE_BEGIN","payload":{"direction":"download","path":"p","size":`+strconv.Itoa(len(content))+`,"sha256":""}}`))
				_ = c.Write(ctx, websocket.MessageBinary, content)
				_ = c.Write(ctx, websocket.MessageText,
					[]byte(`{"type":"FILE_RESULT","payload":`+mustJSONStr(proto.FileResult{
						Bytes: int64(len(content)), Sha256: "deadbeef", Ok: false,
					})+`}`))
				_ = c.Write(ctx, websocket.MessageText,
					[]byte(`{"type":"FILE_ERROR","payload":`+mustJSONStr(proto.FileError{
						Code: proto.CodeHashMismatch,
					})+`}`))
				_ = c.Close(websocket.StatusNormalClosure, "")
			}()
		default:
			http.NotFound(w, r)
		}
	}))
	return srv
}

func TestUploadSendsBinaryAndParsesResult(t *testing.T) {
	payload := []byte("upload-payload") // 14 bytes
	dir := t.TempDir()
	local := filepath.Join(dir, "l.bin")
	require.NoError(t, os.WriteFile(local, payload, 0o600))

	srv := fakeFileServer(t, nil)
	defer srv.Close()

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"upload", "n1", local, `C:\remote\l.bin`,
			"--json", "--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 0, code)
	assert.Contains(t, out, `"ok":true`)
	assert.Contains(t, out, `"bytes":14`)
	assert.Contains(t, out, `"sha256":"`+sha256Hex(payload)+`"`, "agent echoes the declared hash")
}

func TestDownloadWritesFile(t *testing.T) {
	srv := fakeFileServer(t, []byte("download-payload"))
	defer srv.Close()
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.bin")

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"download", "n1", `C:\remote\out.bin`, dst,
			"--json", "--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 0, code)
	assert.Contains(t, out, `"ok":true`)
	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, []byte("download-payload"), got)
}

func TestHashMismatchExits246(t *testing.T) {
	srv := fakeFileServerMismatch(t)
	defer srv.Close()
	dir := t.TempDir()
	_, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"download", "n1", `C:\x`, filepath.Join(dir, "o"),
			"--json", "--server", srv.URL, "--token", "tk"})
	})
	assert.Equal(t, 246, code)
}

// fakeFileServerAccessDenied: upload whose rename fails on the agent side —
// FILE_ERROR ACCESS_DENIED carrying the OS error text (locked target exe).
func fakeFileServerAccessDenied(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/nodes" && r.Method == "GET":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(fileNodesJSON))
		case r.URL.Path == "/api/nodes/n1/files/upload" && r.Method == "POST":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(202)
			_, _ = w.Write([]byte(fileSession202))
		case r.URL.Path == "/api/session/fs1":
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				defer c.CloseNow()
				// 不读上传数据：直接终态 FILE_ERROR（错误码 + 底层错误文本透传）
				_ = c.Write(ctx, websocket.MessageText,
					[]byte(`{"type":"FILE_ERROR","payload":`+mustJSONStr(proto.FileError{
						Code:    proto.CodeAccessDenied,
						Message: `rename C:\x\app.exe.xnc-part C:\x\app.exe: Access is denied.`,
					})+`}`))
				_ = c.Close(websocket.StatusNormalClosure, "")
			}()
		default:
			http.NotFound(w, r)
		}
	}))
	return srv
}

// TestAccessDeniedExits250WithMessage：覆盖被锁目标的 put（rename 失败）必须
// 报明确错误（ACCESS_DENIED + 底层错误文本），而非 FILE_NOT_FOUND/244。
func TestAccessDeniedExits250WithMessage(t *testing.T) {
	srv := fakeFileServerAccessDenied(t)
	defer srv.Close()
	dir := t.TempDir()
	local := filepath.Join(dir, "l.bin")
	require.NoError(t, os.WriteFile(local, []byte("x"), 0o600))

	stderr, code := captureStderr(t, func() int {
		return runCLI(t.Context(), []string{"put", "n1", local, `C:\x\app.exe`,
			"--server", srv.URL, "--token", "tk"})
	})
	assert.Equal(t, 250, code)
	assert.Contains(t, stderr, "ACCESS_DENIED")
	assert.Contains(t, stderr, "Access is denied.")
	assert.NotContains(t, stderr, "FILE_NOT_FOUND")
}
