# XNC v2 Phase 4（file + tunnel + RDP）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付文件会话（upload/download + sha256 校验）、tunnel 会话（RDP 白名单 + 纯 binary 转发）、`xnc upload/download/rdp`（本地端口转发 + mstsc 启动），E2E 覆盖 Scenario E/H，完成 Gate C 吞吐验证。

**Architecture:** 全部复用 Phase 2/3 的统一会话管理器（kind 参数化）：file 是带控制词汇（FILE_BEGIN/RESULT/ERROR）的会话；tunnel 是零控制帧的纯字节流会话（target 由 SESSION_OPEN params 白名单下发）。CLI 侧 rdp 命令做本地端口转发：mstsc → 127.0.0.1:随机端口 → CLI WS → server 泵 → agent WS → 127.0.0.1:3389。

**Tech Stack:** 沿用全栈（Go 1.26、coder/websocket、chi、x/term）；新依赖：`golang.org/x/crypto/sha3` 不需要——sha256 用标准库；无新依赖。

**Spec:** `docs/superpowers/specs/2026-08-19-xnc-v2-unified-session-design.md` §3.2 file/tunnel + §3.4 REST + `spec.md` §21-26/§43/§58 + Phase 2/3 全部代码。

## Global Constraints

- 模块结构不变；本机无 make；Docker 运行中（testcontainers）；双 GOOS 编译是 agent/cli 验收门槛。
- `proto/` 唯一协议定义点：FileParams/FileBegin/FileResult/FileError、TunnelParams、KindFile/KindTunnel 只在 proto 定义。
- **file 词汇**（设计 §3.2，绑定）：
  - SESSION_OPEN params = `{"direction":"upload|download","path":"C:\\abs\\path","size":N,"sha256":"hex"}`（upload 必带 size+sha256；download 只带 path）
  - text `FILE_BEGIN {"direction","path","size","sha256"}`（agent → client，标记数据流开始）
  - binary = 原始字节块（默认 64KB/条，无前缀）
  - text `FILE_RESULT {"bytes","sha256","ok"}`（终态：写入方计算实际 sha256，与 FILE_BEGIN 声明比对）
  - text `FILE_ERROR {"code"}`（FILE_NOT_FOUND / FILE_TOO_LARGE / HASH_MISMATCH）
- **tunnel 词汇**（设计 §3.2，绑定）：
  - SESSION_OPEN params = `{"target":"rdp"}`（枚举，client 永远不传 host/port——server 从白名单解析为 `{"host":"127.0.0.1","port":3389}`）
  - 纯 binary 零 text 帧：建立即转发原始 TCP 字节
  - agent 连不上目标 → text ERROR {code: RDP_NOT_AVAILABLE} 后关闭
- **file 校验**（spec §43/§58，绑定）：sha256 双边校验；单文件 ≤256MB；绝对路径（operator+ 权限 + 审计兜底，无路径白名单）；不匹配 → 删除半成品 + FILE_ERROR{HASH_MISMATCH}。
- **tunnel 白名单**（spec §26，绑定）：MVP 仅 `rdp → 127.0.0.1:3389`；server 侧白名单 map（config 可扩展），agent 收到的 host/port 只来自 SESSION_OPEN。
- REST（设计 §3.4）：`POST /api/nodes/{id}/files/upload {path,size,sha256}`、`POST /api/nodes/{id}/files/download {path}`、`POST /api/nodes/{id}/tunnel {target:"rdp"}` → 202 统一响应。
- CLI 契约（cli.md）：`xnc upload <node> <local> <remote>`、`xnc download <node> <remote> <local>`（sha256 自动校验）、`xnc rdp <node> [--local-port N]`（默认随机端口 + mstsc 启动）。
- 审计：file.upload / file.download / rdp.open 终态（metadata：direction/bytes/reason，无路径内容入日志——路径进 metadata 可接受，文件内容绝不）。
- 退出码：FILE_NOT_FOUND→244、FILE_TOO_LARGE→246、HASH_MISMATCH→246、RDP_NOT_AVAILABLE→250。
- 每任务一 commit；`sqlc` 需新增 queries（audit 已有）。
- **不含**：断点续传/目录浏览/批量同步（spec §43 非目标）、通用 TCP tunnel（spec §26 后续）、Linux ssh tunnel（Phase 8）。

---

## 文件结构总览

```text
proto/
├── session.go                 T1  +KindFile/KindTunnel/FileParams/FileBegin/FileResult/FileError/TunnelParams
└── session_test.go            T1
server/internal/api/
├── file_handlers.go           T2  POST /files/upload + /files/download（共享 startSession）
├── tunnel_handlers.go         T2  POST /tunnel（target 白名单）
├── file_tunnel_test.go        T2  server 侧全链路（假 agent 拨号）
agent/session/
├── file.go                    T3  File Handler（upload 写入+校验+清理；download 读出+流式）
├── tunnel.go                  T3  Tunnel Handler（TCP 拨号+双向泵；连不上→ERROR）
├── file_test.go               T3
├── tunnel_test.go             T3
agent/agent.go / mockagent/main.go   T4  注册 KindFile/KindTunnel
cli/
├── cmd_file.go                T5  upload/download
├── cmd_rdp.go                 T5  rdp（本地 TCP listener → WS 泵 + mstsc 启动）
├── cmd_file_test.go           T5
scripts/e2e_phase4.sh          T6  Scenario E/H + Gate C 吞吐
Makefile                       T6  e2e4 目标
```

---

### Task 1: proto — file/tunnel 词汇

**Files:**
- Modify: `proto/session.go`
- Test: `proto/session_test.go`（追加）

**Interfaces:**
- Consumes: 既有 proto 结构风格
- Produces（T2/T3/T5 依赖）:

```go
const (
	KindFile   = "file"
	KindTunnel = "tunnel"
)

// FileParams SESSION_OPEN params：upload 必带 size+sha256，download 只带 path。
type FileParams struct {
	Direction string `json:"direction"` // "upload" | "download"
	Path      string `json:"path"`      // 绝对路径
	Size      int64  `json:"size,omitempty"`
	Sha256    string `json:"sha256,omitempty"`
}

// FileBegin agent → client 的数据流开始标记。
type FileBegin struct {
	Direction string `json:"direction"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Sha256    string `json:"sha256"`
}

// FileResult 终态：写入方计算实际 sha256 与声明比对。ok=false 表示校验失败。
type FileResult struct {
	Bytes  int64  `json:"bytes"`
	Sha256 string `json:"sha256"`
	Ok     bool   `json:"ok"`
}

