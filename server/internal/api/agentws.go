package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"xnc/proto"
	"xnc/server/internal/db/sqlc"
	"xnc/server/internal/registry"
)

const challengeTTL = 60 * time.Second

func (h *handlers) agentConnect(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	// 无论从哪条路径退出（认证失败、被顶替、读错误、超时）都关掉底层连接。
	defer c.CloseNow()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// 1. 下发挑战
	nonceB := make([]byte, 32)
	if _, err := rand.Read(nonceB); err != nil {
		_ = c.CloseNow()
		return
	}
	nonce := base64.RawURLEncoding.EncodeToString(nonceB)
	h.writeJSON(ctx, c, proto.TypeChallenge, proto.Challenge{Nonce: nonce})

	// 2. 读 CHALLENGE_RESPONSE（挑战 60s 内有效）
	m, ok := h.readMsg(ctx, c, challengeTTL)
	if !ok || m.Type != proto.TypeChallengeResponse {
		_ = c.CloseNow()
		return
	}
	var cr proto.ChallengeResponse
	if err := m.Decode(&cr); err != nil {
		_ = c.CloseNow()
		return
	}
	nodeID, err := uuid.Parse(cr.NodeID)
	if err != nil {
		_ = c.CloseNow()
		return
	}
	node, err := h.st.Q().GetNodeByID(ctx, nodeID)
	if err != nil {
		_ = c.CloseNow()
		return
	}
	pub, err := base64.StdEncoding.DecodeString(node.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize ||
		!ed25519.Verify(ed25519.PublicKey(pub), []byte(nonce), cr.Signature) {
		slog.Warn("agent auth failed", "node", cr.NodeID)
		_ = c.CloseNow()
		return
	}

	// 3. HELLO
	m, ok = h.readMsg(ctx, c, challengeTTL)
	if !ok || m.Type != proto.TypeHello {
		_ = c.CloseNow()
		return
	}
	var hello proto.Hello
	if err := m.Decode(&hello); err != nil || hello.NodeID != node.ID.String() {
		_ = c.CloseNow()
		return
	}
	_ = h.st.Q().UpdateNodeMeta(ctx, sqlc.UpdateNodeMetaParams{
		Hostname: hello.Hostname, ShellType: hello.ShellType,
		AgentVersion: hello.AgentVersion, ID: node.ID,
	})
	_ = h.st.Q().SetNodeStatus(ctx, sqlc.SetNodeStatusParams{ID: node.ID, Status: "online"})
	_ = h.st.Q().TouchNode(ctx, node.ID)

	conn := &registry.NodeConn{NodeID: node.ID.String(), Cancel: cancel, LastBeat: time.Now()}
	// 控制连接写串行化：心跳 ACK 与 SESSION_OPEN/SESSION_CLOSE 共用此发送器，
	// 互斥锁防止并发写交叉破坏帧边界（coder/websocket 不允许并发 Writer）。
	// T4 的 exec handler 经 conn.Send 下发会话消息。
	var wmu sync.Mutex
	sendControl := func(m proto.Message) error {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		wmu.Lock()
		defer wmu.Unlock()
		return c.Write(ctx, websocket.MessageText, b)
	}
	conn.Send = sendControl
	h.reg.Add(conn)
	defer func() {
		// identity-aware 清理：仅当本连接仍是该节点的当前连接时才落 offline/last_seen。
		// 若已被新连接顶替（RemoveIf 返回 false），在线状态由新连接接管，此处什么都不做，
		// 避免把仍在心跳的节点误翻 offline（fix round 1）。
		if !h.reg.RemoveIf(node.ID.String(), conn) {
			return
		}
		_ = h.st.Q().SetNodeStatus(context.Background(),
			sqlc.SetNodeStatusParams{ID: node.ID, Status: "offline"})
		_ = h.st.Q().TouchNode(context.Background(), node.ID)
	}()

	// 快速版本检查 ①：HELLO_ACK 回执携带目标版本；握手后版本落后则立即
	// 下发 UPDATE_OFFER（重连/重启/开机场景秒级感知）。
	targetVer := ""
	if rel, ok := h.targetReleaseFor(ctx, node.ID); ok {
		targetVer = rel.Version
	}
	h.writeJSON(ctx, c, proto.TypeHelloAck, proto.HelloAck{TargetVersion: targetVer})
	if targetVer != "" && targetVer != hello.AgentVersion {
		h.maybeOfferUpdate(ctx, node.ID, hello.AgentVersion, sendControl)
	}

	// 4. 心跳循环：每条消息读超时 = HeartbeatTimeout
	for {
		m, ok := h.readMsg(ctx, c, h.cfg.HeartbeatTimeout)
		if !ok {
			return
		}
		switch m.Type {
		case proto.TypeHeartbeat:
			conn.LastBeat = time.Now()
			if conn.Beats%10 == 0 {
				_ = h.st.Q().TouchNode(ctx, node.ID)
			}
			conn.Beats++
			// 快速版本检查 ②：PING/PONG 搭车——agent 报当前版本，ACK
			// 回目标版本；版本落后即推 OFFER（灰度改 pin 后一个保活周
			// 期内全网感知，零新增消息类型）。targetVer 每拍重解析:
			// 握手时缓存的值在 pin 变更后对长连接永远过期(2026-08-24
			// 生产事故:改 pin 后 agent 死循环在旧 target)。
			var hb proto.Heartbeat
			_ = m.Decode(&hb)
			if rel, ok := h.targetReleaseFor(ctx, node.ID); ok {
				targetVer = rel.Version
			}
			if hb.Version != "" && targetVer != "" && hb.Version != targetVer {
				h.maybeOfferUpdate(ctx, node.ID, hb.Version, sendControl)
			}
			// ACK 经串行化发送器（与 SESSION_OPEN/CLOSE 单一写路径）
			ack, _ := proto.NewMsg(proto.TypeHeartbeatAck, proto.HeartbeatAck{TargetVersion: targetVer})
			_ = sendControl(ack)
		case proto.TypeUpdateAudit:
			// agent 更新结果审计（spec §9.4：update_ok / update_rollback）。
			// 最小接受：记审计日志（DB 化留给后续任务，spec §9 未定义表）。
			var ua proto.UpdateAudit
			if m.Decode(&ua) == nil {
				slog.Info("agent update audit", "node", node.ID, "event", ua.Event,
					"from", ua.From, "to", ua.To, "reason", ua.Reason)
			}
		case proto.TypeUpdateStatus:
			// 遗留 bundle agent 阶段上报（spec §14 迁移期；最终确认 = 新版
			// HELLO 版本）。
			var us proto.UpdateStatus
			if m.Decode(&us) == nil {
				slog.Info("agent update status (legacy)", "node", node.ID, "version", us.Version,
					"phase", us.Phase, "err", us.Error)
			}
		case proto.TypeSessionRefused:
			// agent 能力协商失败（如 kind 不支持）：消费为会话关闭，
			// reason 前缀 refused: 保留 agent 侧错误码。
			var sr proto.SessionRefused
			if err := m.Decode(&sr); err == nil && h.sess != nil {
				h.sess.NotifyClose(sr.SessionID, "refused:"+sr.Code)
			}
		case proto.TypeNodeDelete:
			// 机器自注销（spec §7 deregister）：本连接已按节点身份完成挑战-验签，
			// 机器身份即凭据（无 JWT）。删除节点行 → 审计 → 逐出该节点全部在线
			// 连接 → 关闭本连接（关闭即确认；失败先发 ERROR 帧再关闭）。
			if err := h.nodeDelete(ctx, node); err != nil {
				slog.Warn("node deregister failed", "node", node.ID, "err", err)
				h.writeJSON(ctx, c, proto.TypeError, proto.ErrorPayload{
					Code: proto.CodeInternal, Message: "deregister failed"})
			}
			_ = c.Close(websocket.StatusNormalClosure, "node deleted")
			return
		default:
			h.writeJSON(ctx, c, proto.TypeError, proto.ErrorPayload{
				Code: proto.CodeInternal, Message: "unexpected message"})
		}
	}
}

func (h *handlers) writeJSON(ctx context.Context, c *websocket.Conn, typ string, payload any) {
	m, err := proto.NewMsg(typ, payload)
	if err != nil {
		return
	}
	b, _ := json.Marshal(m)
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_ = c.Write(wctx, websocket.MessageText, b)
}

func (h *handlers) readMsg(ctx context.Context, c *websocket.Conn, timeout time.Duration) (proto.Message, bool) {
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	typ, data, err := c.Read(rctx)
	if err != nil {
		return proto.Message{}, false
	}
	// 协议绑定（design §2.1）：控制连接只接受文本帧，二进制帧立即断开。
	if typ != websocket.MessageText {
		return proto.Message{}, false
	}
	var m proto.Message
	if err := json.Unmarshal(data, &m); err != nil {
		return proto.Message{}, false
	}
	return m, true
}

// nodeDelete 处理已认证控制连接上的 NODE_DELETE（spec §7 机器自注销）：
// 删除 0 行（节点已不存在）按幂等成功处理。审计 actor 为空（机器发起，
// 无 user_id）。必须在取消本连接 ctx 之前完成 DB 操作——逐出在线连接会
// Cancel 该节点（可能就是本连接）的 handler ctx。
func (h *handlers) nodeDelete(ctx context.Context, node sqlc.Node) error {
	if _, err := h.st.Q().DeleteNode(ctx, node.ID); err != nil {
		return err
	}
	if err := h.st.Q().InsertAuditLog(ctx, sqlc.InsertAuditLogParams{
		ClusterID: pgtype.UUID{Bytes: node.ClusterID, Valid: true},
		NodeID:    pgtype.UUID{Bytes: node.ID, Valid: true},
		Action:    "node.deregister", Metadata: []byte("{}"),
	}); err != nil {
		// 节点已删，审计失败不回滚注销（只记日志）。
		slog.Warn("deregister audit failed", "node", node.ID, "err", err)
	}
	// 逐出该节点的在线连接（可能是另一条更早的活跃连接，也可能是本连接）：
	// 先移除注册表项再 Cancel——被逐连接的延迟清理经 RemoveIf=false 跳过
	// 对已删节点的 offline 落库。
	if live := h.reg.Get(node.ID.String()); live != nil && live.Cancel != nil {
		h.reg.Remove(node.ID.String())
		live.Cancel()
	}
	return nil
}
