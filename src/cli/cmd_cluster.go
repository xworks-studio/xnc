package main

import (
	"fmt"
	"net/url"

	"github.com/spf13/cobra"
)

// clusterDTO mirrors the server's cluster JSON (0005 起 personal/role 随行).
type clusterDTO struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Personal bool   `json:"personal"`
	Role     string `json:"role"`
}

func newClusterCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "cluster", Short: "Cluster operations"}
	cmd.AddCommand(newClusterListCmd(), newClusterCreateCmd(), newClusterRenameCmd(),
		newClusterMemberCmd(), newClusterDeleteCmd())
	return cmd
}

func newClusterListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List clusters",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, usage := dial(cmd, true)
			if usage != "" {
				return failUsage(cmd, usage)
			}
			var clusters []clusterDTO
			if e := cl.Do("GET", "/api/clusters", nil, &clusters); e != nil {
				return failAPI(cmd, e)
			}
			if jsonOut(cmd) {
				PrintJSON(true, clusters, nil)
				return nil
			}
			rows := make([][]string, 0, len(clusters))
			for _, c := range clusters {
				name := c.Name
				if c.Personal {
					name += " (personal)"
				}
				rows = append(rows, []string{name, c.Role, c.ID})
			}
			printTable([]string{"NAME", "MY ROLE", "ID"}, rows)
			return nil
		},
	}
	addJSONFlag(cmd)
	return cmd
}

// newClusterCreateCmd: `xnc cluster create <name>` — 任意登录用户自建 cluster
// 并成为 owner（服务端限额 XNC_MAX_CLUSTERS_PER_USER，默认 20）。
func newClusterCreateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a cluster (you become its owner)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, usage := dial(cmd, true)
			if usage != "" {
				return failUsage(cmd, usage)
			}
			var resp clusterDTO
			if e := cl.Do("POST", "/api/clusters",
				map[string]string{"name": args[0]}, &resp); e != nil {
				return failAPI(cmd, e)
			}
			if jsonOut(cmd) {
				PrintJSON(true, resp, nil)
				return nil
			}
			fmt.Printf("created cluster %s (%s)\n", resp.Name, resp.ID)
			return nil
		},
	}
	addJSONFlag(cmd)
	return cmd
}

// newClusterRenameCmd: `xnc cluster rename <cluster> <new-name>` — owner-only
// 改名（撞名 400）。
func newClusterRenameCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rename <cluster> <new-name>",
		Short: "Rename a cluster (owner only)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, usage := dial(cmd, true)
			if usage != "" {
				return failUsage(cmd, usage)
			}
			path := "/api/clusters/" + url.PathEscape(args[0])
			var resp struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			if e := cl.Do("PATCH", path, map[string]string{"name": args[1]}, &resp); e != nil {
				return failAPI(cmd, e)
			}
			if jsonOut(cmd) {
				PrintJSON(true, resp, nil)
				return nil
			}
			fmt.Printf("renamed to %s\n", resp.Name)
			return nil
		},
	}
	addJSONFlag(cmd)
	return cmd
}