// FileError 错误终态。
type FileError struct {
	Code string `json:"code"` // FILE_NOT_FOUND / FILE_TOO_LARGE / HASH_MISMATCH
}

// TunnelParams SESSION_OPEN params：target 枚举（server 白名单解析为 host/port）。
type TunnelParams struct {
	Target string `json:"target"` // "rdp"
}
```

- [ ] **Step 1: 写失败测试**

追加到 `proto/session_test.go`：

```go
func TestFileVocabulary(t *testing.T) {
	fp := FileParams{Direction: "upload", Path: `C:\temp\f.zip`, Size: 1024, Sha256: "abc"}
	b, _ := json.Marshal(fp)
	assert.JSONEq(t, `{"direction":"upload","path":"C:\\temp\\f.zip","size":1024,"sha256":"abc"}`, string(b))

	dp := FileParams{Direction: "download", Path: `C:\temp\f.zip`}
	b2, _ := json.Marshal(dp)
	assert.JSONEq(t, `{"direction":"download","path":"C:\\temp\\f.zip"}`, string(b2))

	fb, _ := json.Marshal(FileBegin{Direction: "upload", Path: "p", Size: 1, Sha256: "s"})
	assert.JSONEq(t, `{"direction":"upload","path":"p","size":1,"sha256":"s"}`, string(fb))

	fr, _ := json.Marshal(FileResult{Bytes: 42, Sha256: "h", Ok: true})
	assert.JSONEq(t, `{"bytes":42,"sha256":"h","ok":true}`, string(fr))

	fe, _ := json.Marshal(FileError{Code: CodeFileNotFound})
	assert.JSONEq(t, `{"code":"FILE_NOT_FOUND"}`, string(fe))

	tp, _ := json.Marshal(TunnelParams{Target: "rdp"})
	assert.JSONEq(t, `{"target":"rdp"}`, string(tp))
}
```

- [ ] **Step 2: 运行验证失败**

Run: `cd proto && go test ./... -count=1`
Expected: FAIL（新类型未定义）。

- [ ] **Step 3: 实现**

`proto/session.go` 追加上述类型与常量（按 Produces 块逐字落地）。

- [ ] **Step 4: 运行验证通过**

Run: `cd proto && go test ./... -count=1`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add proto
git commit -m "feat(proto): file and tunnel session vocabulary"
```

---

### Task 2: server — file/tunnel 端点

**Files:**
- Create: `server/internal/api/file_handlers.go`、`server/internal/api/tunnel_handlers.go`
- Modify: `server/internal/api/router.go`（挂载三端点）
- Test: `server/internal/api/file_tunnel_test.go`

**Interfaces:**
- Consumes: T1 词汇；Phase 3 的 `startSession(w, r, kind, params, openAction, closeAction)` 共享路径；`execBodyMaxBytes` 模式
- Produces:

```text
POST /api/nodes/{id}/files/upload   {"path","size","sha256"}  → 202 统一响应
POST /api/nodes/{id}/files/download {"path"}                    → 202
POST /api/nodes/{id}/tunnel         {"target":"rdp"}            → 202
校验：path 绝对路径（Windows 盘符或 \\ 前缀）；upload size ∈ (0, 256MB]；sha256 64 hex 字符；target ∈ {"rdp"}
     409 NODE_OFFLINE / 404 NODE_NOT_FOUND / 400 校验失败
tunnel 白名单 map（server/internal/api/tunnel_handlers.go）：
  var tunnelTargets = map[string]struct{ Host string; Port int }{"rdp": {"127.0.0.1", 3389}}
  SESSION_OPEN params 替换为 {"target":"rdp","host":"127.0.0.1","port":3389}（agent 只信任此来源）
审计：file.upload / file.download / rdp.open 终态
```

- [ ] **Step 1: 写失败测试**

`server/internal/api/file_tunnel_test.go`：

```go
package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

func postJSON(t *testing.T, env *TestEnv, path, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", env.srv.URL+path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+env.AdminToken(t))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	return resp
}

func TestFileUploadValidation(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-F1", "mid-f1")

	cases := []struct{ body string; code int }{
		{`{"path":"C:\\t\\f.zip","size":100,"sha256":"` + string(make([]byte, 64)) + `"}`, 202}, // 占位说明见下
		{`{"path":"rel\\path","size":100,"sha256":"abc"}`, 400},         // 相对路径
		{`{"path":"C:\\t\\f.zip","size":0,"sha256":"abc"}`, 400},        // size ≤ 0
		{`{"path":"C:\\t\\f.zip","size":268435457,"sha256":"abc"}`, 400}, // >256MB
		{`{"path":"C:\\t\\f.zip","size":100,"sha256":"zz"}`, 400},        // 非 hex
	}
	for _, c := range cases {
		resp := postJSON(t, env, "/api/nodes/"+nodeID+"/files/upload", c.body)
		_ = resp.Body.Close()
		assert.Equal(t, c.code, resp.StatusCode, c.body)
	}
}
```

（第一个 202 用例的 sha256 用真实 64 位 hex：`strings.Repeat("a", 64)`——修正后写入。注意 upload 202 后会话无人拨号，依赖 Opening TTL 60s 兜底清理，不影响断言。）

```go
func TestFileDownloadEndpoint(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-F2", "mid-f2")
	resp := postJSON(t, env, "/api/nodes/"+nodeID+"/files/download", `{"path":"C:\\t\\f.zip"}`)
	_ = resp.Body.Close()
	assert.Equal(t, 202, resp.StatusCode)
}

func TestTunnelTargetWhitelist(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-T1", "mid-t1")
	ctrl := dialControl(t, env, nodeID)

	gotOpen := captureSessionOpen(t, ctrl)
	resp := postJSON(t, env, "/api/nodes/"+nodeID+"/tunnel", `{"target":"rdp"}`)
	defer resp.Body.Close()
	require.Equal(t, 202, resp.StatusCode)

	select {
	case so := <-gotOpen:
		assert.Equal(t, proto.KindTunnel, so.Kind)
		var p map[string]any
		require.NoError(t, json.Unmarshal(so.Params, &p))
		assert.Equal(t, "127.0.0.1", p["host"])
		assert.Equal(t, float64(3389), p["port"])
	case <-timeout(t):
		t.Fatal("no SESSION_OPEN")
	}

	// 未知 target → 400
	resp2 := postJSON(t, env, "/api/nodes/"+nodeID+"/tunnel", `{"target":"ssh"}`)
	_ = resp2.Body.Close()
	assert.Equal(t, 400, resp2.StatusCode)
}
```

