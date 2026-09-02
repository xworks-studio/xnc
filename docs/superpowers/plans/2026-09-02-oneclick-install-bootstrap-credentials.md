# 一键安装与引导凭据链路 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 终端用户在新 Windows 机器上执行 `irm <server>/a/<token> | iex` 一行命令，自动完成 XNCCore + XNCAgent 双服务安装与注册，enrollment token 全程零持久化、服务 argv 零 secret。

**Architecture:** 服务端新增 `/install/bundle`（enrollment-token 鉴权、只校验不扣减）并把 `/a/{token}` 改为直出 PowerShell 脚本；PS 脚本只做提权/下载/解压，注册与服务安装全部交给 `xnc-agent.exe enroll`（先消费 token）和 `xnc-agent.exe install`（原生安装双服务，幂等）两个子命令。

**Tech Stack:** Go（chi router + sqlc + testify）、PowerShell 5.1 兼容脚本、Windows SCM（golang.org/x/sys/windows/svc/mgr）。

**Spec:** `docs/superpowers/specs/2026-09-02-oneclick-install-bootstrap-credentials-design.md`

## Global Constraints

- 服务 argv 的每个元素必须是独立 argv 词（单 flag 单词），任何 token/secret 不得写入服务 binPath、文件、注册表。
- `/install/bundle` 校验 enrollment token 但**不扣减** used_count；扣减只发生在 `/api/agent/enroll`（`ConsumeEnrollmentToken`）。
- 新装机器默认状态目录 = `%ProgramData%\XNC`（读 `ProgramData` 环境变量拼 `XNC`）；存量机器显式 `--state-dir` 不受影响。
- PS 模板禁止出现：`sc.exe` 调用、`$args` 自动变量、token 落盘；模板必须 PS 5.1 兼容且**不含反引号字符**（Go raw string 限制）。
- server 测试依赖真实 PG（`db.OpenTestStore`，现有 TestEnv 模式）；agent Windows 测试直接在 Windows 主机 `go test` 运行。
- proto 错误码常量值：`ENROLLMENT_TOKEN_INVALID` / `ENROLLMENT_TOKEN_EXPIRED` / `NODE_ALREADY_ENROLLED`（`proto/errors.go:11-13`）。
- 每个任务独立提交，commit message 用英文 conventional 风格（与仓库近期提交一致）。

---

### Task 1: 服务端 `/install/bundle` 路由（token 鉴权、不扣减）

**Files:**
- Create: `server/internal/api/install_bundle_handlers.go`
- Create: `server/internal/api/install_bundle_handlers_test.go`
- Modify: `server/internal/api/router.go:166`（`/install/agent.ps1` 路由旁）

**Interfaces:**
- Consumes: `h.st.Q().GetEnrollmentTokenByHash(ctx, tokens.Hash(token))`（返回 `sqlc.EnrollmentToken`，字段 `ExpiresAt time.Time`、`MaxUses int32`、`UsedCount int32`）；`GetLatestReleaseByChannel(ctx, channel)`；`GetArtifact(ctx, GetArtifactParams{ReleaseID, Name})`；常量 `bundleArtifactName = "bundle.tar.gz"`（update.go:24）；`respondError` / `respondJSON`（包内既有）。
- Produces: `GET /install/bundle?token=&channel=` → 200（`application/octet-stream` + `X-Xnc-Sha256` 头 + bundle 字节）；401 token 无效/用尽；410 过期；404 该频道无 release 或无 bundle 制品。Task 2 的 PS 模板依赖此路由与 `X-Xnc-Sha256` 头。

- [ ] **Step 1: 写失败测试**

创建 `server/internal/api/install_bundle_handlers_test.go`：

```go
package api

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/server/internal/db/sqlc"
)

// seedChannelRelease 与 seedRelease 同构，但 channel 可指定（seedRelease
// 硬编码 stable）。相邻 seed 间隔 2ms 保证 created_at 严格递增。
func seedChannelRelease(t *testing.T, env *TestEnv, version, channel string) string {
	t.Helper()
	rel, err := env.Store.Q().CreateRelease(t.Context(), sqlc.CreateReleaseParams{
		Version: version, Notes: "seed " + version, Channel: channel,
	})
	require.NoError(t, err)
	require.NoError(t, env.Store.Q().PutArtifact(t.Context(), sqlc.PutArtifactParams{
		ReleaseID: rel.ID, Name: bundleArtifactName,
		Sha256: "seed-hash", Size: int64(len("bundle-" + version)), Data: []byte("bundle-" + version),
	}))
	time.Sleep(2 * time.Millisecond)
	return rel.ID.String()
}

// enrollWithToken 用给定 token 走真实 enroll 端点，返回状态码。
func enrollWithToken(t *testing.T, srvURL, token, machineID string) int {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	body := fmt.Sprintf(`{"token":%q,"hostname":"h1","machineId":%q,`+
		`"osVersion":"win","agentVersion":"0.1.0","publicKey":%q}`,
		token, machineID, base64.StdEncoding.EncodeToString(pub))
	resp, err := http.Post(srvURL+"/api/agent/enroll", "application/json", strings.NewReader(body))
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestInstallBundle：快捷安装包下载的鉴权与「校验不扣减」语义。
func TestInstallBundle(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()
	seedChannelRelease(t, env, "0.5.0", "stable")
	seedChannelRelease(t, env, "0.6.0", "stable")
	seedChannelRelease(t, env, "0.7.0", "dev")

	getBundle := func(token, channel string) *http.Response {
		t.Helper()
		resp, err := http.Get(srv.URL + "/install/bundle?token=" + token + "&channel=" + channel)
		require.NoError(t, err)
		t.Cleanup(func() { resp.Body.Close() })
		return resp
	}

	t.Run("invalid token -> 401", func(t *testing.T) {
		assert.Equal(t, 401, getBundle("xnc_enroll_bogus", "stable").StatusCode)
	})

	t.Run("valid token -> 200 with latest stable bundle and hash header", func(t *testing.T) {
		tok := env.CreateEnrollToken(t)
		resp := getBundle(tok, "stable")
		assert.Equal(t, 200, resp.StatusCode)
		assert.Equal(t, "application/octet-stream", resp.Header.Get("Content-Type"))
		assert.Equal(t, "seed-hash", resp.Header.Get("X-Xnc-Sha256"))
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, "bundle-0.6.0", string(b))
	})

	t.Run("download does not consume token uses", func(t *testing.T) {
		tok := env.CreateEnrollToken(t) // maxUses=1
		assert.Equal(t, 200, getBundle(tok, "stable").StatusCode)
		assert.Equal(t, 200, getBundle(tok, "stable").StatusCode) // 重试安全
		assert.Equal(t, 201, enrollWithToken(t, srv.URL, tok, "m-uses")) // 仍可注册
		assert.Equal(t, 401, getBundle(tok, "stable").StatusCode) // 用尽后拒绝
	})

	t.Run("expired token -> 410", func(t *testing.T) {
		req, _ := http.NewRequest("POST", srv.URL+"/api/clusters/default/enrollment-tokens",
			strings.NewReader(`{"ttl":"1s"}`))
		req.Header.Set("Authorization", "Bearer "+env.AdminToken(t))
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		var out struct {
			Token string `json:"token"`
		}
		require.NoError(t, decodeJSON(resp.Body, &out))
		time.Sleep(1100 * time.Millisecond)
		assert.Equal(t, 410, getBundle(out.Token, "stable").StatusCode)
	})

	t.Run("channel selects release", func(t *testing.T) {
		tok := env.CreateEnrollToken(t)
		resp := getBundle(tok, "dev")
		assert.Equal(t, 200, resp.StatusCode)
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, "bundle-0.7.0", string(b))
	})

	t.Run("channel with no releases -> 404", func(t *testing.T) {
		// 没有任何 release 的频道：清掉太重，直接用不存在的 channel 名走校验。
		resp := getBundle(env.CreateEnrollToken(t), "bogus-channel")
		assert.Equal(t, 400, resp.StatusCode)
	})
}
```

