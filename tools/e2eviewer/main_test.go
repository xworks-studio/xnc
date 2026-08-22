package main

import (
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

func TestParseTurn(t *testing.T) {
	srv, err := parseTurn("xncdev:xncdev-secret@192.168.1.12:3478", nil)
	if err != nil {
		t.Fatalf("parseTurn: %v", err)
	}
	if len(srv) != 1 {
		t.Fatalf("servers = %d, want 1", len(srv))
	}
	s := srv[0]
	want := []string{"turn:192.168.1.12:3478?transport=tcp", "turn:192.168.1.12:3478"}
	if len(s.URLs) != 2 || s.URLs[0] != want[0] || s.URLs[1] != want[1] {
		t.Fatalf("urls = %v, want %v", s.URLs, want)
	}
	if s.Username != "xncdev" || s.Credential != "xncdev-secret" {
		t.Fatalf("creds = %q/%q", s.Username, s.Credential)
	}

	// 空 = 无 TURN(直连模式本机 ICE)。
	if s, err := parseTurn("", nil); err != nil || s != nil {
		t.Fatalf("empty spec: %v %v", s, err)
	}
	// 形态错误。
	for _, bad := range []string{"no-at-host", "xncdev@1.2.3.4:1", ":pass@1.2.3.4:1", "user:@1.2.3.4:1"} {
		if _, err := parseTurn(bad, nil); err == nil {
			t.Fatalf("parseTurn(%q) should fail", bad)
		}
	}
	// 显式 URL 覆盖(凭据仍来自 --turn)。
	srv, err = parseTurn("u:p@h:1", []string{"turn:h:1?transport=tcp"})
	if err != nil || len(srv) != 1 || srv[0].URLs[0] != "turn:h:1?transport=tcp" || srv[0].Username != "u" {
		t.Fatalf("explicit urls: %+v err=%v", srv, err)
	}
}

func TestSummaryEvaluate(t *testing.T) {
	c := &config{
		expectFirstFrameMs: 2000,
		expectKeyframes:    1,
		expectPliIdrMs:     2000,
		duration:           time.Second,
	}
	ok := &summary{FirstFrameMs: 800, Keyframes: 3}
	ok.evaluate(c)
	if !ok.AssertionsPassed || len(ok.Failures) != 0 {
		t.Fatalf("ok case failed: %+v", ok)
	}

	bad := &summary{FirstFrameMs: 3000, Keyframes: 0, PlisSent: 1, PliToIdrMaxMs: 0}
	bad.evaluate(c)
	if bad.AssertionsPassed || len(bad.Failures) != 3 {
		t.Fatalf("bad case: %+v (failures=%v)", bad, bad.Failures)
	}

	// PLI 已发且 IDR 到达但过慢。
	slow := &summary{FirstFrameMs: 100, Keyframes: 2, PlisSent: 1, PliToIdrMaxMs: 2500}
	slow.evaluate(c)
	if slow.AssertionsPassed || len(slow.Failures) != 1 {
		t.Fatalf("slow PLI case: %+v", slow)
	}

	// 跳过断言(0 值)。
	none := &summary{}
	none.evaluate(&config{})
	if !none.AssertionsPassed {
		t.Fatalf("no-assert config should pass: %+v", none)
	}
}

// 编译期锁 ICE server 形态(pion 版本升级时的兼容哨兵)。
var _ = webrtc.ICETransportPolicyRelay
