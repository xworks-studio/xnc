package session

import (
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/proto"
	"xnc/server/internal/registry"
)

func newMgr(t *testing.T) *Manager {
	t.Helper()
	return New(registry.New(), slog.Default())
}

// onlineMgr 注入一条在线节点连接（nil Cancel），使 Create 的 Online 检查通过。
func onlineMgr(t *testing.T) (uuid.UUID, *Manager) {
	t.Helper()
	reg := registry.New()
	id := uuid.New()
	reg.Add(&registry.NodeConn{NodeID: id.String(), LastBeat: time.Now()})
	return id, New(reg, slog.Default())
}

func TestCreateAttachExpire(t *testing.T) {
	nodeID, m := onlineMgr(t)
	res, apiErr := m.Create(nodeID, uuid.New(), proto.KindExec, []byte(`{}`))
	require.Nil(t, apiErr)
	require.NotEmpty(t, res.Session.ID)
	assert.NotEqual(t, res.AgentToken, res.ClientToken)
	assert.Equal(t, "/api/session/"+res.Session.ID, res.ClientPath)
	assert.WithinDuration(t, time.Now().Add(60*time.Second), res.ExpiresAt, 2*time.Second)

	// token 错误 → 401；会话不存在 → 404
	assert.Equal(t, 401, m.AttachAgent(res.Session.ID, "wrong", nil).Status)
	assert.Equal(t, 404, m.AttachAgent(uuid.NewString(), res.AgentToken, nil).Status)

	// 正确 attach（nil ws 允许：生命周期先行，粘合由 T3 的真实连接触发）
	assert.Nil(t, m.AttachAgent(res.Session.ID, res.AgentToken, nil))
	// token 单用途：二次使用 → 401
	assert.Equal(t, 401, m.AttachAgent(res.Session.ID, res.AgentToken, nil).Status)
	// agent 侧重复 attach（另一 token 不存在）→ 404
	assert.Equal(t, 404, m.AttachAgent(res.Session.ID, "another", nil).Status)
}

func TestOpeningTimeoutFiresFinish(t *testing.T) {
	nodeID, m := onlineMgr(t)
	res, apiErr := m.Create(nodeID, uuid.New(), proto.KindExec, []byte(`{}`))
	require.Nil(t, apiErr)

	var reasons []string
	var notified []string
	m.SetFinishFn(res.Session, func(r string) { reasons = append(reasons, r) })
	m.SetNotifyFn(res.Session, func(sc proto.SessionClose) error {
		notified = append(notified, sc.Reason)
		return nil
	})
	m.shortenOpeningTTL(res.Session, 30*time.Millisecond) // 测试后门：缩短 TTL

	time.Sleep(120 * time.Millisecond)
	assert.Equal(t, []string{"opening-timeout"}, reasons)
	assert.Equal(t, []string{"opening-timeout"}, notified)
	// 过期后 token 失效
	assert.Equal(t, 410, m.AttachClient(res.Session.ID, res.ClientToken, nil).Status)
}

func TestCloseIdempotent(t *testing.T) {
	nodeID, m := onlineMgr(t)
	res, _ := m.Create(nodeID, uuid.New(), proto.KindExec, []byte(`{}`))
	var n atomic.Int32
	m.SetFinishFn(res.Session, func(string) { n.Add(1) })
	m.SetNotifyFn(res.Session, func(proto.SessionClose) error { return nil })

	m.NotifyClose(res.Session.ID, "client-gone")
	m.NotifyClose(res.Session.ID, "client-gone") // 幂等
	assert.Equal(t, int32(1), n.Load())
}

func TestOfflineNodeRefused(t *testing.T) {
	// registry 无此节点连接 → Create 拒绝 NODE_OFFLINE（409）
	m := newMgr(t)
	defer m.Close()
	_, apiErr := m.Create(uuid.New(), uuid.New(), proto.KindExec, []byte(`{}`))
	require.NotNil(t, apiErr)
	assert.Equal(t, 409, apiErr.Status)
	assert.Equal(t, proto.CodeNodeOffline, apiErr.Code)
}

