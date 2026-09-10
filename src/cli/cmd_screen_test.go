package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

// xnc screen 已整体退役：任何参数形态都得到清晰退役提示（exit 2 用法错误
// 路径），而非 unknown command 或发起必败会话。旧 flags 保留仅作兼容。
func TestScreenCmdRetired(t *testing.T) {
	for _, args := range [][]string{
		{"screen", "web-01"},
		{"screen", "web-01", "--open"},
		{"screen", "web-01", "--snapshot", "out.jpg"},
	} {
		code := runCLI(context.Background(), args)
		assert.Equal(t, exitUsage, code, "args %v should exit with usage error", args)
	}
}
