// arbiter.go — 传输级控制权仲裁机（relay-plane spec §3.5）。
//
// 语义逐条对齐原 server session manager（manager.go TakeDesktopControl 一族）：
//   - 自持重取 = 幂等成功（held；传输层切换重连后重取）；
//   - 空闲/持有者失活 = 授予（granted）；
//   - 他人活约在手 = 抢占（last-take-wins，taken），但 Cooldown 窗口内
//     拒绝（防对点互抢，cooldown）；
//   - 租约 TTL 内无活动即失活（连接存活续约走 Touch）。
//
// 差异仅一处（spec §3.5）：准入判据从"manager 租约表 + capabilities 计算"
// 变为 ticket claims 的 cap——策略留在签发侧（server），relay 不发版。
// 被动首约（Grant）保留 desktopStart 的先到先得语义：仅空闲时授予，
// 不抢活约。
package rtv

import (
	"sync"
	"time"
)

// Arbiter per-node 控制权租约表（relay 本地内存软状态）。
type Arbiter struct {
	mu       sync.Mutex
	leases   map[string]*ctrlLease // node → lease
	TTL      time.Duration         // 默认 60s（对齐 manager.DesktopLeaseTTL）
	Cooldown time.Duration         // 默认 3s（对齐 manager.DesktopStealCooldown）
}

type ctrlLease struct {
	SessionID    string
	Name         string
	TakenAt      time.Time
	LastActivity time.Time
	LastStealAt  time.Time
}

func NewArbiter() *Arbiter {
	return &Arbiter{
		leases:   map[string]*ctrlLease{},
		TTL:      60 * time.Second,
		Cooldown: 3 * time.Second,
	}
}

// Grant 被动首约（会话创建时先到先得）：空闲/持有者失活才授予。
func (a *Arbiter) Grant(node, session, name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if l := a.leases[node]; l != nil && a.aliveLocked(node, now) {
		return false
	}
	a.leases[node] = &ctrlLease{SessionID: session, Name: name,
		TakenAt: now, LastActivity: now}
	return true
}

// Take 显式接管（viewer takeControl 消息）。canControl = ticket claims 的
// cap.control；false → "capability"（策略性拒绝，relay 不认识角色为何物）。
func (a *Arbiter) Take(node, session, name string, canControl bool) (ok bool, reason string) {
	if !canControl {
		return false, "capability"
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	l := a.leases[node]
	if l == nil {
		a.leases[node] = &ctrlLease{SessionID: session, Name: name,
			TakenAt: now, LastActivity: now}
		return true, "granted"
	}
	if l.SessionID == session {
		l.LastActivity = now
		return true, "held" // 幂等
	}
	if a.aliveLocked(node, now) {
		if now.Sub(l.LastStealAt) < a.Cooldown {
			return false, "cooldown"
		}
		l.LastStealAt = now
	}
	l.SessionID, l.Name, l.TakenAt, l.LastActivity = session, name, now, now
	return true, "taken"
}

// Release 显式释放（仅持有者有效；非持有者 = 无操作）。
func (a *Arbiter) Release(node, session string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if l := a.leases[node]; l != nil && l.SessionID == session {
		delete(a.leases, node)
	}
}

// Holder 当前归属（TTL 内活约才算；空串 = 空闲）。controlState 广播用。
func (a *Arbiter) Holder(node string) (session, name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if l := a.leases[node]; l != nil && a.aliveLocked(node, time.Now()) {
		return l.SessionID, l.Name
	}
	return "", ""
}

// Touch 持有者活动续约（viewer 控制帧频率远高于 TTL，够用）。
func (a *Arbiter) Touch(node, session string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if l := a.leases[node]; l != nil && l.SessionID == session {
		l.LastActivity = time.Now()
	}
}

// DropSession 会话级清理（viewer 断开）：持有者离场即撤约，别让死会话
// 占着控制权等 TTL。
func (a *Arbiter) DropSession(node, session string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if l := a.leases[node]; l != nil && l.SessionID == session {
		delete(a.leases, node)
	}
}

func (a *Arbiter) aliveLocked(node string, now time.Time) bool {
	l := a.leases[node]
	return l != nil && now.Sub(l.LastActivity) <= a.TTL
}
