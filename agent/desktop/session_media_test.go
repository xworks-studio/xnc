package desktop

// session_media_test.go — M4 Task 4：agent 侧媒体管线版本强制（回环，无需
// 真实 pipe/WebRTC 建联——强制点在 ready 之前）。规则（绑定裁决 2）：
//
//	① server 经 SESSION_OPEN params 选定的 mediaProtocol 是会话钉子；
//	② HOST_HELLO 的 media_protocol 与钉子不符 → error 帧
//	   code=media_protocol_mismatch 响亮收线（双向：v2 选定遇 v1 host、
//	   v1 选定遇 v2 host 都拒）；
//	③ 未知/缺席协议值 fail closed 到 v1（旧 server / 损坏 params 继续可用）；
//	④ 重挂（reattach）换了版本的 host 同样拒绝——live 会话绝不换版。

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// mediaFakeSource 包装 fakeSource，覆写 Hello 的 MediaProtocol（host 说
// v2 wire 的 fake 形态；fakeSource 缺省 Hello 即 v1）。
type mediaFakeSource struct {
	*fakeSource
	proto uint32
}

func (s mediaFakeSource) Hello() *HelloInfo {
	h := s.fakeSource.Hello()
	h.MediaProtocol = s.proto
	return h
}

// mediaStarter 固定返回预置源（计数 Start/Stop 以断言配对）。
type mediaStarter struct {
	src    Source
	starts atomic.Int32
	stops  atomic.Int32
}

func (st *mediaStarter) Start(_ context.Context, _ uint32) (Source, error) {
	st.starts.Add(1)
	return st.src, nil
}

func (st *mediaStarter) Stop() error {
	st.stops.Add(1)
	return nil
}

// mediaSeqStarter 按调用序返回预置源（驱动重挂路径；源可为任意 Source 形态）。
type mediaSeqStarter struct {
	srcs  []Source
	calls atomic.Int32
	stops atomic.Int32
}

func (st *mediaSeqStarter) Start(_ context.Context, _ uint32) (Source, error) {
	n := int(st.calls.Add(1)) - 1
	if n >= len(st.srcs) {
		n = len(st.srcs) - 1
	}
	return st.srcs[n], nil
}

func (st *mediaSeqStarter) Stop() error {
	st.stops.Add(1)
	return nil
}

// runMediaSession 起 httptest WS 服务端跑 Handler.Handle，返回 viewer 连接、
// Handle 返回信号与收线函数。params 为 SESSION_OPEN params 原文。
func runMediaSession(t *testing.T, h *Handler, params string) (*websocket.Conn, chan struct{}, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	handlerDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		c.SetReadLimit(1 << 20)
		h.Handle(ctx, c, "sess-media", json.RawMessage(params))
		close(handlerDone)
	}))
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	vws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		cancel()
		t.Fatalf("viewer dial: %v", err)
	}
	vws.SetReadLimit(1 << 20)
	t.Cleanup(func() { _ = vws.CloseNow() })
	return vws, handlerDone, cancel
}

// TestDesktopMediaProtocolMismatchAtOpen：open 时 host hello 的
// media_protocol 与 server 选定不符 → error 帧 + 会话即收线（Start/Stop
// 仍配对）。两个方向都拒（绝不混版续流）。
func TestDesktopMediaProtocolMismatchAtOpen(t *testing.T) {
	for _, tc := range []struct {
		name       string
		params     string
		hostProto  uint32
		wantStops  int32
		wantStarts int32
	}{
		{
			name:      "v2 selected but host speaks v1",
			params:    `{"signaling":"webrtc","iceTransportPolicy":"all","mediaProtocol":"v2"}`,
			hostProto: 0,
		},
		{
			name:      "v1 selected but host speaks v2",
			params:    `{"signaling":"webrtc","iceTransportPolicy":"all","mediaProtocol":"v1"}`,
			hostProto: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &mediaStarter{src: mediaFakeSource{fakeSource: newFakeSource(), proto: tc.hostProto}}
			h := &Handler{Log: slog.Default(), Starter: st}
			vws, handlerDone, cancel := runMediaSession(t, h, tc.params)
			defer cancel()

			m := drainUntil(t, context.Background(), vws, 10*time.Second,
				func(m map[string]any) bool { return m["type"] == vocabError })
			if m["code"] != "media_protocol_mismatch" {
				t.Fatalf("error frame = %v, want code=media_protocol_mismatch", m)
			}
			select {
			case <-handlerDone:
			case <-time.After(10 * time.Second):
				t.Fatalf("Handle did not return after mismatch")
			}
			if st.starts.Load() != 1 || st.stops.Load() != 1 {
				t.Fatalf("start/stop = %d/%d, want 1/1", st.starts.Load(), st.stops.Load())
			}
		})
	}
}