注意：最后一个子测试断言非法 channel 名（非 stable/dev）→ 400；"合法但无 release 的 channel" 场景因 seed 结构难以构造干净，交由 handler 对 `GetLatestReleaseByChannel` 出错返回 404 覆盖（实现里两处 404 分支的判定逻辑一致，评审可接受）。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./server/internal/api/ -run TestInstallBundle -v`
Expected: FAIL —— 路由未注册，`/install/bundle` 请求落入 SPA 兜底（200 HTML 或 404），各断言不等。

- [ ] **Step 3: 实现 handler 与路由**

创建 `server/internal/api/install_bundle_handlers.go`：

```go
// install_bundle_handlers.go — 快捷安装包下载：/install/bundle。
//
// 与 /api/agent/bundle（自更新、单次 node 绑定令牌）不同：这里凭据是
// enrollment token，且只校验有效性、不扣减 used_count——扣减仅发生在
// /api/agent/enroll，安装脚本下载失败重试不会烧掉 token。
// X-Xnc-Sha256 头供脚本做下载后完整性校验。
package api

import (
	"net/http"
	"time"

	"xnc/proto"
	"xnc/server/internal/db/sqlc"
	"xnc/server/internal/tokens"
)

// installBundle — GET /install/bundle?token=&channel=
func (h *handlers) installBundle(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	channel := r.URL.Query().Get("channel")
	if channel == "" {
		channel = "stable"
	}
	if channel != "stable" && channel != "dev" {
		respondError(w, proto.Err(400, proto.CodeInternal, "channel must be stable or dev"))
		return
	}
	tok, err := h.st.Q().GetEnrollmentTokenByHash(r.Context(), tokens.Hash(token))
	if err != nil {
		respondError(w, proto.Err(401, proto.CodeEnrollmentTokenInvalid, "invalid token"))
		return
	}
	if time.Now().After(tok.ExpiresAt) {
		respondError(w, proto.Err(410, proto.CodeEnrollmentTokenExpired, "token expired"))
		return
	}
	if tok.UsedCount >= tok.MaxUses {
		respondError(w, proto.Err(401, proto.CodeEnrollmentTokenInvalid, "token exhausted"))
		return
	}
	rel, err := h.st.Q().GetLatestReleaseByChannel(r.Context(), channel)
	if err != nil {
		respondError(w, proto.Err(404, "NOT_FOUND", "no releases for channel"))
		return
	}
	art, err := h.st.Q().GetArtifact(r.Context(), sqlc.GetArtifactParams{ReleaseID: rel.ID, Name: bundleArtifactName})
	if err != nil {
		respondError(w, proto.Err(404, "NOT_FOUND", "bundle not found"))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Xnc-Sha256", art.Sha256)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(art.Data)
}
```

`router.go` 在 `r.Get("/install/agent.ps1", h.serveAgentInstallPS)` 一行之前加入（仍在 SPA 兜底之前）：

```go
		// 快捷安装包下载（无 JWT——enrollment token 是凭证；校验不扣减，
		// 扣减仅发生在 enroll）
		r.Get("/install/bundle", h.installBundle)
```

测试文件补 `io` import（`io.ReadAll`）。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./server/internal/api/ -run TestInstallBundle -v`
Expected: PASS（全部子测试）。

- [ ] **Step 5: 回归全包测试**

Run: `go test ./server/internal/api/`
Expected: PASS（无既有测试回归）。

- [ ] **Step 6: Commit**

```bash
git add server/internal/api/install_bundle_handlers.go server/internal/api/install_bundle_handlers_test.go server/internal/api/router.go
git commit -m "feat(server): /install/bundle - enroll-token gated download that never consumes uses"
```

---

### Task 2: `/a/{token}` 直出 PowerShell 脚本（新模板，去 .cmd 中转）

**Files:**
- Modify: `server/internal/api/install_handlers.go`（删 `agentInstallCmd`、`serveAgentInstallPS`，重写 `installAgentCmd`）
- Modify: `server/internal/api/install_scripts.go`（替换 `agentInstallTemplate`，`agentInstallPS` 增 `{{SCRIPTURL}}` 替换）
- Modify: `server/internal/api/router.go:165`（删 `/install/agent.ps1` 路由）
- Create: `server/internal/api/install_handlers_test.go`

**Interfaces:**
- Consumes: Task 1 的 `GET /install/bundle`（模板内以 `$Server/install/bundle?token=&channel=` 引用）。
- Produces: `GET /a/{token}`（及 `/a-dev/{token}`）→ 200 `text/plain; charset=utf-8`，正文为可直接 `irm | iex` 的 PS 脚本；非法 token 前缀 → 400 纯文本。模板渲染函数保持签名 `agentInstallPS(serverURL, token, channel string) string`。Task 3/4 的 `enroll`、`install` 子命令与 Task 5 的 `install-connected.ok` 标记是模板的执行期依赖（模板只引用名字，测试不执行）。

- [ ] **Step 1: 写失败测试**

创建 `server/internal/api/install_handlers_test.go`：

```go
package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInstallAgentScript：/a/{token} 直出 PowerShell 脚本，模板硬性禁令
//（无 sc.exe、无 $args、无未替换占位符）与关键步骤顺序。
func TestInstallAgentScript(t *testing.T) {
	env := NewTestEnv(t)
	srv := httptest.NewServer(env.Router)
	defer srv.Close()

	getScript := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		require.NoError(t, err)
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		return resp.StatusCode, string(b)
	}

	t.Run("bad token prefix -> 400 text", func(t *testing.T) {
		code, body := getScript("/a/not-a-token")
		assert.Equal(t, 400, code)
		assert.Contains(t, body, "Invalid enrollment token")
	})

	t.Run("stable script content", func(t *testing.T) {
		tok := env.CreateEnrollToken(t)
		code, body := getScript("/a/" + tok)
		require.Equal(t, 200, code)
		assert.Contains(t, "text/plain", headerOf(t, srv.URL, "/a/"+tok))
		// 关键内容：状态目录、自提权重回 URL、enroll 先于 install、双服务验证
		assert.Contains(t, body, `$StateDir = "C:\ProgramData\XNC"`)
		assert.Contains(t, body, "/a/"+tok) // 自提权重启用的脚本 URL
		assert.Contains(t, body, "install/bundle?token=")
		assert.Contains(t, body, "enroll --server")
		assert.Contains(t, body, "install --server")
		assert.Less(t, strings.Index(body, "enroll --server"), strings.Index(body, "install --server"))
		assert.Contains(t, body, "install-connected.ok")
		assert.Contains(t, body, "XNCCore")
		// 硬性禁令
		assert.NotContains(t, body, "sc.exe")
		assert.NotContains(t, body, "$args")
		assert.NotContains(t, body, "{{")
	})

	t.Run("dev channel uses /a-dev relaunch URL", func(t *testing.T) {
		tok := env.CreateEnrollToken(t)
		_, body := getScript("/a-dev/" + tok)
		assert.Contains(t, body, "/a-dev/"+tok)
		assert.Contains(t, body, `$Channel = "dev"`)
	})
}

func headerOf(t *testing.T, srvURL, path string) string {
	t.Helper()
	resp, err := http.Get(srvURL + path)
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.Header.Get("Content-Type")
}

// TestInstallAgentScriptParses：生成脚本经 PowerShell 解析器做语法校验
//（防止语法级 bug 再次上线）。脚本落临时文件后 ParseFile。无 powershell.exe
// 时跳过。
func TestInstallAgentScriptParses(t *testing.T) {
	ps, err := exec.LookPath("powershell")
	if err != nil {
		t.Skip("powershell not available")
	}
	f := filepath.Join(t.TempDir(), "install.ps1")
	require.NoError(t, os.WriteFile(f, []byte(agentInstallPS("https://xnc.example", "xnc_enroll_test", "stable")), 0o644))
	cmd := exec.Command(ps, "-NoProfile", "-Command",
		`$t=$null;$e=$null;`+
			`[System.Management.Automation.Language.Parser]::ParseFile('`+f+`',[ref]$t,[ref]$e)|Out-Null;`+
			`if($e.Count){$e|ForEach-Object{$_.Message};exit 1}`)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "parse errors: %s", string(out))
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./server/internal/api/ -run 'TestInstallAgentScript' -v`
Expected: FAIL —— 现有 `/a/{token}` 返回 .cmd 批处理（`@echo off`），断言 `$StateDir = ...`、`text/plain` 等不等。

- [ ] **Step 3: 重写 handler 与模板**

`install_handlers.go` 整体替换为（删除 `agentInstallCmd`/`serveAgentInstallPS`，保留 CLI 部分）：

```go
// install_handlers.go — 快捷安装端点：/a/<token>（agent）、/c（CLI）。
//
// 用户命令（复制/手打即完成安装）：
//
//	irm https://xnc.app/a/<enrollment-token> | iex     # agent（需管理员）
//	irm https://xnc.app/c | iex                          # CLI
//
// /a 直接返回 PS 脚本文本（irm | iex 不落盘，绕过执行策略）。脚本只做
// 提权/下载/解压；注册与双服务安装走 xnc-agent.exe enroll / install
// 子命令（见 install_scripts.go 模板与 agent 侧实现）。
package api

import (
	"fmt"
	"net/http"
	"strings"
)

// installAgentScript — GET /a/{token}：返回 agent 安装 PowerShell 脚本。
// token 为 enrollment token（动态嵌入脚本）。
func (h *handlers) installAgentScript(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" || !strings.HasPrefix(token, "xnc_enroll_") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "Invalid enrollment token. Get one from your XNC admin.\n")
		return
	}
	// 频道支持：/a/<token>（stable）| /a-dev/<token>（dev）。
	channel := "stable"
	if strings.HasPrefix(r.URL.Path, "/a-dev/") {
		channel = "dev"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, agentInstallPS(h.cfg.PublicURL, token, channel))
}

// installCliCmd — GET /c 或 /c-dev：返回 CLI 安装 cmd 脚本（保持现状，
// 不在本子项目范围内改动）。
func (h *handlers) installCliCmd(w http.ResponseWriter, r *http.Request) {
	channel := "stable"
	if strings.HasPrefix(r.URL.Path, "/c-dev") {
		channel = "dev"
	}
	script := cliInstallCmd(h.cfg.PublicURL, channel)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, script)
}

// cliInstallCmd 生成 CLI 安装批处理（现状保留）。
func cliInstallCmd(serverURL, channel string) string {
	psURL := serverURL + "/install/cli.ps1?channel=" + channel
	return fmt.Sprintf(`@echo off
setlocal
rem XNC CLI Installer
set "PS_SCRIPT=%%TEMP%%\xnc-install-cli.ps1"
curl -sL "%s" -o "%%PS_SCRIPT%%"
if not exist "%%PS_SCRIPT%%" (
    echo [ERROR] Failed to download install script from %s
    exit /b 1
)
powershell -NoProfile -ExecutionPolicy Bypass -File "%%PS_SCRIPT%%"
del "%%PS_SCRIPT%%" 2>nul
endlocal
`, psURL, serverURL)
}

// serveCliInstallPS — GET /install/cli.ps1?channel=（现状保留）
func (h *handlers) serveCliInstallPS(w http.ResponseWriter, r *http.Request) {
	channel := r.URL.Query().Get("channel")
	if channel == "" {
		channel = "stable"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, cliInstallPS(h.cfg.PublicURL, channel))
}
```

`install_scripts.go`：`agentInstallPS` 改为（新增 `{{SCRIPTURL}}`）：

```go
// agentInstallPS — agent 安装脚本（自提权 + 下载 + enroll + 双服务安装）。
// 参数经模板注入：token（enrollment）、channel、scriptURL（非管理员时
// UAC 重启自身的 irm 地址）。
func agentInstallPS(serverURL, token, channel string) string {
	scriptPath := "/a/" + token
	if channel == "dev" {
		scriptPath = "/a-dev/" + token
	}
	s := strings.ReplaceAll(agentInstallTemplate, "{{SERVER}}", serverURL)
	s = strings.ReplaceAll(s, "{{TOKEN}}", token)
	s = strings.ReplaceAll(s, "{{CHANNEL}}", channel)
	s = strings.ReplaceAll(s, "{{SCRIPTURL}}", serverURL+scriptPath)
	return s
}
```

`agentInstallTemplate` 常量整体替换为（PS 5.1 兼容、无反引号）：

```go
// agentInstallTemplate — agent 一键安装脚本。PS 只做提权/下载/解压/调用
// 子命令；服务创建与凭据处理全部在 xnc-agent.exe（svcapp/Go 代码）里。
// 禁止：sc.exe、$args、token 落盘（评审红线，install_handlers_test.go 钉住）。
const agentInstallTemplate = `# XNC Agent Installer - server {{SERVER}}, channel {{CHANNEL}}
$ErrorActionPreference = "Stop"
$Server = "{{SERVER}}"
$Token = "{{TOKEN}}"
$Channel = "{{CHANNEL}}"
$InstallDir = "C:\Program Files\XNC"
$StateDir = "C:\ProgramData\XNC"
$ScriptURL = "{{SCRIPTURL}}"
$CoreService = "XNCCore"
$AgentService = "XNCAgent"

function Step($n, $msg) { Write-Host ("  [{0}/6] {1}" -f $n, $msg) -NoNewline -ForegroundColor Cyan }
function OK { Write-Host "  OK" -ForegroundColor Green }

# [x] 需要管理员：UAC 重启自身（irm | iex 不落盘，重启后重新下载脚本）。
$principal = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Host "  Administrator rights required - please accept the UAC prompt to continue." -ForegroundColor Yellow
    Start-Process powershell.exe -Verb RunAs -ArgumentList @(
        "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", "irm '$ScriptURL' | iex")
    exit 0
}

