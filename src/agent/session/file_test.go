// file_test.go — File 处理器用例：upload 写入+校验、hash 不符删半成品、
// download 全量推送+终态、download 不可达。测试跨平台（无 build tag），故
// binary 发送助手自备（sendBin）而非复用 windows-only 的 shell_test.go writeBin。
package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

// runSessionFile 起 httptest WS 让 File 处理器作为客户端拨入；返回服务侧连接。
// 拨号错误经 errCh 回报（goroutine 内禁用 require）；LIFO 先 CloseNow 服务侧
// 连接再关 srv，解除 agent 关闭握手与 srv.Close 等待 handler 退出的互等。
func runSessionFile(t *testing.T, params proto.FileParams) *websocket.Conn {
	t.Helper()
	up := make(chan *websocket.Conn, 1)
	errCh := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		up <- c
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	raw, _ := json.Marshal(params)
	go func() {
		c, _, err := websocket.Dial(context.Background(), "ws"+srv.URL[4:], nil)
		if err != nil {
			errCh <- err
			return
		}
		NewFile(testLogger()).Handle(context.Background(), c, "sess-file", raw)
	}()
	select {
	case c := <-up:
		t.Cleanup(func() { c.CloseNow() })
		return c
	case err := <-errCh:
		t.Fatalf("file dial failed: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("no ws connection")
	}
	return nil
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// sendBin 发送 binary 帧（upload 数据体）。与 shell_test.go 的 windows-only
// writeBin 同义但跨平台，名不同以避免 windows 构建下重复声明。
func sendBin(t *testing.T, ws *websocket.Conn, b []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, ws.Write(ctx, websocket.MessageBinary, b))
}

// skipBegin 读一帧并断言为 FILE_BEGIN，返回解码后的 payload。
func skipBegin(t *testing.T, ws *websocket.Conn) proto.FileBegin {
	t.Helper()
	kind, data := readFrame(t, ws)
	require.Equal(t, "text", kind)
	var m proto.Message
	require.NoError(t, json.Unmarshal(data, &m))
	require.Equal(t, typeFileBegin, m.Type)
	var fb proto.FileBegin
	require.NoError(t, m.Decode(&fb))
	return fb
}

// readUntilResult 循环收集 binary 帧直至 FILE_RESULT text：返回累积内容与
// 解码后的终态。
func readUntilResult(t *testing.T, ws *websocket.Conn) ([]byte, proto.FileResult) {
	t.Helper()
	var content bytes.Buffer
	for {
		kind, data := readFrame(t, ws)
		switch kind {
		case "binary":
			content.Write(data)
		case "text":
			var m proto.Message
			require.NoError(t, json.Unmarshal(data, &m))
			require.Equal(t, typeFileResult, m.Type)
			var fr proto.FileResult
			require.NoError(t, m.Decode(&fr))
			return content.Bytes(), fr
		}
	}
}

func TestFileUploadWriteAndVerify(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "f.bin")
	content := []byte("hello-file-upload")
	ws := runSessionFile(t, proto.FileParams{
		Direction: "upload", Path: dst, Size: int64(len(content)), Sha256: sha256Hex(content),
	})

	// FILE_BEGIN 先行（声明 size/sha256 回显）
	fb := skipBegin(t, ws)
	assert.Equal(t, "upload", fb.Direction)
	assert.Equal(t, dst, fb.Path)
	assert.Equal(t, int64(len(content)), fb.Size)

	// binary 数据 → FILE_RESULT ok=true，文件落盘
	sendBin(t, ws, content)
	_, fr := readUntilResult(t, ws)
	assert.True(t, fr.Ok)
	assert.Equal(t, int64(len(content)), fr.Bytes)
	assert.Equal(t, sha256Hex(content), fr.Sha256)
	written, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, content, written)
}

func TestFileUploadHashMismatchDeletes(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "bad.bin")
	ws := runSessionFile(t, proto.FileParams{
		Direction: "upload", Path: dst, Size: 4, Sha256: strings.Repeat("a", 64), // 声明与实际不符
	})
	skipBegin(t, ws)
	sendBin(t, ws, []byte("data"))

	kind, data := readFrame(t, ws)
	require.Equal(t, "text", kind)
	var m proto.Message
	require.NoError(t, json.Unmarshal(data, &m))
	require.Equal(t, typeFileError, m.Type)
	var fe proto.FileError
	require.NoError(t, m.Decode(&fe))
	assert.Equal(t, proto.CodeHashMismatch, fe.Code)
	_, err := os.Stat(dst)
	assert.True(t, os.IsNotExist(err), "half-written file must be deleted")
	_, err = os.Stat(dst + ".xnc-part")
	assert.True(t, os.IsNotExist(err), "temp part file must not linger")
}

func TestFileDownloadStreamAndVerify(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "g.bin")
	content := []byte("download-me")
	require.NoError(t, os.WriteFile(src, content, 0o600))

	ws := runSessionFile(t, proto.FileParams{Direction: "download", Path: src})
	// FILE_BEGIN → binary 全量 → FILE_RESULT
	fb := skipBegin(t, ws)
	assert.Equal(t, "download", fb.Direction)
	assert.Equal(t, int64(len(content)), fb.Size)

	got, fr := readUntilResult(t, ws)
	assert.Equal(t, content, got)
	assert.True(t, fr.Ok)
	assert.Equal(t, int64(len(content)), fr.Bytes)
	assert.Equal(t, sha256Hex(content), fr.Sha256)
}

func TestFileDownloadNotFound(t *testing.T) {
	ws := runSessionFile(t, proto.FileParams{
		Direction: "download", Path: filepath.Join(t.TempDir(), "missing.bin"),
	})
	kind, data := readFrame(t, ws)
	require.Equal(t, "text", kind)
	var m proto.Message
	require.NoError(t, json.Unmarshal(data, &m))
	require.Equal(t, typeFileError, m.Type)
	var fe proto.FileError
	require.NoError(t, m.Decode(&fe))
	assert.Equal(t, proto.CodeFileNotFound, fe.Code)
}

// TestFileAccessErrCodeMapping：写路径失败（rename 覆盖被锁目标等）的错误码
// 映射——权限/占用类 errno → ACCESS_DENIED，其余 → INTERNAL；绝不回误导性的
// FILE_NOT_FOUND。覆盖真实的 os.Rename *os.LinkError 形态与裸 errno。
func TestFileAccessErrCodeMapping(t *testing.T) {
	access := []error{
		&os.LinkError{Op: "rename", Old: "a.xnc-part", New: "a.exe", Err: syscall.EACCES},
		&os.LinkError{Op: "rename", Old: "a.xnc-part", New: "a.exe", Err: syscall.EPERM},
		&os.PathError{Op: "create", Path: "a.xnc-part", Err: syscall.EACCES},
		&os.PathError{Op: "mkdir", Path: "a", Err: syscall.EPERM},
		syscall.EACCES,
	}
	for _, err := range access {
		assert.Equal(t, proto.CodeAccessDenied, fileAccessErrCode(err), "err=%v", err)
	}
	other := []error{
		&os.LinkError{Op: "rename", Old: "a.xnc-part", New: "a.exe", Err: syscall.EXDEV}, // 跨卷
		errors.New("disk exploded"),
		nil,
	}
	for _, err := range other {
		assert.Equal(t, proto.CodeInternal, fileAccessErrCode(err), "err=%v", err)
	}
}
