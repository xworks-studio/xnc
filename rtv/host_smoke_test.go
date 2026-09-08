//go:build windows

// host_smoke_test.go — 真实 xnc-host（Rust）全链路冒烟：拉起编译产物
// xnc-host.exe 对本测试的 relay 腿出流，viewer 侧校验 config + 媒体包。
// 默认跳过；XNC_HOST_EXE 指向 host\target\release\xnc-host.exe 时启用
// （DXGI 采集需交互会话；非交互/无显示器环境由 waitFor 超时兜底 skip）。
// 验收口径：config 到达 + ≥1 个合法媒体 datagram（完整解码链由
// rtvload + nalcheck + ffmpeg 在实机验收覆盖）。
package rtv

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"
)

func TestRealHostSmoke(t *testing.T) {
	exe := os.Getenv("XNC_HOST_EXE")
	if exe == "" {
		t.Skip("set XNC_HOST_EXE to host/target/release/xnc-host.exe to run the real-host smoke")
	}
	s := newLoopSrv(t, nil)
	hostAddr, wtAddr := s.ActualAddrs()
	wtURL := fmt.Sprintf("https://%s/wt?token=vtok", wtAddr)
	token := s.Hub.HostTokenFor("smoke-node")

	// stdin 配置（与 xnc-core spawn 契约同型）+ CLI 同值兜底。
	cfg := fmt.Sprintf(`{"endpoint":%q,"nodeId":"smoke-node","token":%q,"fps":15,"bitrateKbps":2000,"fec":20}`,
		hostAddr, token)
	cmd := exec.Command(exe, "--server", hostAddr, "--node-id", "smoke-node",
		"--token", token, "--fps", "15", "--bitrate-kbps", "2000", "--tls-insecure")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start host: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	_, _ = stdin.Write([]byte(cfg))
	_ = stdin.Close()

	waitFor(t, "host registered", func() bool { return s.Hub.Host("smoke-node") != nil })

	var sess *webtransport.Session
	var st *webtransport.Stream
	sess, st = dialWT(t, wtURL)
	sendJSON(st, map[string]any{"type": "hello", "role": "viewer"})

	// config：真实 host 在采集+编码器就绪后下发（探测链，秒级）。
	if m, err := readJSONUntil(st, 45*time.Second, "config"); err != nil {
		t.Skipf("no config from real host (headless/no-encoder env?): %v", err)
	} else {
		t.Logf("config: %vx%v@%v enc=%v hw=%v fec=%v%%",
			m["width"], m["height"], m["fps"], m["encoder"], m["encoderHw"], m["fecPercentage"])
		if w, _ := m["width"].(float64); w <= 0 {
			t.Fatalf("bad config: %v", m)
		}
	}

	// 媒体包：viewer 加入触发 frameLoss→IDR；静止桌面靠 viewer-count
	// 强制产帧路径出流。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for {
		b, err := sess.ReceiveDatagram(ctx)
		if err != nil {
			t.Fatalf("media datagram: %v", err)
		}
		if len(b) >= 34 && string(b[0:4]) == "MVP1" && binary.LittleEndian.Uint32(b[16:20]) > 0 {
			t.Logf("media frame pkt len=%d flags=%d frame=%d",
				len(b), b[4], binary.LittleEndian.Uint32(b[16:20]))
			return
		}
	}
}