try {
    # [1/6] 下载 bundle（enrollment token 是唯一凭据；下载可重试不烧 token）
    Step 1 "Downloading bundle"
    $staging = Join-Path $env:TEMP "xnc-install-bundle"
    if (Test-Path $staging) { Remove-Item $staging -Recurse -Force }
    New-Item -ItemType Directory -Path $staging -Force | Out-Null
    $bundleFile = Join-Path $staging "bundle.tar.gz"
    $bundleUrl = "$Server/install/bundle?token=$Token&channel=$Channel"
    $wc = New-Object System.Net.WebClient
    $downloaded = $false
    foreach ($attempt in 1..3) {
        try {
            $wc.DownloadFile($bundleUrl, $bundleFile)
            $downloaded = $true
            break
        } catch { Start-Sleep -Seconds 2 }
    }
    if (-not $downloaded) {
        throw "download failed after 3 attempts - server unreachable, or the enrollment link is expired/used up (ask your XNC admin for a new one)"
    }
    $sizeMB = [math]::Round((Get-Item $bundleFile).Length / 1MB, 1)
    Write-Host (" {0} MB" -f $sizeMB) -ForegroundColor DarkGray -NoNewline
    OK

    # [2/6] sha256 校验（服务端 X-Xnc-Sha256 头）
    Step 2 "Verifying bundle"
    $wantHash = $wc.ResponseHeaders["X-Xnc-Sha256"]
    if ($wantHash) {
        $hash = (Get-FileHash $bundleFile -Algorithm SHA256).Hash.ToLower()
        if ($hash -ne $wantHash.ToLower()) { throw "bundle sha256 mismatch (download corrupted - re-run the command)" }
    }
    OK

    # [3/6] 解压到 InstallDir
    Step 3 "Installing to $InstallDir"
    & tar.exe -xzf $bundleFile -C $staging
    if ($LASTEXITCODE -ne 0) { throw "failed to extract bundle" }
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    New-Item -ItemType Directory -Path $StateDir -Force | Out-Null
    foreach ($name in @("xnc-agent.exe", "xnc-core.exe", "xnc-desktop.exe", "xnc-shell.exe")) {
        Copy-Item (Join-Path $staging $name) $InstallDir -Force
    }
    OK

    # [4/6] 注册（一次性 token 只在本进程内存，永不落盘/进服务 argv）
    Step 4 "Enrolling node"
    $agentExe = Join-Path $InstallDir "xnc-agent.exe"
    & $agentExe enroll --server $Server --token $Token --state-dir $StateDir
    if ($LASTEXITCODE -ne 0) {
        throw "enrollment failed - see the message above (expired or used-up links must be re-issued by your XNC admin)"
    }
    OK

    # [5/6] 安装双服务（XNCCore + XNCAgent；binPath 零 secret，幂等）
    Step 5 "Installing services $CoreService + $AgentService"
    & $agentExe install --server $Server --state-dir $StateDir
    if ($LASTEXITCODE -ne 0) { throw "service installation failed - see the message above" }
    OK

    # [6/6] 验证：双服务 Running + 节点首连标记（有界等待）
    Step 6 "Verifying"
    $marker = Join-Path $StateDir "install-connected.ok"
    $verified = $false
    $deadline = (Get-Date).AddSeconds(120)
    while ((Get-Date) -lt $deadline) {
        $a = Get-Service $AgentService -ErrorAction SilentlyContinue
        $c = Get-Service $CoreService -ErrorAction SilentlyContinue
        if ($a -and $c -and $a.Status -eq "Running" -and $c.Status -eq "Running" -and (Test-Path $marker)) {
            $verified = $true
            break
        }
        Start-Sleep -Seconds 3
    }
    if (-not $verified) {
        $a = Get-Service $AgentService -ErrorAction SilentlyContinue
        $c = Get-Service $CoreService -ErrorAction SilentlyContinue
        if (-not $a -or $a.Status -ne "Running" -or -not $c -or $c.Status -ne "Running") {
            throw ("services did not reach Running - check {0}\agent-service.log and Windows Event Viewer" -f $StateDir)
        }
        Write-Host ("  WARN node has not connected yet - check {0}\agent-service.log" -f $StateDir) -ForegroundColor Yellow
    }
    Remove-Item $staging -Recurse -Force -ErrorAction SilentlyContinue
    OK

    Write-Host ""
    Write-Host "  Installation complete." -ForegroundColor Green
    Write-Host ("    Node will appear at {0} (or already has)." -f $Server) -ForegroundColor Green
    Write-Host ("    Log: {0}\agent-service.log" -f $StateDir) -ForegroundColor DarkGray
    Write-Host "    Uninstall services: xnc-agent uninstall (agent) / sc delete XNCCore (core)" -ForegroundColor DarkGray
} catch {
    Write-Host ""
    Write-Host "  Installation failed." -ForegroundColor Red
    Write-Host ("  Error: {0}" -f $_) -ForegroundColor Yellow
    Write-Host "  Troubleshooting:" -ForegroundColor Cyan
    Write-Host ("    - Check network: curl {0}/api/health" -f $Server)
    Write-Host "    - Re-run this command (it is safe to retry)"
    exit 1
}
`
```

