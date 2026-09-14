package main

import (
	"fmt"
	"net/url"

	"github.com/spf13/cobra"
)

// newNodeDisableCmd / newNodeEnableCmd: owner-only node admin. Both resolve
// their <node> arg (UUID or unique name) then POST to a 204 endpoint; the
// server audits node.disable / node.enable and rejects non-owners with 403
// (CLI exit 241).
func newNodeDisableCmd() *cobra.Command {
	return newNodeAdminCmd("disable",
		"Mark a node disabled (owner only): session endpoints return 403 NODE_DISABLED")
}

func newNodeEnableCmd() *cobra.Command {
	return newNodeAdminCmd("enable",
		"Re-enable a disabled node (owner only): disabled becomes offline")
}

func newNodeAdminCmd(action, short string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   action + " <node>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, usage := dial(cmd, true)
			if usage != "" {
				return failUsage(cmd, usage)
			}
			ref, e := resolveNode(cl, args[0])
			if e != nil {
				return failAPI(cmd, e)
			}
			path := "/api/nodes/" + url.PathEscape(ref.ID) + "/" + action
			if e := cl.Do("POST", path, nil, nil); e != nil {
				return failAPI(cmd, e)
			}
			if jsonOut(cmd) {
				PrintJSON(true, nil, nil)
				return nil
			}
			label := ref.Name
			if label == "" {
				label = ref.ID // UUID arg: no name resolution round trip happened
			}
			fmt.Printf("%sd %s\n", action, label) // disabled web-01 / enabled web-01
			return nil
		},
	}
	addJSONFlag(cmd)
	return cmd
}

// newClusterDeleteCmd: `xnc cluster delete <cluster>` — owner-only soft
// delete. The server refuses clusters that still have nodes (409
// CLUSTER_NOT_EMPTY, CLI exit 250); the row is tombstoned via deleted_at and
// the name becomes reusable (0005).
func newClusterDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <cluster>",
		Short: "Soft-delete an empty cluster (owner only; nodes must be gone first)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, usage := dial(cmd, true)
			if usage != "" {
				return failUsage(cmd, usage)
			}
			path := "/api/clusters/" + url.PathEscape(args[0])
			if e := cl.Do("DELETE", path, nil, nil); e != nil {
				return failAPI(cmd, e)
			}
			if jsonOut(cmd) {
				PrintJSON(true, nil, nil)
				return nil
			}
			fmt.Printf("deleted cluster %s\n", args[0])
			return nil
		},
	}
	addJSONFlag(cmd)
	return cmd
}
