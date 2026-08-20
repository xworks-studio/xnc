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
		promptServer: func() string { return "https://s.example" },
		promptEmail:  func() string { return "u@example.com" },
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
	server, tok, user, err := runLoginFlow(deps, "", "")
	require.NoError(t, err)
	assert.Equal(t, "https://s.example", server)
	assert.Equal(t, "tok", tok)
	assert.Equal(t, "u@example.com", user.Email)
	assert.Equal(t, 2, calls)
}

func TestRunLoginFlowExhaustsRetries(t *testing.T) {
	deps := loginDeps{
		promptServer:   func() string { return "https://s" },
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
		promptServer:   func() string { return "https://s" },
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
		promptServer:   func() string { return "https://s" },
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
