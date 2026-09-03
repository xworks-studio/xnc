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
	// 自更新（spec §9，安装器即更新器）。UPDATE_AVAILABLE：server → agent
	// 新版本推送——仅是"立即检查一轮"的提示，setup.json 才是清单权威
	// （版本/url/sha256 以 agent 自行拉取的 setup.json 为准）。
	TypeUpdateAvailable = "UPDATE_AVAILABLE"
	// UPDATE_AUDIT：agent → server 更新结果审计（update_ok / update_rollback，
	// spec §9.4）。仅经已认证控制连接发送；server 侧记审计日志。
	TypeUpdateAudit = "UPDATE_AUDIT"
	// 遗留 bundle 更新通道（spec §14 迁移期）：仅存量 bundle agent 认识；
	// 新 agent 对 UPDATE_OFFER 前向兼容忽略，存量 agent 对
	// UPDATE_AVAILABLE 同样忽略。bundle agent 全网切换完成后删除。
	TypeUpdateOffer  = "UPDATE_OFFER"
	TypeUpdateStatus = "UPDATE_STATUS"
	// NODE_DELETE：agent → server 机器自注销（spec §7 deregister）。仅经已认证
	// 控制连接发送——机器身份（挑战-应答签名）即凭据，无 JWT；服务端删除节点
	// 行、逐出该节点全部在线连接并以关闭本连接作为确认（无独立 ack 帧）。
	TypeNodeDelete = "NODE_DELETE"
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

// UpdateAvailable 服务端 → agent：新版本推送提示（spec §9.1）。字段与
// setup.json 对齐；agent 收到后立即拉取 setup.json 复核（哈希即真相，
// 推送仅是触发信号，不作为下载凭证）。
type UpdateAvailable struct {
	Version string `json:"version"`
	URL     string `json:"url"` // /setup.exe?channel=<ch>（相对路径）
	SHA256  string `json:"sha256"`
}

// UpdateAudit agent → server：更新结果审计（spec §9.4）。事件二值：
// 更新完成（自检通过）/ 回滚（自检失败、安装器失败、看门狗兜底）。
const (
	UpdateEventOK       = "update_ok"
	UpdateEventRollback = "update_rollback"
)

type UpdateAudit struct {
	Event  string `json:"event"` // update_ok | update_rollback
	From   string `json:"from"`
	To     string `json:"to"`
	Reason string `json:"reason,omitempty"` // 回滚原因
}

// ---- 遗留 bundle 更新通道载荷（spec §14 迁移期，随 UPDATE_OFFER 退役）----

// UpdateOffer 遗留 bundle 下载凭证（短时效单次令牌，绑定节点）。
type UpdateOffer struct {
	Version   string `json:"version"`
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

// UpdateStatus 遗留阶段上报（bundle agent → server；最终确认由新版
// HELLO 版本承担）。
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