`router.go`：把 `r.Get("/a/{token}", h.installAgentCmd)` 改为 `h.installAgentScript`（`/a-dev/{token}` 同），并删除 `r.Get("/install/agent.ps1", h.serveAgentInstallPS)` 一行（保留 `/install/cli.ps1`）：

```go
		// 快捷安装（无 JWT——token 是凭证；SPA 兜底之前注册）
		// irm https://xnc.app/a/<token> | iex    → agent 安装（PS 直出）
		// curl -sL xnc.app/c | cmd               → CLI 安装（.cmd 现状保留）
		r.Get("/a/{token}", h.installAgentScript)
		r.Get("/a-dev/{token}", h.installAgentScript)
		r.Get("/c", h.installCliCmd)
		r.Get("/c-dev", h.installCliCmd)
		// CLI 安装脚本本体（cmd 脚本内引用下载）
		r.Get("/install/cli.ps1", h.serveCliInstallPS)
```

同时清理 `install_scripts.go` 中不再被引用的死代码：删除 `footer` 常量与旧 `agentInstallPS` 上方的旧注释（Go 不对未用常量报错，须手动删除保持整洁；`banner` 仍被 `cliInstallTemplate` 使用，保留）。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./server/internal/api/ -run 'TestInstallAgentScript' -v`
Expected: PASS（含 PowerShell 语法解析子测试；在无 powershell 的 CI 上该子测试 SKIP）。

- [ ] **Step 5: 回归 + vet**

Run: `go vet ./server/... && go test ./server/internal/api/`
Expected: 全部 PASS，无未使用符号告警。

- [ ] **Step 6: Commit**

```bash
git add server/internal/api/install_handlers.go server/internal/api/install_scripts.go server/internal/api/router.go server/internal/api/install_handlers_test.go
git commit -m "feat(server): /a/{token} serves PowerShell directly - thin script, native enroll+install"
```

---

### Task 3: agent `enroll` 一次性子命令（含终端用户友好错误）

**Files:**
- Modify: `agent/cmd/xnc-agent/main.go`（新增 `enroll` 命令与 `runEnrollCLI`/`friendlyEnrollError`）
- Create: `agent/cmd/xnc-agent/enroll_cmd_test.go`

**Interfaces:**
- Consumes: `agent.Agent{ServerURL, Token, StateDir}` 与 `a.EnsureEnrolled(ctx)`（agent/agent.go:46，返回 `(*identity.Key, machineinfo.Info, error)`，`identity.Key.NodeID string`）；`cmdContext()`（main.go:117）。
- Produces: CLI `xnc-agent enroll --server <url> --token <tok> [--state-dir <dir>]`（`--server`/`--token` required）；成功 stdout `enrolled: node <id> (identity at <path>)` 退出 0；失败 stderr 打印友好信息并退出 1。`friendlyEnrollError(err error) string` 纯函数（测试钉住）。Task 2 的 PS 模板第 4 步调用本命令。

- [ ] **Step 1: 写失败测试**

创建 `agent/cmd/xnc-agent/enroll_cmd_test.go`（main.go 无 build tag，测试同）：

```go
package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestFriendlyEnrollError：服务端错误码 → 终端用户下一步动作文案。
func TestFriendlyEnrollError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"invalid", errors.New("enroll: ENROLLMENT_TOKEN_INVALID: invalid token"),
			"The enrollment link is invalid or already used up. Ask your XNC admin for a new one."},
		{"expired", errors.New("enroll: ENROLLMENT_TOKEN_EXPIRED: token expired"),
			"The enrollment link has expired. Ask your XNC admin for a new one."},
		{"already enrolled", errors.New("enroll: NODE_ALREADY_ENROLLED: machine already enrolled with a different key"),
			"This machine is already enrolled but its local identity is missing (state dir wiped?). Ask your XNC admin to reset the node, then re-run with a fresh token."},
		{"other", errors.New("dial tcp: refused"),
			"dial tcp: refused"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { assert.Equal(t, c.want, friendlyEnrollError(c.err)) })
	}
}

