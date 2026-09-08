package session

import (
	"encoding/json"
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

// —— desktop 会话治理：每节点并发上限（RTV 后默认 8，XNC_DESKTOP_PER_NODE
// 可覆写）+ idle 5min janitor ——

func TestDesktopPerNodeLimit(t *testing.T) {
	id, m := onlineMgr(t)
	defer m.Close()
	// New 默认 DesktopPerNode=8（多 viewer）；显式覆盖证明配置生效。
	require.Equal(t, 8, m.DesktopPerNode)
	m.DesktopPerNode = 4

	// 覆写 4 并发：第 2 个会话（第二 viewer）至第 4 个均允许
	for i := 0; i < 4; i++ {
		_, apiErr := m.Create(id, uuid.New(), proto.KindDesktop, []byte(`{}`))
		require.Nil(t, apiErr)
	}
	// 第 5 个 → 409 SESSION_LIMIT_EXCEEDED
	_, apiErr := m.Create(id, uuid.New(), proto.KindDesktop, []byte(`{}`))
	require.NotNil(t, apiErr)
	assert.Equal(t, 409, apiErr.Status)
	assert.Equal(t, proto.CodeSessionLimited, apiErr.Code)
	// 其他 kind 不受限
	_, apiErr = m.Create(id, uuid.New(), proto.KindExec, []byte(`{}`))
	assert.Nil(t, apiErr)
	_, apiErr = m.Create(id, uuid.New(), proto.KindScreen, []byte(`{}`))
	assert.Nil(t, apiErr)
	// 关掉一个 desktop 会话后名额归还
	d := m.SessionsOf(id, proto.KindDesktop)
	require.Len(t, d, 4)
	m.NotifyClose(d[0].ID, "test")
	_, apiErr = m.Create(id, uuid.New(), proto.KindDesktop, []byte(`{}`))
	assert.Nil(t, apiErr)

	// env 覆写路径（XNC_DESKTOP_PER_NODE=1）：单会话语义——清空后第 2 发 409
	for _, s := range m.SessionsOf(id, proto.KindDesktop) {
		m.NotifyClose(s.ID, "test")
	}
	m.DesktopPerNode = 1
	_, apiErr = m.Create(id, uuid.New(), proto.KindDesktop, []byte(`{}`))
	require.Nil(t, apiErr)
	_, apiErr = m.Create(id, uuid.New(), proto.KindDesktop, []byte(`{}`))
	require.NotNil(t, apiErr)
	assert.Equal(t, proto.CodeSessionLimited, apiErr.Code)
	// 0 = 不限
	m.DesktopPerNode = 0
	_, apiErr = m.Create(id, uuid.New(), proto.KindDesktop, []byte(`{}`))
	assert.Nil(t, apiErr)
}

func TestDesktopIdleTimeoutCloses(t *testing.T) {
	id, m := onlineMgr(t)
	defer m.Close()
	m.DesktopIdleTimeout = 100 * time.Millisecond
	m.setJanitorInterval(30 * time.Millisecond)

	res, apiErr := m.Create(id, uuid.New(), proto.KindDesktop, []byte(`{}`))
	require.Nil(t, apiErr)
	closed := make(chan string, 1)
	m.SetFinishFn(res.Session, func(r string) { closed <- r })

	select {
	case r := <-closed:
		assert.Equal(t, "idle-timeout", r)
	case <-time.After(3 * time.Second):
		t.Fatal("no idle close for desktop session")
	}
}

func TestDesktopActivityPreventsIdleClose(t *testing.T) {
	id, m := onlineMgr(t)
	defer m.Close()
	m.DesktopIdleTimeout = 250 * time.Millisecond
	m.setJanitorInterval(50 * time.Millisecond)

	res, _ := m.Create(id, uuid.New(), proto.KindDesktop, []byte(`{}`))
	closed := make(chan string, 1)
	m.SetFinishFn(res.Session, func(r string) { closed <- r })

	// 信令帧持续流动（pump 每帧刷新 lastActivity；touch 同字段）
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
	select {
	case r := <-closed:
		assert.Equal(t, "idle-timeout", r)
	case <-time.After(3 * time.Second):
		t.Fatal("no idle close after activity stopped")
	}
}

// TestDesktopNoMaxLifetime：desktop 治理只有 idle——shell 的 maxLifetime
// 配置不影响 desktop 会话（长时间观看是合法形态，寿命上限属 Slice3+）。
func TestDesktopNoMaxLifetime(t *testing.T) {
	id, m := onlineMgr(t)
	defer m.Close()
	m.ShellMaxLifetime = 80 * time.Millisecond
	m.DesktopIdleTimeout = 0 // 只考察寿命字段不生效
	m.setJanitorInterval(30 * time.Millisecond)

	res, _ := m.Create(id, uuid.New(), proto.KindDesktop, []byte(`{}`))
	m.touch(res.Session, time.Now().Add(time.Hour)) // 持续活跃，逃过任何 idle

	time.Sleep(300 * time.Millisecond) // 跨越多个 janitor 周期
	s := m.SessionsOf(id, proto.KindDesktop)
	require.Len(t, s, 1, "desktop session must not be lifetime-closed by shell governance")
}

// TestCountActive：全表按 kind 计数（监控页聚合口径）——空表 0；混 kind
// 只数目标 kind；跨节点聚合（与 countByNodeLocked 的 per-node 口径对照）。
// registry 注入在线节点：Create 的 Online 检查在表操作之前。
func TestCountActive(t *testing.T) {
	reg := registry.New()
	n1, n2 := uuid.New(), uuid.New()
	reg.Add(&registry.NodeConn{NodeID: n1.String(), LastBeat: time.Now()})
	reg.Add(&registry.NodeConn{NodeID: n2.String(), LastBeat: time.Now()})
	m := New(reg, slog.Default())
	defer m.Close()
	require.Equal(t, 0, m.CountActive(proto.KindDesktop), "空表 = 0")
	_, apiErr := m.Create(n1, uuid.New(), proto.KindDesktop, json.RawMessage(`{}`))
	require.Nil(t, apiErr, "create desktop")
	_, apiErr = m.Create(n1, uuid.New(), proto.KindDesktop, json.RawMessage(`{}`))
	require.Nil(t, apiErr, "create desktop 2")
	_, apiErr = m.Create(n2, uuid.New(), proto.KindShell, json.RawMessage(`{}`))
	require.Nil(t, apiErr, "create shell")
	assert.Equal(t, 2, m.CountActive(proto.KindDesktop))
	assert.Equal(t, 1, m.CountActive(proto.KindShell))
}

// —— M2-Slice3 Task 4:per-node desktop lease 仲裁(spec §11.1)——

// TestDesktopLeaseArbitration:单授予/并发拒绝/持有者关闭释放/再授予;
// params 嵌入 leaseId 与 CreateResult 一致。
func TestDesktopLeaseArbitration(t *testing.T) {
	id, m := onlineMgr(t)
	defer m.Close()

	r1, apiErr := m.Create(id, uuid.New(), proto.KindDesktop, []byte(`{"signaling":"webrtc"}`))
	require.Nil(t, apiErr)
	assert.True(t, r1.LeaseGranted)
	assert.Len(t, r1.LeaseID, 16)
	// RTV：lease 为 server 侧簿记（relay input 门控键），params 不再嵌入
	// leaseId——只断言仲裁表与响应同源。
	sid, lid := m.DesktopLeaseOf(id)
	assert.Equal(t, r1.Session.ID, sid)
	assert.Equal(t, r1.LeaseID, lid)

	// 第二会话(仍在并发上限内):照常创建但 view-only。
	r2, apiErr := m.Create(id, uuid.New(), proto.KindDesktop, []byte(`{}`))
	require.Nil(t, apiErr)
	assert.False(t, r2.LeaseGranted)
	assert.Empty(t, r2.LeaseID)
	// 约仍归 r1。
	sid, _ = m.DesktopLeaseOf(id)
	assert.Equal(t, r1.Session.ID, sid)

	// 持有者关闭 → 释放 → 下一会话可授予。
	m.NotifyClose(r1.Session.ID, "client-gone")
	sid, _ = m.DesktopLeaseOf(id)
	assert.Empty(t, sid)
	r3, apiErr := m.Create(id, uuid.New(), proto.KindDesktop, []byte(`{}`))
	require.Nil(t, apiErr)
	assert.True(t, r3.LeaseGranted)
}

// TestDesktopLeaseIdleExpiry:持有者 >TTL 无信令活动 → janitor 撤约
// (会话保留 view-only);TTL 内活动续期。
func TestDesktopLeaseIdleExpiry(t *testing.T) {
	id, m := onlineMgr(t)
	defer m.Close()
	m.DesktopIdleTimeout = 0                   // 不让 idle 会话回收干扰
	m.DesktopLeaseTTL = 100 * time.Millisecond // 只考察 lease TTL
	m.setJanitorInterval(20 * time.Millisecond)

	r1, _ := m.Create(id, uuid.New(), proto.KindDesktop, []byte(`{}`))
	require.True(t, r1.LeaseGranted)

	// 活动续期:持续 touch 跨越多个 TTL 窗口,约不丢。
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		m.touch(r1.Session, time.Now())
		time.Sleep(30 * time.Millisecond)
	}
	if _, lid := m.DesktopLeaseOf(id); lid == "" {
		t.Fatal("active holder must keep the lease (activity renewal)")
	}

	// 停止活动 → TTL 内撤销;会话仍在(view-only)。
	require.Eventually(t, func() bool {
		_, lid := m.DesktopLeaseOf(id)
		return lid == ""
	}, 3*time.Second, 20*time.Millisecond, "idle lease must be revoked")
	assert.Len(t, m.SessionsOf(id, proto.KindDesktop), 1, "session survives lease expiry (view-only)")

	// 撤约后新会话可授予。
	r2, _ := m.Create(id, uuid.New(), proto.KindDesktop, []byte(`{}`))
	assert.True(t, r2.LeaseGranted)
}

