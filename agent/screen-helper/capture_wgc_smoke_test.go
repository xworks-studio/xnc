//go:build windows

// capture_wgc_smoke_test.go — WGC 捕获管线冒烟测试（真机交互会话才有
// 意义；CI/无桌面环境用 XNC_SKIP_WGC_SMOKE=1 跳过）：captureLoop 跑在
// net.Pipe 上，校验 dims + capturing 状态帧 + 至少一个关键帧，并转储
// 裸流供 ffmpeg 解码验证。
package main

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWGCCaptureLoopSmoke(t *testing.T) {
	if os.Getenv("XNC_SKIP_WGC_SMOKE") != "" {
		t.Skip("XNC_SKIP_WGC_SMOKE set")
	}

	client, server := net.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- captureLoop(ctx, server, captureOpts{fps: 15, maxWidth: 1280, quality: 60})
	}()

	outDir := filepath.Join(os.TempDir(), "xnc-wgc-smoke")
	os.MkdirAll(outDir, 0o755)
	f, err := os.Create(filepath.Join(outDir, "stream.h264"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var gotDims, gotState bool
	bins, keys := 0, 0
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	for {
		hdr := make([]byte, 5)
		if _, err := io.ReadFull(client, hdr); err != nil {
			break
		}
		n := binary.LittleEndian.Uint32(hdr[1:5])
		payload := make([]byte, n)
		if _, err := io.ReadFull(client, payload); err != nil {
			t.Fatalf("short payload: %v", err)
		}
		switch hdr[0] {
		case pipeFrameDims:
			w := int(int32(binary.LittleEndian.Uint32(payload[0:4])))
			h := int(int32(binary.LittleEndian.Uint32(payload[4:8])))
			if w <= 0 || h <= 0 {
				t.Fatalf("bad dims %dx%d", w, h)
			}
			t.Logf("dims %dx%d", w, h)
			gotDims = true
		case pipeFrameState:
			t.Logf("state %s", payload)
			gotState = true
		case pipeFrameKey, pipeFrameDelta:
			bins++
			if hdr[0] == pipeFrameKey {
				keys++
			}
			f.Write(payload)
		}
		if keys >= 2 {
			break
		}
	}
	// net.Pipe 同步无缓冲：停止读取后必须持续排水，否则写端阻塞在
	// writeFrame 内无法观察 ctx 取消。
	cancel()
	go io.Copy(io.Discard, client)
	<-done

	if !gotDims {
		t.Error("no dims frame")
	}
	if !gotState {
		t.Error("no state frame")
	}
	if keys == 0 {
		t.Fatal("no keyframe within timeout (static-desktop first frame missing)")
	}
	st, err := os.Stat(f.Name())
	if err != nil || st.Size() < 1024 {
		t.Fatalf("stream dump too small: %v", st)
	}
	t.Logf("frames=%d keys=%d dump=%s (%dB) — 用 ffmpeg 解码验证像素",
		bins, keys, f.Name(), st.Size())
}
