package main

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"xnc/proto"
)

// nodeRef is the node identity resolveNode returns: enough for commands
// (node show, exec, later run) to address the node and label their output.
type nodeRef struct {
	ID      string
	Name    string
	Cluster string
}

var uuidRe = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// resolveNode: UUIDs pass through (no list round trip, Name/Cluster empty);
// other args are matched against node names via the list endpoint. Ambiguous
// names error with the candidates listed (sorted by cluster then id) so
// callers need no extra node-list round trip.
func resolveNode(cl *Client, arg string) (nodeRef, *proto.APIError) {
	if uuidRe.MatchString(arg) {
		return nodeRef{ID: arg}, nil
	}
	nodes, e := fetchNodes(cl, "/api/nodes")
	if e != nil {
		return nodeRef{}, e
	}
	var matches []nodeDTO
	for _, n := range nodes {
		if n.Name == arg {
			matches = append(matches, n)
		}
	}
	switch len(matches) {
	case 1:
		n := matches[0]
		return nodeRef{ID: n.ID, Name: n.Name, Cluster: n.Cluster}, nil
	case 0:
		return nodeRef{}, proto.Err(404, proto.CodeNodeNotFound, "no node named "+arg)
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Cluster != matches[j].Cluster {
			return matches[i].Cluster < matches[j].Cluster
		}
		return matches[i].ID < matches[j].ID
	})
	cands := make([]string, len(matches))
	for i, m := range matches {
		cands[i] = fmt.Sprintf("%s (%s, %s)", m.Name, m.Cluster, m.ID)
	}
	return nodeRef{}, proto.Err(404, proto.CodeNodeNotFound,
		"ambiguous node "+strconv.Quote(arg)+": "+strings.Join(cands, ", ")+
			"; use a UUID or cluster/name")
}
