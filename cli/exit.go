package main

import "xnc/proto"

// Exit codes: 0 ok, 2 usage; 240+ are the stable Agent-First API failure codes.
const (
	exitOK       = 0
	exitUsage    = 2
	exitAuth     = 240 // UNAUTHORIZED, ENROLLMENT_TOKEN_INVALID
	exitForbid   = 241 // FORBIDDEN
	exitOffline  = 242 // NODE_OFFLINE
	exitTimeout  = 243 // reserved: command timeout
	exitMissing  = 244 // CLUSTER_NOT_FOUND, NODE_NOT_FOUND, NODE_ALREADY_ENROLLED
	exitNet      = 245 // NETWORK (client-side network failure)
	exitQuota    = 246 // reserved: quota
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
	case proto.CodeClusterNotFound, proto.CodeNodeNotFound, proto.CodeNodeAlreadyEnrolled:
		return exitMissing
	case "NETWORK":
		return exitNet
	default:
		return exitInternal
	}
}
