// input.go — viewer 输入/光标通道 + agent 侧 lease 仲裁与预校验
// (M1-Slice3 Task 3;spec §11 输入、§7.8 光标、§11.7 预校验)。
//
// == DataChannel 词汇(在 offer 应答前创建;T4/T5 契约)==
//
//	"input"  reliable/ordered   viewer→agent:[u64 seq][u8 type][payload']
//	                              type ∈ BUTTON/WHEEL/KEY/TEXT/LOCK(payload'
//	                              与 0x0108 逐字节相同,见下);MOVE 只走
//	                              mouse 通道,出现在 input 通道即丢弃计数。
//	"mouse"  unreliable/unordered viewer→agent:MOVE 变体
//	                              [u64 seq][s32 x][s32 y][u16 buttons]
//	                              (无 sub_id,agent 填充)。
//	"cursor" unreliable/unordered agent→viewer:[s32 x][s32 y][u8 visible]
//	                              ← pipe 0x0109 事件透传。
//
// agent 收到后编成 0x0108 payload `[u32 sub_id][u64 seq][u8 type][payload']`
// (sub_id = 本会话 ATTACH 的 SubID)经 Source.SendInput 注入 pipe;布局与
// native/desktop/rt_pipe_server.h EncodeInputMsg 逐字节一致(双实现交叉,
// 见 input_test.go 黄金字节):
//
//	1 MOVE   [s32 x][s32 y][u16 buttons]   buttons bit 1L 2R 4M 8X1 16X2
//	2 BUTTON [u8 btn][u8 down]             btn = mask 值 1/2/4/8/16
//	3 WHEEL  [s32 dx][s32 dy][u8 trackpad]
//	4 KEY    [u16 scan][u8 down][u8 extended]
//	5 TEXT   [u16 len][utf16le units]      代理对按 unit 透传
//	6 LOCK   [u8 caps][u8 num]
//
// == lease(control WS 信令,M1 简化仲裁;server 侧 = M2,spec §11.1 偏差
//
//	   已裁决)==
//
//		viewer → agent:{"type":"lease_request"}
//		agent → viewer:{"type":"lease_granted","leaseId":"<16 hex>"}
//		              {"type":"lease_denied","reason":"held"}
//		              {"type":"lease_revoked","reason":"idle"|"disconnect"}
//
// 首请求者得;持有者 WS 断连(Handle 返回)或 30s 无输入 → 撤销并广播
// lease_revoked;非持有者输入丢弃+计数(不断连)。lease 表每 Handler 一份
// (= 一个 Starter = 一个采集实例;多 viewer 会话共享)。
//
// == 预校验(§11.7;全部丢弃+计数)==
//
//	消息 ≤2KiB;按钮位 ≤0x1F;seq 严格递增(跨两通道一个计数器——0x0108
//	seq 在 host 按 sub_id 单调,T4 须用同一全局计数器);MOVE ≤500Hz(中间
//
// 丢弃保留最新,barrier 事件先冲刷 pending);总转发 ≤1000eps(1s 滑窗)。
//
// 会话终结:对本会话仍记录为 down 的键/钮合成 KEY/BUTTON up(seq 续接,
// host 全局键态中只有持有者的输入会被注入,故全部可安全释放——卡键清理),
// 再释放 lease。全部计数随 close 记一条日志(无凭据)。
package desktop

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

// DataChannel 标签(T4/T5 契约)。
const (
	dcLabelInput  = "input"
	dcLabelMouse  = "mouse"
	dcLabelCursor = "cursor"
)

// 0x0108 type 值(native/desktop/rt_pipe_server.h 镜像)。
const (
	inputTypeMove   uint8 = 1
	inputTypeButton uint8 = 2
	inputTypeWheel  uint8 = 3
	inputTypeKey    uint8 = 4
	inputTypeText   uint8 = 5
	inputTypeLock   uint8 = 6
)

