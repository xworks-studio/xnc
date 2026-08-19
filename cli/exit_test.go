package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"xnc/proto"
)

func TestExitCode(t *testing.T) {
	cases := []struct {
		e    *proto.APIError
		want int
	}{
		{nil, 0},
		{proto.Err(401, proto.CodeUnauthorized, ""), 240},
		{proto.Err(403, proto.CodeForbidden, ""), 241},
		{proto.Err(409, proto.CodeNodeOffline, ""), 242},
		{proto.Err(404, proto.CodeNodeNotFound, ""), 244},
		{proto.Err(0, "NETWORK", ""), 245}, // 网络错误约定 code=NETWORK
		{proto.Err(500, proto.CodeInternal, ""), 250},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, ExitCode(c.e))
	}
}