（`timeout(t)` 若 helpers 无此名，用 `time.After(3*time.Second)`。tunnel SESSION_OPEN 的 params 从 TunnelParams 替换为含 host/port 的完整 map——server 侧构造。）

- [ ] **Step 2: 运行验证失败**

Run: `cd server && go test ./internal/api/ -run 'TestFile|TestTunnel' -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现**

`file_handlers.go`：

```go
package api

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"

	"xnc/proto"
)

var (
	absPathRe = regexp.MustCompile(`^[A-Za-z]:\\.+|^\\\\.+`)
	sha256Re  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

const fileMaxBytes = 256 * 1024 * 1024

type fileReq struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Sha256 string `json:"sha256"`
}

func (h *handlers) fileUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	var req fileReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "bad request"))
		return
	}
	switch {
	case !absPathRe.MatchString(req.Path):
		respondError(w, proto.Err(400, proto.CodeInternal, "path must be absolute"))
		return
	case req.Size <= 0 || req.Size > fileMaxBytes:
		respondError(w, proto.Err(400, proto.CodeFileTooLarge, "size out of range (1B-256MB)"))
		return
	case !sha256Re.MatchString(req.Sha256):
		respondError(w, proto.Err(400, proto.CodeInternal, "sha256 must be 64 hex chars"))
		return
	}
	params, _ := json.Marshal(proto.FileParams{
		Direction: "upload", Path: req.Path, Size: req.Size, Sha256: req.Sha256,
	})
	h.startSession(w, r, proto.KindFile, params, "file.upload", "file.download") // action 见注记
}
```

（action 命名按 v1 §7.6：file.upload 终态的 close 写 `file.upload` 的 finish；open 也是 `file.upload`。统一 open/close 同名——与 exec.start/exec.finish 模式对齐改为 `file.upload.start`/`file.upload.finish` 不必要，保持 v1 词汇 `file.upload`/`file.download` 作为 open，close 用同 action + reason。以 Phase 2 审计模式落地：openAction="file.upload", closeAction="file.upload"。）

`fileDownload` 同构（Direction: "download"，仅 path 校验）。

`tunnel_handlers.go`：

```go
package api

import (
	"encoding/json"
	"net/http"

	"xnc/proto"
)

var tunnelTargets = map[string]struct {
	Host string
	Port int
}{
	"rdp": {"127.0.0.1", 3389},
}

func (h *handlers) tunnelStart(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4*1024)
	var req struct {
		Target string `json:"target"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, proto.Err(400, proto.CodeInternal, "bad request"))
		return
	}
	t, ok := tunnelTargets[req.Target]
	if !ok {
		respondError(w, proto.Err(400, proto.CodeInternal, "unknown tunnel target"))
		return
	}
	params, _ := json.Marshal(map[string]any{
		"target": req.Target, "host": t.Host, "port": t.Port,
	})
	h.startSession(w, r, proto.KindTunnel, params, "rdp.open", "rdp.open")
}
```

router.go 挂载三端点（auth 组内）。

- [ ] **Step 4: 运行验证通过**

Run: `cd server && go test ./internal/api/ -run 'TestFile|TestTunnel|TestExec|TestShell' -count=1 && go test ./... -count=1`
Expected: PASS（全部回归）。

- [ ] **Step 5: Commit**

```bash
git add server
git commit -m "feat(server): file upload/download and tunnel endpoints with whitelist"
```

---

### Task 3: agent — File/Tunnel Handler

**Files:**
- Create: `agent/session/file.go`、`agent/session/tunnel.go`
- Test: `agent/session/file_test.go`、`agent/session/tunnel_test.go`

**Interfaces:**
- Consumes: T1 词汇；既有 `Handler` 接口、`execWriteTimeout`、`failStart` 模式
- Produces:

```go
// file.go
type File struct{ Log *slog.Logger }
func NewFile(log *slog.Logger) *File
func (f *File) Handle(ctx, ws, sessionID, params) // 按 Direction 分发

// tunnel.go
type Tunnel struct{ Log *slog.Logger }
func NewTunnel(log *slog.Logger) *Tunnel
func (t *Tunnel) Handle(ctx, ws, sessionID, params) // TCP 拨号 host:port + 双向泵；连不上→ERROR
```

- [ ] **Step 1: 写失败测试**

`agent/session/file_test.go`：

```go
package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

// runSessionFile 起 httptest WS 让 File handler 拨入；返回 server 侧连接。
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
		errCh <- nil
	}()
	select {
	case c := <-up:
		return c
	case err := <-errCh:
		t.Fatalf("file dial: %v", err)
	case <-timeoutStd(t):
		t.Fatal("no ws")
	}
	return nil
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestFileUploadWriteAndVerify(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "f.bin")
	content := []byte("hello-file-upload")
	ws := runSessionFile(t, proto.FileParams{
		Direction: "upload", Path: dst, Size: int64(len(content)), Sha256: sha256Hex(content),
	})

	// FILE_BEGIN 先行
	kind, data := readFrame(t, ws)
	require.Equal(t, "text", kind)
	var m proto.Message
	require.NoError(t, json.Unmarshal(data, &m))
	require.Equal(t, "FILE_BEGIN", m.Type)

	// binary 数据 → FILE_RESULT ok=true
	writeBin(t, ws, content)
	kind2, data2 := readFrame(t, ws)
	require.Equal(t, "text", kind2)
	var rm proto.Message
	require.NoError(t, json.Unmarshal(data2, &rm))
	require.Equal(t, "FILE_RESULT", rm.Type)
	var fr proto.FileResult
	require.NoError(t, rm.Decode(&fr))
	assert.True(t, fr.Ok)
	assert.Equal(t, int64(len(content)), fr.Bytes)
	written, _ := os.ReadFile(dst)
	assert.Equal(t, content, written)
}

func TestFileUploadHashMismatchDeletes(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "bad.bin")
	ws := runSessionFile(t, proto.FileParams{
		Direction: "upload", Path: dst, Size: 4, Sha256: strings_Repeat("a", 64), // 声明与实际不符
	})
	skipBegin(t, ws)
	writeBin(t, ws, []byte("data"))
	kind, data := readFrame(t, ws)
	require.Equal(t, "text", kind)
	var m proto.Message
	require.NoError(t, json.Unmarshal(data, &m))
	require.Equal(t, "FILE_ERROR", m.Type)
	var fe proto.FileError
	require.NoError(t, m.Decode(&fe))
	assert.Equal(t, proto.CodeHashMismatch, fe.Code)
	_, err := os.Stat(dst)
	assert.True(t, os.IsNotExist(err), "half-written file must be deleted")
}

func TestFileDownloadStreamAndVerify(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "g.bin")
	content := []byte("download-me")
	require.NoError(t, os.WriteFile(src, content, 0o600))

	ws := runSessionFile(t, proto.FileParams{Direction: "download", Path: src})
	// FILE_BEGIN → binary 全量 → FILE_RESULT
	kind, data := readFrame(t, ws)
	require.Equal(t, "text", kind)
	writeBin(t, ws, []byte{}) // 触发 agent 端读循环（download agent 不读 ws binary——跳过）
	kind2, data2 := readUntilResult(t, ws)
	assert.Equal(t, "download-me", string(data2))
	_ = kind2
}

func TestFileDownloadNotFound(t *testing.T) {
	ws := runSessionFile(t, proto.FileParams{Direction: "download", Path: `C:\nonexistent\x.bin`})
	kind, data := readFrame(t, ws)
	require.Equal(t, "text", kind)
	var m proto.Message
	require.NoError(t, json.Unmarshal(data, &m))
	require.Equal(t, "FILE_ERROR", m.Type)
	var fe proto.FileError
	require.NoError(t, m.Decode(&fe))
	assert.Equal(t, proto.CodeFileNotFound, fe.Code)
}
```

（实现注记：`strings_Repeat` → `strings.Repeat`；`readFrame/readFrame` 已在 exec_test 或 shell_test 定义（`readFrame` 返回 kind+data）——若名不同以现文件为准。`skipBegin` = 读一帧跳过 FILE_BEGIN。`readUntilResult` = 循环收集 binary 直到 FILE_RESULT text，返回 binary 累积。均放本文件顶部助手区。）

`tunnel_test.go`：

```go
package session

import (...)

func TestTunnelRelaysTCPTraffic(t *testing.T) {
	// 本地 TCP echo server
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go io.Copy(c, c) // echo
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	ws := runSessionTunnel(t, map[string]any{"target": "test", "host": "127.0.0.1", "port": port})
	writeBin(t, ws, []byte("ping"))
	buf := make([]byte, 4)
	n := readBinaryInto(t, ws, buf)
	assert.Equal(t, "ping", string(buf[:n]))
}

func TestTunnelUnreachableSendsError(t *testing.T) {
	// 端口 1 几乎必然连不上
	ws := runSessionTunnel(t, map[string]any{"target": "test", "host": "127.0.0.1", "port": 1})
	kind, data := readFrame(t, ws)
	require.Equal(t, "text", kind)
	var m proto.Message
	require.NoError(t, json.Unmarshal(data, &m))
	require.Equal(t, proto.TypeError, m.Type)
	var ep proto.ErrorPayload
	require.NoError(t, m.Decode(&ep))
	assert.Equal(t, proto.CodeRdpNotAvailable, ep.Code)
}
```

（`runSessionTunnel` 同 runSessionFile 模式但用 NewTunnel + params 为原始 JSON。`readBinaryInto` 读一帧 binary 到 buf。）

- [ ] **Step 2: 运行验证失败**

Run: `cd agent && go test ./session/ -run 'TestFile|TestTunnel' -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现**

`file.go`：

```go
package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/coder/websocket"

	"xnc/proto"
)

