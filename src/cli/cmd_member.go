package main

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"xnc/proto"
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

// looksLikeEmail：成员参数形态分流——email 走 {email}（owner 无须 admin 列全量
// 用户），否则按 user_id（UUID）。
func looksLikeEmail(s string) bool { return strings.Contains(s, "@") }

// resolveMemberUser 把 <email-or-user-id> 解析为 user_id：email 时拉成员表
// 匹配（remove/role 的路径参数只收 UUID）。
func resolveMemberUser(cl *Client, clusterRef, arg string) (string, *proto.APIError) {
	if !looksLikeEmail(arg) {
		return arg, nil
	}
	var members []memberDTO
	path := "/api/clusters/" + url.PathEscape(clusterRef) + "/members"
	if e := cl.Do("GET", path, nil, &members); e != nil {
		return "", e
	}
	for _, m := range members {
		if strings.EqualFold(m.Email, arg) {
			return m.UserID, nil
		}
	}
	return "", proto.Err(404, proto.CodeUserNotFound, "no member "+arg)
}

// newClusterMemberCmd: `xnc cluster member list|add|role|remove` — membership
// management under the existing cluster group. list is any-member; add/role/
// remove are owner-only (the server enforces).
func newClusterMemberCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "member", Short: "Cluster membership management"}
	cmd.AddCommand(newMemberListCmd(), newMemberAddCmd(), newMemberRoleCmd(), newMemberRemoveCmd())
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
				rows = append(rows, []string{m.Email, m.Role, m.UserID})
			}
			printTable([]string{"EMAIL", "ROLE", "USER_ID"}, rows)
			return nil
		},
	}
	addJSONFlag(cmd)
	return cmd
}

func newMemberAddCmd() *cobra.Command {
	var role string
	cmd := &cobra.Command{
		Use:   "add <cluster> <email-or-user-id>",
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
			// email 优先（GET /api/users 是 admin-only，普通 owner 只有 email）。
			body := map[string]string{"user_id": args[1], "role": role}
			if looksLikeEmail(args[1]) {
				body = map[string]string{"email": args[1], "role": role}
			}
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

// newMemberRoleCmd: `xnc cluster member role <cluster> <email-or-user-id> <role>`
// — owner-only 改角色；最后 owner 不可降级（服务端 400）。
func newMemberRoleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "role <cluster> <email-or-user-id> <owner|operator|viewer>",
		Short: "Change a member's role (owner only; last owner cannot be demoted)",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !clusterMemberRoles[args[2]] {
				return failUsage(cmd, "role must be owner, operator or viewer")
			}
			cl, usage := dial(cmd, true)
			if usage != "" {
				return failUsage(cmd, usage)
			}
			uid, e := resolveMemberUser(cl, args[0], args[1])
			if e != nil {
				return failAPI(cmd, e)
			}
			var resp struct {
				UserID string `json:"user_id"`
				Role   string `json:"role"`
			}
			path := "/api/clusters/" + url.PathEscape(args[0]) +
				"/members/" + url.PathEscape(uid)
			if e := cl.Do("PATCH", path, map[string]string{"role": args[2]}, &resp); e != nil {
				return failAPI(cmd, e)
			}
			if jsonOut(cmd) {
				PrintJSON(true, resp, nil)
				return nil
			}
			fmt.Printf("%s is now %s\n", args[1], resp.Role)
			return nil
		},
	}
	addJSONFlag(cmd)
	return cmd
}

func newMemberRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove <cluster> <email-or-user-id>",
		Short: "Remove a member from a cluster (owner only; idempotent 204)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, usage := dial(cmd, true)
			if usage != "" {
				return failUsage(cmd, usage)
			}
			uid, e := resolveMemberUser(cl, args[0], args[1])
			if e != nil {
				return failAPI(cmd, e)
			}
			path := "/api/clusters/" + url.PathEscape(args[0]) +
				"/members/" + url.PathEscape(uid)
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
