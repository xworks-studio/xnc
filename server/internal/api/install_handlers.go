// install_handlers.go — 快捷安装端点：/a/<token>（agent）、/c（CLI）。
//
// DEPRECATED（设计 §14，docs/superpowers/specs/2026-09-03-innosetup-installer-
// unified-auth-design.md）：本文件全套（含 /install/*.ps1 脚本本体）已被
// setup.exe + `xnc register` 流程取代——安装器（/setup.exe 下载、Inno Setup
// 安装、首用 register 绑定）是唯一安装入口。迁移期保留 2 个 release 周期
// 供存量用户过渡，随后整套删除。每次请求记一条 deprecation 日志（见
// logDeprecatedInstall），便于观测剩余流量、确认退役时机。
//
// 用户命令（复制/手打即完成安装）：
//
//	curl -sL xnc.app/a/xnc_enroll_XXXX | cmd
//	curl -sL xnc.app/c | cmd
//
// 端点返回一个 .cmd 批处理（Content-Type: application/octet-stream，curl |
// cmd 直接执行）。cmd 写 PS 脚本到 %TEMP% 后以 -ExecutionPolicy Bypass 调
// 用——绕过所有 PS 限制。脚本从 server 下载 bundle 并安装，UI 见
// installScriptAgent。
package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
)

// logDeprecatedInstall — 一行流每请求一条 deprecation 日志（§14 退役观测）。
func logDeprecatedInstall(r *http.Request) {
	slog.Warn("deprecated one-liner install served (removal after 2 release cycles; use setup.exe + xnc register)",
		"path", r.URL.Path, "remote", r.RemoteAddr)
}

// installAgentCmd — GET /a/{token}：返回 agent 安装 cmd 脚本。
// token 为 enrollment token（server 校验有效性后动态嵌入安装脚本）。
func (h *handlers) installAgentCmd(w http.ResponseWriter, r *http.Request) {
	logDeprecatedInstall(r)
	token := r.PathValue("token")
	if token == "" || !strings.HasPrefix(token, "xnc_enroll_") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "echo Invalid enrollment token. Get one from your XNC admin.\n")
		return
	}
	// 频道支持：/a/<token>（stable）| /a-dev/<token>（dev）。
	channel := "stable"
	if strings.HasPrefix(r.URL.Path, "/a-dev/") {
		channel = "dev"
	}

	script := agentInstallCmd(h.cfg.PublicURL, token, channel)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, script)
}

// installCliCmd — GET /c 或 /c-dev：返回 CLI 安装 cmd 脚本。
func (h *handlers) installCliCmd(w http.ResponseWriter, r *http.Request) {
	logDeprecatedInstall(r)
	channel := "stable"
	if strings.HasPrefix(r.URL.Path, "/c-dev") {
		channel = "dev"
	}
	script := cliInstallCmd(h.cfg.PublicURL, channel)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, script)
}

// agentInstallCmd 生成 agent 安装批处理。
func agentInstallCmd(serverURL, token, channel string) string {
	psURL := serverURL + "/install/agent.ps1?token=" + token + "&channel=" + channel
	return fmt.Sprintf(`@echo off
setlocal
rem XNC Agent Installer — generated for token %s
rem Downloads and runs the install script with ExecutionPolicy Bypass.
set "PS_SCRIPT=%%TEMP%%\xnc-install-agent.ps1"
curl -sL "%s" -o "%%PS_SCRIPT%%"
if not exist "%%PS_SCRIPT%%" (
    echo [ERROR] Failed to download install script from %s
    echo         Check network connection and try again.
    exit /b 1
)
powershell -NoProfile -ExecutionPolicy Bypass -File "%%PS_SCRIPT%%"
del "%%PS_SCRIPT%%" 2>nul
endlocal
`, token, psURL, serverURL)
}

// cliInstallCmd 生成 CLI 安装批处理。
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

// serveAgentInstallPS — GET /install/agent.ps1?token=&channel=：
// 输出 agent 安装 PowerShell 脚本（cmd 批处理引用下载此文件）。
func (h *handlers) serveAgentInstallPS(w http.ResponseWriter, r *http.Request) {
	logDeprecatedInstall(r)
	token := r.URL.Query().Get("token")
	channel := r.URL.Query().Get("channel")
	if channel == "" {
		channel = "stable"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, agentInstallPS(h.cfg.PublicURL, token, channel))
}

// serveCliInstallPS — GET /install/cli.ps1?channel=
func (h *handlers) serveCliInstallPS(w http.ResponseWriter, r *http.Request) {
	logDeprecatedInstall(r)
	channel := r.URL.Query().Get("channel")
	if channel == "" {
		channel = "stable"
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, cliInstallPS(h.cfg.PublicURL, channel))
}
