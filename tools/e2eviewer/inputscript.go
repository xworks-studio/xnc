// inputscript.go — e2eviewer --input-script:JSON 步骤脚本驱动 viewer → agent
// 的桌面输入(M1-Slice3 Task 5;T6 消费)。
//
// == 脚本形态(文件 = JSON 数组;未知字段拒绝,错误带步骤下标)==
//
//	[{"op":"lease"},                                 // control WS: lease_request
//	 {"op":"sas"},                                   // control WS: secure_attention(M2-Slice1 Task 5)
//	 {"op":"move","x":640,"y":360,"buttons":0},      // mouse 通道(x/y = 流逻辑 px)
//	 {"op":"button","btn":1,"down":true},            // input 通道(btn∈1/2/4/8/16)
//	 {"op":"wheel","dx":0,"dy":2},                    // input 通道(notch;正 dy=向下滚)
//	 {"op":"key","code":"KeyA","down":true},          // input 通道(code 经 keymap.go)
//	 {"op":"text","s":"hello"},                       // input 通道(UTF-16 ≤512 unit)
//	 {"op":"lock","caps":false,"num":true},          // input 通道 LOCK(native 差异才注入)
//	 {"op":"wait","ms":500}]                          // 本地等待
//
// lease_drop 刻意不支持:T3 词汇没有主动释放(撤销 = WS 断连/idle 30s),
// 写了会在解析期报错并提示。
//
// == wire 契约(agent/desktop/input.go 头注释;两通道一个 seq 计数器,
//
//	发送序分配 —— 与 web DesktopLive 完全同规则)==
//
//	mouse(不可靠):[u64 seq][s32 x][s32 y][u16 buttons]              18B
//	input (可靠): [u64 seq][u8 type][payload' 同 0x0108]
//	               BUTTON 2 [u8 btn][u8 down]                        11B
//	               WHEEL  3 [s32 dx][s32 dy][u8 trackpad=0]          18B
//	               KEY    4 [u16 scan][u8 down][u8 ext]              13B
//	               TEXT   5 [u16 len][utf16le units]              11+2n B
//	               LOCK   6 [u8 caps][u8 num]                       11B
//
// 首个 seq = 1(先加后发,web 同款)。步骤在连接 + 首关键帧后开始执行;
// 任何一步出错即中止(余步不执行),结果进 summary JSON(input 字段),
// 不影响 --expect-* 断言(场景期望由 T6 脚本对 input.steps 自行判定)。
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"time"
	"unicode/utf16"
)

// scriptStep 是一步的宽松解析形态(指针区分「缺字段」与「零值」)。
type scriptStep struct {
	Op      string  `json:"op"`
	X       *int32  `json:"x"`
	Y       *int32  `json:"y"`
	Buttons *int    `json:"buttons"`
	Btn     *int    `json:"btn"`
	Down    *bool   `json:"down"`
	Dx      *int32  `json:"dx"`
	Dy      *int32  `json:"dy"`
	Code    *string `json:"code"`
	S       *string `json:"s"`
	Ms      *int    `json:"ms"`
	Caps    *bool   `json:"caps"`
	Num     *bool   `json:"num"`

	// scan 在解析期由 Code 查 keymap.go 得出(未知 code = 解析错误)。
	scan scanCode
	// units 是 S 的 UTF-16 编码(TEXT 直接送 unit)。
	units []uint16
}

// 脚本约束(与 agent 预校验对齐,解析期即拒;T3 契约)。
const (
	maxScriptTextUnits = 512
	maxScriptWaitMs    = 60000
	scriptStepLimit    = 1000
)

// parseInputScript 解析并全量校验;错误信息带步骤下标(0 基)。
func parseInputScript(r io.Reader) ([]scriptStep, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var steps []scriptStep
	if err := dec.Decode(&steps); err != nil {
		return nil, fmt.Errorf("input script: bad json: %w", err)
	}
	if len(steps) == 0 {
		return nil, fmt.Errorf("input script: empty array")
	}
	if len(steps) > scriptStepLimit {
		return nil, fmt.Errorf("input script: %d steps > %d", len(steps), scriptStepLimit)
	}
	for i := range steps {
		if err := validateStep(i, &steps[i]); err != nil {
			return nil, err
		}
	}
	return steps, nil
}

