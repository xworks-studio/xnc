package main

import (
	"net/url"
	"time"

	"github.com/spf13/cobra"
)

func newTokenCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Enrollment token operations"}
	cmd.AddCommand(newTokenCreateCmd())
	return cmd
}

func newTokenCreateCmd() *cobra.Command {
	var ttl string
	var maxUses int
	cmd := &cobra.Command{
		Use:   "create <cluster>",
		Short: "Create an enrollment token (plaintext token is printed exactly once)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, usage := dial(cmd, true)
			if usage != "" {
				return failUsage(cmd, usage)
			}
			var resp struct {
				ID        string    `json:"id"`
				Token     string    `json:"token"`
				ExpiresAt time.Time `json:"expiresAt"`
			}
			path := "/api/clusters/" + url.PathEscape(args[0]) + "/enrollment-tokens"
			e := cl.Do("POST", path, map[string]any{"ttl": ttl, "maxUses": maxUses}, &resp)
			if e != nil {
				return failAPI(cmd, e)
			}
			if jsonOut(cmd) {
				PrintJSON(true, resp, nil)
				return nil
			}
			printTable([]string{"TOKEN", "EXPIRES", "ID"},
				[][]string{{resp.Token, resp.ExpiresAt.Format(time.RFC3339), resp.ID}})
			return nil
		},
	}
	cmd.Flags().StringVar(&ttl, "ttl", "30m", "token lifetime (Go duration)")
	cmd.Flags().IntVar(&maxUses, "max-uses", 1, "maximum number of enrollments")
	addJSONFlag(cmd)
	return cmd
}