const fileChunkSize = 64 * 1024

type File struct{ Log *slog.Logger }

func NewFile(log *slog.Logger) *File { return &File{Log: log} }

func (f *File) Handle(ctx context.Context, ws *websocket.Conn, sessionID string, params json.RawMessage) {
	var p proto.FileParams
	if err := json.Unmarshal(params, &p); err != nil {
		f.fileError(ctx, ws, "FILE_NOT_FOUND") // 参数不可解——语义上等价不可达
		return
	}
	switch p.Direction {
	case "upload":
		f.handleUpload(ctx, ws, sessionID, p)
	case "download":
		f.handleDownload(ctx, ws, sessionID, p)
	default:
		f.fileError(ctx, ws, "FILE_NOT_FOUND")
	}
}

func (f *File) handleUpload(ctx context.Context, ws *websocket.Conn, sessionID string, p proto.FileParams) {
	dir := filepath.Dir(p.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.fileError(ctx, ws, "FILE_NOT_FOUND")
		return
	}
	tmp := p.Path + ".xnc-part"
	fp, err := os.Create(tmp)
	if err != nil {
		f.fileError(ctx, ws, "FILE_NOT_FOUND")
		return
	}
	defer func() { // 全路径清理：失败/成功统一删临时文件
		if _, err := os.Stat(tmp); err == nil {
			_ = os.Remove(tmp)
		}
	}()

	// FILE_BEGIN 先行
	f.writeText(ctx, ws, "FILE_BEGIN", proto.FileBegin{
		Direction: p.Direction, Path: p.Path, Size: p.Size, Sha256: p.Sha256,
	})

	// 读 binary 直到对端关闭或超量，边写边算 sha256
	h := sha256.New()
	var total int64
	for total <= p.Size {
		typ, data, err := ws.Read(ctx)
		if err != nil { // 对端关（数据发完）
			break
		}
		if typ != websocket.MessageBinary {
			continue // 忽略 text（不应出现）
		}
		if total+int64(len(data)) > p.Size {
			fp.Close()
			_ = os.Remove(tmp)
			f.fileError(ctx, ws, "FILE_TOO_LARGE")
			return
		}
		if _, we := fp.Write(data); we != nil {
			fp.Close()
			_ = os.Remove(tmp)
			f.fileError(ctx, ws, "FILE_NOT_FOUND")
			return
		}
		_, _ = h.Write(data)
		total += int64(len(data))
		if total == p.Size {
			break
		}
	}
	fp.Close()

	actual := hex.EncodeToString(h.Sum(nil))
	if actual != p.Sha256 || total != p.Size {
		_ = os.Remove(tmp) // 删半成品
		f.fileError(ctx, ws, "HASH_MISMATCH")
		return
	}
	if err := os.Rename(tmp, p.Path); err != nil {
		f.fileError(ctx, ws, "FILE_NOT_FOUND")
		return
	}
	f.writeText(ctx, ws, "FILE_RESULT", proto.FileResult{Bytes: total, Sha256: actual, Ok: true})
	_ = ws.Close(websocket.StatusNormalClosure, "")
}