// 预校验参数(§11.7)。
const (
	maxInputMsgBytes  = 2048
	maxInputTextUnits = 512
	btnMaskAny        = uint16(0x1F)
	moveMinGap        = 2 * time.Millisecond // ≤500Hz
	inputEPSBudget    = 1000
	inputBudgetWindow = time.Second
	leaseIdleTimeout  = 30 * time.Second
	cursorWireBytes   = 9
	mouseMsgWireBytes = 18 // [u64 seq][s32 x][s32 y][u16 buttons]
)

// InputMsg 是一条已解码的 viewer 输入消息(0x0108 载荷除 sub_id 外)。
type InputMsg struct {
	Seq      uint64
	Type     uint8
	X, Y     int32 // MOVE 坐标 / WHEEL dx,dy
	Buttons  uint16
	Btn      uint8
	Down     uint8 // BUTTON/KEY 共用
	Trackpad uint8
	Scan     uint16
	Extended uint8
	Text     []uint16
	Caps     uint8
	Num      uint8
}

// encodeInputWire 编码完整 0x0108 payload(与 C++ EncodeInputMsg 逐字节
// 一致;type 非法返回 nil)。
func encodeInputWire(subID uint32, m *InputMsg) []byte {
	var p []byte
	switch m.Type {
	case inputTypeMove:
		p = make([]byte, 23)
		binary.LittleEndian.PutUint32(p[13:], uint32(m.X))
		binary.LittleEndian.PutUint32(p[17:], uint32(m.Y))
		binary.LittleEndian.PutUint16(p[21:], m.Buttons)
	case inputTypeButton:
		p = make([]byte, 15)
		p[13], p[14] = m.Btn, m.Down
	case inputTypeWheel:
		p = make([]byte, 22)
		binary.LittleEndian.PutUint32(p[13:], uint32(m.X))
		binary.LittleEndian.PutUint32(p[17:], uint32(m.Y))
		p[21] = m.Trackpad
	case inputTypeKey:
		p = make([]byte, 17)
		binary.LittleEndian.PutUint16(p[13:], m.Scan)
		p[15], p[16] = m.Down, m.Extended
	case inputTypeText:
		p = make([]byte, 15+2*len(m.Text))
		binary.LittleEndian.PutUint16(p[13:], uint16(len(m.Text)))
		for i, u := range m.Text {
			binary.LittleEndian.PutUint16(p[15+2*i:], u)
		}
	case inputTypeLock:
		p = make([]byte, 15)
		p[13], p[14] = m.Caps, m.Num
	default:
		return nil
	}
	binary.LittleEndian.PutUint32(p, subID)
	binary.LittleEndian.PutUint64(p[4:], m.Seq)
	p[12] = m.Type
	return p
}

// ---- lease 表(每 Handler = 每采集实例一份)----

// leaseGrant 是一次持有记录;idle 定时器链在最后输入后 idle 无新输入时撤销。
type leaseGrant struct {
	id        string
	notify    func(reason string)
	lastInput time.Time
	stop      func() // 停掉当前 idle 定时器(nil 安全)
}

// leaseTable 串行化授予/释放/idle 检查;idle 与 now/after 可注入(测试)。
type leaseTable struct {
	mu     sync.Mutex
	holder *leaseGrant

	idle  time.Duration
	now   func() time.Time
	after func(time.Duration, func()) func()
}

func newLeaseTable() *leaseTable {
	return &leaseTable{
		idle:  leaseIdleTimeout,
		now:   time.Now,
		after: stoppableAfterFunc,
	}
}

// stoppableAfterFunc 把 time.AfterFunc 包成返回 stop 的 after 形态。
func stoppableAfterFunc(d time.Duration, fn func()) func() {
	tm := time.AfterFunc(d, fn)
	return func() { tm.Stop() }
}

