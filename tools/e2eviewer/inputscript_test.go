package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

func parse(t *testing.T, s string) []scriptStep {
	t.Helper()
	steps, err := parseInputScript(strings.NewReader(s))
	if err != nil {
		t.Fatalf("parseInputScript: %v", err)
	}
	return steps
}

func TestParseInputScriptValid(t *testing.T) {
	steps := parse(t, `[
		{"op":"lease"},
		{"op":"sas"},
		{"op":"wait","ms":500},
		{"op":"move","x":640,"y":360},
		{"op":"move","x":0,"y":0,"buttons":31},
		{"op":"button","btn":1,"down":true},
		{"op":"button","btn":16,"down":false},
		{"op":"wheel","dx":-2,"dy":3},
		{"op":"key","code":"KeyA","down":true},
		{"op":"key","code":"ControlRight","down":false},
		{"op":"text","s":"xnc\nslice3"},
		{"op":"lock","num":true},
		{"op":"lock","caps":false,"num":false},
		{"op":"wait","ms":1}
	]`)
	wantOps := []string{"lease", "sas", "wait", "move", "move", "button", "button",
		"wheel", "key", "key", "text", "lock", "lock", "wait"}
	if len(steps) != len(wantOps) {
		t.Fatalf("steps = %d, want %d", len(steps), len(wantOps))
	}
	for i, op := range wantOps {
		if steps[i].Op != op {
			t.Errorf("step %d op = %q, want %q", i, steps[i].Op, op)
		}
	}
	// key 步骤的 keymap 解析在 parse 期完成。
	if steps[8].scan != (scanCode{scan: 0x1e, ext: false}) {
		t.Errorf("KeyA scan = %+v", steps[8].scan)
	}
	if steps[9].scan != (scanCode{scan: 0x1d, ext: true}) {
		t.Errorf("ControlRight scan = %+v", steps[9].scan)
	}
	// 换行 = 1 UTF-16 unit("xnc\nslice3" = 10 unit)。
	if got := len(steps[10].units); got != 10 {
		t.Errorf("text units = %d, want 10", got)
	}
}

func TestParseInputScriptErrors(t *testing.T) {
	cases := []struct {
		json string
		want string
	}{
		{`[{"op":"nope"}]`, "unknown op"},
		{`[{"op":"move","x":1}]`, "move needs x and y"},
		{`[{"op":"move","x":1,"y":2,"buttons":32}]`, "buttons must be 0..0x1F"},
		{`[{"op":"move","x":1,"y":2,"buttons":-1}]`, "buttons must be 0..0x1F"},
		{`[{"op":"button","btn":3,"down":true}]`, "btn must be 1/2/4/8/16"},
		{`[{"op":"button","btn":1}]`, "button needs btn and down"},
		{`[{"op":"wheel","dx":0}]`, "wheel needs dx and dy"},
		{`[{"op":"key","code":"Pause","down":true}]`, "unknown KeyboardEvent.code"},
		{`{}`, "bad json"}, // 非数组形态
		{`[{"op":"lease"},{"op":"text","s":""}]`, "text must be non-empty"},
		{`[{"op":"text"}]`, "text needs s"},
		{`[{"op":"lock"}]`, "lock needs caps and/or num"},
		{`[{"op":"wait","ms":0}]`, "wait ms must be 1..60000"},
		{`[{"op":"wait","ms":60001}]`, "wait ms must be 1..60000"},
		{`[{"op":"wait"}]`, "wait needs ms"},
		{`[{"op":"move","x":1,"y":2,"z":3}]`, "unknown field"}, // DisallowUnknownFields
		{`[{"op":"lease_drop"}]`, "lease_drop not supported"},
		{`[]`, "empty array"},
		{`[{"op":"lease"},{"op":"nope"}]`, "step 1"}, // 下标进错误信息
	}
	for _, c := range cases {
		_, err := parseInputScript(strings.NewReader(c.json))
		if err == nil {
			t.Errorf("parse(%s): want error", c.json)
			continue
		}
		if c.want != "" && !strings.Contains(err.Error(), c.want) {
			t.Errorf("parse(%s) err = %q, want containing %q", c.json, err, c.want)
		}
	}
	// >512 UTF-16 unit 文本拒绝(agent 同限)。
	long := strings.Repeat("x", 513)
	if _, err := parseInputScript(strings.NewReader(`[{"op":"text","s":"` + long + `"}]`)); err == nil ||
		!strings.Contains(err.Error(), "513 UTF-16 units") {
		t.Errorf("513-unit text: err = %v", err)
	}
}

