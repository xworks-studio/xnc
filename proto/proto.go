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
	// 自更新（agent 拉取 bundle 的指令走已认证控制通道，内容走 HTTPS）。
	TypeUpdateOffer  = "UPDATE_OFFER"
	TypeUpdateStatus = "UPDATE_STATUS"
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
	NodeID       string   `json:"nodeId"`
	Hostname     string   `json:"hostname"`
	AgentVersion string   `json:"agentVersion"`
	ShellType    string   `json:"shellType"`
	Shells       []string `json:"shells,omitempty"` // 可用 shell 列表（bash/pwsh/powershell/cmd）
}

// HelloAck 携带目标版本（快速版本检查三通道之一）：agent 比对后若落后，
// 服务端会紧随其后推 UPDATE_OFFER。
type HelloAck struct {
	TargetVersion string `json:"targetVersion,omitempty"`
}

// Heartbeat 搭车上报当前版本（可选，省独立轮询）。
type Heartbeat struct {
	Version string `json:"version,omitempty"`
}

// HeartbeatAck 搭车下发目标版本——灰度改 pin 后一个保活周期内全网感知。
type HeartbeatAck struct {
	TargetVersion string `json:"targetVersion,omitempty"`
}

// UpdateOffer 服务端 → agent：目标版本与下载凭证。URL 携带短时效单次
// 令牌（绑定节点）；SHA256 为信任根（控制通道已认证，哈希即真相）。
type UpdateOffer struct {
	Version   string `json:"version"`
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

// UpdateStatus agent → 服务端：更新阶段上报（apply 后的最终确认由新版
// agent 的 HELLO 版本承担）。
const (
	UpdatePhaseDownloading = "downloading"
	UpdatePhaseVerifying   = "verifying"
	UpdatePhaseStaging     = "staging"
	UpdatePhaseApplying    = "applying"
	UpdatePhaseDone        = "done"
	UpdatePhaseFailed      = "failed"
)

type UpdateStatus struct {
	Version string `json:"version"`
	Phase   string `json:"phase"`
	Error   string `json:"error,omitempty"`
}

type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