// —— Phase 3 shell 会话治理：每节点限额 + idle/max-lifetime janitor ——

func TestShellPerNodeLimit(t *testing.T) {
	id, m := onlineMgr(t)
	defer m.Close()
	m.ShellPerNode = 2
	for i := 0; i < 2; i++ {
		res, apiErr := m.Create(id, uuid.New(), proto.KindShell, []byte(`{}`))
		require.Nil(t, apiErr)
		_ = res
	}
	_, apiErr := m.Create(id, uuid.New(), proto.KindShell, []byte(`{}`))
	require.NotNil(t, apiErr)
	assert.Equal(t, 409, apiErr.Status)
	assert.Equal(t, proto.CodeSessionLimited, apiErr.Code)
	// exec 不受限
	_, apiErr = m.Create(id, uuid.New(), proto.KindExec, []byte(`{}`))
	assert.Nil(t, apiErr)
	// 关掉一个 shell 后可再建
	m.NotifyClose(firstShellID(t, m, id), "test")
	res, apiErr := m.Create(id, uuid.New(), proto.KindShell, []byte(`{}`))
	assert.Nil(t, apiErr)
	_ = res
}

func firstShellID(t *testing.T, m *Manager, nodeID uuid.UUID) string {
	t.Helper()
	sess := m.SessionsOf(nodeID, proto.KindShell)
	require.NotEmpty(t, sess)
	return sess[0].ID
}

func TestIdleTimeoutClosesShell(t *testing.T) {
	id, m := onlineMgr(t)
	defer m.Close()
	m.ShellIdleTimeout = 100 * time.Millisecond
	m.setJanitorInterval(30 * time.Millisecond)

	var reasons []string
	res, _ := m.Create(id, uuid.New(), proto.KindShell, []byte(`{}`))
	m.SetFinishFn(res.Session, func(r string) { reasons = append(reasons, r) })

	require.Eventually(t, func() bool { return len(reasons) > 0 }, 3*time.Second, 50*time.Millisecond)
	assert.Equal(t, "idle-timeout", reasons[0])
}

func TestActivityPreventsIdleTimeout(t *testing.T) {
	id, m := onlineMgr(t)
	defer m.Close()
	m.ShellIdleTimeout = 250 * time.Millisecond
	m.setJanitorInterval(50 * time.Millisecond)

	res, _ := m.Create(id, uuid.New(), proto.KindShell, []byte(`{}`))
	closed := make(chan string, 1)
	m.SetFinishFn(res.Session, func(r string) { closed <- r })

	// 每 100ms 活动一次，持续 600ms（跨越 2 个 idle 窗口）
	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		m.touch(res.Session, time.Now())
		time.Sleep(100 * time.Millisecond)
	}
	select {
	case r := <-closed:
		t.Fatalf("closed early: %s", r)
	default:
	}
	// 停止活动 → 应在 idle+janitor 内关闭
	select {
	case r := <-closed:
		assert.Equal(t, "idle-timeout", r)
	case <-time.After(3 * time.Second):
		t.Fatal("no idle close after activity stopped")
	}
}

func TestMaxLifetimeClosesShell(t *testing.T) {
	id, m := onlineMgr(t)
	defer m.Close()
	m.ShellMaxLifetime = 120 * time.Millisecond
	m.setJanitorInterval(30 * time.Millisecond)

	res, _ := m.Create(id, uuid.New(), proto.KindShell, []byte(`{}`))
	closed := make(chan string, 1)
	m.SetFinishFn(res.Session, func(r string) { closed <- r })
	m.touch(res.Session, time.Now().Add(time.Hour)) // 活动再频繁也逃不过寿命

	select {
	case r := <-closed:
		assert.Equal(t, "max-lifetime", r)
	case <-time.After(3 * time.Second):
		t.Fatal("no max-lifetime close")
	}
}
