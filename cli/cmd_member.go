package main

import (
	"fmt"
	"net/url"

	"github.com/spf13/cobra"
)

// memberDTO mirrors the server's member JSON exactly (json output must be stable).
type memberDTO struct {
	UserID      string `json:"user_id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// clusterMemberRoles matches the server's CHECK constraint (spec §6 role matrix);
// invalid roles are rejected client-side with exit 2 before any traffic.
var clusterMemberRoles = map[string]bool{"owner": true, "operator": true, "viewer": true}

// newClusterMemberCmd: `xnc cluster member list|add|remove` — membership
// management under the existing cluster group. list is any-member; add/remove
// are owner-only (the server enforces both).
func newClusterMemberCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "member", Short: "Cluster membership management"}
	cmd.AddCommand(newMemberListCmd(), newMemberAddCmd(), newMemberRemoveCmd())
	return cmd
}

func newMemberListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list <cluster>",
		Short: "List cluster members (name or UUID; table: EMAIL ROLE)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, usage := dial(cmd, true)
			if usage != "" {
				return failUsage(cmd, usage)
			}
			var members []memberDTO
			path := "/api/clusters/" + url.PathEscape(args[0]) + "/members"
			if e := cl.Do("GET", path, nil, &members); e != nil {
				return failAPI(cmd, e)
			}
			if jsonOut(cmd) {
				PrintJSON(true, members, nil)
				return nil
			}
			rows := make([][]string, 0, len(members))
			for _, m := range members {
				rows = append(rows, []string{m.Email, m.Role})
			}
			printTable([]string{"EMAIL", "ROLE"}, rows)
			return nil
		},
	}
	addJSONFlag(cmd)
	return cmd
}

func newMemberAddCmd() *cobra.Command {
	var role string
	cmd := &cobra.Command{
		Use:   "add <cluster> <user-id>",
		Short: "Add a user to a cluster as owner, operator or viewer (owner only)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !clusterMemberRoles[role] {
				return failUsage(cmd, "role must be owner, operator or viewer")
			}
			cl, usage := dial(cmd, true)
			if usage != "" {
				return failUsage(cmd, usage)
			}
			var resp struct {
				UserID string `json:"user_id"`
				Role   string `json:"role"`
			}
			path := "/api/clusters/" + url.PathEscape(args[0]) + "/members"
			body := map[string]string{"user_id": args[1], "role": role}
			if e := cl.Do("POST", path, body, &resp); e != nil {
				return failAPI(cmd, e)
			}
			if jsonOut(cmd) {
				PrintJSON(true, resp, nil)
				return nil
			}
			fmt.Printf("added %s to %s as %s\n", resp.UserID, args[0], resp.Role)
			return nil
		},
	}
	cmd.Flags().StringVar(&role, "role", "", "membership role: owner, operator or viewer")
	addJSONFlag(cmd)
	return cmd
}

func newMemberRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <cluster> <user-id>",
		Short: "Remove a member from a cluster (owner only; idempotent 204)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, usage := dial(cmd, true)
			if usage != "" {
				return failUsage(cmd, usage)
			}
			path := "/api/clusters/" + url.PathEscape(args[0]) +
				"/members/" + url.PathEscape(args[1])
			if e := cl.Do("DELETE", path, nil, nil); e != nil {
				return failAPI(cmd, e)
			}
			if jsonOut(cmd) {
				PrintJSON(true, nil, nil)
				return nil
			}
			fmt.Printf("removed %s from %s\n", args[1], args[0])
			return nil
		},
	}
	addJSONFlag(cmd)
	return cmd
}
