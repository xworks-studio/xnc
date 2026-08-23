// inputlive.go — --input-script 的 server 模式运行时:agent 预建的三条
// DataChannel(in-band 协商,经 OnDataChannel 到达)中 input/mouse 用于
// 发送、cursor 用于接收记录;lease 走 control WS 词汇。
package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"
)

// DataChannel 标签(agent/desktop/input.go attach;T3 契约)。
const (
	dcLabelInput  = "input"
	dcLabelMouse  = "mouse"
	dcLabelCursor = "cursor"
)

// dcRef 包一条 agent 建的通道;open 经 chan 通知(发送前等待用)。
type dcRef struct {
	dc   *webrtc.DataChannel
	open chan struct{}
	once sync.Once
}

func newDCRef(dc *webrtc.DataChannel) *dcRef {
	r := &dcRef{dc: dc, open: make(chan struct{})}
	dc.OnOpen(r.markOpen)
	if dc.ReadyState() == webrtc.DataChannelStateOpen {
		r.markOpen() // 到手即已 open 的兜底(时序上罕见)
	}
	return r
}

func (r *dcRef) markOpen() { r.once.Do(func() { close(r.open) }) }

// channelSet 持 input/mouse 两发送通道(重复协商 = 后者覆盖,词汇既定:
// 重复 offer 被忽略,不会发生)。
type channelSet struct {
	mu    sync.Mutex
	input *dcRef
	mouse *dcRef
}

func (c *channelSet) put(dc *webrtc.DataChannel) {
	switch dc.Label() {
	case dcLabelInput:
		c.mu.Lock()
		c.input = newDCRef(dc)
		c.mu.Unlock()
	case dcLabelMouse:
		c.mu.Lock()
		c.mouse = newDCRef(dc)
		c.mu.Unlock()
	}
}

// send 等通道 open(上限 10s)后发送。
func (c *channelSet) send(ctx context.Context, label string, b []byte) error {
	c.mu.Lock()
	var r *dcRef
	switch label {
	case dcLabelInput:
		r = c.input
	case dcLabelMouse:
		r = c.mouse
	}
	c.mu.Unlock()
	if r == nil {
		return fmt.Errorf("%s datachannel not negotiated (agent offer-side channels missing?)", label)
	}
	select {
	case <-r.open:
	case <-ctx.Done():
		return fmt.Errorf("%s channel wait: %w", label, ctx.Err())
	case <-time.After(10 * time.Second):
		return fmt.Errorf("%s channel not open after 10s (state=%s)", label, r.dc.ReadyState())
	}
	return r.dc.Send(b)
}

// leaseEvent 是 control WS 的 lease 三帧解析结果(granted/denied/revoked)。
type leaseEvent struct {
	kind    string
	leaseID string
	reason  string
}

// leaseNote 记录执行期收到的 lease_revoked 原因(进 scriptResult)。
type leaseNote struct {
	mu      sync.Mutex
	revoked string
}

func (n *leaseNote) setRevoked(reason string) {
	n.mu.Lock()
	n.revoked = reason
	n.mu.Unlock()
}

func (n *leaseNote) revocation() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.revoked
}

// serverInputEnv 把脚本步骤落到真实通道/WS(stepEnv 实现)。
type serverInputEnv struct {
	sctx    context.Context // 脚本级 ctx(含预算时限)
	chs     *channelSet
	ws      *websocket.Conn
	leaseCh chan leaseEvent
	sasCh   chan sasReply // secure_attention_result 交接(main.go 信令泵产出)
	v       *viewer
}

func (e *serverInputEnv) sendMouse(b []byte) error { return e.chs.send(e.sctx, dcLabelMouse, b) }
func (e *serverInputEnv) sendInput(b []byte) error { return e.chs.send(e.sctx, dcLabelInput, b) }

func (e *serverInputEnv) cursorCount() uint64 {
	n, _ := e.v.cursorStats()
	return n
}

