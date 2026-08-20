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

	h.writeJSON(ctx, c, proto.TypeHelloAck, struct{}{})

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
			// ACK 经串行化发送器（与 SESSION_OPEN/CLOSE 单一写路径）
			ack, _ := proto.NewMsg(proto.TypeHeartbeatAck, struct{}{})
			_ = sendControl(ack)
		case proto.TypeSessionRefused:
			// agent 能力协商失败（如 kind 不支持）：消费为会话关闭，
			// reason 前缀 refused: 保留 agent 侧错误码。
			var sr proto.SessionRefused
			if err := m.Decode(&sr); err == nil && h.sess != nil {
				h.sess.NotifyClose(sr.SessionID, "refused:"+sr.Code)
			}
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
