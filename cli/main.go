// Command xnc is the XNC v2 CLI: login/whoami/status/version, cluster list,
// token create, node list/show, exec, run. Agent-First contract: --json
// envelope {"ok",data,"error"} on stdout plus stable exit codes; no
// interactive prompts.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"xnc/proto"
)

const cliVersion = "0.1.0"

func main() {
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
		Use:           "xnc",
		Short:         "XNC control plane CLI",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			out, _ := cmd.Flags().GetString("output")
			if out != "json" && out != "table" {
				return fmt.Errorf("--output must be json or table (got %q)", out)
			}
			return nil
		},
	}
	root.PersistentFlags().String("server", "",
		"XNC server base URL (env XNC_SERVER, then config file)")
	root.PersistentFlags().String("token", "",
		"API bearer token (env XNC_TOKEN, then config file)")
	root.PersistentFlags().String("output", "table", "output format: table or json")
	root.AddCommand(
		newLoginCmd(), newWhoamiCmd(), newStatusCmd(), newVersionCmd(),
		newClusterCmd(), newTokenCmd(), newNodeCmd(), newExecCmd(), newRunCmd(),
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
// (LoadConfig already merged env over file).
func resolveServer(cmd *cobra.Command, cfg Config) string {
	if v, _ := cmd.Flags().GetString("server"); v != "" {
		return v
	}
	return cfg.Server
}

func resolveToken(cmd *cobra.Command, cfg Config) string {
	if v, _ := cmd.Flags().GetString("token"); v != "" {
		return v
	}
	return cfg.Token
}

// dial builds a Client from flag/env/file. The returned string, when
// non-empty, is a usage error message (missing server or token).
func dial(cmd *cobra.Command, needToken bool) (*Client, string) {
	cfg, _ := LoadConfig()
	server, token := resolveServer(cmd, cfg), resolveToken(cmd, cfg)
	if server == "" {
		return nil, "--server, XNC_SERVER, or xnc login required"
	}
	if needToken && token == "" {
		return nil, "--token, XNC_TOKEN, or xnc login required"
	}
	return NewClient(server, token), ""
}
