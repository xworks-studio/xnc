package main

import (
	"net/url"
	"regexp"
	"strconv"

	"github.com/spf13/cobra"

	"xnc/proto"
)

// nodeDTO mirrors the server's node JSON exactly (json output must be stable).
type nodeDTO struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Cluster      string  `json:"cluster"`
	Hostname     string  `json:"hostname"`
	OSVersion    string  `json:"os_version"`
	AgentVersion string  `json:"agent_version"`
	ShellType    string  `json:"shell_type"`
	Status       string  `json:"status"`
	LastSeenAt   *string `json:"last_seen_at"`
}

func (n nodeDTO) lastSeen() string {
	if n.LastSeenAt == nil {
		return "never"
	}
	return *n.LastSeenAt
}

func newNodeCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "node", Short: "Node operations"}
	cmd.AddCommand(newNodeListCmd(), newNodeShowCmd())
	return cmd
}

func newNodeListCmd() *cobra.Command {
	var cluster, statusFilter string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List nodes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, usage := dial(cmd, true)
			if usage != "" {
				return failUsage(cmd, usage)
			}
			q := url.Values{}
			if cluster != "" {
				q.Set("clusterId", cluster) // server accepts cluster name or UUID
			}
			if statusFilter != "" {
				q.Set("status", statusFilter)
			}
			path := "/api/nodes"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			nodes, e := fetchNodes(cl, path)
			if e != nil {
				return failAPI(cmd, e)
			}
			if jsonOut(cmd) {
				PrintJSON(true, nodes, nil)
				return nil
			}
			rows := make([][]string, 0, len(nodes))
			for _, n := range nodes {
				rows = append(rows, []string{n.Name, n.Cluster, n.Status})
			}
			printTable([]string{"NAME", "CLUSTER", "STATUS"}, rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&cluster, "cluster", "", "filter by cluster name or UUID")
	cmd.Flags().StringVar(&statusFilter, "status", "", "filter by status (online/offline)")
	addJSONFlag(cmd)
	return cmd
}

func fetchNodes(cl *Client, path string) ([]nodeDTO, *proto.APIError) {
	var nodes []nodeDTO
	if e := cl.Do("GET", path, nil, &nodes); e != nil {
		return nil, e
	}
	return nodes, nil
}

var uuidRe = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// resolveNode: UUIDs pass through; other args are matched against node names
// via the list endpoint, erroring with candidates when ambiguous.
func resolveNode(cl *Client, arg string) (string, *proto.APIError) {
	if uuidRe.MatchString(arg) {
		return arg, nil
	}
	nodes, e := fetchNodes(cl, "/api/nodes")
	if e != nil {
		return "", e
	}
	var matches []string
	for _, n := range nodes {
		if n.Name == arg {
			matches = append(matches, n.ID)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", proto.Err(404, proto.CodeNodeNotFound, "no node named "+arg)
	default:
		return "", proto.Err(404, proto.CodeNodeNotFound,
			"ambiguous node name "+arg+" matches "+strconv.Itoa(len(matches))+" nodes; use a UUID")
	}
}

func newNodeShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <node>",
		Short: "Show node details (UUID or unique node name)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, usage := dial(cmd, true)
			if usage != "" {
				return failUsage(cmd, usage)
			}
			id, e := resolveNode(cl, args[0])
			if e != nil {
				return failAPI(cmd, e)
			}
			var n nodeDTO
			if e := cl.Do("GET", "/api/nodes/"+url.PathEscape(id), nil, &n); e != nil {
				return failAPI(cmd, e)
			}
			if jsonOut(cmd) {
				PrintJSON(true, n, nil)
				return nil
			}
			printKV([][2]string{
				{"id", n.ID}, {"name", n.Name}, {"cluster", n.Cluster},
				{"hostname", n.Hostname}, {"os_version", n.OSVersion},
				{"agent_version", n.AgentVersion}, {"shell_type", n.ShellType},
				{"status", n.Status}, {"last_seen_at", n.lastSeen()},
			})
			return nil
		},
	}
	addJSONFlag(cmd)
	return cmd
}
