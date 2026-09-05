package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
)

func TestEscapeDetector(t *testing.T) {
	cases := []struct {
		name       string
		in         []byte
		want       string
		wantEscape bool
	}{
		{"plain typing", []byte("echo hi\r"), "echo hi\r", false},
		{"tilde mid-line is passed through", []byte("a~b\r"), "a~b\r", false},
		{"escape at line start", []byte("~."), "", true},
		{"escape after newline", []byte("echo hi\r\n~."), "echo hi\r\n", true},
		{"tilde then other char passes both", []byte("~x\r"), "~x\r", false},
		{"double tilde emits one literal tilde", []byte("~~."), "~.", false},
		{"bare tilde + Enter disconnects", []byte("~\r"), "", true},
		{"bare tilde + LF disconnects", []byte("~\n"), "", true},
		{"tilde then path char passes both", []byte("~/dir\r"), "~/dir\r", false},
		// 全角变体（中文 IME）：～ = EF BD 9E，． = EF BC 8E，。 = E3 80 82
		{"fullwidth tilde + ascii dot", append([]byte{0xEF, 0xBD, 0x9E}, '.'), "", true},
		{"fullwidth tilde + fullwidth dot", []byte{0xEF, 0xBD, 0x9E, 0xEF, 0xBC, 0x8E}, "", true},
		{"fullwidth tilde + CJK dot", []byte{0xEF, 0xBD, 0x9E, 0xE3, 0x80, 0x82}, "", true},
		{"ascii tilde + fullwidth dot", []byte{'~', 0xEF, 0xBC, 0x8E}, "", true},
		{"fullwidth tilde + Enter disconnects", append([]byte{0xEF, 0xBD, 0x9E}, '\r'), "", true},
		{"fullwidth tilde then text passes through", append(append([]byte{}, 0xEF, 0xBD, 0x9E), 'x'), string([]byte{0xEF, 0xBD, 0x9E, 'x'}), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newEscapeDetector()
			got, esc := d.feed(c.in)
			assert.Equal(t, c.want, string(got))
			assert.Equal(t, c.wantEscape, esc)
		})
	}
}

// win32 键盘增强协议序列（取自真实 stdin 转储）。
const (
	seqTildeDown = "\x1b[192;41;126;1;48;1_" // Shift+` → ~  按下
	seqTildeUp   = "\x1b[192;41;96;0;32;1_"  // ~ 抬起（无 shift，char 回落 96）
	seqShiftUp   = "\x1b[16;42;0;0;32;1_"
	seqDotDown   = "\x1b[190;52;46;1;32;1_"  // . 按下
	seqEnterDown = "\x1b[13;28;13;1;32;1_"
	seqEnterUp   = "\x1b[13;28;13;0;32;1_"
	seqEDown     = "\x1b[69;18;101;1;32;1_" // e 按下
	seqEUp       = "\x1b[69;18;101;0;32;1_"
)

func TestEscapeDetectorWin32Protocol(t *testing.T) {
	t.Run("~. via key sequences disconnects and swallows only the escape keys", func(t *testing.T) {
		d := newEscapeDetector()
		var out []byte
		var esc bool
		for _, chunk := range []string{seqTildeDown, seqShiftUp, seqTildeUp, seqDotDown} {
			var o []byte
			o, esc = d.feed([]byte(chunk))
			out = append(out, o...)
		}
		require.True(t, esc)
		// tilde-down 被悬置（断开成立则吞掉），shift-up / tilde-up 原样放行
		assert.Equal(t, seqShiftUp+seqTildeUp, string(out))
	})

	t.Run("bare ~ + Enter via sequences disconnects", func(t *testing.T) {
		d := newEscapeDetector()
		_, _ = d.feed([]byte(seqTildeDown))
		_, _ = d.feed([]byte(seqTildeUp))
		_, esc := d.feed([]byte(seqEnterDown))
		require.True(t, esc)
	})

	t.Run("typing via sequences passes through unchanged", func(t *testing.T) {
		d := newEscapeDetector()
		out, esc := d.feed([]byte(seqEDown + seqEUp + seqEnterDown + seqEnterUp))
		require.False(t, esc)
		assert.Equal(t, seqEDown+seqEUp+seqEnterDown+seqEnterUp, string(out))
	})

	t.Run("~ then e flushes held and forwards all", func(t *testing.T) {
		d := newEscapeDetector()
		var out []byte
		for _, chunk := range []string{seqTildeDown, seqTildeUp, seqEDown} {
			o, _ := d.feed([]byte(chunk))
			out = append(out, o...)
		}
		assert.Equal(t, seqTildeDown+seqTildeUp+seqEDown, string(out))
	})

	t.Run("exact dump sequence: shift-down + tilde-down + tilde-up + shift-up + enter", func(t *testing.T) {
		// dump 里用户 ~+Enter 的真实序列（shift 修饰在 tilde 之前按下）
		d := newEscapeDetector()
		out1, esc := d.feed([]byte("\x1b[16;42;0;1;48;1_" + seqTildeDown))
		require.False(t, esc) // shift-down 放行，tilde-down 悬置
		assert.Equal(t, "\x1b[16;42;0;1;48;1_", string(out1))
		out2, esc := d.feed([]byte(seqTildeUp))
		require.False(t, esc)
		assert.Empty(t, out2) // 悬置期积压
		out3, esc := d.feed([]byte(seqShiftUp))
		require.False(t, esc)
		assert.Empty(t, out3)
		out4, esc := d.feed([]byte(seqEnterDown))
		require.True(t, esc) // 裸 ~ + 回车 → 断开，积压放行
		assert.Equal(t, seqTildeUp+seqShiftUp, string(out4))
	})
}