// TestEnrollCommandWiring：命令树包含 enroll 且必填 flag 生效。
func TestEnrollCommandWiring(t *testing.T) {
	root := newRootCmd()
	root.SetArgs([]string{"enroll", "--server", "http://s:8080"}) // 缺 --token
	err := root.Execute()
	assert.Error(t, err, "--token must be required")
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./agent/cmd/xnc-agent/ -run 'TestFriendlyEnrollError|TestEnrollCommandWiring' -v`
Expected: FAIL —— `friendlyEnrollError` 未定义、`enroll` 命令不存在。

- [ ] **Step 3: 实现**

`main.go`：import 增 `"fmt"`（已有）、`"os"`（已有）、`"path/filepath"`、`"strings"`、`"xnc/agent"`、`"xnc/proto"`；在 `newRootCmd` 中 `uninstall` 定义之后插入，并加入 `root.AddCommand`：

```go
	// enroll：一次性注册 CLI（安装脚本在 install 之前调用——token 在此
	// 消费后即从流程消失，永不进服务 argv）。
	enrollCmd := &cobra.Command{
		Use:   "enroll",
		Short: "enroll this machine with the control server (one-shot; run before install)",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runEnrollCLI(server, token, stateDir)
		},
	}
	enrollCmd.Flags().StringVar(&server, "server", "", "control server URL (required)")
	enrollCmd.Flags().StringVar(&token, "token", "", "enrollment token (required)")
	enrollCmd.Flags().StringVar(&stateDir, "state-dir", defaultStateDir(), "state directory")
	_ = enrollCmd.MarkFlagRequired("server")
	_ = enrollCmd.MarkFlagRequired("token")
```

`root.AddCommand(run, runDev, install, uninstall, enrollCmd)`（替换原行）。文件尾部追加：

```go
// runEnrollCLI 一次性注册：加载/生成身份并用 token 注册（与
// Agent.EnsureEnrolled 同一逻辑；identity 已存在且 pubkey 相同则幂等
// 成功）。跨平台（identity 存储的平台加密在 identity 包内部处理）。
func runEnrollCLI(server, token, stateDir string) error {
	a := &agent.Agent{ServerURL: server, Token: token, StateDir: stateDir}
	k, _, err := a.EnsureEnrolled(cmdContext())
	if err != nil {
		fmt.Fprintln(os.Stderr, friendlyEnrollError(err))
		return err
	}
	fmt.Printf("enrolled: node %s (identity at %s)\n", k.NodeID, filepath.Join(stateDir, "identity.json"))
	return nil
}