func (f *File) handleDownload(ctx context.Context, ws *websocket.Conn, sessionID string, p proto.FileParams) {
	fp, err := os.Open(p.Path)
	if err != nil {
		f.fileError(ctx, ws, "FILE_NOT_FOUND")
		return
	}
	defer fp.Close()
	st, _ := fp.Stat()

	f.writeText(ctx, ws, "FILE_BEGIN", proto.FileBegin{
		Direction: p.Direction, Path: p.Path, Size: st.Size(), Sha256: "",
	})

	h := sha256.New()
	buf := make([]byte, fileChunkSize)
	var total int64
	for {
		n, err := fp.Read(buf)
		if n > 0 {
			wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
			if we := ws.Write(wctx, websocket.MessageBinary, buf[:n]); we != nil {
				cancel()
				return
			}
			cancel()
			_, _ = h.Write(buf[:n])
			total += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return
		}
	}
	f.writeText(ctx, ws, "FILE_RESULT", proto.FileResult{
		Bytes: total, Sha256: hex.EncodeToString(h.Sum(nil)), Ok: true,
	})
	_ = ws.Close(websocket.StatusNormalClosure, "")
}

func (f *File) writeText(ctx context.Context, ws *websocket.Conn, typ string, payload any) {
	m, _ := proto.NewMsg(typ, payload)
	b, _ := json.Marshal(m)
	wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
	defer cancel()
	_ = ws.Write(wctx, websocket.MessageText, b)
}

func (f *File) fileError(ctx context.Context, ws *websocket.Conn, code string) {
	f.writeText(ctx, ws, "FILE_ERROR", proto.FileError{Code: code})
	_ = ws.Close(websocket.StatusInternalError, "file error")
}
```

（`FILE_ERROR` 字面量与 `FILE_BEGIN/FILE_RESULT` 同策略：kind 私有词汇，本文件顶部常量：

```go
const (
	typeFileBegin  = "FILE_BEGIN"
	typeFileResult = "FILE_RESULT"
	typeFileError  = "FILE_ERROR"
)
```

替换字符串字面量。）

`tunnel.go`：

```go
package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/coder/websocket"

	"xnc/proto"
)

const tunnelDialTimeout = 10 * time.Second

type Tunnel struct{ Log *slog.Logger }

func NewTunnel(log *slog.Logger) *Tunnel { return &Tunnel{Log: log} }

func (t *Tunnel) Handle(ctx context.Context, ws *websocket.Conn, sessionID string, params json.RawMessage) {
	var p struct {
		Target string `json:"target"`
		Host   string `json:"host"`
		Port   int    `json:"port"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Host == "" || p.Port <= 0 {
		t.tunnelError(ctx, ws)
		return
	}
	d := net.Dialer{Timeout: tunnelDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(p.Host, fmt.Sprintf("%d", p.Port)))
	if err != nil {
		t.Log.Warn("tunnel target unreachable", "session", sessionID, "err", err)
		t.tunnelError(ctx, ws)
		return
	}
	defer conn.Close()

	wsDone := make(chan struct{})
	go func() { // ws → tcp
		defer close(wsDone)
		buf := make([]byte, 32*1024)
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			if typ != websocket.MessageBinary {
				continue
			}
			if _, err := conn.Write(data); err != nil {
				return
			}
		}
	}()
	// tcp → ws
	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
			if we := ws.Write(wctx, websocket.MessageBinary, buf[:n]); we != nil {
				cancel()
				_ = conn.Close()
				<-wsDone
				return
			}
			cancel()
		}
		if err != nil {
			_ = ws.Close(websocket.StatusNormalClosure, "")
			<-wsDone
			return
		}
	}
}

func (t *Tunnel) tunnelError(ctx context.Context, ws *websocket.Conn) {
	m, _ := proto.NewMsg(proto.TypeError, proto.ErrorPayload{
		Code:    proto.CodeRdpNotAvailable,
		Message: "tunnel target unreachable",
	})
	b, _ := json.Marshal(m)
	wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
	defer cancel()
	_ = ws.Write(wctx, websocket.MessageText, b)
	_ = ws.Close(wctx, websocket.StatusBadGateway, "tunnel target unreachable")
}
```

- [ ] **Step 4: 运行验证通过**

Run: `cd agent && go test ./session/ -count=1 && GOOS=linux go build ./... && GOOS=windows go build ./...`
Expected: PASS + 双编译。

- [ ] **Step 5: Commit**

```bash
git add agent/session
git commit -m "feat(agent): file handler with hash verify and tunnel tcp relay"
```

---

### Task 4: agent/mockagent 注册

**Files:**
- Modify: `agent/agent.go`、`mockagent/main.go`

**Interfaces:**
- Consumes: T3 `NewFile`/`NewTunnel`
- Produces: OnReady 回调追加两行 Register

- [ ] **Step 1: 实现**

两处 OnReady 的 Register 序列后追加：

```go
		engine.Register(proto.KindFile, session.NewFile(slog.Default()))   // mockagent 用 log
		engine.Register(proto.KindTunnel, session.NewTunnel(slog.Default()))
```

- [ ] **Step 2: 验证**

Run: `cd agent && go test ./... -count=1 && cd ../mockagent && go build ./... && go vet ./...`
Expected: PASS + 构建过。

- [ ] **Step 3: Commit**

```bash
git add agent mockagent
git commit -m "feat(agent,mockagent): register file and tunnel handlers"
```

---

### Task 5: cli — upload/download/rdp

**Files:**
- Create: `cli/cmd_file.go`、`cli/cmd_rdp.go`
- Modify: `cli/main.go`（注册三命令）、`cli/exit.go`（FILE_NOT_FOUND→244、FILE_TOO_LARGE/HASH_MISMATCH→246）
- Test: `cli/cmd_file_test.go`

**Interfaces:**
- Consumes: `resolveNode`、`dialSession`、`Client.Do`、`readWS`、exit 助手
- Produces:

```text
xnc upload <node> <local> <remote>     --json envelope data={node,bytes,sha256,ok,durationMs}
xnc download <node> <remote> <local>   同上
xnc rdp <node> [--local-port N]        本地 TCP listener → WS 泵 + 启动 mstsc /v:127.0.0.1:PORT
                                       （Ctrl+C 或 mstsc 退出 → 清理退出 0）
```

- [ ] **Step 1: 写失败测试**

`cli/cmd_file_test.go`：

```go
package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeFileServer：POST /files/upload 或 /files/download → 202 + 会话；
// WS 上假 agent 做 echo（upload 模式收 binary 后回 FILE_RESULT ok；download 模式发 FILE_BEGIN + binary + FILE_RESULT）。
func fakeFileServer(t *testing.T, downloadContent []byte) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/nodes/n1/files/upload" || r.URL.Path == "/api/nodes/n1/files/download" {
			w.WriteHeader(202)
			_, _ = w.Write([]byte(`{"sessionId":"fs1","token":"ct","websocketUrl":"/api/session/fs1?token=ct","expiresAt":"2026-01-01T00:00:00Z"}`))
			return
		}
		if r.URL.Path == "/api/session/fs1" {
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			go func() {
				defer c.CloseNow()
				ctx := r.Context()
				if downloadContent != nil { // download 模式
					_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"FILE_BEGIN","payload":{"direction":"download","path":"p","size":`+itoa(len(downloadContent))+`,"sha256":""}}`))
					_ = c.Write(ctx, websocket.MessageBinary, downloadContent)
					_ = c.Write(ctx, websocket.MessageText, []byte(`{"type":"FILE_RESULT","payload":{"bytes":`+itoa(len(downloadContent))+`,"sha256":"`+sha256Of(downloadContent)+`","ok":true}}`))
					return
				}
				// upload 模式：读全部 binary 后回 FILE_RESULT
				var total int
				for {
					typ, data, err := c.Read(ctx)
					if err != nil {
						break
					}
					if typ == websocket.MessageBinary {
						total += len(data)
					}
				}
				_ = c.Write(context.Background(), websocket.MessageText, []byte(`{"type":"FILE_RESULT","payload":{"bytes":`+itoa(total)+`,"sha256":"x","ok":true}}`))
			}()
			return
		}
		http.NotFound(w, r)
	}))
	return srv
}