// TestDesktopMediaProtocolMatchAndFailClosed：匹配 → ready 帧正常下发；
// 未知值（"bogus"）与缺席（旧 server）fail closed 到 v1——v1 host 上照常开会。
func TestDesktopMediaProtocolMatchAndFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name      string
		params    string
		hostProto uint32
	}{
		{
			name:      "v2 selected matches v2 host",
			params:    `{"signaling":"webrtc","iceTransportPolicy":"all","mediaProtocol":"v2"}`,
			hostProto: 2,
		},
		{
			name:      "unknown value fails closed to v1 host",
			params:    `{"signaling":"webrtc","iceTransportPolicy":"all","mediaProtocol":"bogus"}`,
			hostProto: 0,
		},
		{
			name:      "absent value (old server) defaults to v1 host",
			params:    `{"signaling":"webrtc","iceTransportPolicy":"all"}`,
			hostProto: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &mediaStarter{src: mediaFakeSource{fakeSource: newFakeSource(), proto: tc.hostProto}}
			h := &Handler{Log: slog.Default(), Starter: st}
			vws, handlerDone, cancel := runMediaSession(t, h, tc.params)

			m := drainUntil(t, context.Background(), vws, 10*time.Second,
				func(m map[string]any) bool { return m["type"] == vocabReady })
			if m["type"] != vocabReady {
				t.Fatalf("no ready frame; got %v", m)
			}
			cancel()
			select {
			case <-handlerDone:
			case <-time.After(10 * time.Second):
				t.Fatalf("Handle did not return after cancel")
			}
		})
	}
}

// TestDesktopMediaProtocolMismatchOnReattach：live v1 会话重挂到 v2 host →
// error 帧 + 会话收线（绝不换版续流；两次 Start 各配对一次 Stop）。
func TestDesktopMediaProtocolMismatchOnReattach(t *testing.T) {
	src1 := newFakeSource() // v1（fakeSource.Hello 缺省 0）
	src2 := mediaFakeSource{fakeSource: newFakeSource(), proto: 2}
	st := &mediaSeqStarter{srcs: []Source{src1, src2}}
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
		h.Handle(ctx, c, "sess-media-reattach",
			json.RawMessage(`{"signaling":"webrtc","iceTransportPolicy":"all"}`))
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

	drainUntil(t, ctx, vws, 10*time.Second,
		func(m map[string]any) bool { return m["type"] == vocabReady })

	// 杀 v1 源 → 意图退避 1s 后 re-Start 返回 v2 源 → onPublish 强制拒绝。
	src1.Close()

	m := drainUntil(t, ctx, vws, 15*time.Second,
		func(m map[string]any) bool { return m["type"] == vocabError })
	if m["code"] != "media_protocol_mismatch" {
		t.Fatalf("frame after reattach = %v, want error code=media_protocol_mismatch", m)
	}
	select {
	case <-handlerDone:
	case <-time.After(10 * time.Second):
		t.Fatalf("Handle did not return after reattach mismatch")
	}
	if st.calls.Load() != 2 {
		t.Fatalf("core.Start calls = %d, want 2", st.calls.Load())
	}
	deadline := time.Now().Add(5 * time.Second)
	for st.stops.Load() != 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if st.stops.Load() != 2 {
		t.Fatalf("core.Stop calls = %d, want 2", st.stops.Load())
	}
}

// TestMediaProtocolWireVersion：协议值 → HOST_HELLO 尾随 u32 的映射表
//（v2 → 2；其余一律 0 = v1，fail closed）。
func TestMediaProtocolWireVersion(t *testing.T) {
	cases := map[string]uint32{
		"":       0,
		"v1":     0,
		"v2":     2,
		"V2":     0, // 大小写敏感：未知即 v1
		"2":      0,
		"bogus":  0,
		"v3":     0,
	}
	for in, want := range cases {
		if got := mediaProtocolWireVersion(in); got != want {
			t.Errorf("mediaProtocolWireVersion(%q) = %d, want %d", in, got, want)
		}
	}
}