func validateStep(i int, s *scriptStep) error {
	at := fmt.Sprintf("input script step %d", i)
	req := func(ok bool, what string) error {
		if !ok {
			return fmt.Errorf("%s: %s", at, what)
		}
		return nil
	}
	switch s.Op {
	case "lease":
		return nil
	case "sas", "sas_async":
		// 无字段;控制面 op(不发 DataChannel,不占 seq)。sas_async =
		// 发出不等待(M2-Slice2 T5 门⑥:与随后的 sas op 构成同会话
		// 并发,后到者应回 busy)。
		return nil
	case "move":
		if err := req(s.X != nil && s.Y != nil, "move needs x and y"); err != nil {
			return err
		}
		b := 0
		if s.Buttons != nil {
			b = *s.Buttons
		}
		return req(b >= 0 && b <= 0x1F, "move buttons must be 0..0x1F (bit 1L/2R/4M/8X1/16X2)")
	case "button":
		if err := req(s.Btn != nil && s.Down != nil, "button needs btn and down"); err != nil {
			return err
		}
		switch *s.Btn {
		case 1, 2, 4, 8, 16:
			return nil
		}
		return fmt.Errorf("%s: btn must be 1/2/4/8/16 (mask value), got %d", at, *s.Btn)
	case "wheel":
		return req(s.Dx != nil && s.Dy != nil, "wheel needs dx and dy (notches; native clamps ±10000)")
	case "key":
		if err := req(s.Code != nil && s.Down != nil, "key needs code and down"); err != nil {
			return err
		}
		sc, ok := keymapLookup(*s.Code)
		if !ok {
			return fmt.Errorf("%s: unknown KeyboardEvent.code %q (not in keymap.go; Pause has no Set 1 code)", at, *s.Code)
		}
		s.scan = sc
		return nil
	case "text":
		if err := req(s.S != nil, "text needs s"); err != nil {
			return err
		}
		s.units = utf16.Encode([]rune(*s.S))
		if len(s.units) == 0 {
			return fmt.Errorf("%s: text must be non-empty", at)
		}
		return req(len(s.units) <= maxScriptTextUnits,
			fmt.Sprintf("text is %d UTF-16 units, max %d", len(s.units), maxScriptTextUnits))
	case "lock":
		return req(s.Caps != nil || s.Num != nil, "lock needs caps and/or num (desired lock state; native injects only on diff)")
	case "wait":
		if err := req(s.Ms != nil, "wait needs ms"); err != nil {
			return err
		}
		return req(*s.Ms > 0 && *s.Ms <= maxScriptWaitMs,
			fmt.Sprintf("wait ms must be 1..%d, got %d", maxScriptWaitMs, *s.Ms))
	case "lease_drop":
		return fmt.Errorf("%s: lease_drop not supported — T3 lease vocabulary has no voluntary release (disconnect or 30s idle revokes); drop the step or end the viewer", at)
	default:
		return fmt.Errorf("%s: unknown op %q (want lease/sas/move/button/wheel/key/text/lock/wait)", at, s.Op)
	}
}

// ---- wire 构造(纯函数;布局 = agent/desktop/input.go 契约)----

const (
	wireTypeButton uint8 = 2
	wireTypeWheel  uint8 = 3
	wireTypeKey    uint8 = 4
	wireTypeText   uint8 = 5
	wireTypeLock   uint8 = 6
)

// buildMouseMove 编 mouse 通道 18B:[u64 seq][s32 x][s32 y][u16 buttons]。
func buildMouseMove(seq uint64, x, y int32, buttons uint16) []byte {
	b := make([]byte, 18)
	binary.LittleEndian.PutUint64(b, seq)
	binary.LittleEndian.PutUint32(b[8:], uint32(x))
	binary.LittleEndian.PutUint32(b[12:], uint32(y))
	binary.LittleEndian.PutUint16(b[16:], buttons)
	return b
}

// buildButton 编 input 通道 11B:[u64 seq][u8 2][u8 btn][u8 down]。
func buildButton(seq uint64, btn uint8, down bool) []byte {
	b := make([]byte, 11)
	binary.LittleEndian.PutUint64(b, seq)
	b[8] = wireTypeButton
	b[9] = btn
	b[10] = boolByte(down)
	return b
}