// 黄金字节锁 wire 布局(与 agent/desktop/input.go handleMouse/handleInput
// 解码逐字段对齐;T3 契约)。
func TestBuildersGolden(t *testing.T) {
	// MOVE:seq=7 x=-5 y=6 buttons=0x05。
	if got, want := buildMouseMove(7, -5, 6, 0x05),
		"0700000000000000fbffffff060000000500"; hex.EncodeToString(got) != want {
		t.Errorf("move = %s, want %s", hex.EncodeToString(got), want)
	}
	// BUTTON:seq=1 btn=1 down=1。
	if got, want := buildButton(1, 1, true), "0100000000000000020101"; hex.EncodeToString(got) != want {
		t.Errorf("button = %s, want %s", hex.EncodeToString(got), want)
	}
	// WHEEL:seq=2 dx=-1 dy=2 trackpad=0。
	if got, want := buildWheel(2, -1, 2), "020000000000000003ffffffff0200000000"; hex.EncodeToString(got) != want {
		t.Errorf("wheel = %s, want %s", hex.EncodeToString(got), want)
	}
	// KEY:seq=3 scan=0x1e(u16 LE)down=1 ext=0;extended 例 scan=0x1d ext=1。
	if got, want := buildKey(3, scanCode{scan: 0x1e}, true), "0300000000000000041e000100"; hex.EncodeToString(got) != want {
		t.Errorf("key = %s, want %s", hex.EncodeToString(got), want)
	}
	if got, want := buildKey(4, scanCode{scan: 0x1d, ext: true}, false), "0400000000000000041d000001"; hex.EncodeToString(got) != want {
		t.Errorf("key ext = %s, want %s", hex.EncodeToString(got), want)
	}
	// TEXT:seq=5 "AB"(len=2,units 0x41 0x42)。
	if got, want := buildText(5, []uint16{0x41, 0x42}), "050000000000000005020041004200"; hex.EncodeToString(got) != want {
		t.Errorf("text = %s, want %s", hex.EncodeToString(got), want)
	}
	// LOCK:seq=6 caps=1 num=0。
	if got, want := buildLock(6, true, false), "0600000000000000060100"; hex.EncodeToString(got) != want {
		t.Errorf("lock = %s, want %s", hex.EncodeToString(got), want)
	}
	// 长度自检(契约值)。
	if len(buildMouseMove(0, 0, 0, 0)) != 18 || len(buildWheel(0, 0, 0)) != 18 ||
		len(buildKey(0, scanCode{}, false)) != 13 || len(buildButton(0, 1, false)) != 11 ||
		len(buildLock(0, false, false)) != 11 ||
		len(buildText(0, make([]uint16, 512))) != 11+1024 {
		t.Errorf("builder lengths drifted from contract")
	}
	// 代理对按 unit 透传(U+1F600 = D83D DE00)。
	b := buildText(1, []uint16{0xD83D, 0xDE00})
	if binary.LittleEndian.Uint16(b[11:]) != 0xD83D || binary.LittleEndian.Uint16(b[13:]) != 0xDE00 {
		t.Errorf("surrogate units not passed through verbatim: % x", b)
	}
}

// fakeEnv 记录发送,供 seq 分配与中止语义断言。
type fakeEnv struct {
	mouse, input [][]byte
	leaseID      string
	leaseErr     error
	cursors      uint64
	waits        []time.Duration
	sasOK        bool
	sasHR        uint32
	sasCode      string
	sasErr       error
}

