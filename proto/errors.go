package proto

const (
	CodeUnauthorized            = "UNAUTHORIZED"
	CodeForbidden               = "FORBIDDEN"
	CodeClusterNotFound         = "CLUSTER_NOT_FOUND"
	CodeNodeNotFound            = "NODE_NOT_FOUND"
	CodeNodeOffline             = "NODE_OFFLINE"
	CodeNodeDisabled            = "NODE_DISABLED"
	CodeEnrollmentTokenInvalid  = "ENROLLMENT_TOKEN_INVALID"
	CodeEnrollmentTokenExpired  = "ENROLLMENT_TOKEN_EXPIRED"
	CodeNodeAlreadyEnrolled     = "NODE_ALREADY_ENROLLED"
	CodeSessionNotFound         = "SESSION_NOT_FOUND"
	CodeSessionExpired          = "SESSION_EXPIRED"
	CodeSessionLimited          = "SESSION_LIMIT_EXCEEDED"
	CodeShellStartFailed        = "SHELL_START_FAILED"
	CodeKindUnsupported         = "KIND_UNSUPPORTED"
	CodeRdpNotAvailable         = "RDP_NOT_AVAILABLE"
	CodeHashMismatch            = "HASH_MISMATCH"
	CodeFileTooLarge            = "FILE_TOO_LARGE"
	CodeAgentVersionUnsupported = "AGENT_VERSION_UNSUPPORTED"
	CodeInternal                = "INTERNAL"
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
