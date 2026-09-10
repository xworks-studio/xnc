package session

import (
	"testing"
	"time"

	"xnc/proto"
)

// TestDesktopGlueOnTouch：relay-plane 票据流下 viewer 不经 AttachClientRTV
// 粘合——首个 viewer 触碰即视为粘合，Opening TTL 不再收会话（真机验收
// 踩坑回归：晚于 60s 到达的 viewer 收 401）。
func TestDesktopGlueOnTouch(t *testing.T) {
	m := New(nil, nil)
	s := &session{ID: "s-glue", Kind: proto.KindDesktop,
		expiresAt: time.Now().Add(time.Minute)}
	m.mu.Lock()
	m.sessions[s.ID] = s
	m.mu.Unlock()
	s.ttl = time.AfterFunc(50*time.Millisecond, func() { m.expire(s) })

	m.TouchActivity(s.ID)
	if !s.glued {
		t.Fatal("touch must glue desktop session")
	}
	time.Sleep(150 * time.Millisecond)
	m.mu.Lock()
	alive := m.sessions[s.ID] != nil
	m.mu.Unlock()
	if !alive {
		t.Fatal("glued session must survive opening TTL")
	}
}