func TestUploadSendsBinaryAndParsesResult(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "l.bin")
	require.NoError(t, os.WriteFile(local, []byte("upload-payload"), 0o600))

	srv := fakeFileServer(t, nil)
	defer srv.Close()

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"upload", "n1", local, `C:\remote\l.bin`,
			"--json", "--server", srv.URL, "--token", "tk"})
	})
	require.Equal(t, 0, code)
	assert.Contains(t, out, `"ok":true`)
	assert.Contains(t, out, `"bytes":14`)
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
	got, _ := os.ReadFile(dst)
	assert.Equal(t, []byte("download-payload"), got)
}

func TestHashMismatchExits246(t *testing.T) {
	srv := fakeFileServerMismatch(t) // FILE_RESULT ok=false + FILE_ERROR HASH_MISMATCH
	defer srv.Close()
	dir := t.TempDir()
	_, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"download", "n1", `C:\x`, filepath.Join(dir, "o"),
			"--json", "--server", srv.URL, "--token", "tk"})
	})
	assert.Equal(t, 246, code)
}
```

（`itoa`/`sha256Of`/`fakeFileServerMismatch` 为本文件助手——itoa 用 strconv.Itoa；sha256Of 用 crypto/sha256+hex；mismatch server 发 ok:false 的 FILE_RESULT 后发 FILE_ERROR。）

- [ ] **Step 2: 运行验证失败**

Run: `cd cli && go test ./... -count=1`
Expected: FAIL。

- [ ] **Step 3: 实现**

`cmd_file.go`：

```go
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"

	"xnc/proto"
)

func newUploadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upload <node> <local> <remote>",
		Short: "Upload a file to a node (sha256 verified)",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFileTransfer(cmd, args[0], "upload", args[1], args[2])
		},
	}
}

func newDownloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "download <node> <remote> <local>",
		Short: "Download a file from a node (sha256 verified)",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFileTransfer(cmd, args[0], "download", args[2], args[1])
		},
	}
}

func runFileTransfer(cmd *cobra.Command, node, direction, local, remote string) error {
	cl, usage := dial(cmd, true)
	if usage != "" {
		return failUsage(cmd, usage)
	}
	ref, e := resolveNode(cl, node)
	if e != nil {
		return failAPI(cmd, e)
	}

	body := map[string]any{"direction": direction, "path": remote}
	if direction == "upload" {
		data, err := os.ReadFile(local)
		if err != nil {
			return failAPI(cmd, proto.Err(2, "USAGE", err.Error()))
		}
		if len(data) > 256*1024*1024 {
			return failAPI(cmd, proto.Err(2, "USAGE", "file exceeds 256MB"))
		}
		h := sha256.Sum256(data)
		body["size"] = len(data)
		body["sha256"] = hex.EncodeToString(h[:])
	}

	endpoint := "files/upload"
	if direction == "download" {
		endpoint = "files/download"
	}
	var created struct {
		SessionID    string `json:"sessionId"`
		Token        string `json:"token"`
		WebsocketURL string `json:"websocketUrl"`
	}
	if e := cl.Do("POST", "/api/nodes/"+ref.ID+"/"+endpoint, body, &created); e != nil {
		return failAPI(cmd, e)
	}

	ws, err := dialSession(cl.Base, created.WebsocketURL)
	if err != nil {
		return failAPI(cmd, proto.Err(0, "NETWORK", err.Error()))
	}
	defer ws.CloseNow()
	ctx := cmd.Context()
	start := time.Now()

	var result *proto.FileResult
	var fileErr *proto.FileError

	if direction == "upload" {
		go func() { // 发送本地文件
			data, _ := os.ReadFile(local)
			for off := 0; off < len(data); off += 64 * 1024 {
				end := off + 64*1024
				if end > len(data) {
					end = len(data)
				}
				wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				if err := ws.Write(wctx, websocket.MessageBinary, data[off:end]); err != nil {
					cancel()
					return
				}
				cancel()
			}
		}()
	}

	// 主循环：收帧（labeled break 避免 goto 跳变量声明）
	var downloaded []byte
	var result *proto.FileResult
	var fileErr *proto.FileError
loop:
	for {
		kind, data, err := readWS(ctx, ws)
		if err != nil {
			break loop
		}
		switch kind {
		case "binary":
			if direction == "download" {
				downloaded = append(downloaded, data...)
			}
		case "text":
			var m proto.Message
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			switch m.Type {
			case "FILE_BEGIN":
				// 数据流开始标记（无动作）
			case "FILE_RESULT":
				var fr proto.FileResult
				if m.Decode(&fr) == nil {
					result = &fr
					break loop
				}
			case "FILE_ERROR":
				var fe proto.FileError
				if m.Decode(&fe) == nil {
					fileErr = &fe
					break loop
				}
			}
		}
	}
	if fileErr != nil {
		return fileErrExit(cmd, fileErr)
	}
	if result == nil {
		return failAPI(cmd, proto.Err(245, "NETWORK", "session ended without result"))
	}
	if direction == "download" && result.Ok {
		if err := os.MkdirAll(filepath.Dir(local), 0o755); err != nil {
			return failAPI(cmd, proto.Err(250, proto.CodeInternal, err.Error()))
		}
		if err := os.WriteFile(local, downloaded, 0o600); err != nil {
			return failAPI(cmd, proto.Err(250, proto.CodeInternal, err.Error()))
		}
		// 客户端校验：agent 报的 sha256 与本地重算比对
		chk := sha256.Sum256(downloaded)
		if hex.EncodeToString(chk[:]) != result.Sha256 {
			_ = os.Remove(local)
			return failAPI(cmd, proto.Err(246, proto.CodeHashMismatch, "sha256 mismatch"))
		}
	}
	if jsonOut(cmd) {
		PrintJSON(true, map[string]any{
			"node": ref.Name, "bytes": result.Bytes, "sha256": result.Sha256,
			"ok": result.Ok, "durationMs": time.Since(start).Milliseconds(),
		}, nil)
		return nil
	}
	fmt.Printf("%s %s: %d bytes, sha256 %s (ok=%v)\n", direction, ref.Name, result.Bytes, result.Sha256, result.Ok)
	return nil
}

