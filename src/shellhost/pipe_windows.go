//go:build windows

// pipe_windows.go — winio named pipe 服务端监听(M0 先例:core pipe 为
// SY+BA;desktop rt pipe 为 SY+BA+控制台用户 SID)。xnc-shell 的连接方
// = agent(控制台用户令牌,dev 拓扑)或 SYSTEM 会话代理,故沿用 rt pipe
// 的 BuildUserSddl 模式:SY + BA + 本进程用户 SID(core 以用户令牌 spawn
// xnc-shell 时本进程用户即该控制台用户,agent 同用户直连;SYSTEM@会话
// spawn 时 SID=SY,重复 ACE 无害,agent 走 BA——与今日 dev 拓扑经
// SY+BA 连 core pipe 的通路一致)。secret 仍需 M0 握手证明,DACL 只控
// 连接面。
package main

import (
	"fmt"
	"net"
	"strings"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// buildPipeSDDL 构造 pipe DACL SDDL SY+BA+own user(镜像
// native/desktop/rt_pipe_server.cpp BuildUserSddl)。
func buildPipeSDDL() string {
	sid, err := ownUserSID()
	if err != nil || sid == "" {
		// 取不到用户 SID 时退回 SY+BA(与 rt server 同一兜底)。
		return "D:P(A;;GA;;;SY)(A;;GA;;;BA)"
	}
	return "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + sid + ")"
}

func ownUserSID() (string, error) {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &tok); err != nil {
		return "", err
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", err
	}
	sidStr := u.User.Sid.String()
	return strings.ToUpper(sidStr), nil
}

// listenNetPipe 返回 winio pipe 监听(sddlOverride 空 = 默认 DACL)。
func listenNetPipe(pipeName string, sddlOverride string) (net.Listener, error) {
	sddl := sddlOverride
	if sddl == "" {
		sddl = buildPipeSDDL()
	}
	ln, err := winio.ListenPipe(pipeName, &winio.PipeConfig{
		SecurityDescriptor: sddl,
		MessageMode:        false,
		InputBufferSize:    64 * 1024,
		OutputBufferSize:   64 * 1024,
	})
	if err != nil {
		return nil, fmt.Errorf("shellhost: listen %s: %w", pipeName, err)
	}
	return ln, nil
}
