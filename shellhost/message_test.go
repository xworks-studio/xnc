package main

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMessageGoldenBytes 黄金字节:payload 布局钉死(Task 3 native/core
// 按同一契约实现 CreateShell)。
func TestMessageGoldenBytes(t *testing.T) {
	assert.Equal(t, "00000000504f5745525348454c4c000000000000",
		hex.EncodeToString(encodeBegin(0, 0, "POWERSHELL")))
	assert.Equal(t, "c8001e00504f5745525348454c4c000000000000",
		hex.EncodeToString(encodeBegin(200, 30, "POWERSHELL")))
	// 超长 profile 截断到 16 字节字段宽(有效 16)。
	assert.Equal(t, "c8001e0042415348434f4d4d414e444c4f4e4700",
		hex.EncodeToString(encodeBegin(200, 30, "BASHCOMMANDLONG")))

	assert.Equal(t, "010500000068656c6c6f", hex.EncodeToString(encodeData(streamStdout, []byte("hello"))))
	assert.Equal(t, "0200000000", hex.EncodeToString(encodeData(streamStderr, nil)))
	assert.Equal(t, "0004000000aabbccdd", hex.EncodeToString(encodeData(streamStdin, []byte{0xaa, 0xbb, 0xcc, 0xdd})))

	assert.Equal(t, "5000c800", hex.EncodeToString(encodeResize(80, 200)))
	assert.Equal(t, "2a000000", hex.EncodeToString(encodeExit(42)))
	assert.Equal(t, "01000000", hex.EncodeToString(encodeExit(1)))

	assert.Equal(t, "737061776e5f6661696c6564"+"000000000000000000000000",
		hex.EncodeToString(encodeState(stateSpawnFailed)))
	assert.Equal(t, "70726f66696c655f6d697373696e67"+"000000000000000000",
		hex.EncodeToString(encodeState(stateProfileMissing)))
}

// TestMessageRoundTrip 编解码往返。
func TestMessageRoundTrip(t *testing.T) {
	c, r, p, err := decodeBegin(encodeBegin(120, 30, "CMD"))
	require.NoError(t, err)
	assert.Equal(t, uint16(120), c)
	assert.Equal(t, uint16(30), r)
	assert.Equal(t, "CMD", p)

	stream, data, err := decodeData(encodeData(streamStdout, []byte("x\ny")))
	require.NoError(t, err)
	assert.Equal(t, streamStdout, stream)
	assert.Equal(t, "x\ny", string(data))

	// 长度不符必须报错。
	_, _, err = decodeData([]byte{1, 0x05, 0, 0, 0, 'h', 'i'})
	assert.Error(t, err)

	c, r, err = decodeResize(encodeResize(132, 43))
	require.NoError(t, err)
	assert.Equal(t, uint16(132), c)
	assert.Equal(t, uint16(43), r)

	code, err := decodeExit(encodeExit(0xC0000142))
	require.NoError(t, err)
	assert.Equal(t, uint32(0xC0000142), code)

	s, err := decodeState(encodeState("spawn_failed"))
	require.NoError(t, err)
	assert.Equal(t, "spawn_failed", s)

	for _, bad := range [][]byte{{}, {1, 2, 3}, make([]byte, 25)} {
		_, _, _, err = decodeBegin(bad)
		assert.Error(t, err)
		_, err = decodeState(bad)
		assert.Error(t, err)
	}
}

// TestSecretStdin 镜像 xnc-desktop ParseSecretStdinLine 语义。
func TestSecretStdin(t *testing.T) {
	good := "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	sec, err := readSecretStdin(strReader(good))
	require.NoError(t, err)
	assert.Len(t, sec, 32)

	for _, in := range []string{good + "\n", good + "\r\n", good + "\r"} {
		sec, err = readSecretStdin(strReader(in))
		require.NoError(t, err)
		assert.Len(t, sec, 32)
	}

	for _, in := range []string{"", "abcd", good[:63], good + "ff", "zz" + good[2:], good + "\nsecond"} {
		_, err = readSecretStdin(strReader(in))
		assert.Errorf(t, err, "input len=%d must be rejected", len(in))
	}
}