// request 授予(空闲时)或拒绝;notify 在撤销(idle)时被调用。
func (t *leaseTable) request(notify func(reason string)) (string, bool) {
	t.mu.Lock()
	if t.holder != nil {
		t.mu.Unlock()
		return "", false
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	g := &leaseGrant{id: hex.EncodeToString(b[:]), notify: notify, lastInput: t.now()}
	t.holder = g
	g.stop = t.armIdleLocked(g)
	t.mu.Unlock()
	return g.id, true
}

// armIdleLocked 布防 idle 检查;调用方持 mu。
func (t *leaseTable) armIdleLocked(g *leaseGrant) func() {
	return t.after(t.idle, func() { t.idleCheck(g) })
}

// idleCheck 是定时器回调:无输入满 idle → 撤销;否则按剩余时间重布防。
func (t *leaseTable) idleCheck(g *leaseGrant) {
	t.mu.Lock()
	if t.holder != g {
		t.mu.Unlock()
		return // 已释放/被替换
	}
	elapsed := t.now().Sub(g.lastInput)
	if elapsed >= t.idle {
		t.holder = nil
		t.mu.Unlock()
		if g.notify != nil {
			g.notify("idle")
		}
		return
	}
	g.stop = t.after(t.idle-elapsed, func() { t.idleCheck(g) })
	t.mu.Unlock()
}

// allows 判定 id 是否当前持有者;是则顺带刷新 idle 基准。
func (t *leaseTable) allows(id string) bool {
	if id == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.holder == nil || t.holder.id != id {
		return false
	}
	t.holder.lastInput = t.now()
	return true
}

// release 主动释放(持有者会话终结);notify(reason) 尽力送达。
func (t *leaseTable) release(id, reason string) {
	t.mu.Lock()
	g := t.holder
	if g == nil || g.id != id {
		t.mu.Unlock()
		return
	}
	t.holder = nil
	stop := g.stop
	t.mu.Unlock()
	if stop != nil {
		stop()
	}
	if g.notify != nil {
		g.notify(reason)
	}
}

// ---- 计数器 ----

type inputCounters struct {
	mouseMsgs     atomic.Uint64
	inputMsgs     atomic.Uint64
	oversize      atomic.Uint64
	invalid       atomic.Uint64
	noLease       atomic.Uint64
	staleSeq      atomic.Uint64
	budgetDropped atomic.Uint64
	coalesced     atomic.Uint64
	sent          atomic.Uint64
	sendFailed    atomic.Uint64
	cursorSent    atomic.Uint64
	cursorFailed  atomic.Uint64
}

// InputStats 是计数快照(日志/T6 观测;无凭据)。
type InputStats struct {
	MouseMsgs     uint64
	InputMsgs     uint64
	Oversize      uint64
	Invalid       uint64
	NoLease       uint64
	StaleSeq      uint64
	BudgetDropped uint64
	Coalesced     uint64
	Sent          uint64
	SendFailed    uint64
	CursorSent    uint64
	CursorFailed  uint64
}

// ---- inputController:每会话一条 ----

type inputController struct {
	src      Source
	tbl      *leaseTable
	log      *slog.Logger
	counters inputCounters

	// now/after 可注入(确定性单测)。
	now   func() time.Time
	after func(time.Duration, func()) func()

	mu       sync.Mutex
	leaseID  string
	lastSeq  uint64
	fwdAt    []time.Time // 1s 滑窗内转发时戳(≤1000eps 预算)
	pendMove *InputMsg   // ≤500Hz 合并的待发 MOVE(保留最新)
	pendStop func()      // 冲刷定时器的 stop(armed 标记)
	lastFwd  time.Time
	heldKeys map[uint32]struct{} // scan | extended<<16
	heldBtns map[uint8]struct{}  // mask 值

	closed atomic.Bool

	inputDC  *webrtc.DataChannel
	mouseDC  *webrtc.DataChannel
	cursorDC *webrtc.DataChannel
}

func newInputController(src Source, tbl *leaseTable, log *slog.Logger) *inputController {
	if log == nil {
		log = slog.Default()
	}
	return &inputController{
		src: src, tbl: tbl, log: log,
		now: time.Now, after: stoppableAfterFunc,
		heldKeys: map[uint32]struct{}{}, heldBtns: map[uint8]struct{}{},
	}
}

// newDataChannel 在 publisher PC 上建一条 DC(lossy = 不可靠无序)。
func (p *Publisher) newDataChannel(label string, lossy bool) (*webrtc.DataChannel, error) {
	ordered := true
	opts := &webrtc.DataChannelInit{Ordered: &ordered}
	if lossy {
		no := uint16(0)
		f := false
		opts.MaxRetransmits = &no
		opts.Ordered = &f
	}
	return p.pc.CreateDataChannel(label, opts)
}

// attach 在 offer 应答前创建三条通道并接 OnMessage。恰调用一次。
func (c *inputController) attach(pub *Publisher) error {
	var err error
	if c.inputDC, err = pub.newDataChannel(dcLabelInput, false); err != nil {
		return err
	}
	if c.mouseDC, err = pub.newDataChannel(dcLabelMouse, true); err != nil {
		return err
	}
	if c.cursorDC, err = pub.newDataChannel(dcLabelCursor, true); err != nil {
		return err
	}
	c.inputDC.OnMessage(func(m webrtc.DataChannelMessage) { c.handleInput(m.Data) })
	c.mouseDC.OnMessage(func(m webrtc.DataChannelMessage) { c.handleMouse(m.Data) })
	return nil
}

// grantLease 向本会话的表请求并记录 leaseID(撤销后表自然拒绝旧 id)。
func (c *inputController) grantLease(notify func(reason string)) (string, bool) {
	id, ok := c.tbl.request(notify)
	if ok {
		c.mu.Lock()
		c.leaseID = id
		c.mu.Unlock()
	}
	return id, ok
}

// currentLease 返回当前记录的 leaseID(可能已被撤销——以表判定为准)。
func (c *inputController) currentLease() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.leaseID
}