// buildWheel 编 input 通道 18B:[u64 seq][u8 3][s32 dx][s32 dy][u8 0]
// (trackpad=0 = notch 模式,native ×WHEEL_DELTA)。
func buildWheel(seq uint64, dx, dy int32) []byte {
	b := make([]byte, 18)
	binary.LittleEndian.PutUint64(b, seq)
	b[8] = wireTypeWheel
	binary.LittleEndian.PutUint32(b[9:], uint32(dx))
	binary.LittleEndian.PutUint32(b[13:], uint32(dy))
	b[17] = 0
	return b
}

// buildKey 编 input 通道 13B:[u64 seq][u8 4][u16 scan][u8 down][u8 ext]。
func buildKey(seq uint64, sc scanCode, down bool) []byte {
	b := make([]byte, 13)
	binary.LittleEndian.PutUint64(b, seq)
	b[8] = wireTypeKey
	binary.LittleEndian.PutUint16(b[9:], sc.scan)
	b[11] = boolByte(down)
	b[12] = boolByte(sc.ext)
	return b
}

// buildText 编 input 通道 11+2n B:[u64 seq][u8 5][u16 len][utf16le units]。
func buildText(seq uint64, units []uint16) []byte {
	b := make([]byte, 11+2*len(units))
	binary.LittleEndian.PutUint64(b, seq)
	b[8] = wireTypeText
	binary.LittleEndian.PutUint16(b[9:], uint16(len(units)))
	for i, u := range units {
		binary.LittleEndian.PutUint16(b[11+2*i:], u)
	}
	return b
}

// buildLock 编 input 通道 11B:[u64 seq][u8 6][u8 caps][u8 num](缺省字段
// 按 0 发——native 只对与 GetKeyState 不同的位注入)。
func buildLock(seq uint64, caps, num bool) []byte {
	b := make([]byte, 11)
	binary.LittleEndian.PutUint64(b, seq)
	b[8] = wireTypeLock
	b[9] = boolByte(caps)
	b[10] = boolByte(num)
	return b
}

func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}

// ---- 执行器 ----

// stepResult 是一步的结果(进 summary JSON 的 input.steps;T6 消费)。
type stepResult struct {
	Index  int    `json:"i"`
	Op     string `json:"op"`
	OK     bool   `json:"ok"`
	Seq    uint64 `json:"seq,omitempty"`   // wire 步骤占用(消耗)的 seq
	Detail string `json:"detail,omitempty"` // lease:leaseId 前 8 hex
	Err    string `json:"err,omitempty"`
}

// scriptResult 是 --input-script 的整体结果(summary.input)。
type scriptResult struct {
	OK             bool         `json:"ok"`
	Err            string       `json:"err,omitempty"`      // 预备失败(无关键帧/通道)或中止原因
	Steps          []stepResult `json:"steps"`
	LeaseGranted   bool         `json:"leaseGranted"`
	LeaseID        string       `json:"leaseId,omitempty"`   // 随机 ephemeral id,非凭据
	LeaseRevokedAs string       `json:"leaseRevoked,omitempty"` // 执行期被撤销的原因(idle/disconnect)
	LastSeq        uint64       `json:"lastSeq"`
	CursorEvents   uint64       `json:"cursorEvents"` // cursor 通道累计消息数(脚本收尾时快照)
}

// stepEnv 抽象脚本运行环境(live = DataChannel+WS;测试 = 记录器)。
type stepEnv interface {
	sendMouse(b []byte) error
	sendInput(b []byte) error
	// requestLease 发 lease_request 并等待 granted/denied(自带超时)。
	requestLease(ctx context.Context) (leaseID string, err error)
	// sendSAS 发 secure_attention 并等 secure_attention_result(自带
	// 超时;M2-Slice1 Task 5)。门控拒绝(ok=false + 稳定码)不算错误
	// —— 场景期望由 e2e 脚本对 detail 自行判定。
	sendSAS(ctx context.Context) (ok bool, hr uint32, code string, err error)
	// sasAsync 发 secure_attention 不等待回执(M2-Slice2 T5 门⑥:
	// 并发 SAS busy 证据,回执经主循环 INFO 日志 + 后续 sas op 观测)。
	sasAsync(ctx context.Context) error
	wait(ctx context.Context, d time.Duration) // 可中断等待
	cursorCount() uint64                        // cursor 通道累计消息数
}