func TestEscapeDetectorAcrossChunks(t *testing.T) {
	d := newEscapeDetector()
	out1, esc := d.feed([]byte("ls\r\n~"))
	require.False(t, esc)
	assert.Equal(t, "ls\r\n", string(out1))
	out2, esc := d.feed([]byte("."))
	require.True(t, esc)
	assert.Empty(t, out2)
}

func TestRunLoginFlowRetryThenSuccess(t *testing.T) {
	var calls int
	answers := []string{"wrong", "right"}
	deps := loginDeps{
		promptEmail: func() string { return "u@example.com" },
		promptPassword: func() (string, error) {
			a := answers[0]
			answers = answers[1:]
			return a, nil
		},
		doLogin: func(server, email, password string) (string, userDTO, *proto.APIError) {
			calls++
			if password == "wrong" {
				return "", userDTO{}, proto.Err(401, proto.CodeUnauthorized, "invalid credentials")
			}
			return "tok", userDTO{Email: email}, nil
		},
	}
	server, tok, user, err := runLoginFlow(deps, "https://s.example", "")
	require.NoError(t, err)
	assert.Equal(t, "https://s.example", server)
	assert.Equal(t, "tok", tok)
	assert.Equal(t, "u@example.com", user.Email)
	assert.Equal(t, 2, calls)
}

func TestRunLoginFlowExhaustsRetries(t *testing.T) {
	deps := loginDeps{
		promptEmail:    func() string { return "u" },
		promptPassword: func() (string, error) { return "x", nil },
		doLogin: func(_, _, _ string) (string, userDTO, *proto.APIError) {
			return "", userDTO{}, proto.Err(401, proto.CodeUnauthorized, "invalid credentials")
		},
	}
	_, _, _, err := runLoginFlow(deps, "", "")
	require.ErrorIs(t, err, errLoginRetries)
}

func TestRunLoginFlowNonAuthFailsFast(t *testing.T) {
	calls := 0
	deps := loginDeps{
		promptEmail:    func() string { return "u" },
		promptPassword: func() (string, error) { return "x", nil },
		doLogin: func(_, _, _ string) (string, userDTO, *proto.APIError) {
			calls++
			return "", userDTO{}, proto.Err(0, "NETWORK", "dial fail")
		},
	}
	_, _, _, err := runLoginFlow(deps, "", "")
	require.Error(t, err)
	var apiErr *proto.APIError
	require.True(t, errors.As(err, &apiErr))
	assert.Equal(t, "NETWORK", apiErr.Code)
	assert.Equal(t, 1, calls, "non-401 must not retry")
}

func TestRunLoginFlowEmptyPasswordRetries(t *testing.T) {
	calls := 0
	deps := loginDeps{
		promptEmail:    func() string { return "u" },
		promptPassword: func() (string, error) { return "", nil }, // 空密码不计入网络调用
		doLogin: func(_, _, _ string) (string, userDTO, *proto.APIError) {
			calls++
			return "tok", userDTO{}, nil
		},
	}
	_, _, _, err := runLoginFlow(deps, "", "")
	require.ErrorIs(t, err, errLoginRetries)
	assert.Zero(t, calls, "empty passwords must not hit the server")
}
