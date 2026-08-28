package api

// desktop_media_select_test.go — M4 Task 4 Step 1（纯选择单测，无 DB/容器）：
// server 侧 per-session mediaProtocol 选择的四条绑定规则——
//
//	① 显式节点 allowlist 胜过百分比（百分比 0 也选 v2）；
//	② 回滚开关把一切新会话钉回 v1（胜过 allowlist 与百分比）；
//	③ 未知/非法输入 fail closed 到 v1（百分比越界）；
//	④ 百分比 = 节点键确定的桶（同一节点稳定，重估才可能变——所以 handler
//	   必须在会话打开时快照一次；live 会话绝不重选）。

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

func TestSelectMediaProtocol(t *testing.T) {
	const node = "11111111-1111-1111-1111-111111111111"
	cases := []struct {
		name string
		roll MediaRollout
		node string
		want string
	}{
		{
			name: "default off: no allowlist, percent 0 -> v1 (fail closed)",
			roll: MediaRollout{}, node: node, want: proto.MediaProtocolV1,
		},
		{
			name: "allowlist beats percentage: listed node gets v2 even at percent 0",
			roll: MediaRollout{Percent: 0, Allowlist: []string{node}}, node: node,
			want: proto.MediaProtocolV2,
		},
		{
			name: "allowlist match is case-insensitive (env-entered UUIDs)",
			roll: MediaRollout{Allowlist: []string{"AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA"}},
			node: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", want: proto.MediaProtocolV2,
		},
		{
			name: "allowlist does not match other nodes",
			roll: MediaRollout{Allowlist: []string{"22222222-2222-2222-2222-222222222222"}},
			node: node, want: proto.MediaProtocolV1,
		},
		{
			name: "rollback sets all new sessions to v1 (beats allowlist)",
			roll: MediaRollout{Rollback: true, Percent: 100, Allowlist: []string{node}},
			node: node, want: proto.MediaProtocolV1,
		},
		{
			name: "percent 100 -> v2 for every node (full canary on)",
			roll: MediaRollout{Percent: 100}, node: node, want: proto.MediaProtocolV2,
		},
		{
			name: "percent below range fails closed to v1",
			roll: MediaRollout{Percent: -1}, node: node, want: proto.MediaProtocolV1,
		},
		{
			name: "percent above range fails closed to v1",
			roll: MediaRollout{Percent: 101}, node: node, want: proto.MediaProtocolV1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, selectMediaProtocol(tc.roll, tc.node))
		})
	}
}

// TestSelectMediaProtocolBucketSplits：百分比桶对节点集合可分（50% 既能选
// v2 也能选 v1），且逐节点确定性——同一节点重复评估结果不变（handler 因此
// 得到稳定的快照语义）。
func TestSelectMediaProtocolBucketSplits(t *testing.T) {
	var v2Node, v1Node string
	for i := 0; i < 1000 && (v2Node == "" || v1Node == ""); i++ {
		id := fmt.Sprintf("00000000-0000-0000-0000-%012d", i)
		switch selectMediaProtocol(MediaRollout{Percent: 50}, id) {
		case proto.MediaProtocolV2:
			if v2Node == "" {
				v2Node = id
			}
		case proto.MediaProtocolV1:
			if v1Node == "" {
				v1Node = id
			}
		}
	}
	require.NotEmpty(t, v2Node, "50 percent rollout must select v2 for some nodes")
	require.NotEmpty(t, v1Node, "50 percent rollout must select v1 for some nodes")
	// 确定性：同节点重估不变（快照语义的前提）。
	assert.Equal(t, proto.MediaProtocolV2, selectMediaProtocol(MediaRollout{Percent: 50}, v2Node))
	assert.Equal(t, proto.MediaProtocolV1, selectMediaProtocol(MediaRollout{Percent: 50}, v1Node))
	// 桶键与大小写无关（节点 UUID 规范形态小写；env 输入可能大写）。
	assert.Equal(t,
		selectMediaProtocol(MediaRollout{Percent: 50}, v2Node),
		selectMediaProtocol(MediaRollout{Percent: 50}, strings.ToUpper(v2Node)))
}

// TestSelectMediaProtocolReselectChanges：配置变更只影响**新**会话——同节点
// 在百分比 0→100 之间重估会翻转，这正是 handler 必须在会话打开时一次性快照
// （params 不可变）的原因；live 会话的版本钉子不受后续配置变化影响。
func TestSelectMediaProtocolReselectChanges(t *testing.T) {
	const node = "33333333-3333-3333-3333-333333333333"
	assert.Equal(t, proto.MediaProtocolV1, selectMediaProtocol(MediaRollout{Percent: 0}, node))
	assert.Equal(t, proto.MediaProtocolV2, selectMediaProtocol(MediaRollout{Percent: 100}, node))
	// 回滚开关同样只作用于重估（= 新会话）。
	assert.Equal(t, proto.MediaProtocolV1,
		selectMediaProtocol(MediaRollout{Percent: 100, Rollback: true}, node))
}