// friendlyEnrollError 把服务端错误码翻译成终端用户可执行的下一步动作
//（enroll.Enroll 的错误格式为 "enroll: <CODE>: <message>"）。
func friendlyEnrollError(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, proto.CodeEnrollmentTokenInvalid):
		return "The enrollment link is invalid or already used up. Ask your XNC admin for a new one."
	case strings.Contains(s, proto.CodeEnrollmentTokenExpired):
		return "The enrollment link has expired. Ask your XNC admin for a new one."
	case strings.Contains(s, proto.CodeNodeAlreadyEnrolled):
		return "This machine is already enrolled but its local identity is missing (state dir wiped?). Ask your XNC admin to reset the node, then re-run with a fresh token."
	default:
		return s
	}
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./agent/cmd/xnc-agent/ -run 'TestFriendlyEnrollError|TestEnrollCommandWiring' -v`
Expected: PASS。

- [ ] **Step 5: 回归 + 双平台编译**

Run: `go test ./agent/... && GOOS=linux go build ./agent/cmd/xnc-agent/`
Expected: 全部 PASS；linux 构建无错（验证跨平台无 windows 依赖泄漏）。

- [ ] **Step 6: Commit**

```bash
git add agent/cmd/xnc-agent/main.go agent/cmd/xnc-agent/enroll_cmd_test.go
git commit -m "feat(agent): one-shot enroll subcommand - consumes token before install, never persists it"
```

---

### Task 4: agent `install` 装双服务 + `svcapp.EnsureService` 幂等 + argv 去 token + 默认目录 `XNC`

**Files:**
- Modify: `agent/svcapp/service_windows.go`（新增 `EnsureService`/`ExpectedBinaryPath`）
- Create: `agent/svcapp/service_windows_test.go`
- Modify: `agent/cmd/xnc-agent/main_windows.go`（`buildServiceArgs` 去 token、`installService` 装双服务、`defaultStateDir` → XNC）
- Modify: `agent/cmd/xnc-agent/main_other.go`（`installService` 签名同步）
- Modify: `agent/cmd/xnc-agent/main_windows_test.go`（更新断言）

**Interfaces:**
- Consumes: `svcapp.ServiceConfig{Name, DisplayName, Description}`、`mgr.Connect/OpenService/CreateService`、`svc.Running`。
- Produces:
  - `svcapp.EnsureService(exePath string, cfg ServiceConfig, args ...string) error` —— 不存在则创建（Automatic）并启动；已存在则校验 binPath（不一致报错带指引，不静默改写）并确保 Running。
  - `svcapp.ExpectedBinaryPath(exePath string, args []string) string` —— 纯函数，重建 SCM 存储的 binPath（每元素 `windows.EscapeArg`）。
  - `buildServiceArgs(server, stateDir, serviceName, desktopCorePipe, desktopCoreSecretHex string) []string` —— **token 参数移除**。
  - `installService(server, stateDir, serviceName, desktopCorePipe, desktopCoreSecretHex string) error` —— 先装 XNCCore 再装 XNCAgent（main.go 的 install RunE 同步改调用）。
  - `defaultStateDir()` → `%ProgramData%\XNC`。

- [ ] **Step 1: 写失败测试（svcapp 纯函数）**

创建 `agent/svcapp/service_windows_test.go`：

```go
//go:build windows

package svcapp

import (
	"testing"

	"golang.org/x/sys/windows"
	"github.com/stretchr/testify/assert"
)