// runInputSteps 顺序执行步骤;seq 单计数器(input+mouse 共用,发送序,
// 首 seq=1);任何 wire/lease 步骤出错即中止并返回该结果集。
func runInputSteps(ctx context.Context, steps []scriptStep, env stepEnv) *scriptResult {
	res := &scriptResult{Steps: make([]stepResult, 0, len(steps))}
	var seq uint64
	for i := range steps {
		s := &steps[i]
		r := stepResult{Index: i, Op: s.Op}
		err := func() error {
			switch s.Op {
			case "lease":
				id, err := env.requestLease(ctx)
				if err != nil {
					return err
				}
				res.LeaseGranted = true
				res.LeaseID = id
				if len(id) >= 8 {
					r.Detail = id[:8]
				} else {
					r.Detail = id
				}
				return nil
			case "sas_async":
				// 控制面 op:发出即成功(不等回执;并发 busy 门用)。
				if err := env.sasAsync(ctx); err != nil {
					return err
				}
				r.Detail = "fired"
				return nil
			case "sas":
				// 控制面 op:round-trip 成功即步骤成功(拒绝码进 detail,
				// 不占 seq——不发 DataChannel)。
				ok, hr, code, err := env.sendSAS(ctx)
				if err != nil {
					return err
				}
				if ok {
					r.Detail = fmt.Sprintf("ok hr=0x%x", hr)
				} else {
					r.Detail = "denied:" + code
				}
				return nil
			case "move":
				seq++
				buttons := uint16(0)
				if s.Buttons != nil {
					buttons = uint16(*s.Buttons)
				}
				r.Seq = seq
				return env.sendMouse(buildMouseMove(seq, *s.X, *s.Y, buttons))
			case "button":
				seq++
				r.Seq = seq
				return env.sendInput(buildButton(seq, uint8(*s.Btn), *s.Down))
			case "wheel":
				seq++
				r.Seq = seq
				return env.sendInput(buildWheel(seq, *s.Dx, *s.Dy))
			case "key":
				seq++
				r.Seq = seq
				return env.sendInput(buildKey(seq, s.scan, *s.Down))
			case "text":
				seq++
				r.Seq = seq
				return env.sendInput(buildText(seq, s.units))
			case "lock":
				seq++
				r.Seq = seq
				var caps, num bool
				if s.Caps != nil {
					caps = *s.Caps
				}
				if s.Num != nil {
					num = *s.Num
				}
				return env.sendInput(buildLock(seq, caps, num))
			case "wait":
				env.wait(ctx, time.Duration(*s.Ms)*time.Millisecond)
				return ctx.Err()
			default:
				return fmt.Errorf("internal: unvalidated op %q", s.Op)
			}
		}()
		if err != nil {
			r.Err = err.Error()
			res.Steps = append(res.Steps, r)
			res.Err = fmt.Sprintf("aborted at step %d (%s): %v", i, s.Op, err)
			res.LastSeq = seq
			res.CursorEvents = env.cursorCount()
			return res
		}
		r.OK = true
		res.Steps = append(res.Steps, r)
	}
	res.OK = true
	res.LastSeq = seq
	res.CursorEvents = env.cursorCount()
	return res
}

// scriptWaitBudget 估算脚本最长耗时(外层 ctx 时限用):等首关键帧 +
// wait 全额 + 每步握手/发送保守余量。解析后调用,步骤已定界。
func scriptWaitBudget(steps []scriptStep) time.Duration {
	total := 25 * time.Second // waitFirstKeyframe 的预算
	for i := range steps {
		switch steps[i].Op {
		case "wait":
			total += time.Duration(*steps[i].Ms) * time.Millisecond
		case "lease":
			total += 6 * time.Second // requestLease 超时 5s + 余量
		case "sas":
			total += sasResultWait + 2*time.Second // 回执等待(agent 界对齐)+ 余量
		case "sas_async":
			total += time.Second // 不等回执;余量覆盖写时限
		default:
			total += 2 * time.Second
		}
	}
	return total
}