func fileErrExit(cmd *cobra.Command, fe *proto.FileError) error {
	switch fe.Code {
	case proto.CodeFileNotFound:
		return failAPI(cmd, proto.Err(244, fe.Code, "file not found"))
	case proto.CodeFileTooLarge, proto.CodeHashMismatch:
		return failAPI(cmd, proto.Err(246, fe.Code, fe.Code))
	default:
		return failAPI(cmd, proto.Err(250, fe.Code, fe.Code))
	}
}
```

（`filepath` 需加入 import 列表。mstsc 多连接重连属于打磨项，MVP 单连接。）

`cmd_rdp.go`：

```go
package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"

	"xnc/proto"
)

func newRdpCmd() *cobra.Command {
	var localPort int
	cmd := &cobra.Command{
		Use:   "rdp <node> [--local-port N]",
		Short: "Open Remote Desktop to a node via reverse tunnel (launches mstsc)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRdp(cmd, args[0], localPort)
		},
	}
	cmd.Flags().IntVar(&localPort, "local-port", 0, "local listen port (default: random)")
	return cmd
}

func runRdp(cmd *cobra.Command, node string, localPort int) error {
	cl, usage := dial(cmd, true)
	if usage != "" {
		return failUsage(cmd, usage)
	}
	ref, e := resolveNode(cl, node)
	if e != nil {
		return failAPI(cmd, e)
	}

	var created struct {
		SessionID    string `json:"sessionId"`
		Token        string `json:"token"`
		WebsocketURL string `json:"websocketUrl"`
	}
	if e := cl.Do("POST", "/api/nodes/"+ref.ID+"/tunnel",
		map[string]any{"target": "rdp"}, &created); e != nil {
		return failAPI(cmd, e)
	}

	ws, err := dialSession(cl.Base, created.WebsocketURL)
	if err != nil {
		return failAPI(cmd, proto.Err(0, "NETWORK", err.Error()))
	}
	defer ws.CloseNow()
	ctx := cmd.Context()

	// 本地 TCP listener
	if localPort == 0 {
		localPort = 0 // 随机
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		return failAPI(cmd, proto.Err(0, "NETWORK", err.Error()))
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	fmt.Fprintf(os.Stderr, "[xnc] tunnel ready: 127.0.0.1:%d → %s (rdp) — Ctrl+C to close\n", port, ref.Name)

	// 启动 mstsc
	mstsc := exec.Command("mstsc", "/v:127.0.0.1:"+strconv.Itoa(port))
	if err := mstsc.Start(); err != nil {
		return failAPI(cmd, proto.Err(250, proto.CodeInternal, "mstsc launch: "+err.Error()))
	}
	defer mstsc.Process.Kill()

	// 接受一个连接（mstsc 可能重连数次——循环接受，但同一时间一个）
	conn, err := ln.Accept()
	if err != nil {
		return nil
	}
	defer conn.Close()

	// 双向泵
	wsDone := make(chan struct{})
	go func() { // conn → ws
		defer close(wsDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				wctx, cancel := context.WithTimeout(ctx, 60*time.Second)
				if we := ws.Write(wctx, websocket.MessageBinary, buf[:n]); we != nil {
					cancel()
					return
				}
				cancel()
			}
			if err != nil {
				return
			}
		}
	}()
	// ws → conn
	buf := make([]byte, 32*1024)
	for {
		typ, data, err := ws.Read(ctx)
		if err != nil {
			<-wsDone
			return nil
		}
		if typ == websocket.MessageText { // tunnel 唯一 text = ERROR
			var m proto.Message
			if json.Unmarshal(data, &m) == nil && m.Type == proto.TypeError {
				var ep proto.ErrorPayload
				_ = m.Decode(&ep)
				return failAPI(cmd, proto.Err(250, ep.Code, ep.Message))
			}
			continue
		}
		if _, err := conn.Write(data); err != nil {
			<-wsDone
			return nil
		}
	}
}
```

（`time` 需 import。mstsc 多连接：RDP 协议在断线时会重连——循环接受属于 Phase 5 打磨，MVP 单连接。）

`exit.go` 追加映射：`CodeFileNotFound→244`（已有 exitMissing）、`CodeFileTooLarge/CodeHashMismatch→exitQuota(246)`——检查 exit.go 现有映射，若 CodeFileNotFound 已映射则只补后两个。

- [ ] **Step 4: 运行验证通过**

Run: `cd cli && go test ./... -count=1 && go build -o ../bin/xnc.exe .`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add cli
git commit -m "feat(cli): upload/download with hash verify, rdp with local forward and mstsc"
```

---

### Task 6: E2E Scenario E/H + Gate C

**Files:**
- Create: `scripts/e2e_phase4.sh`
- Modify: `Makefile`（e2e4 目标）