// handleMouse 处理 mouse 通道(MOVE);入口即计数,后续逐级预校验。
func (c *inputController) handleMouse(b []byte) {
	if c.closed.Load() {
		return
	}
	c.counters.mouseMsgs.Add(1)
	if len(b) > maxInputMsgBytes {
		c.counters.oversize.Add(1)
		return
	}
	if len(b) != mouseMsgWireBytes {
		c.counters.invalid.Add(1)
		return
	}
	m := InputMsg{
		Seq:     binary.LittleEndian.Uint64(b),
		Type:    inputTypeMove,
		X:       int32(binary.LittleEndian.Uint32(b[8:])),
		Y:       int32(binary.LittleEndian.Uint32(b[12:])),
		Buttons: binary.LittleEndian.Uint16(b[16:]),
	}
	if m.Buttons&^btnMaskAny != 0 {
		c.counters.invalid.Add(1)
		return
	}
	c.process(&m)
}

// handleInput 处理 input 通道(BUTTON/WHEEL/KEY/TEXT/LOCK;MOVE 拒绝)。
func (c *inputController) handleInput(b []byte) {
	if c.closed.Load() {
		return
	}
	c.counters.inputMsgs.Add(1)
	if len(b) > maxInputMsgBytes {
		c.counters.oversize.Add(1)
		return
	}
	if len(b) < 10 { // [u64 seq][u8 type][≥2B payload']
		c.counters.invalid.Add(1)
		return
	}
	m := InputMsg{Seq: binary.LittleEndian.Uint64(b), Type: b[8]}
	p := b[9:]
	switch m.Type {
	case inputTypeButton:
		if len(p) != 2 {
			c.counters.invalid.Add(1)
			return
		}
		m.Btn, m.Down = p[0], p[1]
		if !(m.Btn == 1 || m.Btn == 2 || m.Btn == 4 || m.Btn == 8 || m.Btn == 16) || m.Down > 1 {
			c.counters.invalid.Add(1)
			return
		}
	case inputTypeWheel:
		if len(p) != 9 {
			c.counters.invalid.Add(1)
			return
		}
		m.X = int32(binary.LittleEndian.Uint32(p))
		m.Y = int32(binary.LittleEndian.Uint32(p[4:]))
		m.Trackpad = p[8]
		if m.Trackpad > 1 {
			c.counters.invalid.Add(1)
			return
		}
	case inputTypeKey:
		if len(p) != 4 {
			c.counters.invalid.Add(1)
			return
		}
		m.Scan = binary.LittleEndian.Uint16(p)
		m.Down, m.Extended = p[2], p[3]
		if m.Scan == 0 || m.Down > 1 || m.Extended > 1 {
			c.counters.invalid.Add(1)
			return
		}
	case inputTypeText:
		if len(p) < 2 {
			c.counters.invalid.Add(1)
			return
		}
		n := int(binary.LittleEndian.Uint16(p))
		if n == 0 || n > maxInputTextUnits || len(p) != 2+2*n {
			c.counters.invalid.Add(1)
			return
		}
		m.Text = make([]uint16, n)
		for i := 0; i < n; i++ {
			m.Text[i] = binary.LittleEndian.Uint16(p[2+2*i:])
		}
	case inputTypeLock:
		if len(p) != 2 {
			c.counters.invalid.Add(1)
			return
		}
		m.Caps, m.Num = p[0], p[1]
		if m.Caps > 1 || m.Num > 1 {
			c.counters.invalid.Add(1)
			return
		}
	default: // 含 MOVE(mouse 通道专属)与未知 type
		c.counters.invalid.Add(1)
		return
	}
	c.process(&m)
}