func (f *fakeEnv) sendMouse(b []byte) error { f.mouse = append(f.mouse, b); return nil }
func (f *fakeEnv) sendInput(b []byte) error { f.input = append(f.input, b); return nil }
func (f *fakeEnv) requestLease(ctx context.Context) (string, error) {
	return f.leaseID, f.leaseErr
}
func (f *fakeEnv) sendSAS(ctx context.Context) (bool, uint32, string, error) {
	return f.sasOK, f.sasHR, f.sasCode, f.sasErr
}
func (f *fakeEnv) sasAsync(ctx context.Context) error { return f.sasErr }
func (f *fakeEnv) wait(ctx context.Context, d time.Duration) { f.waits = append(f.waits, d) }
func (f *fakeEnv) cursorCount() uint64                       { return f.cursors }

// sendMouseFailIdx 让第 n 条 mouse 消息(0 基)失败,测中止。
type failEnv struct {
	fakeEnv
	failAtMouse int
	mouseN      int
}

func (f *failEnv) sendMouse(b []byte) error {
	if f.mouseN == f.failAtMouse {
		return errors.New("mouse channel closed")
	}
	f.mouseN++
	return f.fakeEnv.sendMouse(b)
}

func TestRunInputStepsSeqAssignment(t *testing.T) {
	steps := parse(t, `[
		{"op":"lease"},
		{"op":"wait","ms":100},
		{"op":"move","x":10,"y":20},
		{"op":"key","code":"KeyA","down":true},
		{"op":"move","x":30,"y":40},
		{"op":"text","s":"hi"},
		{"op":"wait","ms":50},
		{"op":"wheel","dy":1,"dx":0}
	]`)
	env := &fakeEnv{leaseID: "0123456789abcdef", cursors: 9}
	res := runInputSteps(context.Background(), steps, env)
	if !res.OK || res.Err != "" {
		t.Fatalf("script should succeed: %+v", res)
	}
	// 单计数器跨 mouse+input 按发送序:move=1, key=2, move=3, text=4, wheel=5。
	seqOf := func(b []byte) uint64 { return binary.LittleEndian.Uint64(b) }
	if got := seqOf(env.mouse[0]); got != 1 {
		t.Errorf("mouse[0] seq = %d, want 1", got)
	}
	if got := seqOf(env.input[0]); got != 2 {
		t.Errorf("input[0](key) seq = %d, want 2", got)
	}
	if got := seqOf(env.mouse[1]); got != 3 {
		t.Errorf("mouse[1] seq = %d, want 3", got)
	}
	if got := seqOf(env.input[1]); got != 4 {
		t.Errorf("input[1](text) seq = %d, want 4", got)
	}
	if got := seqOf(env.input[2]); got != 5 {
		t.Errorf("input[2](wheel) seq = %d, want 5", got)
	}
	if res.LastSeq != 5 {
		t.Errorf("LastSeq = %d, want 5", res.LastSeq)
	}
	// wait/lease 不占 seq;结果逐条 ok;lease 记 id 前 8 hex;cursor 数带回。
	for _, r := range res.Steps {
		if !r.OK {
			t.Errorf("step %d unexpectedly failed: %+v", r.Index, r)
		}
	}
	if res.LeaseGranted != true || res.LeaseID != "0123456789abcdef" {
		t.Errorf("lease result = %+v", res)
	}
	if res.Steps[0].Detail != "01234567" {
		t.Errorf("lease detail = %q, want first 8 hex", res.Steps[0].Detail)
	}
	if res.CursorEvents != 9 {
		t.Errorf("CursorEvents = %d, want 9", res.CursorEvents)
	}
	if len(env.waits) != 2 || env.waits[0] != 100*time.Millisecond || env.waits[1] != 50*time.Millisecond {
		t.Errorf("waits = %v", env.waits)
	}
}