func (e *serverInputEnv) wait(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// requestLease 发 lease_request 并等 granted/denied(5s 上限);迟到的
// revoked(上一持有期)忽略继续等。
func (e *serverInputEnv) requestLease(ctx context.Context) (string, error) {
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	jb, _ := json.Marshal(map[string]any{"type": "lease_request"})
	if err := e.ws.Write(wctx, websocket.MessageText, jb); err != nil {
		return "", fmt.Errorf("send lease_request: %w", err)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-e.leaseCh:
			switch ev.kind {
			case "granted":
				return ev.leaseID, nil
			case "denied":
				return "", fmt.Errorf("denied: %s", ev.reason)
			}
		case <-deadline:
			return "", errors.New("lease response timeout after 5s")
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// sendSAS 发 secure_attention 并等 secure_attention_result(sasResultWait
// 上限 = agent core RPC 界 15s + 传输余量;M2-Slice1 Task 6 对齐修复:旧
// 10s 会把界内回执误判为超时)。关联:词汇无请求 id,SAS op 严格顺序且
// 各等满 agent 界 → 发送前清空迟到回执(上一 op 残留不被本 op 误领,见
// main.go drainSasReplies)。门控拒绝(ok=false + 稳定码)按观测返回,
// 不算错误。
func (e *serverInputEnv) sendSAS(ctx context.Context) (bool, uint32, string, error) {
	if stale := drainSasReplies(e.sasCh); stale > 0 {
		e.v.log.Warn("sas: discarded stale result frame(s) from an earlier op", "count", stale)
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	jb, _ := json.Marshal(map[string]any{"type": "secure_attention"})
	if err := e.ws.Write(wctx, websocket.MessageText, jb); err != nil {
		return false, 0, "", fmt.Errorf("send secure_attention: %w", err)
	}
	select {
	case rep := <-e.sasCh:
		return rep.ok, rep.hr, rep.code, nil
	case <-time.After(sasResultWait):
		return false, 0, "", fmt.Errorf("secure_attention_result timeout after %v", sasResultWait)
	case <-ctx.Done():
		return false, 0, "", ctx.Err()
	}
}

// runServerScript:等首关键帧 → 预算内执行脚本 → 附带 revoke 记录。
// noKeyframeGate(诊断 --input-before-keyframe,如 M2-Slice1 锁屏探针:
// 静止锁屏在现行 warmup 界内不出首 IDR,注入本身不依赖解码帧)跳过
// 该等待 —— 仅探针用,常规门保持"看得见桌面才注入"。
func runServerScript(ctx context.Context, steps []scriptStep, v *viewer,
	chs *channelSet, leaseCh chan leaseEvent, note *leaseNote,
	ws *websocket.Conn, sasCh chan sasReply, log *slog.Logger, noKeyframeGate bool) *scriptResult {
	if !noKeyframeGate {
		if err := waitFirstKeyframe(v, 25*time.Second); err != nil {
			return &scriptResult{Err: err.Error()}
		}
	}
	sctx, cancel := context.WithTimeout(ctx, scriptWaitBudget(steps))
	defer cancel()
	env := &serverInputEnv{sctx: sctx, chs: chs, ws: ws, leaseCh: leaseCh, sasCh: sasCh, v: v}
	res := runInputSteps(sctx, steps, env)
	res.LeaseRevokedAs = note.revocation()
	log.Info("input script done", "ok", res.OK, "steps", len(res.Steps), "err", res.Err)
	return res
}

// waitFirstKeyframe 轮询首个解码 IDR(脚本开始的前提:看得见桌面)。
func waitFirstKeyframe(v *viewer, d time.Duration) error {
	deadline := time.Now().Add(d)
	for v.keyframes.Load() == 0 {
		if time.Now().After(deadline) {
			return fmt.Errorf("no keyframe decoded within %v (input script aborted)", d)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

// decodeCursorWire 解 cursor 通道 9B [s32 x][s32 y][u8 visible]
// (长度不符 → ok=false;T3 契约)。
func decodeCursorWire(b []byte) (x, y int32, visible bool, ok bool) {
	if len(b) != 9 {
		return 0, 0, false, false
	}
	return int32(binary.LittleEndian.Uint32(b)),
		int32(binary.LittleEndian.Uint32(b[4:])), b[8] != 0, true
}