// process 是公共预校验管线:lease → seq 单调 → move 合并/barrier 转发。
func (c *inputController) process(m *InputMsg) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.tbl.allows(c.leaseID) {
		c.counters.noLease.Add(1)
		return
	}
	if m.Seq <= c.lastSeq {
		c.counters.staleSeq.Add(1)
		return
	}
	c.lastSeq = m.Seq
	if m.Type == inputTypeMove {
		c.deliverMoveLocked(m)
		return
	}
	c.flushPendingLocked() // barrier:先冲 pending MOVE(seq 序保持)
	c.forwardLocked(m)
}

// deliverMoveLocked:≤500Hz —— 距上次转发 ≥2ms 即发,否则记 pending
// (保留最新,中间丢弃);定时器到点冲刷。
func (c *inputController) deliverMoveLocked(m *InputMsg) {
	now := c.now()
	if c.pendMove == nil && now.Sub(c.lastFwd) >= moveMinGap {
		c.forwardLocked(m)
		c.lastFwd = now
		return
	}
	c.counters.coalesced.Add(1)
	c.pendMove = m
	if c.pendStop == nil {
		d := moveMinGap - now.Sub(c.lastFwd)
		if d < 0 {
			d = 0
		}
		c.pendStop = c.after(d, c.flushPending)
	}
}

// flushPending 是冲刷定时器回调(也经 barrier 路径调用)。
func (c *inputController) flushPending() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pendStop = nil
	m := c.pendMove
	c.pendMove = nil
	if m != nil {
		c.forwardLocked(m)
		c.lastFwd = c.now()
	}
}

// flushPendingLocked 先停定时器再冲 pending(barrier 事件前调用)。
func (c *inputController) flushPendingLocked() {
	if c.pendStop != nil {
		c.pendStop()
		c.pendStop = nil
	}
	if m := c.pendMove; m != nil {
		c.pendMove = nil
		c.forwardLocked(m)
		c.lastFwd = c.now()
	}
}

// forwardLocked:预算(1s 滑窗 ≤1000)→ 编码 → Source.SendInput;并维护
// 本会话的 down 键/钮表(close 时合成释放)。
func (c *inputController) forwardLocked(m *InputMsg) {
	now := c.now()
	// 滑窗剪枝:丢弃窗口外时戳。
	i := 0
	for i < len(c.fwdAt) && now.Sub(c.fwdAt[i]) >= inputBudgetWindow {
		i++
	}
	c.fwdAt = c.fwdAt[i:]
	if len(c.fwdAt) >= inputEPSBudget {
		c.counters.budgetDropped.Add(1)
		return
	}
	c.fwdAt = append(c.fwdAt, now)
	payload := encodeInputWire(c.src.SubID(), m)
	if payload == nil {
		c.counters.invalid.Add(1)
		return
	}
	if err := c.src.SendInput(payload); err != nil {
		c.counters.sendFailed.Add(1)
		return
	}
	c.counters.sent.Add(1)
	c.trackHeldLocked(m)
}