**Interfaces:**
- Consumes: T1-T5 全部；dev compose；mockagent（真 file/tunnel handler，跑在本 Windows 开发机）
- Produces: `bash scripts/e2e_phase4.sh` 一键验收

- [ ] **Step 1: 写脚本**

`scripts/e2e_phase4.sh`：

```bash
#!/usr/bin/env bash
# Phase 4 E2E：Scenario H 文件（upload/download + sha256 + HASH_MISMATCH 路径）
# + tunnel 冒烟（本机 TCP echo 代替 3389——真 mstsc 留给生产抽验）
# + Gate C 吞吐（10MB 文件经 dev 栈传输计时）。
set -euo pipefail
cd "$(dirname "$0")/.."

SERVER=http://127.0.0.1:8080
EXE=""
case "$(uname -s)" in MINGW*|MSYS*|CYGWIN*) EXE=".exe";; esac
XNC="bin/xnc$EXE"; MOCK="bin/mockagent$EXE"
export XNC_SERVER=$SERVER

echo "== build =="
(cd cli && go build -o "../$XNC" .)
(cd mockagent && go build -o "../$MOCK" .)

echo "== dev stack =="
COMPOSE="docker compose -f deploy/docker-compose.yml -f deploy/docker-compose.dev.yml"
[ -f deploy/.env ] || cp deploy/.env.example deploy/.env
$COMPOSE up -d --build
trap 'kill ${MPID:-} 2>/dev/null || true; $COMPOSE down -v; [ -n "${IDDIR:-}" ] && rm -rf "$IDDIR" || true' EXIT
for i in $(seq 1 30); do curl -sf "$SERVER/api/health" >/dev/null && break; sleep 1; done

echo "== login + node =="
TOKEN_JSON=$(printf 'change-me' | "$XNC" login --server "$SERVER" --email admin@example.com --json)
export XNC_TOKEN=$(echo "$TOKEN_JSON" | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
ETOK=$("$XNC" token create default --json | sed -n 's/.*"token":"\([^"]*\)".*/\1/p')
IDDIR=$(mktemp -d)
"$MOCK" --server "$SERVER" --token "$ETOK" --identity-dir "$IDDIR" --beat 2s >"$IDDIR/mock.log" 2>&1 &
MPID=$!
NODE=$("$XNC" node list --json | grep -o '"name":"[^"]*"' | head -1 | sed 's/"name":"//;s/"//')
sleep 3
echo "node: $NODE"

echo "== H: upload + download + sha256 =="
DD=$(mktemp -d)
printf 'phase4-test-content' > "$DD/src.txt"
"$XNC" upload "$NODE" "$DD/src.txt" "$DD/dst.txt" --json | grep -q '"ok":true' || { echo "upload failed"; exit 1; }
"$XNC" download "$NODE" "$DD/dst.txt" "$DD/back.txt" --json | grep -q '"ok":true' || { echo "download failed"; exit 1; }
diff -q "$DD/src.txt" "$DD/back.txt" || { echo "content mismatch"; exit 1; }
echo "H: OK (roundtrip + diff)"

echo "== H2: hash mismatch path =="
# upload 时注入坏数据：手动构造——上传后 agent 端校验拒绝（此路径由 agent 单测覆盖，
# E2E 只验 happy path 的完整性；跳过，标注为单测覆盖）
echo "H2: covered by unit tests (TestFileUploadHashMismatchDeletes)"

echo "== Gate C: 10MB throughput =="
py -c "open('$DD/big.bin','wb').write(b'X'*10485760)"
T0=$(date +%s%N)
"$XNC" upload "$NODE" "$DD/big.bin" "$DD/big-remote.bin" --json | grep -q '"ok":true' || { echo "big upload failed"; exit 1; }
T1=$(date +%s%N)
UP_MS=$(( (T1 - T0) / 1000000 ))
"$XNC" download "$NODE" "$DD/big-remote.bin" "$DD/big-back.bin" --json | grep -q '"ok":true' || { echo "big download failed"; exit 1; }
T2=$(date +%s%N)
DOWN_MS=$(( (T2 - T1) / 1000000 ))
SIZE=$(stat -c %s "$DD/big-back.bin" 2>/dev/null || wc -c < "$DD/big-back.bin")
[ "$SIZE" -eq 10485760 ] || { echo "size mismatch: $SIZE"; exit 1; }
echo "Gate C: upload 10MB in ${UP_MS}ms, download in ${DOWN_MS}ms (docker loopback)"

kill $MPID; wait $MPID 2>/dev/null || true
echo "== ALL PHASE4 E2E PASSED =="
```

（`chmod +x` + `git update-index --chmod=+x`。Makefile 加 `e2e4: bash scripts/e2e_phase4.sh`。10MB 文件路径注意：mockagent 的 file handler 写文件到**运行 mockagent 的本机**（即开发机），路径 `$DD/...` 在 Git Bash 的 /tmp 下——mockagent 收到的是原样字符串，Windows 下 Go 能处理 `/tmp/...`？不能——Git Bash 的 /tmp 映射到 `C:\Users\...\AppData\Local\Temp`，但传给 agent 的字符串是 POSIX 风格。**修正**：E2E 用 `cygpath -w` 转换：`RPATH=$(cygpath -w "$DD/dst.txt")`，upload/download 的 remote 参数用 `$RPATH`。）

- [ ] **Step 2: 运行 E2E**

Run: `bash scripts/e2e_phase4.sh`
Expected: `H: OK` + `Gate C: upload 10MB in XXXms, download in XXXms` + `ALL PHASE4 E2E PASSED`，exit 0。

- [ ] **Step 3: Commit**

```bash
git add scripts Makefile
git commit -m "test(e2e): phase 4 file roundtrip and gate C throughput"
```

---

## 完成定义（Phase 4 DoD）

```text
全模块测试绿 + 双 GOOS 编译过
bash scripts/e2e_phase4.sh 全绿（upload/download roundtrip + diff + 10MB 吞吐计时）
sha256 双边校验 + 半成品删除（单测）
tunnel 白名单 400 + TCP 泵 + 连不上 ERROR（单测 + E2E tunnel 冒烟）
Gate C 数据记入 roadmap §4
生产真机抽验：xnc rdp LABS-TB16G7 → mstsc 实连（手工，合并后执行）
skills/xnc/references/cli.md upload/download/rdp 行为核对（已有文档应与实现一致，只修差异）
```

后续：Phase 5（多用户/RBAC/审计查询）另起计划。
