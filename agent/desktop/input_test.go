// input_test.go — Task 3 确定性单测(无 WebRTC):0x0108 编码黄金字节、
// 预校验(§11.7:尺寸/按钮位/seq 单调/无 lease/预算)、move 合并(≤500Hz,
// barrier 冲刷)、lease 表语义(授予/拒绝/断连释放/30s idle 撤销)。
// 时钟全部注入(now/after),无真实 sleep 依赖;loopback(真实双 PC)见
// input_loopback_test.go。
package desktop

import (
	"encoding/binary"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// ---- 手写黄金字节辅助(独立于实现的第二实现,镜像 C++ EncodeInputMsg) ----

func gPut16(p []byte, off int, v uint16) { binary.LittleEndian.PutUint16(p[off:], v) }
func gPut32(p []byte, off int, v uint32) { binary.LittleEndian.PutUint32(p[off:], v) }
func gPut64(p []byte, off int, v uint64) { binary.LittleEndian.PutUint64(p[off:], v) }

// s32u:负常量直接转 uint32 是编译错(常量溢出),经变量转即可。
func s32u(v int32) uint32 { return uint32(v) }

// encMouseMsg 构造 mouse 通道 payload [u64 seq][s32 x][s32 y][u16 buttons]。
func encMouseMsg(seq uint64, x, y int32, buttons uint16) []byte {
	b := make([]byte, 18)
	gPut64(b, 0, seq)
	gPut32(b, 8, uint32(x))
	gPut32(b, 12, uint32(y))
	gPut16(b, 16, buttons)
	return b
}

// encInputMsg 构造 input 通道 payload [u64 seq][u8 type][payload']。
func encInputMsg(seq uint64, typ uint8, payload []byte) []byte {
	b := make([]byte, 9+len(payload))
	gPut64(b, 0, seq)
	b[8] = typ
	copy(b[9:], payload)
	return b
}

func keyPayload(scan uint16, down, extended uint8) []byte {
	p := make([]byte, 4)
	gPut16(p, 0, scan)
	p[2], p[3] = down, extended
	return p
}

func textPayload(units []uint16) []byte {
	p := make([]byte, 2+2*len(units))
	gPut16(p, 0, uint16(len(units)))
	for i, u := range units {
		gPut16(p, 2+2*i, u)
	}
	return p
}

// manualTimerQueue 是 after 注入:定时器入队,fireDue 按当前假时钟触发。
type manualTimerQueue struct {
	mu     sync.Mutex
	now    func() time.Time
	timers []*manualTimer
}

type manualTimer struct {
	at      time.Time
	fn      func()
	stopped bool
}

func (q *manualTimerQueue) after(d time.Duration, fn func()) func() {
	q.mu.Lock()
	defer q.mu.Unlock()
	t := &manualTimer{at: q.now().Add(d), fn: fn}
	q.timers = append(q.timers, t)
	return func() { t.stopped = true }
}

func (q *manualTimerQueue) fireDue() {
	q.mu.Lock()
	now := q.now()
	var due []func()
	keep := q.timers[:0]
	for _, t := range q.timers {
		if !t.stopped && !t.at.After(now) {
			due = append(due, t.fn)
		} else {
			keep = append(keep, t)
		}
	}
	q.timers = keep
	q.mu.Unlock()
	for _, fn := range due {
		fn()
	}
}

// newTestController 组装一个不依赖 WebRTC 的 controller(假时钟由调用方接)。
func newTestController(t *testing.T, subID uint32) (*inputController, *inputFakeSource, *leaseTable) {
	t.Helper()
	host := newInputHost()
	src := &inputFakeSource{host: host, subID: subID,
		frameCh: make(chan Frame, 8), stateCh: make(chan StateEvent, 4),
		cursorCh: make(chan CursorEvent, 4), done: make(chan struct{})}
	tbl := newLeaseTable()
	return newInputController(src, tbl, slog.Default()), src, tbl
}

func stat(t *testing.T, c *inputController, f func(InputStats) uint64) uint64 {
	t.Helper()
	return f(c.stats())
}

// TestEncodeInputWireGoldens:agent 产出的 0x0108 payload 逐字节镜像
// native/desktop/rt_pipe_server.h EncodeInputMsg(双实现交叉钉死布局)。
func TestEncodeInputWireGoldens(t *testing.T) {
	// MOVE: 23B [u32 sub][u64 seq][u8 1][s32 x][s32 y][u16 buttons]
	got := encodeInputWire(7, &InputMsg{Seq: 11, Type: inputTypeMove, X: -5, Y: 300, Buttons: 3})
	want := make([]byte, 23)
	gPut32(want, 0, 7)
	gPut64(want, 4, 11)
	want[12] = 1
	gPut32(want, 13, s32u(-5))
	gPut32(want, 17, 300)
	gPut16(want, 21, 3)
	if string(got) != string(want) {
		t.Fatalf("MOVE wire = %x, want %x", got, want)
	}
	// BUTTON: 15B [u8 btn][u8 down]
	got = encodeInputWire(7, &InputMsg{Seq: 12, Type: inputTypeButton, Btn: 2, Down: 1})
	want = make([]byte, 15)
	gPut32(want, 0, 7)
	gPut64(want, 4, 12)
	want[12] = 2
	want[13], want[14] = 2, 1
	if string(got) != string(want) {
		t.Fatalf("BUTTON wire = %x, want %x", got, want)
	}
	// WHEEL: 22B [s32 dx][s32 dy][u8 trackpad]
	got = encodeInputWire(7, &InputMsg{Seq: 13, Type: inputTypeWheel, X: -3, Y: 120, Trackpad: 1})
	want = make([]byte, 22)
	gPut32(want, 0, 7)
	gPut64(want, 4, 13)
	want[12] = 3
	gPut32(want, 13, s32u(-3))
	gPut32(want, 17, 120)
	want[21] = 1
	if string(got) != string(want) {
		t.Fatalf("WHEEL wire = %x, want %x", got, want)
	}
	// KEY: 17B [u16 scan][u8 down][u8 extended]
	got = encodeInputWire(7, &InputMsg{Seq: 14, Type: inputTypeKey, Scan: 0x1E, Down: 1, Extended: 1})
	want = make([]byte, 17)
	gPut32(want, 0, 7)
	gPut64(want, 4, 14)
	want[12] = 4
	gPut16(want, 13, 0x1E)
	want[15], want[16] = 1, 1
	if string(got) != string(want) {
		t.Fatalf("KEY wire = %x, want %x", got, want)
	}
	// TEXT: 15+2n [u16 len][utf16le]
	got = encodeInputWire(7, &InputMsg{Seq: 15, Type: inputTypeText, Text: []uint16{0x68, 0x4E2D}})
	want = make([]byte, 19)
	gPut32(want, 0, 7)
	gPut64(want, 4, 15)
	want[12] = 5
	gPut16(want, 13, 2)
	gPut16(want, 15, 0x68)
	gPut16(want, 17, 0x4E2D)
	if string(got) != string(want) {
		t.Fatalf("TEXT wire = %x, want %x", got, want)
	}
	// LOCK: 15B [u8 caps][u8 num]
	got = encodeInputWire(7, &InputMsg{Seq: 16, Type: inputTypeLock, Caps: 1})
	want = make([]byte, 15)
	gPut32(want, 0, 7)
	gPut64(want, 4, 16)
	want[12] = 6
	want[13] = 1
	if string(got) != string(want) {
		t.Fatalf("LOCK wire = %x, want %x", got, want)
	}
}

// TestInputPreValidation:尺寸/形状/按钮位/seq/lease 门,全确定性(假时钟)。
func TestInputPreValidation(t *testing.T) {
	c, src, _ := newTestController(t, 9)

	// 无 lease:合法 move 也必须丢弃(计数,不断连)。
	c.handleMouse(encMouseMsg(1, 5, 5, 0))
	if got := stat(t, c, func(s InputStats) uint64 { return s.NoLease }); got != 1 {
		t.Fatalf("noLease = %d, want 1", got)
	}
	if n := src.host.count(); n != 0 {
		t.Fatalf("host saw %d msgs before lease", n)
	}

	// 授予后预校验逐项。
	if _, ok := c.grantLease(nil); !ok {
		t.Fatal("first lease request must be granted")
	}
	c.handleMouse(make([]byte, maxInputMsgBytes+1)) // 2049B → oversize
	c.handleMouse(make([]byte, 10))                 // 短帧 → invalid
	c.handleInput(make([]byte, 4))                  // 短帧 → invalid
	c.handleMouse(encMouseMsg(2, 1, 1, 0x20))       // buttons > 0x1F → invalid
	c.handleInput(encInputMsg(3, inputTypeMove, make([]byte, 10))) // MOVE 走 input 通道 → invalid
	if got := stat(t, c, func(s InputStats) uint64 { return s.Oversize }); got != 1 {
		t.Fatalf("oversize = %d, want 1", got)
	}
	if got := stat(t, c, func(s InputStats) uint64 { return s.Invalid }); got != 4 {
		t.Fatalf("invalid = %d, want 4", got)
	}

	// 合法 MOVE(seq 1 被无 lease 丢弃时未消耗)。
	c.handleMouse(encMouseMsg(1, 100, 200, 1))
	waitCount(t, src.host, 1)
	rec := src.host.records()[0]
	if rec.Type != inputTypeMove || rec.X != 100 || rec.Y != 200 || rec.Buttons != 1 || rec.SubID != 9 {
		t.Fatalf("MOVE rec = %+v", rec)
	}

	// seq 回退 → 丢弃。
	c.handleMouse(encMouseMsg(1, 1, 1, 0))
	if got := stat(t, c, func(s InputStats) uint64 { return s.StaleSeq }); got != 1 {
		t.Fatalf("staleSeq = %d, want 1", got)
	}
	if n := src.host.count(); n != 1 {
		t.Fatalf("host saw %d after stale", n)
	}

	// KEY scan=0 / TEXT 长度不符 → invalid。
	c.handleInput(encInputMsg(2, inputTypeKey, keyPayload(0, 1, 0)))
	c.handleInput(encInputMsg(3, inputTypeText, append(textPayload([]uint16{0x41}), 0)))
	if got := stat(t, c, func(s InputStats) uint64 { return s.Invalid }); got != 6 {
		t.Fatalf("invalid = %d, want 6", got)
	}

	// 合法 KEY + TEXT。
	c.handleInput(encInputMsg(2, inputTypeKey, keyPayload(0x1E, 1, 0)))
	c.handleInput(encInputMsg(3, inputTypeText, textPayload([]uint16{0x41})))
	waitCount(t, src.host, 3)
	if got := stat(t, c, func(s InputStats) uint64 { return s.Sent }); got != 3 {
		t.Fatalf("sent = %d, want 3", got)
	}
}

// TestInputBudget1000eps:同一 1s 窗口内转发 ≤1000;窗口滑动后恢复。
func TestInputBudget1000eps(t *testing.T) {
	c, src, _ := newTestController(t, 9)
	clock := time.Unix(0, 0)
	c.now = func() time.Time { return clock }
	q := &manualTimerQueue{now: c.now}
	c.after = q.after
	if _, ok := c.grantLease(nil); !ok {
		t.Fatal("grant failed")
	}
	send := func(seq uint64) {
		c.handleInput(encInputMsg(seq, inputTypeKey, keyPayload(uint16(seq%255)+1, 1, 0)))
	}
	for i := 1; i <= inputEPSBudget; i++ {
		send(uint64(i))
	}
	if got := stat(t, c, func(s InputStats) uint64 { return s.Sent }); got != inputEPSBudget {
		t.Fatalf("sent = %d, want %d", got, inputEPSBudget)
	}
	send(inputEPSBudget + 1) // 第 1001 条 → 预算丢弃
	if got := stat(t, c, func(s InputStats) uint64 { return s.BudgetDropped }); got != 1 {
		t.Fatalf("budgetDropped = %d, want 1", got)
	}
	if n := src.host.count(); n != inputEPSBudget {
		t.Fatalf("host count = %d, want %d", n, inputEPSBudget)
	}
	// 时钟前进 1s:窗口清空,恢复转发。
	clock = clock.Add(inputBudgetWindow + time.Millisecond)
	send(inputEPSBudget + 2)
	waitCount(t, src.host, inputEPSBudget+1)
}

// TestMoveCoalescing:≤500Hz,中间丢弃保留最新;barrier(KEY 等)先冲刷。
func TestMoveCoalescing(t *testing.T) {
	c, src, _ := newTestController(t, 9)
	clock := time.Unix(0, 0)
	c.now = func() time.Time { return clock }
	q := &manualTimerQueue{now: c.now}
	c.after = q.after
	if _, ok := c.grantLease(nil); !ok {
		t.Fatal("grant failed")
	}
	// t0:seq1 立即转发;seq2/3/4 同瞬到达 → 合并(保 4)。
	c.handleMouse(encMouseMsg(1, 10, 10, 0))
	c.handleMouse(encMouseMsg(2, 20, 10, 0))
	c.handleMouse(encMouseMsg(3, 30, 10, 0))
	c.handleMouse(encMouseMsg(4, 40, 10, 0))
	if n := src.host.count(); n != 1 {
		t.Fatalf("host count = %d after burst, want 1 (immediate move only)", n)
	}
	// 推进 3ms 触发冲刷:最新(seq4)到达。
	clock = clock.Add(3 * time.Millisecond)
	q.fireDue()
	waitCount(t, src.host, 2)
	recs := src.host.records()
	if recs[0].Seq != 1 || recs[1].Seq != 4 {
		t.Fatalf("coalesced seqs = %d,%d, want 1,4", recs[0].Seq, recs[1].Seq)
	}
	// 同瞬 seq5 进入 pending;KEY(seq6)是 barrier:先冲刷 seq5 再转发 KEY。
	c.handleMouse(encMouseMsg(5, 50, 10, 0))
	c.handleInput(encInputMsg(6, inputTypeKey, keyPayload(0x1E, 1, 0)))
	waitCount(t, src.host, 4)
	recs = src.host.records()
	if recs[2].Seq != 5 || recs[2].Type != inputTypeMove || recs[3].Seq != 6 || recs[3].Type != inputTypeKey {
		t.Fatalf("barrier order broken: %+v", recs[2:])
	}
	if got := stat(t, c, func(s InputStats) uint64 { return s.Coalesced }); got != 4 {
		t.Fatalf("coalesced = %d, want 4 (burst seq2/3/4 dropped-keep-latest + seq5 held-then-flushed)", got)
	}
}

// TestLeaseTable:授予/拒绝/断连释放/idle 撤销(真定时器,短 idle)。
func TestLeaseTable(t *testing.T) {
	tbl := newLeaseTable()
	tbl.idle = 60 * time.Millisecond

	var mu sync.Mutex
	reasons := []string{}
	notify := func(r string) { mu.Lock(); reasons = append(reasons, r); mu.Unlock() }

	id1, ok := tbl.request(notify)
	if !ok || len(id1) != 16 {
		t.Fatalf("grant = %q ok=%v", id1, ok)
	}
	if _, ok := tbl.request(notify); ok {
		t.Fatal("second request must be denied while held")
	}
	if !tbl.allows(id1) {
		t.Fatal("holder must be allowed")
	}
	if tbl.allows("bogus") || tbl.allows("") {
		t.Fatal("non-holder must be rejected")
	}
	tbl.release(id1, "disconnect")
	if tbl.allows(id1) {
		t.Fatal("released lease must be rejected")
	}
	mu.Lock()
	if len(reasons) != 1 || reasons[0] != "disconnect" {
		t.Fatalf("reasons = %v, want [disconnect]", reasons)
	}
	mu.Unlock()

	// idle:60ms 无输入 → revoke("idle") + 表释放 → 可再授予。
	revoked := make(chan string, 1)
	id2, ok := tbl.request(func(r string) { revoked <- r })
	if !ok {
		t.Fatal("re-grant after release failed")
	}
	if !tbl.allows(id2) {
		t.Fatal("fresh holder must be allowed")
	}
	select {
	case r := <-revoked:
		if r != "idle" {
			t.Fatalf("idle revoke reason = %q", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idle revoke never fired")
	}
	if _, ok := tbl.request(nil); !ok {
		t.Fatal("lease must be free after idle revoke")
	}
}

// TestHeldKeyReleaseOnClose:会话终结合成 KEY/BUTTON up(卡键清理),seq 续接。
func TestHeldKeyReleaseOnClose(t *testing.T) {
	c, src, tbl := newTestController(t, 9)
	if _, ok := c.grantLease(nil); !ok {
		t.Fatal("grant failed")
	}
	c.handleInput(encInputMsg(1, inputTypeKey, keyPayload(0x1E, 1, 0)))
	c.handleInput(encInputMsg(2, inputTypeKey, keyPayload(0x2A, 1, 1))) // extended LShift
	c.handleInput(encInputMsg(3, inputTypeButton, []byte{1, 1}))        // L down
	c.handleInput(encInputMsg(4, inputTypeKey, keyPayload(0x1E, 0, 0))) // A up → 不再持有
	waitCount(t, src.host, 4)
	c.close()
	waitCount(t, src.host, 6) // KEY up(0x2A ext) + BUTTON up(1)
	recs := src.host.records()
	if len(recs) != 6 {
		t.Fatalf("recs = %d", len(recs))
	}
	// 合成 up 的 seq 严格续接(5、6),host 无 stale 丢弃。
	for i, r := range recs {
		if r.Seq != uint64(i+1) {
			t.Fatalf("seq order broken at %d: %+v", i, r)
		}
	}
	last := recs[5]
	if last.Type != inputTypeButton || last.Btn != 1 || last.Down != 0 {
		t.Fatalf("button up rec = %+v", last)
	}
	if recs[4].Type != inputTypeKey || recs[4].Scan != 0x2A || recs[4].Extended != 1 || recs[4].Down != 0 {
		t.Fatalf("key up rec = %+v", recs[4])
	}
	// close 后 lease 已释放。
	if tbl.allows(c.currentLease()) {
		t.Fatal("lease must be released after close")
	}
	// 幂等。
	c.close()
	waitCount(t, src.host, 6)
}

// TestIdleRevokeClearsHeldTracking(T3 review Minor 2 回归):持有者按住键
// → idle 撤销 → 新持有者按下同键 → 旧持有者 WS 关闭不得合成 up 抬掉新
// 持有者的键。撤销即清跟踪;物理残留由 host 侧 janitor 兜底。假时钟驱动
// (controller 与 lease 表共享 manualTimerQueue),idle 只在推进时钟时发生。
func TestIdleRevokeClearsHeldTracking(t *testing.T) {
	c, src, tbl := newTestController(t, 9)
	clock := time.Unix(0, 0)
	c.now = func() time.Time { return clock }
	q := &manualTimerQueue{now: c.now}
	c.after = q.after
	tbl.now, tbl.after = c.now, q.after
	tbl.idle = 30 * time.Second

	revoked := make(chan string, 1)
	if _, ok := c.grantLease(func(r string) { revoked <- r }); !ok {
		t.Fatal("grant failed")
	}
	// 旧持有者按下 A;30s 无输入 → idle 撤销。
	c.handleInput(encInputMsg(1, inputTypeKey, keyPayload(0x1E, 1, 0)))
	waitCount(t, src.host, 1)
	clock = clock.Add(tbl.idle + time.Millisecond)
	q.fireDue()
	select {
	case r := <-revoked:
		if r != "idle" {
			t.Fatalf("revoke reason = %q, want idle", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idle revoke never fired")
	}

	// 新持有者(共享同一 lease 表/同一 host 全局键态)取得 lease 并按下 A。
	src2 := &inputFakeSource{host: src.host, subID: 10,
		frameCh: make(chan Frame, 8), stateCh: make(chan StateEvent, 4),
		cursorCh: make(chan CursorEvent, 4), done: make(chan struct{})}
	c2 := newInputController(src2, tbl, slog.Default())
	c2.now, c2.after = c.now, q.after
	if _, ok := c2.grantLease(nil); !ok {
		t.Fatal("re-grant after idle revoke failed")
	}
	c2.handleInput(encInputMsg(1, inputTypeKey, keyPayload(0x1E, 1, 0)))
	waitCount(t, src.host, 2)
	if !tbl.allows(c2.currentLease()) {
		t.Fatal("new holder must hold the lease")
	}

	// 旧持有者现在才断连(close 同步完成):不得合成任何 up。
	c.close()
	recs := src.host.records()
	if len(recs) != 2 {
		t.Fatalf("old holder close synthesized %d extra msgs: %+v", len(recs)-2, recs[2:])
	}
	last := recs[1]
	if last.Type != inputTypeKey || last.Scan != 0x1E || last.Down != 1 || last.SubID != 10 {
		t.Fatalf("new holder key-down must be the last host record: %+v", last)
	}
	// 新持有者随后正常 close(未 idle):仍应释放自己的键(修复不破坏正常路径)。
	c2.close()
	waitCount(t, src.host, 3)
	up := src.host.records()[2]
	if up.Type != inputTypeKey || up.Scan != 0x1E || up.Down != 0 || up.SubID != 10 {
		t.Fatalf("new holder synthetic up = %+v", up)
	}
}

// waitCount 轮询 host 记录数到 n(转发异步经 controller 锁外计数)。
func waitCount(t *testing.T, h *inputHost, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for h.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("host count = %d, want %d (recs=%+v)", h.count(), n, h.records())
		}
		time.Sleep(10 * time.Millisecond)
	}
}
