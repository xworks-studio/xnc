// Package proto is the single source of truth for the XNC v2 wire protocol.
package proto

import "encoding/json"

type Message struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

const (
	TypeChallenge         = "CHALLENGE"
	TypeChallengeResponse = "CHALLENGE_RESPONSE"
	TypeHello             = "HELLO"
	TypeHelloAck          = "HELLO_ACK"
	TypeHeartbeat         = "HEARTBEAT"
	TypeHeartbeatAck      = "HEARTBEAT_ACK"
	TypeError             = "ERROR"
	// Phase 2+ 会话消息，协议现在锁定。
	TypeSessionOpen    = "SESSION_OPEN"
	TypeSessionRefused = "SESSION_REFUSED"
	TypeSessionClose   = "SESSION_CLOSE"
)

func NewMsg(typ string, payload any) (Message, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return Message{}, err
	}
	return Message{Type: typ, Payload: b}, nil
}

func (m Message) Decode(v any) error { return json.Unmarshal(m.Payload, v) }

type Challenge struct {
	Nonce string `json:"nonce"`
}

type ChallengeResponse struct {
	NodeID    string `json:"nodeId"`
	Signature []byte `json:"signature"`
}

type Hello struct {
	NodeID       string `json:"nodeId"`
	Hostname     string `json:"hostname"`
	AgentVersion string `json:"agentVersion"`
	ShellType    string `json:"shellType"`
}

type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
