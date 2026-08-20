package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"xnc/proto"
)

// promptLine 展示 label（含默认值提示）并读取一行；空输入/EOF 返回 def。
// 仅交互 TTY 路径使用（非 TTY 走 stdin 严格模式，Agent 契约不变）。
func promptLine(label, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return def
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

// promptPassword 掩码读取密码（无回显）。
func promptPassword() (string, error) {
	fmt.Print("Password: ")
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	return string(b), err
}

var errLoginRetries = errors.New("login: too many failed attempts")

// loginDeps 把登录编排的交互面收窄为可注入的函数，便于单测。
type loginDeps struct {
	promptServer   func() string
	promptEmail    func() string
	promptPassword func() (string, error)
	doLogin        func(server, email, password string) (string, userDTO, *proto.APIError)
}

// runLoginFlow 交互式登录编排：缺省提示 → 掩码密码 → 401 重试（≤3 次）。
// 返回补齐后的 server/token/user；非 401 错误立即失败。
func runLoginFlow(deps loginDeps, server, email string) (string, string, userDTO, error) {
	if server == "" {
		server = deps.promptServer()
	}
	if email == "" {
		email = deps.promptEmail()
	}
	for attempt := 0; attempt < 3; attempt++ {
		password, err := deps.promptPassword()
		if err != nil {
			return "", "", userDTO{}, err
		}
		if password == "" {
			fmt.Println("xnc: empty password")
			continue
		}
		token, user, apiErr := deps.doLogin(server, email, password)
		if apiErr == nil {
			return server, token, user, nil
		}
		if apiErr.Code != proto.CodeUnauthorized {
			return "", "", userDTO{}, apiErr
		}
		fmt.Printf("xnc: login failed (%s), attempt %d of 3\n", apiErr.Message, attempt+1)
	}
	return "", "", userDTO{}, errLoginRetries
}
