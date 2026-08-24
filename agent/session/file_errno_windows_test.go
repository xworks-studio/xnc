//go:build windows

package session

import (
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"

	"xnc/proto"
)

// TestFileAccessErrCodeWindows：Windows 特有 errno——rename 覆盖运行中 exe
// 的典型形态（ERROR_ACCESS_DENIED / ERROR_SHARING_VIOLATION）必须映射为
// ACCESS_DENIED。
func TestFileAccessErrCodeWindows(t *testing.T) {
	for _, err := range []error{
		&os.LinkError{Op: "rename", Old: "a.xnc-part", New: "a.exe", Err: syscall.ERROR_ACCESS_DENIED},
		&os.LinkError{Op: "rename", Old: "a.xnc-part", New: "a.exe", Err: errorSharingViolation},
	} {
		assert.Equal(t, proto.CodeAccessDenied, fileAccessErrCode(err), "err=%v", err)
	}
}
