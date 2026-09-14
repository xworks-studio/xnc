// commandstdin_test.go — --secret-stdin/--command-stdin 的 stdin 契约:
// secret 行与命令帧可能同批抵达同一个 Read(匿名管道默认一次写完),
// bufio 共享读端必须保住换行之后的帧字节(2026-09-14 引号事故修复的
// 传输层钉子:裸块读会吞帧)。
package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func secretLine() string {
	return strings.Repeat("ab", 32) + "\n" // 64 hex + LF
}

func commandFrame(cmd string) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, uint32(len(cmd)))
	b.WriteString(cmd)
	return b.Bytes()
}

// TestSecretAndFrameArriveInOneRead 同批抵达:整段(secret 行 + 帧头 + 帧体)
// 一次性可读时,secret 与命令都必须完整读出。
func TestSecretAndFrameArriveInOneRead(t *testing.T) {
	raw := append([]byte(secretLine()), commandFrame(`Write-Output "hello world"`)...)
	br := bufio.NewReader(bytes.NewReader(raw))

	sec, err := readSecretStdin(br)
	require.NoError(t, err)
	assert.Len(t, sec, 32)

	cmd, err := readCommandFrame(br)
	require.NoError(t, err)
	assert.Equal(t, `Write-Output "hello world"`, cmd)
}

// TestSecretLineVariants 换行形态:CRLF、无换行 EOF 均为合法 secret 行。
func TestSecretLineVariants(t *testing.T) {
	for name, line := range map[string]string{
		"crlf": strings.Repeat("ab", 32) + "\r\n",
		"lf":   secretLine(),
		"eof":  strings.Repeat("ab", 32), // 无换行,EOF 即首行
	} {
		t.Run(name, func(t *testing.T) {
			sec, err := readSecretStdin(bufio.NewReader(strings.NewReader(line)))
			require.NoError(t, err)
			assert.Len(t, sec, 32)
			assert.Equal(t, []byte{0xab, 0xab}, sec[:2])
		})
	}
}

// TestCommandFrameRejectsOversize 超界帧(>0xFFFF)必须拒绝,绝不截断执行。
func TestCommandFrameRejectsOversize(t *testing.T) {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, uint32(0x10000))
	cmd, err := readCommandFrame(&b)
	require.Error(t, err)
	assert.Empty(t, cmd)
	assert.Contains(t, err.Error(), "64KB")
}

// TestCommandFrameRejectsTruncated 半截帧(声明长度 > 实际字节)必须报错。
func TestCommandFrameRejectsTruncated(t *testing.T) {
	frame := commandFrame("echo hi")
	truncated := frame[:len(frame)-2] // 声明 7 字节,只给 5
	cmd, err := readCommandFrame(bytes.NewReader(truncated))
	require.Error(t, err)
	assert.Empty(t, cmd)
}

// TestCommandFrameEmptyLength 零长帧合法读出空命令(oneshot 必填命令由
// main 的 usage 校验兜底,读取层不重复裁决)。
func TestCommandFrameEmptyLength(t *testing.T) {
	cmd, err := readCommandFrame(bytes.NewReader(commandFrame("")))
	require.NoError(t, err)
	assert.Empty(t, cmd)
}

// TestEmptyReaderIsEOFError 空流:secret 读取按长度校验失败(而非 panic/阻塞)。
func TestEmptyReaderIsEOFError(t *testing.T) {
	_, err := readSecretStdin(bufio.NewReader(bytes.NewReader(nil)))
	require.Error(t, err)
}

// TestShortFrameHeader 帧头不足 4 字节即报错。
func TestShortFrameHeader(t *testing.T) {
	_, err := readCommandFrame(bytes.NewReader([]byte{1, 2, 3}))
	require.Error(t, err)
}
