package desktop

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

// frameDuration 时间轴纪律:mono 差直接采用(长静止恢复、IDR lookahead
// 回填都是真实时间,必须原样计入 RTP 时间轴——钳位会让 Chrome 把首帧
// 判为迟到丢弃,画面停在旧帧=回退帧观感);仅 mono 不前进(首帧/同帧
// re-feed)时回退 DefaultDuration。
func TestFrameDurationTimeline(t *testing.T) {
	p := &Publisher{defDur: 33 * time.Millisecond}

	// 首帧(lastMono==0)→ DefaultDuration。
	if d := p.frameDuration(1_000_000); d != 33*time.Millisecond {
		t.Fatalf("first frame: got %v, want %v", d, 33*time.Millisecond)
	}
	// 正常帧间隔(33ms)→ 真实差。
	if d := p.frameDuration(1_000_000 + 33_000); d != 33*time.Millisecond {
		t.Fatalf("normal gap: got %v, want 33ms", d)
	}
	// 长静止 30s 后恢复 → 真实差 30s(此前 2s 钳位会回退 33ms)。
	if d := p.frameDuration(1_000_000 + 33_000 + 30_000_000); d != 30*time.Second {
		t.Fatalf("idle resume: got %v, want 30s", d)
	}
	// IDR lookahead 回填(533ms)→ 真实差。
	if d := p.frameDuration(1_000_000 + 33_000 + 30_000_000 + 533_000); d != 533*time.Millisecond {
		t.Fatalf("idr gap: got %v, want 533ms", d)
	}
	// 同帧 re-feed(时间戳不前进)→ DefaultDuration(单调保持)。
	if d := p.frameDuration(1_000_000 + 33_000 + 30_000_000 + 533_000); d != 33*time.Millisecond {
		t.Fatalf("same-frame refeed: got %v, want %v", d, 33*time.Millisecond)
	}
	// mono 回跳(时钟异常)→ DefaultDuration。
	if d := p.frameDuration(1_000); d != 33*time.Millisecond {
		t.Fatalf("mono rollback: got %v, want %v", d, 33*time.Millisecond)
	}
}

// TestSessionAPIGCCAssembly:newSessionAPI 装配冒烟(缺陷 A):发送侧
// GCC 估计器与既有默认件(NACK/SenderReport/TWCC 头扩展/统计)同注册表
// 共存,无重复注册冲突;per-会话 API 能建出 PeerConnection 并随其关闭
// 干净收线(cc 拦截器/GCC 内部泵由 PC Close 级联关闭)。估计回调 →
// streamQoS.AgentEstimate 的控制器语义由 TestAgentEstimatePrecedence 钉死。
func TestSessionAPIGCCAssembly(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	q := newStreamQoS(QoSControllerConfig{
		Initial: VideoConfig{Bitrate: bitrateForWidth(1920), FPS: 30, MaxW: 1920},
		AspectW: 1920,
		AspectH: 1080,
	}, log)
	api, err := newSessionAPI(q, "smoke")
	if err != nil {
		t.Fatalf("newSessionAPI: %v", err)
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("peer connection: %v", err)
	}
	if err := pc.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
