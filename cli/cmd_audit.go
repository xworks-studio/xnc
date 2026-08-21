package main

import (
	"encoding/json"
	"net/url"
	"strconv"
	"time"

	"github.com/spf13/cobra"
)

// auditDTO mirrors the server's audit row JSON exactly (json output must be
// stable); nullable dimensions serialize as null like the server's.
type auditDTO struct {
	ID        int64           `json:"id"`
	UserID    *string         `json:"user_id"`
	UserEmail *string         `json:"user_email"`
	ClusterID *string         `json:"cluster_id"`
	NodeID    *string         `json:"node_id"`
	NodeName  *string         `json:"node_name"`
	Action    string          `json:"action"`
	SessionID string          `json:"session_id"`
	Metadata  json.RawMessage `json:"metadata"`
	CreatedAt time.Time       `json:"created_at"`
}

// orDash renders a nullable UUID column for table output.
func orDash(s *string) string {
	if s == nil {
		return "-"
	}
	return *s
}

// newAuditCmd: `xnc audit list` — admin-only audit query with combinable
// filters. since accepts Go durations (24h), Nd day shorthand (7d) or an
// RFC3339 timestamp; the server rejects anything else with 400.
func newAuditCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "audit", Short: "Audit log operations"}
	cmd.AddCommand(newAuditListCmd())
	return cmd
}

func newAuditListCmd() *cobra.Command {
	var node, user, action, since string
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "list [--node n] [--user u] [--action a] [--since 7d] [--limit N] [--offset N]",
		Short: "Query audit entries, newest first (admin only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, usage := dial(cmd, true)
			if usage != "" {
				return failUsage(cmd, usage)
			}
			q := url.Values{}
			if node != "" {
				ref, e := resolveNode(cl, node) // --node accepts a name or UUID
				if e != nil {
					return failAPI(cmd, e)
				}
				q.Set("nodeId", ref.ID)
			}
			if user != "" {
				q.Set("userId", user)
			}
			if action != "" {
				q.Set("action", action)
			}
			if since != "" {
				q.Set("since", since)
			}
			if limit > 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			if offset > 0 {
				q.Set("offset", strconv.Itoa(offset))
			}
			path := "/api/audit"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			var rows []auditDTO
			if e := cl.Do("GET", path, nil, &rows); e != nil {
				return failAPI(cmd, e)
			}
			if jsonOut(cmd) {
				PrintJSON(true, rows, nil)
				return nil
			}
			out := make([][]string, 0, len(rows))
			for _, a := range rows {
				user := orDash(a.UserEmail)
				if user == "-" {
					user = orDash(a.UserID) // fallback to UUID if no email
				}
				node := orDash(a.NodeName)
				if node == "-" {
					node = orDash(a.NodeID)
				}
				out = append(out, []string{
					strconv.FormatInt(a.ID, 10), a.Action,
					user, node,
					relTime(a.CreatedAt),
				})
			}
			printTable([]string{"ID", "ACTION", "USER", "NODE", "WHEN"}, out)
			return nil
		},
	}
	cmd.Flags().StringVar(&node, "node", "", "filter by node name or UUID")
	cmd.Flags().StringVar(&user, "user", "", "filter by user UUID")
	cmd.Flags().StringVar(&action, "action", "", "filter by action (e.g. node.disable)")
	cmd.Flags().StringVar(&since, "since", "", "only entries after: duration (24h, 7d) or RFC3339")
	cmd.Flags().IntVar(&limit, "limit", 0, "page size (server default 50, max 200)")
	cmd.Flags().IntVar(&offset, "offset", 0, "page offset")
	addJSONFlag(cmd)
	return cmd
}