// TestDesktopControlFlow:显式控制权流转——接管（抢占+冷却）/释放/幂等；
// 控制表与 DesktopLeaseOf 门控视角同源。
func TestDesktopControlFlow(t *testing.T) {
	id, m := onlineMgr(t)
	defer m.Close()
	m.DesktopStealCooldown = 200 * time.Millisecond

	u1, u2, u3 := uuid.New(), uuid.New(), uuid.New()
	r1, apiErr := m.Create(id, u1, proto.KindDesktop, []byte(`{}`))
	require.Nil(t, apiErr)
	require.True(t, r1.LeaseGranted, "first session passively granted")

	r2, apiErr := m.Create(id, u2, proto.KindDesktop, []byte(`{}`))
	require.Nil(t, apiErr)
	require.False(t, r2.LeaseGranted, "second session view-only at create")

	// r2 显式接管（r1 活约在手 → 抢占）。
	ok, reason := m.TakeDesktopControl(id, r2.Session.ID, time.Now())
	require.True(t, ok)
	assert.Equal(t, "taken", reason)
	sid, uid := m.DesktopControlOf(id)
	assert.Equal(t, r2.Session.ID, sid)
	assert.Equal(t, u2, uid)
	// 门控视角同步翻转：r1 输入被拒、r2 放行。
	holder, _ := m.DesktopLeaseOf(id)
	assert.Equal(t, r2.Session.ID, holder)

	// r3 冷却窗口内接管 → 拒绝。
	r3, apiErr := m.Create(id, u3, proto.KindDesktop, []byte(`{}`))
	require.Nil(t, apiErr)
	ok, reason = m.TakeDesktopControl(id, r3.Session.ID, time.Now())
	assert.False(t, ok)
	assert.Equal(t, "cooldown", reason)

	// r2 释放 → 空闲 → r3 接管成功 → 幂等。
	m.ReleaseDesktopControl(id, r2.Session.ID)
	sid, _ = m.DesktopControlOf(id)
	assert.Empty(t, sid)
	ok, reason = m.TakeDesktopControl(id, r3.Session.ID, time.Now())
	require.True(t, ok)
	assert.Equal(t, "granted", reason)
	ok, reason = m.TakeDesktopControl(id, r3.Session.ID, time.Now())
	assert.True(t, ok)
	assert.Equal(t, "held", reason)

	// 未知会话接管 → 拒绝。
	ok, reason = m.TakeDesktopControl(id, "no-such-session", time.Now())
	assert.False(t, ok)
	assert.Equal(t, "no-session", reason)
}
