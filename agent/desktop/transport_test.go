package desktop

import (
	"testing"
	"time"
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
