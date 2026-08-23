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
// == lease(control WS 信令词汇保留,仲裁已移 server;M2-Slice3 Task 4,
//
//	   spec §11.1 split-arbitration 裁决)==
//
//		viewer → agent:{"type":"lease_request"}
//		agent → viewer:{"type":"lease_granted","leaseId":"<16 hex>"}
//		              {"type":"lease_denied","reason":"held"}
//		              {"type":"lease_revoked","reason":"idle"|"disconnect"}
//
// server = 谁 MAY hold(每节点同时至多一个活约;60s TTL 由持有者会话 WS
// 信令活跃续期,持有者会话关闭即释放——均在 server manager)。agent =
// 执行:会话 params 带 server 签发 leaseId 才是持有者(本地仲裁表已退役);
// lease_request 仅按「本会话是否持有 server 约」作答 granted/denied{held};
// 输入放行 = leaseId 匹配。断连语义:持有者 WS 关闭 → agent 释放本地记录
// (server 侧撤销/再授以其 TTL/关闭钩子为权威)。lease_revoked 帧保留于
// 词汇(旧 viewer 兼容),server 仲裁下 agent 不再主动产生。
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
	"encoding/binary"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"

	"xnc/proto"
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

// stoppableAfterFunc 把 time.AfterFunc 包成返回 stop 的 after 形态(测试注入)。
func stoppableAfterFunc(d time.Duration, fn func()) func() {
	tm := time.AfterFunc(d, fn)
	return func() { tm.Stop() }
}

// ---- server lease 登记表(M2-Slice3 Task 4;每 Handler = 每采集实例一份)----

// serverLease 登记当前活约:server 每节点同时至多签发一个 leaseId,且只
// 写进被授予会话的 params。agent 侧执行 = 输入放行仅对 leaseId 匹配的
// 会话;新签发覆盖旧值(server 撤销旧约后才可能签新约——后到者为权威,
// 过期的旧持有者自然失配)。持有者 WS 关闭 → release 清空(移交语义:
// 下一个被 server 授予的会话注册后即生效)。
type serverLease struct {
	mu        sync.Mutex
	activeID  string // 当前活约 leaseId("" = 本实例无持有者)
	activeKey string // 持有者会话标识(登记键;释放时比对)
}

func newServerLease() *serverLease { return &serverLease{} }

// register 登记一个携带 server leaseId 的会话(后到覆盖:server 单活约,
// 新签发即撤销旧约)。leaseID 空(view-only 会话)不登记。
func (l *serverLease) register(key, leaseID string) {
	if leaseID == "" {
		return
	}
	l.mu.Lock()
	l.activeID, l.activeKey = leaseID, key
	l.mu.Unlock()
}

// release 会话终结时清登记(仅当它仍是持有者)。
func (l *serverLease) release(key, leaseID string) {
	if leaseID == "" {
		return
	}
	l.mu.Lock()
	if l.activeKey == key && l.activeID == leaseID {
		l.activeID, l.activeKey = "", ""
	}
	l.mu.Unlock()
}

// allows 判定 leaseID 是否当前活约(view-only 会话恒 false)。
func (l *serverLease) allows(leaseID string) bool {
	if leaseID == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.activeID != "" && l.activeID == leaseID
}

// ---- 计数器 ----

type inputCounters struct {
	mouseMsgs     atomic.Uint64
	inputMsgs     atomic.Uint64
	oversize      atomic.Uint64
	invalid       atomic.Uint64
	noLease       atomic.Uint64
	noCapability  atomic.Uint64
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
	NoCapability  uint64
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
	leases   *serverLease // 采集实例共享的 server 活约登记表
	sessKey  string       // 会话标识(登记键;每个 Handle 调用唯一)
	leaseID  string       // server 签发的本会话 leaseId("" = view-only)
	caps     map[string]bool
	log      *slog.Logger
	counters inputCounters

	// now/after 可注入(确定性单测)。
	now   func() time.Time
	after func(time.Duration, func()) func()

	mu       sync.Mutex
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

// newInputController 组装本会话的输入控制器。leaseID = server 授予时
// params 携带的 leaseId("" = view-only);caps = server 下发的 capability
// 集(输入放行前按 input.mouse/input.keyboard 校验,spec §14)。
func newInputController(src Source, leases *serverLease, sessKey, leaseID string,
	caps []string, log *slog.Logger) *inputController {
	if log == nil {
		log = slog.Default()
	}
	cm := map[string]bool{}
	for _, cp := range caps {
		cm[cp] = true
	}
	leases.register(sessKey, leaseID)
	return &inputController{
		src: src, leases: leases, sessKey: sessKey, leaseID: leaseID, caps: cm,
		log: log,
		now: time.Now, after: stoppableAfterFunc,
		heldKeys: map[uint32]struct{}{}, heldBtns: map[uint8]struct{}{},
	}
}

// holdsLease 报告本会话是否当前 server 活约持有者(lease_request 作答依据)。
func (c *inputController) holdsLease() bool {
	return c.leases.allows(c.leaseID)
}

// serverLeaseID 返回 server 签发给本会话的 leaseId(view-only 为 "")。
func (c *inputController) serverLeaseID() string {
	return c.leaseID
}

// capAllows 报告本会话是否具备 capability(server 下发集;未下发 = 拒)。
func (c *inputController) capAllows(cap string) bool {
	return c.caps[cap]
}

// capForInputType 输入类型 → 所需 capability;"" = 未知(前级已拒)。
func capForInputType(t uint8) string {
	switch t {
	case inputTypeMove, inputTypeButton, inputTypeWheel:
		return proto.CapInputMouse
	case inputTypeKey, inputTypeText, inputTypeLock:
		return proto.CapInputKeyboard
	}
	return ""
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
	if !c.leases.allows(c.leaseID) {
		c.counters.noLease.Add(1)
		return
	}
	// capability 强制(spec §14):input.mouse / input.keyboard 缺失即拒
	// 对应输入(丢弃+计数;server 只对 operator+ 下发输入集)。
	if cp := capForInputType(m.Type); cp != "" && !c.caps[cp] {
		c.counters.noCapability.Add(1)
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
	c.mu.Unlock()

	// 卡键合成释放仅限仍持有 server 约:server 已移交(新 leaseId 覆盖)的
	// 旧会话 close 不得合成 up 抬掉新持有者按下的同键(T3 review Minor 2
	// 语义在 server 仲裁下的等价物);物理残留由 host 侧 janitor(>30s
	// 强制 KeyUp)兜底。
	if c.leases.allows(c.leaseID) {
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
	}
	c.leases.release(c.sessKey, c.leaseID)
	st := c.stats()
	c.log.Info("desktop input closed",
		"mouseMsgs", st.MouseMsgs, "inputMsgs", st.InputMsgs,
		"oversize", st.Oversize, "invalid", st.Invalid,
		"noLease", st.NoLease, "noCapability", st.NoCapability, "staleSeq", st.StaleSeq,
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
		NoCapability:  c.counters.noCapability.Load(),
		StaleSeq:      c.counters.staleSeq.Load(),
		BudgetDropped: c.counters.budgetDropped.Load(),
		Coalesced:     c.counters.coalesced.Load(),
		Sent:          c.counters.sent.Load(),
		SendFailed:    c.counters.sendFailed.Load(),
		CursorSent:    c.counters.cursorSent.Load(),
		CursorFailed:  c.counters.cursorFailed.Load(),
	}
}
