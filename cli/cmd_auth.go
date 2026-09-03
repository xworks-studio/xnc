package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"xnc/proto"
)

type userDTO struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
}

func newLoginCmd() *cobra.Command {
	var email string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in to an XNC server (interactive on a TTY; password on stdin otherwise)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _ := LoadConfig()
			server := resolveServer(cmd, cfg)

			var token string
			var user userDTO

			if term.IsTerminal(int(os.Stdin.Fd())) {
				// 交互模式：缺省提示 + 掩码密码 + 401 重试。
				deps := loginDeps{
					promptServer: func() string {
						return promptLine("Server URL", cfg.Server)
					},
					promptEmail: func() string {
						return promptLine("Email", cfg.RememberedEmail)
					},
					promptPassword: promptPassword,
					doLogin:        doLogin,
				}
				s, t, u, err := runLoginFlow(deps, server, email)
				if err != nil {
					return loginFlowError(cmd, err)
				}
				server, token, user = s, t, u
			} else {
				// 非交互（Agent/脚本）：严格 flag + stdin 一行密码。
				if server == "" {
					return failUsage(cmd, "--server, XNC_SERVER, or config file required")
				}
				if email == "" {
					return failUsage(cmd, "--email required in non-interactive mode")
				}
				password, _ := readLine(os.Stdin)
				if password == "" {
					return failUsage(cmd, "password expected on stdin")
				}
				t, u, apiErr := doLogin(server, email, password)
				if apiErr != nil {
					return failAPI(cmd, apiErr)
				}
				token, user = t, u
			}

			if err := SaveConfig(Config{Server: server, Token: token, RememberedEmail: user.Email, Channel: savedChannel()}); err != nil {
				return failAPI(cmd, proto.Err(0, proto.CodeInternal, "save config: "+err.Error()))
			}
			if jsonOut(cmd) {
				PrintJSON(true, map[string]any{"user": user, "server": server,
					"token": token}, nil)
				printRegisterHint(cmd) // 未注册尾行提示（spec §10；json 模式走 stderr）
				return nil
			}
			fmt.Printf("Logged in to %s as %s (%s)\ntoken saved to %s\n",
				server, user.Email, user.DisplayName, configPath())
			printRegisterHint(cmd) // 未注册尾行提示（spec §10；管道不可达静默）
			return nil
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "account email")
	addJSONFlag(cmd)
	return cmd
}

// doLogin 执行一次登录请求。
func doLogin(server, email, password string) (string, userDTO, *proto.APIError) {
	cl := NewClient(server, "")
	var resp struct {
		Token string  `json:"token"`
		User  userDTO `json:"user"`
	}
	if e := cl.Do("POST", "/api/auth/login",
		map[string]string{"email": email, "password": password}, &resp); e != nil {
		return "", userDTO{}, e
	}
	return resp.Token, resp.User, nil
}

// readLine reads one line from r without printing any prompt (Agent-First:
// no interactive prompts; E2E pipes the password via stdin).
func readLine(r *os.File) (string, error) {
	line, err := bufio.NewReader(r).ReadString('\n')
	return strings.TrimSpace(line), err
}

func newWhoamiCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "whoami",
		Short: "Show the current user",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, usage := dial(cmd, true)
			if usage != "" {
				return failUsage(cmd, usage)
			}
			var resp struct {
				User userDTO `json:"user"`
			}
			if e := cl.Do("GET", "/api/auth/me", nil, &resp); e != nil {
				return failAPI(cmd, e)
			}
			if jsonOut(cmd) {
				PrintJSON(true, resp.User, nil)
				return nil
			}
			printKV([][2]string{
				{"id", resp.User.ID},
				{"email", resp.User.Email},
				{"display_name", resp.User.DisplayName},
			})
			return nil
		},
	}
	addJSONFlag(cmd)
	return cmd
}

func newVersionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if jsonOut(cmd) {
				PrintJSON(true, map[string]string{"version": cliVersion}, nil)
				return nil
			}
			fmt.Println("xnc " + cliVersion)
			return nil
		},
	}
	addJSONFlag(cmd)
	return cmd
}
