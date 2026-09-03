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
	// CodeTurnUnconfigured：server 无 TURN 配置时拒绝 desktop 会话
	// （M1-Slice2：relay-only 无 TURN 不可用，503 优于开一个必死的会话）。
	CodeTurnUnconfigured = "TURN_UNCONFIGURED"
	CodeInternal         = "INTERNAL"
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
