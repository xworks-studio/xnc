package proto

const (
	CodeUnauthorized            = "UNAUTHORIZED"
	CodeForbidden               = "FORBIDDEN"
	CodeClusterNotFound         = "CLUSTER_NOT_FOUND"
	CodeClusterNotEmpty         = "CLUSTER_NOT_EMPTY"
	CodeNodeNotFound            = "NODE_NOT_FOUND"
	CodeNodeOffline             = "NODE_OFFLINE"
	CodeNodeDisabled            = "NODE_DISABLED"
	CodeEnrollmentTokenInvalid  = "ENROLLMENT_TOKEN_INVALID"
	CodeEnrollmentTokenExpired  = "ENROLLMENT_TOKEN_EXPIRED"
	CodeNodeAlreadyEnrolled     = "NODE_ALREADY_ENROLLED"
	// CodeMachineIDConflict：machineId 已注册于其他 cluster（§6.4 用户 JWT 注册
	// 的 409，message 含冲突 cluster 名；与同 cluster 异 key 的
	// NODE_ALREADY_ENROLLED 区分——前者走 --force/管理端，后者是错误）。
	CodeMachineIDConflict = "MACHINE_ID_CONFLICT"
	CodeSessionNotFound         = "SESSION_NOT_FOUND"
	CodeSessionExpired          = "SESSION_EXPIRED"
	CodeSessionLimited          = "SESSION_LIMIT_EXCEEDED"
	CodeShellStartFailed        = "SHELL_START_FAILED"
	CodeKindUnsupported         = "KIND_UNSUPPORTED"
	CodeRdpNotAvailable         = "RDP_NOT_AVAILABLE"
	CodeHashMismatch            = "HASH_MISMATCH"
	CodeFileNotFound            = "FILE_NOT_FOUND"
	CodeFileTooLarge            = "FILE_TOO_LARGE"
	CodeAccessDenied            = "ACCESS_DENIED" // 目标存在但不可写/被占用（如覆盖运行中的 exe）
	CodeAgentVersionUnsupported = "AGENT_VERSION_UNSUPPORTED"
	// CodeRtvUnconfigured：server 无 RTV 配置（XNC_RTV_ENDPOINT）时拒绝
	// desktop 会话（RTV 重构：503 优于开一个必死的会话；TURN 语义随栈退役）。
	// CodeRtvUnconfigured：内嵌模式（XNC_RTV_EMBEDDED=true）下 server 无
	// XNC_RTV_ENDPOINT 配置时拒绝 desktop 会话（RTV 重构：503 优于开一个
	// 必死的会话；TURN 语义随栈退役）。
	CodeRtvUnconfigured = "RTV_UNCONFIGURED"
	// CodeRtvNoRelay：relay-only 形态（XNC_RTV_EMBEDDED=false，2026-09-11
	// 主站缩减默认）下 relay 池无可用中继（全离线/pending/健康探测不过）——
	// 主站不跑媒体，无 relay 即无桌面，503 直报不做内嵌兜底。
	CodeRtvNoRelay = "RTV_NO_RELAY"
	// CodeUserNotFound：按 email/ID 解析目标用户失败（add member 等）。与
	// CodeClusterNotFound 同为 404 存在性语义。
	CodeUserNotFound = "USER_NOT_FOUND"
	// CodeLastAdmin：最后 admin 保护——系统内最后一个 is_admin=true 用户不可
	// 被撤销/自撤/删除（清光 admin 后 EnsureAdmin 不自愈，只剩手工 SQL）。
	CodeLastAdmin = "LAST_ADMIN"
	// CodeUserOwnsNodes：删除用户前置检查——用户名下集群仍挂节点（409），
	// 出路 = 先 move/删除节点（节点是资产，删用户不得连带吞掉注册记录）。
	CodeUserOwnsNodes = "USER_OWNS_NODES"
	CodeInternal      = "INTERNAL"
)

// APIError is the REST error envelope body: {"error":{"code","message"}}.
type APIError struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string { return e.Code + ": " + e.Message }

func Err(status int, code, message string) *APIError {
	return &APIError{Status: status, Code: code, Message: message}
}
