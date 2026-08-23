// reattach_loopback_test.go — M2-Slice3 Task 2 回环扩展:连接中途源死亡
// (模拟 logoff/pipe 断/core 掉线)→ fake server 会话仍开(ctx 未取消)→
// 意图自愈 re-StartCapture → 同一 WebRTC PC 上帧流恢复 + viewer 收到
// {"type":"state","code":"reattached"} 信令帧(重挂可观测)。
package desktop

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

// reattachStarter 按调用序返回预置源(fake core:StartCapture 幂等语义
// 简化为每次新源)。
type reattachStarter struct {
	srcs  []*fakeSource
	calls atomic.Int32
	stops atomic.Int32
}

func (st *reattachStarter) Start(_ context.Context, _ uint32) (Source, error) {
	n := int(st.calls.Add(1)) - 1
	if n >= len(st.srcs) {
		n = len(st.srcs) - 1
	}
	return st.srcs[n], nil
}

func (st *reattachStarter) Stop() error {
	st.stops.Add(1)
	return nil
}

func TestSessionReattachMidStream(t *testing.T) {
	src1, src2 := newFakeSource(), newFakeSource()
	src1.run(t)
	st := &reattachStarter{srcs: []*fakeSource{src1, src2}}
	h := &Handler{Log: slog.Default(), Starter: st}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handlerDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(1 << 20)
		h.Handle(ctx, c, "sess-reattach", json.RawMessage(`{"signaling":"webrtc","iceTransportPolicy":"all"}`))
		close(handlerDone)
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	vws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("viewer dial: %v", err)
	}
	vws.SetReadLimit(1 << 20)
	defer vws.CloseNow()

	drainUntil(t, ctx, vws, 10*time.Second, func(m map[string]any) bool { return m["type"] == vocabReady })

	// 建联(与主回环门同一 trickle 流程)。
	vpc, vstats := startViewerPC(t)
	defer func() { _ = vpc.Close() }()
	iceCh := make(chan webrtc.ICECandidateInit, 16)
	vpc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			iceCh <- c.ToJSON()
		}
	})
	offer, err := vpc.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	if err := vpc.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local: %v", err)
	}
	sendJSON(t, ctx, vws, map[string]any{"type": vocabOffer, "sdp": offer.SDP})
	ans := drainUntil(t, ctx, vws, 10*time.Second, func(m map[string]any) bool { return m["type"] == vocabAnswer })
	if err := vpc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: ans["sdp"].(string)}); err != nil {
		t.Fatalf("set remote answer: %v", err)
	}
	go func() {
		for c := range iceCh {
			sendJSON(t, ctx, vws, map[string]any{"type": vocabICE, "candidate": c})
		}
	}()
	stateCh := make(chan map[string]any, 4)
	go func() { // agent→viewer ice + state 帧后台泵(并发 Reader 不安全,统一单读者)
		for {
			mt, r, err := vws.Reader(ctx)
			if err != nil {
				return
			}
			if mt != websocket.MessageText {
				continue
			}
			b, err := io.ReadAll(io.LimitReader(r, 1<<20))
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(b, &m) != nil {
				continue
			}
			switch m["type"] {
			case vocabICE:
				if m["candidate"] != nil {
					var ci webrtc.ICECandidateInit
					jb, _ := json.Marshal(m["candidate"])
					if json.Unmarshal(jb, &ci) == nil {
						_ = vpc.AddICECandidate(ci)
					}
				}
			case vocabState:
				select {
				case stateCh <- m:
				default:
				}
			}
		}
	}()
	connected := time.Now().Add(15 * time.Second)
	for vpc.ConnectionState() != webrtc.PeerConnectionStateConnected {
		if time.Now().After(connected) {
			t.Fatalf("viewer never connected, state=%v", vpc.ConnectionState())
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 帧流就绪。
	select {
	case <-vstats.firstFrameCh:
	case <-time.After(10 * time.Second):
		t.Fatalf("no first frame; rtp=%d", vstats.rtpPkts.Load())
	}
	waitFrames(t, vstats, 10, 10*time.Second)
	mark := vstats.frames.Load()

	// 中途杀源(logoff 语义:desktoppipe Done)→ server 会话仍开 →
	// 意图 1s 退避后 re-Start(src2)→ 同一 PC 续流。
	src1.Close()
	src2.run(t) // 新源就绪供帧(在 re-Start 前启动 run 无害:帧在 attach 后才被泵)

	// reattached 信令帧(重挂可观测)。
	var got map[string]any
	select {
	case got = <-stateCh:
	case <-time.After(15 * time.Second):
		t.Fatalf("no state frame after mid-stream kill (want code=%s)", vocabStateReattached)
	}
	if got["code"] != vocabStateReattached || got["recoverable"] != true {
		t.Fatalf("state frame after kill = %v, want code=%s recoverable=true", got, vocabStateReattached)
	}
	// 同一 PC:新源帧到达(frames 在 mark 之后继续增长)。
	waitFrames(t, vstats, mark+5, 15*time.Second)
	if vpc.ConnectionState() != webrtc.PeerConnectionStateConnected {
		t.Fatalf("PC state = %v, want connected (no renegotiation)", vpc.ConnectionState())
	}

	// 会话收线:两个 Start 各配对一个 Stop。
	cancel()
	select {
	case <-handlerDone:
	case <-time.After(10 * time.Second):
		t.Fatalf("Handler did not return after ctx cancel")
	}
	if calls := st.calls.Load(); calls != 2 {
		t.Fatalf("core.Start calls = %d, want 2", calls)
	}
	deadline := time.Now().Add(5 * time.Second)
	for st.stops.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if st.stops.Load() != 2 {
		t.Fatalf("core.Stop calls = %d, want 2 (每个 Start 一个 Stop)", st.stops.Load())
	}
	if !src2.isClosed() {
		t.Fatalf("second source not closed after session end")
	}
}
