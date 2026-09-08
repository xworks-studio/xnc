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
	TypeUpdateAudit  = "UPDATE_AUDIT"
	TypeUpdateStatus = "UPDATE_STATUS"
	// NODE_DELETE：agent → server 机器自注销（spec §7 deregister）。仅经已认证
	// 控制连接发送——机器身份（挑战-应答签名）即凭据，无 JWT；服务端删除节点
	// 行、逐出该节点全部在线连接并以关闭本连接作为确认（无独立 ack 帧）。
	TypeNodeDelete = "NODE_DELETE"
	// ── relay 控制连接（relay ↔ server，relay-plane spec §3.4）──
	// 与 agent 控制连接同构：持久 WS + ed25519 挑战-应答 + 公钥准入。
	// P1 子集；P2 的 RESOLVE/DRAIN/TICKET_REFRESH 不在本次定义。
	TypeRelayChallengeResponse = "RELAY_CHALLENGE_RESPONSE"
	TypeRelayRegister          = "RELAY_REGISTER"
	TypeRelayHeartbeat         = "RELAY_HEARTBEAT"
	TypeRelayHeartbeatAck      = "RELAY_HEARTBEAT_ACK"
	TypeRelayStats             = "RELAY_STATS"
	TypeRelayReconcile         = "RELAY_RECONCILE"
	TypeRelaySessionKill       = "RELAY_SESSION_KILL"
	// RELAY_CONFIG：server → relay，下行票据验签公钥集合（双窗口轮换：
	// 新旧并存；relay 收到即重建 Verifier）。认证通过后立即下发一次。
	TypeRelayConfig = "RELAY_CONFIG"
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
	// RelayID 仅 relay 控制连接携带：server 在挑战里回传注册身份（首注册
	// 时 relay 尚不知自己被分配的 id；agent 端永远为空）。
	RelayID string `json:"relayId,omitempty"`
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

// ── relay 控制连接载荷（relay-plane spec §2.3/§3.2/§3.4）──

// EndpointDesc 端点描述符（spec §3.2）：候选列表元素。候选 = 同一 relay 的
// 传输变体；transport ∈ quic（host 腿 raw QUIC）/ wt（viewer WT 主路）/
// ws（viewer WS 兜底，仅域名模式）。CertSHA256 仅纯 IP 自签模式携带。
type EndpointDesc struct {
	Transport  string `json:"transport"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	ALPN       string `json:"alpn,omitempty"`
	Path       string `json:"path,omitempty"`
	CertSHA256 string `json:"certSha256,omitempty"`
}

// RelayChallengeResponse 挑战应答（relay 侧；复用 Challenge 的 nonce）。
// 首次注册 RelayID 为空——server 按 PublicKey 做准入判定并分配 ID。
type RelayChallengeResponse struct {
	RelayID   string `json:"relayId,omitempty"`
	Signature []byte `json:"signature"`
}

// RelayRegister 注册/再注册：endpoints 为本 relay 对外暴露的腿（本机视角）；
// capacity 为自愿声明，分配器打分用；ClockUnix 供 server 观测时钟漂移
// （±120s leeway 内不影响分配，超 5m 停止分配并告警）。
type RelayRegister struct {
	RelayID     string         `json:"relayId,omitempty"`
	PublicKey   string         `json:"publicKey"` // ed25519 hex
	Region      string         `json:"region,omitempty"`
	Endpoints   []EndpointDesc `json:"endpoints"`
	MaxSessions int            `json:"maxSessions,omitempty"`
	MaxMbpsOut  int            `json:"maxMbpsOut,omitempty"`
	Version     string         `json:"version,omitempty"`
	ClockUnix   int64          `json:"clockUnix"`
}

type RelayHeartbeat struct {
	ClockUnix int64 `json:"clockUnix"`
}

// RelayStats 10s 一报的负载画像；MbpsOut 是分配打分口径（扇出瓶颈在 egress）。
type RelayStats struct {
	Sessions   int      `json:"sessions"`
	Viewers    int      `json:"viewers"`
	MbpsIn     float64  `json:"mbpsIn"`
	MbpsOut    float64  `json:"mbpsOut"`
	RttP50Ms   float64  `json:"rttP50Ms,omitempty"`
	ActiveSids []string `json:"activeSids,omitempty"` // 在服 viewer 会话键（server 侧代为 Touch：外部 relay 的 viewer 触碰不出进程）
}

// RelayReconcile （重）建立控制连接时的双向对账：relay → server 上报在服
// 会话集（server 重建 sticky/最小会话记录）；server → relay 下发仍在世
// sid 集（relay 把差集记墓碑——server 重启后的孤儿收敛）。
type RelayReconcile struct {
	Live  []RelayLiveSession `json:"live,omitempty"`  // relay → server
	Alive []string           `json:"alive,omitempty"` // server → relay
}

type RelayLiveSession struct {
	SessionID string `json:"sid"`
	NodeID    string `json:"nid"`
	Gen       int    `json:"gen"`
	Viewers   int    `json:"viewers"`
}

// RelayConfig 票据验签公钥集合（server 签名公钥，hex；≥1）。
type RelayConfig struct {
	SigningPubkeys []string `json:"signingPubkeys"`
}

// RelaySessionKill 撤销语义（spec §3.4）：relay 收到后断开该键全部连接并
// 记墓碑——单次断线挡不住可重放票 + 客户端重连循环，墓碑期内一律拒绝。
type RelaySessionKill struct {
	SessionID string `json:"sid,omitempty"`
	NodeID    string `json:"nid,omitempty"`
	Reason    string `json:"reason,omitempty"`
}