// trackHeldLocked 记录仍为 down 的键/钮(host 全局键态中只有持有者的输入
// 会被注入,故本表完备覆盖需要清理的键)。
func (c *inputController) trackHeldLocked(m *InputMsg) {
	switch m.Type {
	case inputTypeKey:
		id := uint32(m.Scan) | uint32(m.Extended)<<16
		if m.Down == 1 {
			c.heldKeys[id] = struct{}{}
		} else {
			delete(c.heldKeys, id)
		}
	case inputTypeButton:
		if m.Down == 1 {
			c.heldBtns[m.Btn] = struct{}{}
		} else {
			delete(c.heldBtns, m.Btn)
		}
	}
}

// forwardCursor 把 0x0109 事件透传到 cursor 通道([s32 x][s32 y][u8 vis])。
func (c *inputController) forwardCursor(ev CursorEvent) {
	if c.closed.Load() || c.cursorDC == nil {
		return
	}
	b := make([]byte, cursorWireBytes)
	binary.LittleEndian.PutUint32(b, uint32(ev.X))
	binary.LittleEndian.PutUint32(b[4:], uint32(ev.Y))
	if ev.Visible {
		b[8] = 1
	}
	if err := c.cursorDC.Send(b); err != nil {
		c.counters.cursorFailed.Add(1)
		return
	}
	c.counters.cursorSent.Add(1)
}

// close 是会话终结路径(在 Source 关闭前调用):停冲刷定时器、对本会话
// 仍 down 的键/钮合成 KEY/BUTTON up(seq 续接,卡键清理)、释放 lease、
// 记统计日志。幂等。
func (c *inputController) close() {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	c.mu.Lock()
	if c.pendStop != nil {
		c.pendStop()
		c.pendStop = nil
	}
	c.pendMove = nil
	keys := make([]uint32, 0, len(c.heldKeys))
	for k := range c.heldKeys {
		keys = append(keys, k)
	}
	btns := make([]uint8, 0, len(c.heldBtns))
	for b := range c.heldBtns {
		btns = append(btns, b)
	}
	seq := c.lastSeq
	leaseID := c.leaseID
	c.mu.Unlock()

	sub := c.src.SubID()
	for _, k := range keys {
		seq++
		m := &InputMsg{Seq: seq, Type: inputTypeKey, Scan: uint16(k & 0xFFFF), Extended: uint8(k >> 16), Down: 0}
		if err := c.src.SendInput(encodeInputWire(sub, m)); err == nil {
			c.counters.sent.Add(1)
		}
	}
	for _, b := range btns {
		seq++
		m := &InputMsg{Seq: seq, Type: inputTypeButton, Btn: b, Down: 0}
		if err := c.src.SendInput(encodeInputWire(sub, m)); err == nil {
			c.counters.sent.Add(1)
		}
	}
	if leaseID != "" {
		c.tbl.release(leaseID, "disconnect")
	}
	st := c.stats()
	c.log.Info("desktop input closed",
		"mouseMsgs", st.MouseMsgs, "inputMsgs", st.InputMsgs,
		"oversize", st.Oversize, "invalid", st.Invalid,
		"noLease", st.NoLease, "staleSeq", st.StaleSeq,
		"budgetDropped", st.BudgetDropped, "coalesced", st.Coalesced,
		"sent", st.Sent, "sendFailed", st.SendFailed,
		"cursorSent", st.CursorSent, "cursorFailed", st.CursorFailed)
}

func (c *inputController) stats() InputStats {
	return InputStats{
		MouseMsgs:     c.counters.mouseMsgs.Load(),
		InputMsgs:     c.counters.inputMsgs.Load(),
		Oversize:      c.counters.oversize.Load(),
		Invalid:       c.counters.invalid.Load(),
		NoLease:       c.counters.noLease.Load(),
		StaleSeq:      c.counters.staleSeq.Load(),
		BudgetDropped: c.counters.budgetDropped.Load(),
		Coalesced:     c.counters.coalesced.Load(),
		Sent:          c.counters.sent.Load(),
		SendFailed:    c.counters.sendFailed.Load(),
		CursorSent:    c.counters.cursorSent.Load(),
		CursorFailed:  c.counters.cursorFailed.Load(),
	}
}
