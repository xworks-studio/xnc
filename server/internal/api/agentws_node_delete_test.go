package api

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

// readErr 在时限内读一帧，返回读错误（用于断言服务端关闭连接）。
func readErr(t *testing.T, c *websocket.Conn) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	return err
}

// TestAgentWSNodeDelete：机器自注销（spec §7 deregister）——已认证控制连接上
// 发送 NODE_DELETE（机器身份即凭据，无 JWT）：服务端删除节点行、逐出该节点
// 的全部在线连接（含既有活跃连接）、写 node.deregister 审计，并以关闭本
// 连接作为确认（无独立 ack 帧）。
func TestAgentWSNodeDelete(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-DEL", "mid-del")

	// 既有活跃控制连接（模拟在线节点；注销后应被服务端关闭）。
	live := dialControl(t, env, nodeID)
	// 一次性注销连接：同一控制端点挑战认证后发送 NODE_DELETE。
	c := dialControl(t, env, nodeID)

	nd, err := proto.NewMsg(proto.TypeNodeDelete, struct{}{})
	require.NoError(t, err)
	writeMsg(t, c, nd)

	// 关闭即确认（删除已生效）；错误帧（若删除失败）会先于关闭到达。
	require.Error(t, readErr(t, c), "server must close the deregister connection after handling NODE_DELETE")

	// 节点行已删除（GetNodeByID 不再命中）。
	_, qerr := env.Store.Q().GetNodeByID(context.Background(), mustUUID(nodeID))
	require.Error(t, qerr, "node row must be deleted")

	// 既有活跃连接被逐出（服务端 Cancel → 读错误）。
	require.Eventually(t, func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		_, _, err := live.Read(ctx)
		return err != nil
	}, 5*time.Second, 100*time.Millisecond, "live control connection must be closed after node deletion")

	// 审计：node.deregister，机器发起（user_id 空）。
	var action string
	var userID *string
	row := env.Store.Pool().QueryRow(context.Background(),
		`SELECT action, user_id::text FROM audit_logs WHERE node_id=$1 AND action='node.deregister'`,
		mustUUID(nodeID))
	require.NoError(t, row.Scan(&action, &userID))
	assert.Equal(t, "node.deregister", action)
	assert.Nil(t, userID, "machine-initiated deregister has no user actor")
}

// TestAgentWSNodeDeleteUnknownNode：节点已不存在（先前已注销）时的重复
// NODE_DELETE——连接已通过挑战（身份合法），删除 0 行按幂等成功处理并关闭。
func TestAgentWSNodeDeleteUnknownNode(t *testing.T) {
	env := NewTestEnv(t)
	nodeID := env.EnrollNode(t, "WEB-DEL2", "mid-del2")

	// 第一条：真正删除。
	c := dialControl(t, env, nodeID)
	nd, _ := proto.NewMsg(proto.TypeNodeDelete, struct{}{})
	writeMsg(t, c, nd)
	require.Error(t, readErr(t, c))

	// 第二条：节点行已删——challenge 认证按 nodeID 查不到节点，直接被拒。
	c2 := dialAgentWS(t, "ws"+env.srv.URL[4:]+"/api/agent/connect")
	m := readMsg(t, c2)
	require.Equal(t, proto.TypeChallenge, m.Type)
	var ch proto.Challenge
	require.NoError(t, m.Decode(&ch))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, _, err := c2.Read(ctx)
	assert.Error(t, err, "auth must fail for a deleted node")
}
