package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

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
		Short: "Log in to an XNC server (reads password from stdin)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _ := LoadConfig()
			server := resolveServer(cmd, cfg)
			if server == "" {
				return failUsage(cmd, "--server, XNC_SERVER, or config file required")
			}
			password, _ := readLine(os.Stdin)
			if password == "" {
				return failUsage(cmd, "password expected on stdin")
			}
			cl := NewClient(server, "")
			var resp struct {
				Token string  `json:"token"`
				User  userDTO `json:"user"`
			}
			e := cl.Do("POST", "/api/auth/login",
				map[string]string{"email": email, "password": password}, &resp)
			if e != nil {
				return failAPI(cmd, e)
			}
			if err := SaveConfig(Config{Server: server, Token: resp.Token}); err != nil {
				return failAPI(cmd, proto.Err(0, proto.CodeInternal, "save config: "+err.Error()))
			}
			if jsonOut(cmd) {
				PrintJSON(true, map[string]any{"user": resp.User, "server": server,
					"token": resp.Token}, nil)
				return nil
			}
			fmt.Printf("logged in as %s (%s)\nserver %s\nconfig saved to %s\n",
				resp.User.Email, resp.User.DisplayName, server, configPath())
			return nil
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "account email")
	cmd.MarkFlagRequired("email")
	addJSONFlag(cmd)
	return cmd
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

func newStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Check server reachability (no auth required)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, usage := dial(cmd, false)
			if usage != "" {
				return failUsage(cmd, usage)
			}
			var health struct {
				Status  string `json:"status"`
				Version string `json:"version"`
			}
			if e := cl.Do("GET", "/api/health", nil, &health); e != nil {
				return failAPI(cmd, e)
			}
			if jsonOut(cmd) {
				PrintJSON(true, map[string]any{"server": cl.Base,
					"status": health.Status, "version": health.Version}, nil)
				return nil
			}
			fmt.Printf("server %s %s (version %s)\n", cl.Base, health.Status, health.Version)
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
