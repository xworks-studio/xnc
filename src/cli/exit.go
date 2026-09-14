package main

import "xnc/proto"

// Exit codes: 0 ok, 2 usage; 240+ are the stable Agent-First API failure codes.
const (
	exitOK       = 0
	exitUsage    = 2
	exitAuth     = 240 // UNAUTHORIZED, ENROLLMENT_TOKEN_INVALID
	exitForbid   = 241 // FORBIDDEN
	exitOffline  = 242 // NODE_OFFLINE
	exitTimeout  = 243 // EXEC_RESULT with timedOut and no exit code
	exitMissing  = 244 // CLUSTER_NOT_FOUND, NODE_NOT_FOUND, NODE_ALREADY_ENROLLED, MACHINE_ID_CONFLICT, FILE_NOT_FOUND
	exitNet      = 245 // NETWORK (client-side network failure)
	exitQuota    = 246 // SESSION_LIMIT_EXCEEDED, FILE_TOO_LARGE, HASH_MISMATCH
	exitRejected = 247 // EXEC_RESULT with a stable rejection code (node refused to run: CORE_UNAVAILABLE, BAD_PAYLOAD, ...)
	exitInternal = 250 // everything else
)

// ExitCode maps an APIError to the stable process exit code; nil is success.
func ExitCode(e *proto.APIError) int {
	if e == nil {
		return exitOK
	}
	switch e.Code {
	case proto.CodeUnauthorized, proto.CodeEnrollmentTokenInvalid:
		return exitAuth
	case proto.CodeForbidden:
		return exitForbid
	case proto.CodeNodeOffline:
		return exitOffline
	case proto.CodeSessionLimited, proto.CodeFileTooLarge, proto.CodeHashMismatch:
		return exitQuota
	case proto.CodeClusterNotFound, proto.CodeNodeNotFound, proto.CodeNodeAlreadyEnrolled,
		proto.CodeMachineIDConflict, proto.CodeFileNotFound:
		return exitMissing
	case "NETWORK":
		return exitNet
	case "USAGE":
		return exitUsage // client-side validation error（upload 超限等）
	default:
		return exitInternal
	}
}
