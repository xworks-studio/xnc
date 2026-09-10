package main

import (
	"github.com/spf13/cobra"
)

func newClusterCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "cluster", Short: "Cluster operations"}
	cmd.AddCommand(newClusterListCmd(), newClusterMemberCmd(), newClusterDeleteCmd())
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
			var clusters []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			if e := cl.Do("GET", "/api/clusters", nil, &clusters); e != nil {
				return failAPI(cmd, e)
			}
			if jsonOut(cmd) {
				PrintJSON(true, clusters, nil)
				return nil
			}
			rows := make([][]string, 0, len(clusters))
			for _, c := range clusters {
				rows = append(rows, []string{c.Name, c.ID})
			}
			printTable([]string{"NAME", "ID"}, rows)
			return nil
		},
	}
	addJSONFlag(cmd)
	return cmd
}