func TestRunInputStepsAbortOnError(t *testing.T) {
	steps := parse(t, `[
		{"op":"move","x":1,"y":1},
		{"op":"move","x":2,"y":2},
		{"op":"move","x":3,"y":3}
	]`)
	env := &failEnv{failAtMouse: 1}
	res := runInputSteps(context.Background(), steps, env)
	if res.OK {
		t.Fatalf("script should abort: %+v", res)
	}
	if len(env.mouse) != 1 { // 第 2 条失败,第 3 条不再发送
		t.Errorf("mouse sends = %d, want 1", len(env.mouse))
	}
	if len(res.Steps) != 2 || res.Steps[1].OK || res.Steps[1].Err == "" {
		t.Errorf("steps = %+v", res.Steps)
	}
	if !strings.Contains(res.Err, "aborted at step 1") {
		t.Errorf("Err = %q", res.Err)
	}
	if res.LastSeq != 2 { // 失败步也占用了 seq(已发送或已构帧)
		t.Errorf("LastSeq = %d, want 2", res.LastSeq)
	}
}

func TestRunInputStepsLeaseDeniedAborts(t *testing.T) {
	steps := parse(t, `[
		{"op":"lease"},
		{"op":"move","x":1,"y":1}
	]`)
	env := &fakeEnv{leaseErr: errors.New("denied: held")}
	res := runInputSteps(context.Background(), steps, env)
	if res.OK || res.LeaseGranted {
		t.Fatalf("denied lease should abort: %+v", res)
	}
	if len(env.mouse) != 0 {
		t.Errorf("no input should be sent after lease denial")
	}
	if !strings.Contains(res.Steps[0].Err, "denied") {
		t.Errorf("step err = %q", res.Steps[0].Err)
	}
}

func TestScriptWaitBudget(t *testing.T) {
	steps := parse(t, `[{"op":"lease"},{"op":"sas"},{"op":"wait","ms":1000},{"op":"move","x":0,"y":0}]`)
	// 25s(首关键帧)+ 6s(lease)+ 19s(sas = sasResultWait 17s + 2s 余量,
	// Task 6 对齐 agent 界)+ 1s(wait)+ 2s(move)= 53s。
	if got, want := scriptWaitBudget(steps), 53*time.Second; got != want {
		t.Errorf("budget = %v, want %v", got, want)
	}
}

// TestRunInputStepsSasOp(M2-Slice1 Task 5):sas 是控制面 op —— 门控
// 拒绝(ok=false + 稳定码)round-trip 成功即步骤成功(码进 detail),
// 不占 seq;传输错误(超时/断连)照常中止脚本。
func TestRunInputStepsSasOp(t *testing.T) {
	t.Run("denied-is-observation", func(t *testing.T) {
		steps := parse(t, `[{"op":"sas"},{"op":"move","x":1,"y":1}]`)
		env := &fakeEnv{sasOK: false, sasCode: "SAS_DENIED"}
		res := runInputSteps(context.Background(), steps, env)
		if !res.OK || res.Err != "" {
			t.Fatalf("denied SAS should not abort: %+v", res)
		}
		if res.Steps[0].OK != true || res.Steps[0].Detail != "denied:SAS_DENIED" {
			t.Errorf("sas step = %+v", res.Steps[0])
		}
		if res.LastSeq != 1 { // 只有 move 占 seq
			t.Errorf("LastSeq = %d, want 1 (sas is control-plane)", res.LastSeq)
		}
	})
	t.Run("ok-carries-hr", func(t *testing.T) {
		steps := parse(t, `[{"op":"sas"}]`)
		env := &fakeEnv{sasOK: true, sasHR: 0x80070005}
		res := runInputSteps(context.Background(), steps, env)
		if !res.OK || res.Steps[0].Detail != "ok hr=0x80070005" {
			t.Errorf("sas(ok) step = %+v", res.Steps[0])
		}
	})
	t.Run("timeout-aborts", func(t *testing.T) {
		steps := parse(t, `[{"op":"sas"},{"op":"move","x":1,"y":1}]`)
		env := &fakeEnv{sasErr: errors.New("secure_attention_result timeout after 10s")}
		res := runInputSteps(context.Background(), steps, env)
		if res.OK {
			t.Fatalf("timeout must abort: %+v", res)
		}
		if len(env.mouse) != 0 {
			t.Errorf("no input should follow a SAS timeout")
		}
	})
}
