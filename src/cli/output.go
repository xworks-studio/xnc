package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"xnc/proto"
)

// PrintJSON writes the single-line Agent-First envelope to stdout:
// {"ok":bool,"data":...,"error":null|{"code","message"}}.
func PrintJSON(ok bool, data any, e *proto.APIError) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(map[string]any{"ok": ok, "data": data, "error": e})
}

func printTable(headers []string, rows [][]string) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for _, h := range headers {
		fmt.Fprintf(w, "%s\t", h)
	}
	fmt.Fprintln(w)
	for _, r := range rows {
		for _, c := range r {
			fmt.Fprintf(w, "%s\t", c)
		}
		fmt.Fprintln(w)
	}
	_ = w.Flush()
}

// printKV prints key=value detail pairs as a two-column table.
func printKV(pairs [][2]string) {
	rows := make([][]string, len(pairs))
	for i, p := range pairs {
		rows[i] = []string{p[0], p[1]}
	}
	printTable([]string{"KEY", "VALUE"}, rows)
}

// outf writes a progress/prompt line for humans; --json mode diverts it to
// stderr so stdout stays a single JSON envelope (Agent-First contract).
func outf(cmd *cobra.Command, format string, args ...any) {
	w := io.Writer(os.Stdout)
	if jsonOut(cmd) {
		w = os.Stderr
	}
	fmt.Fprintf(w, format, args...)
}
