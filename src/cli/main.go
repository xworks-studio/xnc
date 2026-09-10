// Command xnc is the XNC v2 CLI: login/whoami/status/version, cluster
// list/member/delete, token create, node list/show/disable/enable, exec, run,
// shell, upload/download, rdp, audit list.
// Agent-First contract:
// --json envelope {"ok",data,"error"} on stdout plus stable exit codes; no
// interactive prompts outside `xnc shell` (which requires a real TTY).
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"xnc/proto"
)

const cliVersion = "0.3.0"

// defaultServerURL 是 CLI 的固定生产控制面（设计 §3.4）：register/login
// 不再询问 server；--server 与 XNC_SERVER 仅为开发/测试保留（MarkHidden，
// 帮助文本不出现）。
const defaultServerURL = "https://xnc.app"

func main() {
	cleanupOldCLI() // 自更新残留清扫（幂等）
	os.Exit(runCLI(context.Background(), os.Args[1:]))
}

// runCLI executes the root command with args and returns the process exit
// code: 0 ok, 2 usage, 240+ mapped from the API error code.
func runCLI(ctx context.Context, args []string) int {
	root := newRootCmd()
	root.SetArgs(args)
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	if err := root.ExecuteContext(ctx); err != nil {
		var ex *exitError
		if errors.As(err, &ex) {
			return ex.code
		}
		fmt.Fprintln(os.Stderr, "xnc: "+err.Error())
		return exitUsage
	}
	return exitOK
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "xnc",
		Short: "XNC — remote node control",
		Long: `XNC control plane CLI.

Manage remote Windows nodes: execute commands, transfer files, view screens,
open remote desktops, and manage clusters.

Quick start:
  xnc login                          connect to a server
  xnc register                       bind this machine as a node
  xnc node list                      see your nodes
  xnc exec <node> "hostname"         run a command
  xnc exec <node> --shell bash "ls" run in bash (simplest quoting)

Global flags:
  --token TOKEN   auth token (env XNC_TOKEN, or config)
  --output FMT    table | json (affects data commands)
  --json          per-command shorthand for --output json

Exit codes:
  0 success | 2 usage | 240 auth | 241 forbidden | 242 node offline
  243 timeout | 244 not found | 245 network | 246 quota | 250 internal
  Other values: remote process exit code passthrough (exec only)`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       cliVersion, // enables --version flag
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			out, _ := cmd.Flags().GetString("output")
			if out != "json" && out != "table" {
				return fmt.Errorf("--output must be json or table (got %q)", out)
			}
			return nil
		},
		// 无参数时打印完整帮助（比 cobra 默认的裸 usage 更友好）。
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.PersistentFlags().String("server", "",
		"XNC server base URL (env XNC_SERVER, then config file)")
	_ = root.PersistentFlags().MarkHidden("server")
	root.PersistentFlags().String("token", "",
		"API bearer token (env XNC_TOKEN, then config file)")
	root.PersistentFlags().String("output", "table", "output format: table or json")
	root.AddCommand(
		// Core operations
		newExecCmd(), newRunCmd(), newShellCmd(),
		newUploadCmd(), newDownloadCmd(),
		newScreenCmd(), newRdpCmd(),
		// Node & cluster management
		newNodeCmd(), newClusterCmd(), newTokenCmd(),
		// Auth & info
		newLoginCmd(), newLogoutCmd(), newRegisterCmd(), newDeregisterCmd(),
		newWhoamiCmd(), newStatusCmd(), newVersionCmd(),
		// Admin & maintenance
		newAuditCmd(), newUpdateCmd(), newUpgradeCmd(),
	)
	return root
}

// exitError carries an explicit process exit code out of a RunE.
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

// failAPI reports an API error: envelope on stdout in --json mode, a plain
// line on stderr otherwise, and returns the mapped exit code.
func failAPI(cmd *cobra.Command, e *proto.APIError) error {
	if jsonOut(cmd) {
		PrintJSON(false, nil, e)
	} else {
		fmt.Fprintln(os.Stderr, "xnc: "+e.Error())
	}
	return &exitError{code: ExitCode(e), msg: e.Error()}
}

// failUsage reports a client-side usage problem (exit code 2).
func failUsage(cmd *cobra.Command, msg string) error {
	if jsonOut(cmd) {
		PrintJSON(false, nil, proto.Err(0, "USAGE", msg))
	}
	fmt.Fprintln(os.Stderr, "xnc: "+msg)
	return &exitError{code: exitUsage, msg: msg}
}

// jsonOut: --json per-command flag is equivalent to the global --output json.
func jsonOut(cmd *cobra.Command) bool {
	if j, _ := cmd.Flags().GetBool("json"); j {
		return true
	}
	o, _ := cmd.Flags().GetString("output")
	return o == "json"
}

// addJSONFlag registers the per-command --json shorthand on a data command.
func addJSONFlag(cmd *cobra.Command) {
	cmd.Flags().Bool("json", false, "output JSON envelope (same as --output json)")
}

// resolveServer/resolveToken apply precedence: flag > env > config file
// > 生产默认（server 恒非空；设计 §3.4）。
func resolveServer(cmd *cobra.Command, cfg Config) string {
	if v, _ := cmd.Flags().GetString("server"); v != "" {
		return v
	}
	if cfg.Server != "" {
		return cfg.Server
	}
	return defaultServerURL
}

func resolveToken(cmd *cobra.Command, cfg Config) string {
	if v, _ := cmd.Flags().GetString("token"); v != "" {
		return v
	}
	return cfg.Token
}

// dial builds a Client from flag/env/file. The returned string, when
// non-empty, is a usage error message (missing token; server 恒有默认值).
func dial(cmd *cobra.Command, needToken bool) (*Client, string) {
	cfg, _ := LoadConfig()
	token := resolveToken(cmd, cfg)
	if needToken && token == "" {
		return nil, "--token, XNC_TOKEN, or xnc login required"
	}
	return NewClient(resolveServer(cmd, cfg), token), ""
}

// aliasCmd 返回一个使用不同名称的命令浅拷贝（共享 RunE 与 flags）。