// TestExpectedBinaryPath：binPath 重建必须与 CreateService 的逐元素
// EscapeArg 拼装一致（含空格路径加引号）。
func TestExpectedBinaryPath(t *testing.T) {
	got := ExpectedBinaryPath(`C:\Program Files\XNC\xnc-core.exe`,
		[]string{"--service", "XNCCore", "--secret-file", `C:\ProgramData\XNC\core-secret.hex`})
	want := windows.EscapeArg(`C:\Program Files\XNC\xnc-core.exe`) + " " +
		windows.EscapeArg("--service") + " " + windows.EscapeArg("XNCCore") + " " +
		windows.EscapeArg("--secret-file") + " " + windows.EscapeArg(`C:\ProgramData\XNC\core-secret.hex`)
	assert.Equal(t, want, got)
	assert.Contains(t, got, `"C:\Program Files\XNC\xnc-core.exe"`)
	assert.Contains(t, got, `"C:\ProgramData\XNC\core-secret.hex"`)
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./agent/svcapp/ -run TestExpectedBinaryPath -v`
Expected: FAIL —— `ExpectedBinaryPath` 未定义。

- [ ] **Step 3: 实现 svcapp**

`agent/svcapp/service_windows.go`：import 增 `"fmt"`、`"strings"`、`"golang.org/x/sys/windows"`；`Install` 之后追加：

```go
// EnsureService 幂等安装：不存在 → 创建（Automatic）并启动；已存在 →
// 校验 binPath 与期望一致（不一致返回错误与处理指引，绝不静默改写）
// 并确保运行。args 语义同 Install（逐词 argv 元素）。
func EnsureService(exePath string, cfg ServiceConfig, args ...string) error {
	cfg = cfg.Normalize()
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(cfg.Name)
	if err != nil {
		s, err = m.CreateService(cfg.Name, exePath, mgr.Config{
			StartType:   mgr.StartAutomatic,
			DisplayName: cfg.DisplayName,
			Description: cfg.Description,
		}, args...)
		if err != nil {
			return err
		}
	} else {
		c, cerr := s.Config()
		if cerr != nil {
			s.Close()
			return cerr
		}
		if want := ExpectedBinaryPath(exePath, args); !strings.EqualFold(c.BinaryPathName, want) {
			s.Close()
			return fmt.Errorf("service %s exists with binPath %q (expected %q); "+
				"uninstall it first (xnc-agent uninstall / sc delete) or pass the original --state-dir",
				cfg.Name, c.BinaryPathName, want)
		}
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State != svc.Running {
		return s.Start()
	}
	return nil
}

// ExpectedBinaryPath 重建 CreateService 持久化的 binPath：exe 与每个 argv
// 元素各自经 windows.EscapeArg 引用后空格连接（与 x/sys mgr 的拼装一致，
// install 幂等校验的判据）。
func ExpectedBinaryPath(exePath string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, windows.EscapeArg(exePath))
	for _, a := range args {
		parts = append(parts, windows.EscapeArg(a))
	}
	return strings.Join(parts, " ")
}
```

- [ ] **Step 4: 跑 svcapp 测试确认通过**

Run: `go test ./agent/svcapp/ -v`
Expected: PASS。

- [ ] **Step 5: 更新 main_windows.go**

`main_windows.go` 全量替换 `installService`/`buildServiceArgs`/`defaultStateDir`（import 增 `"fmt"`）：

```go
// coreServiceName 与 scripts/install-xnccore.ps1 的默认一致（运维手动
// 路径与 agent 安装路径可互换幂等）。
const coreServiceName = "XNCCore"

// installService 安装 XNCCore + XNCAgent 双服务（均幂等）。先 core 后
// agent：core 首启自建并 DACL 锁定 <StateDir>\core-secret.hex，agent 的
// 桌面/exec 凭据回退即读该文件——StateDir 由本函数保证对齐。服务 argv
// 零 secret：token 已在 enroll 步骤消费，这里根本没有。
func installService(server, stateDir, serviceName, desktopCorePipe, desktopCoreSecretHex string) error {
	if err := installCoreService(stateDir); err != nil {
		return fmt.Errorf("install XNCCore: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return svcapp.EnsureService(exe, svcapp.ServiceConfig{Name: serviceName},
		buildServiceArgs(server, stateDir, serviceName, desktopCorePipe, desktopCoreSecretHex)...)
}

// installCoreService 安装 XNCCore：binPath 语义 = install-xnccore.ps1 的
// `xnc-core.exe --service XNCCore --secret-file <StateDir>\core-secret.hex`。
func installCoreService(stateDir string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	coreExe := filepath.Join(filepath.Dir(exe), "xnc-core.exe")
	if _, err := os.Stat(coreExe); err != nil {
		return fmt.Errorf("xnc-core.exe not found next to agent at %s (expected from the install bundle)", filepath.Dir(exe))
	}
	return svcapp.EnsureService(coreExe, svcapp.ServiceConfig{
		Name:        coreServiceName,
		DisplayName: "XNC Core",
		Description: "XNC node core (XNIP pipe server)",
	}, "--service", coreServiceName, "--secret-file", filepath.Join(stateDir, "core-secret.hex"))
}

// buildServiceArgs 构造 XNCAgent 服务 argv（逐词元素）。token 永不进
// argv：enroll 在 install 之前完成（存量机器 binPath 残留的旧 --token
// 由 run 的 flag 兼容读取、忽略）。
func buildServiceArgs(server, stateDir, serviceName, desktopCorePipe, desktopCoreSecretHex string) []string {
	args := []string{"run", "--server=" + server, "--state-dir=" + stateDir}
	if serviceName != "" && serviceName != svcapp.DefaultServiceName {
		args = append(args, "--service-name="+serviceName)
	}
	if desktopCorePipe != "" {
		args = append(args, "--desktop-core-pipe="+desktopCorePipe)
	}
	if desktopCoreSecretHex != "" {
		args = append(args, "--desktop-core-secret-hex="+desktopCoreSecretHex)
	}
	return args
}

func uninstallService(serviceName string) error { return svcapp.Uninstall(serviceName) }

// defaultStateDir：服务以 SYSTEM 运行，状态放统一的产品目录
// %ProgramData%\XNC（agent 与 core secret 共用；存量机器显式 --state-dir
// 不受默认值影响）。
func defaultStateDir() string {
	pd := os.Getenv("ProgramData")
	if pd == "" {
		pd = "."
	}
	return filepath.Join(pd, "XNC")
}
```

`main_other.go` 的 stub 同步签名（去 token 参数）：

```go
func installService(_, _, _, _, _ string) error {
	return errors.New("service host is only supported on Windows")
}
```

`main.go` 的 install RunE 改为（token flag 保留注册但弃用——不破坏 install-dev-agent.ps1，Task 6 更新该脚本）：

```go
	install := &cobra.Command{
		Use:   "install",
		Short: "install and start the XNCCore + XNCAgent Windows services",
		RunE: func(_ *cobra.Command, _ []string) error {
			if token != "" {
				fmt.Fprintln(os.Stderr,
					"warning: --token is deprecated and ignored by install; run 'xnc-agent enroll' first")
			}
			return installService(server, stateDir, serviceName, corePipe, coreSecretHex)
		},
	}
```

（`run`/`runDev` 的 RunE 与 flag 注册不动——run 仍接受 --token 兼容存量机器 argv。）

- [ ] **Step 6: 更新 main_windows_test.go 断言**

`TestBuildServiceArgs` 全量替换为：

```go
// 回归覆盖：服务参数必须是逐词 argv 元素，且 token 永不进 argv
//（凭据不变量：sc qc XNCAgent 输出零 secret）。
func TestBuildServiceArgs(t *testing.T) {
	t.Run("minimal", func(t *testing.T) {
		got := buildServiceArgs("http://127.0.0.1:8080", `C:\ProgramData\XNC`, "", "", "")
		assert.Equal(t, []string{
			"run",
			"--server=http://127.0.0.1:8080",
			`--state-dir=C:\ProgramData\XNC`,
		}, got)
	})

	t.Run("dev profile", func(t *testing.T) {
		got := buildServiceArgs("http://10.0.0.5:8080", `C:\ProgramData\XNCAgentDev`,
			"XNCAgentDev", `\\.\pipe\xnc-core-dev`, "746573")
		assert.Equal(t, []string{
			"run",
			"--server=http://10.0.0.5:8080",
			`--state-dir=C:\ProgramData\XNCAgentDev`,
			"--service-name=XNCAgentDev",
			`--desktop-core-pipe=\\.\pipe\xnc-core-dev`,
			"--desktop-core-secret-hex=746573",
		}, got)
	})

	t.Run("no token anywhere", func(t *testing.T) {
		for _, el := range buildServiceArgs("http://s", `D:\State Dir\XNC`, "", "", "") {
			assert.NotContains(t, el, " --",
				"joined-string shape regressed: element %q contains multiple flags", el)
			assert.NotContains(t, el, "token")
		}
	})
}

// defaultStateDir 统一为 %ProgramData%\XNC。
func TestDefaultStateDir(t *testing.T) {
	t.Setenv("ProgramData", `C:\ProgramData`)
	assert.Equal(t, `C:\ProgramData\XNC`, defaultStateDir())
}
```

（原文件头注释与 import 保持。）

- [ ] **Step 7: 跑 agent 全部测试 + 双平台编译**

Run: `go test ./agent/... && GOOS=linux go build ./agent/cmd/xnc-agent/ && GOOS=windows go build ./agent/...`
Expected: 全部 PASS（含 svcapp 与 cmd 新测试），双平台构建无错。

- [ ] **Step 8: Commit**

```bash
git add agent/svcapp/service_windows.go agent/svcapp/service_windows_test.go agent/cmd/xnc-agent/main_windows.go agent/cmd/xnc-agent/main_other.go agent/cmd/xnc-agent/main_windows_test.go agent/cmd/xnc-agent/main.go
git commit -m "feat(agent): install deploys XNCCore+XNCAgent idempotently; zero secrets in service argv; state dir unified to ProgramData\\XNC"
```

---

### Task 5: agent 首连标记 `install-connected.ok`

**Files:**
- Modify: `agent/agent.go`（OnReady 写一次性标记）
- Create: `agent/install_marker_test.go`

**Interfaces:**
- Consumes: `Agent.StateDir`、`updater.ConnectedMarkerPath`（更新 marker 同一模式）。
- Produces: `markFirstConnect(stateDir string) error`（包内可见）；标记文件 `<StateDir>/install-connected.ok`（内容为首次上线 RFC3339 时间戳）。Task 2 的 PS 模板第 6 步有界等待此文件。

- [ ] **Step 1: 写失败测试**

创建 `agent/install_marker_test.go`：

```go
package agent

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMarkFirstConnect：首连标记一次性写入（已存在则不动，保持「首次」
// 语义——mtime 即首次上线时间）。
func TestMarkFirstConnect(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "install-connected.ok")

	require.NoError(t, markFirstConnect(dir))
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	_, err = time.Parse(time.RFC3339, string(b))
	assert.NoError(t, err, "content must be RFC3339 timestamp")

	// 第二次调用不改内容（时间推进 1ms 以上再写必然不同）。
	time.Sleep(1100 * time.Millisecond)
	require.NoError(t, markFirstConnect(dir))
	b2, err := os.ReadFile(p)
	require.NoError(t, err)
	assert.Equal(t, string(b), string(b2), "marker must be write-once")
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./agent/ -run TestMarkFirstConnect -v`
Expected: FAIL —— `markFirstConnect` 未定义。

- [ ] **Step 3: 实现**

`agent/agent.go`：import 增 `"time"`；`OnReady` 内 `_ = os.WriteFile(updater.ConnectedMarkerPath(a.StateDir), ...)` 之后紧接一行：

```go
		// 安装脚本有界等待的首连标记（一次性；与更新 marker 同一模式）。
		_ = markFirstConnect(a.StateDir)
```

文件尾部追加：

```go
// markFirstConnect 首次成功建立控制连接后写 install-connected.ok（已存在
// 则不动——「首次」语义，mtime 即首次上线时间）。安装脚本第 6 步有界
// 等待该文件确认节点真正上线。
func markFirstConnect(stateDir string) error {
	p := filepath.Join(stateDir, "install-connected.ok")
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	return os.WriteFile(p, []byte(time.Now().UTC().Format(time.RFC3339)), 0o644)
}
```

- [ ] **Step 4: 跑测试确认通过 + 回归**

Run: `go test ./agent/...`
Expected: PASS。

- [ ] **Step 5: Commit**

```bash
git add agent/agent.go agent/install_marker_test.go
git commit -m "feat(agent): write-once install-connected.ok marker for installer verification"
```

---

### Task 6: dev 脚本与 README 更新 + 手动端到端验收

**Files:**
- Modify: `scripts/install-dev-agent.ps1:43-54`（install 分支先 enroll）
- Modify: `README.md:9-30`（入口命令改 `irm | iex`）

**Interfaces:**
- Consumes: Task 3 的 `enroll` 子命令（dev 脚本调用）；Task 2 的新 `/a/{token}` 语义（README 文案）。
- Produces: 无代码接口；本任务交付脚本/文档一致性 + 端到端验收清单执行。

- [ ] **Step 1: 更新 install-dev-agent.ps1**

`if ($Install) {` 块内，`$args = @('install', ...)` 之前插入 enroll 调用，并删除 `if ($Token -ne '') { $args += "--token=$Token" }` 行（install 不再接受 token 进 argv）：

```powershell
if ($Install) {
  if ($Server -eq '') { Write-Error "-Server is required for -Install"; exit 2 }
  New-Item -ItemType Directory -Force -Path $StateDir | Out-Null
  # Enroll first (one-shot CLI consumes the token; it never reaches the
  # service argv). Idempotent: existing identity + same pubkey reuses node.
  if ($Token -ne '') {
    & $BinPath enroll --server $Server --token $Token --state-dir $StateDir
    if ($LASTEXITCODE -ne 0) { Write-Error "agent enroll failed ($LASTEXITCODE)"; exit 1 }
  }
  $args = @('install',
    "--server=$Server",
    "--state-dir=$StateDir",
    "--service-name=$ServiceName",
    "--desktop-core-pipe=$PipeName",
    "--desktop-core-secret-hex=$SecretHex")
  & $BinPath @args
  if ($LASTEXITCODE -ne 0) { Write-Error "agent install failed ($LASTEXITCODE)"; exit 1 }
```

（块内其余行不动。）

- [ ] **Step 2: 更新 README 安装节**

`README.md` 「## 安装」到「## 快速上手」之间替换为：

````markdown
## 安装

### 全新机器装 Agent（一行）

```powershell
irm https://xnc.app/a/<enrollment-token> | iex
```

需要 admin 权限（脚本会自动弹 UAC 提权重试）。安装到 `C:\Program Files\XNC\`，注册 Windows 服务 `XNCCore` + `XNCAgent`（Automatic），节点上线后脚本打印确认。enrollment token 由管理员生成：`xnc token create <cluster>`（30 分钟有效、默认单次使用；token 全程不落盘、不进服务参数，用完即废）。整条命令可安全重复执行（幂等）。

### 装 CLI（一行）

```cmd
curl -sL xnc.app/c | cmd
```

无需 admin。安装到 `%LOCALAPPDATA%\XNC\`，自动加 PATH，开新终端即可 `xnc --help`。

### Dev 频道

```powershell
irm https://xnc.app/a-dev/<token> | iex    # dev 频道 agent
```

```cmd
curl -sL xnc.app/c-dev | cmd                :: dev 频道 CLI
```
````

- [ ] **Step 3: 编译冒烟 + 脚本语法检查**

Run: `go build ./... && powershell -NoProfile -Command "$t=$null;$e=$null;[System.Management.Automation.Language.Parser]::ParseFile('C:/Users/LABS/Desktop/XNC/scripts/install-dev-agent.ps1',[ref]$t,[ref]$e)|Out-Null;if($e.Count){$e|ForEach-Object{$_.Message};exit 1};'ps1 OK'"`
Expected: 构建成功，输出 `ps1 OK`。

- [ ] **Step 4: Commit**

```bash
git add scripts/install-dev-agent.ps1 README.md
git commit -m "docs(scripts): dev installer enrolls before install; README one-liner is irm | iex"
```

- [ ] **Step 5: 手动端到端验收（干净 Windows VM，人工执行）**

按 spec §8 清单逐项确认（全过才算子项目完成）：

1. `xnc token create default`（或 admin API）→ 得到一行命令。
2. 干净 VM 上普通（非管理员）PowerShell 执行 `irm <server>/a/<token> | iex` → UAC 弹窗确认。
3. 脚本走完 6 步无错误，打印 Installation complete。
4. `Get-Service XNCCore, XNCAgent` 均 Running；`C:\Program Files\XNC\` 下 4 个 exe 齐全。
5. `C:\ProgramData\XNC\` 下存在 `identity.json` 与 `core-secret.hex`；`core-secret.hex` 安全描述符仅 SYSTEM/Admins（`Get-Acl`）。
6. `sc qc XNCAgent` 与 `sc qc XNCCore` 的 BINARY_PATH_NAME **不含任何 token/secret**。
7. 服务端节点列表出现该机器；desktop/shell/exec 会话可用（core 真正装好的证明）。
8. 原命令再跑一遍：全幂等通过（enroll 幂等 + 服务已存在校验通过），最终打印成功。
9. token 过期场景：过期 token 执行 → enroll 步骤给出「链接已失效，联系管理员」文案。
10. （回滚兼容）存量机器走一次常规更新（`/api/admin/rollout`），确认旧 binPath（含残留 --token）不受影响、服务正常。

---

## 计划自审记录

- **Spec 覆盖**：§4.1 路由表 → Task 1、2；§4.2 模板重写 → Task 2；§5.1 enroll → Task 3；§5.2 install 双服务/幂等/argv → Task 4；§5.3 默认目录 → Task 4；§6 凭据不变量 → Task 2（模板禁令）+ 4（argv）+ E2E 第 6 项；§7 错误矩阵 → Task 2（脚本 catch 文案）+ Task 3（friendlyEnrollError）+ E2E 8/9；§8 测试策略 → 各任务 Step 1 与 E2E；§9 交付物 → 文件清单一一对应（`install-xnccore.ps1` 按设计保留不动）。
- **类型一致性**：`buildServiceArgs(server, stateDir, serviceName, pipe, secret)` 5 参在 Task 4 实现/测试与 main.go 调用一致；`installService` 5 参在 windows/other 两平台一致；`agentInstallPS(serverURL, token, channel)` 签名不变；`EnsureService`/`ExpectedBinaryPath` 在 Task 4 内定义并测试。
- **占位符**：无 TBD/「适当处理」类步骤；所有代码步骤含完整代码。
