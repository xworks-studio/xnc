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
	// 新版本推送。推送路径（HandlePush）下推送载荷即目标清单（见
	// UpdateAvailable 注释）；轮询路径（6h / 目标版本信号）由 agent 自行
	// 拉取 setup.json 作清单。两路收敛到同一编排。
	TypeUpdateAvailable = "UPDATE_AVAILABLE"
	// UPDATE_AUDIT：agent → server 更新结果审计（update_ok / update_rollback，
	// spec §9.4）。仅经已认证控制连接发送；server 侧记审计日志。
	TypeUpdateAudit = "UPDATE_AUDIT"
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
// 服务端会紧随其后推 UPDATE_AVAILABLE。
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

// UpdateAvailable 服务端 → agent：新版本推送（spec §9.1）。推送路径
// （HandlePush）下载荷即目标清单：URL + SHA256 直接作为下载凭证与信任
// 根——控制通道已经挑战-应答认证，sha256 即真相；URL 须与 server 同源
// （agent 侧强制）。轮询路径则由 agent 自行拉取 setup.json 作清单。
// 字段与 setup.json 对齐。
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

// UpdateStatus 更新阶段上报（agent → server；最终确认由新版 HELLO 上报
// 的版本承担）。
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
