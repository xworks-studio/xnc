// screen.go — 会话 kind=screen 的 agent 侧架构：ScreenStreamManager 单例
// （单 helper 进程，多观众共享管线）+ FrameHub（named pipe 读循环解析帧协议
// 并广播）+ ScreenHandler（订阅 → SCREEN_BEGIN → 缓存 I 帧立即出画面 →
// H.264 NALU 以 binary 帧转发，状态变化以 SCREEN_STATE text 帧通知）。
//
// pipe 帧协议（helper → agent）：[1B 类型][4B 长度 LE][payload]
//   0x01 I 帧（含 SPS/PPS/IDR）/ 0x02 P 帧 / 0x03 状态 / 0x04 分辨率。
package session

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"

	"github.com/coder/websocket"

	"xnc/proto"
)

const (
	// typeScreenBegin / typeScreenState screen 会话 WS 的 text 帧类型
	// （kind 私有词汇，归属规则同 SHELL_BEGIN）。
	typeScreenBegin = "SCREEN_BEGIN"
	typeScreenState = "SCREEN_STATE"

	// pipe 帧类型。
	screenFrameKey     byte = 0x01
	screenFrameDelta   byte = 0x02
	screenFrameState   byte = 0x03
	screenFrameDims    byte = 0x04
	screenMaxFrameSize      = 32 << 20 // 单帧 payload 上限，防损坏头撑爆内存

	// subscriber channel 缓冲：慢观众丢帧（等下一个 I 帧）而非阻塞管线。
	screenSubBuffer = 64

	screenDefaultFps      = 15
	screenDefaultQuality  = 60
	screenDefaultMaxWidth = 1920
)

// ScreenFrame 是 FrameHub 广播给订阅者的帧（pipe 帧去掉长度头）。
type ScreenFrame struct {
	Type byte   // 0x01 I 帧 / 0x02 P 帧 / 0x03 状态 / 0x04 分辨率
	Data []byte
}

// ScreenStreamManager 管理共享捕获管线。单例——无论观众数，仅一个 helper
// 进程：首个 Subscribe 启动，最后一个 Unsubscribe 停止。
type ScreenStreamManager struct {
	mu           sync.Mutex
	helperCmd    *exec.Cmd
	pipeConn     net.Conn
	subscribers  map[string]chan ScreenFrame // sessionID → 帧 channel
	lastKeyFrame []byte                      // 最新 I 帧缓存（新观众立即推送）
	lastSPSPPS   []byte                      // SPS/PPS 缓存（解码器初始化）
	width, height int
	state        string // capturing / locked / no_session
	helperPath   string
	log          *slog.Logger
	stopCh       chan struct{}
	running      bool

	// starter 可注入替换 helper 启动路径（测试用 net.Pipe 模拟）；nil 时
	// 走 launchHelperLocked（真实 helper 进程 + named pipe）。
	starter func(m *ScreenStreamManager) (net.Conn, *exec.Cmd, error)
}

var (
	screenStreamManager *ScreenStreamManager
	screenStreamOnce    sync.Once
)

// GetScreenStreamManager 返回全局单例（helper 与 agent 二进制同目录）。
func GetScreenStreamManager() *ScreenStreamManager {
	screenStreamOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			exe = ""
		}
		screenStreamManager = newScreenStreamManager(exe, slog.Default())
	})
	return screenStreamManager
}

// newScreenStreamManager 构造未启动的管理器；agentDir 为 agent 二进制目录
// （其下寻找 xnc-screen-helper.exe）。
func newScreenStreamManager(agentExe string, log *slog.Logger) *ScreenStreamManager {
	if log == nil {
		log = slog.Default()
	}
	helperPath := helperPathFromExe(agentExe)
	return &ScreenStreamManager{
		subscribers: map[string]chan ScreenFrame{},
		state:       "no_session",
		helperPath:  helperPath,
		log:         log,
	}
}

// helperPathFromExe 返回与 agent 同目录的 helper 路径（跨平台文件名一致，
// Windows 上为 xnc-screen-helper.exe）。
func helperPathFromExe(agentExe string) string {
	if agentExe == "" {
		return "xnc-screen-helper"
	}
	if runtime.GOOS == "windows" {
		return agentExe[:len(agentExe)-len(".exe")] + "-screen-helper.exe"
	}
	return agentExe + "-screen-helper"
}

// Subscribe 添加一个观众；helper 未运行则启动。返回帧广播 channel
// （manager 停止时关闭）。
func (m *ScreenStreamManager) Subscribe(sessionID string) <-chan ScreenFrame {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running {
		if err := m.startPipelineLocked(); err != nil {
			m.log.Warn("screen helper start failed", "err", err)
			m.state = "no_session"
		}
	}
	ch := make(chan ScreenFrame, screenSubBuffer)
	m.subscribers[sessionID] = ch
	return ch
}

// Unsubscribe 移除一个观众；最后一个离开则停止 helper 并关闭 pipe。
func (m *ScreenStreamManager) Unsubscribe(sessionID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopSubscriberLocked(sessionID)
	if len(m.subscribers) == 0 {
		m.stopLocked()
	}
}

// stopSubscriberLocked 关闭并移除单个订阅 channel（ broadcaster 持锁发送，
// 不会与 close 竞争）。
func (m *ScreenStreamManager) stopSubscriberLocked(sessionID string) {
	if ch, ok := m.subscribers[sessionID]; ok {
		close(ch)
		delete(m.subscribers, sessionID)
	}
}

// stopLocked 停止管线：关 stopCh、杀 helper、关 pipe、解散全部订阅者。
func (m *ScreenStreamManager) stopLocked() {
	if !m.running {
		return
	}
	m.running = false
	close(m.stopCh)
	if m.helperCmd != nil && m.helperCmd.Process != nil {
		_ = m.helperCmd.Process.Kill()
		_ = m.helperCmd.Wait()
		m.helperCmd = nil
	}
	if m.pipeConn != nil {
		_ = m.pipeConn.Close()
		m.pipeConn = nil
	}
	for id, ch := range m.subscribers {
		close(ch)
		delete(m.subscribers, id)
	}
}

// Running 报告管线是否在运行（测试与非 windows 桩路径使用）。
func (m *ScreenStreamManager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running
}

// State 返回当前分辨率与捕获状态。
func (m *ScreenStreamManager) State() (width, height int, state string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.width, m.height, m.state
}

// CachedKeyFrame 返回缓存的 SPS/PPS 与最新 I 帧（新观众立即出画面）。
func (m *ScreenStreamManager) CachedKeyFrame() (spspps, key []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastSPSPPS, m.lastKeyFrame
}

// Snapshot 运行 helper 的 --jpeg-single 模式：单帧 GDI 截屏 → JPEG 文件 →
// 读回字节。不触碰流式管线（helper 截完即退出，无 named pipe）。
func (m *ScreenStreamManager) Snapshot(quality int) ([]byte, error) {
	m.mu.Lock()
	helperPath := m.helperPath
	m.mu.Unlock()
	if quality <= 0 {
		quality = screenDefaultQuality
	}
	tmp, err := os.CreateTemp("", "xnc-snapshot-*.jpg")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	cmd := exec.Command(helperPath, "--jpeg-single", tmpPath,
		"--quality", strconv.Itoa(quality))
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("screen helper: %v: %s", err, bytes.TrimSpace(out))
	}
	return os.ReadFile(tmpPath)
}

// startPipelineLocked 启动管线（注入或真实 helper），成功即置 running 并
// 拉起读循环。
func (m *ScreenStreamManager) startPipelineLocked() error {
	m.stopCh = make(chan struct{})
	var (
		conn net.Conn
		cmd  *exec.Cmd
		err  error
	)
	if m.starter != nil {
		conn, cmd, err = m.starter(m)
	} else {
		conn, cmd, err = m.launchHelperLocked()
	}
	if err != nil {
		m.stopCh = nil
		return err
	}
	m.running = true
	m.helperCmd = cmd
	m.pipeConn = conn
	go m.pipeReadLoop(conn)
	return nil
}

// launchHelperLocked 拉起 helper 进程并连接其 named pipe（pipe 名按 agent
// PID 确定性生成，同一 agent 运行期内不变）。
func (m *ScreenStreamManager) launchHelperLocked() (net.Conn, *exec.Cmd, error) {
	pipeName := screenPipeName()
	cmd := exec.Command(m.helperPath, "--pipe", pipeName)
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	conn, err := dialPipe(pipeName)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil, nil, err
	}
	return conn, cmd, nil
}

// screenPipeName 返回确定性 pipe 名（同一 agent 运行期内一致）。
func screenPipeName() string {
	return "xnc-screen-" + strconv.Itoa(os.Getpid())
}

// pipeReadLoop 从 named pipe 读取帧协议并分发：更新缓存（I 帧 / SPS/PPS /
// 状态 / 分辨率）后广播到所有订阅者。pipe 断开（helper 退出 / manager 主动
// 停止）即收线；若非主动停止，则整体停管线，下一次 Subscribe 会重新拉起。
func (m *ScreenStreamManager) pipeReadLoop(conn net.Conn) {
	hdr := make([]byte, 5)
	for {
		if _, err := io.ReadFull(conn, hdr); err != nil {
			break
		}
		typ := hdr[0]
		n := binary.LittleEndian.Uint32(hdr[1:5])
		if n > screenMaxFrameSize {
			m.log.Warn("screen pipe frame too large", "size", n)
			break
		}
		payload := make([]byte, n)
		if _, err := io.ReadFull(conn, payload); err != nil {
			break
		}
		m.dispatchFrame(typ, payload)
	}
	// helper 死亡 / manager 主动停止都走到这里：若仍是当前连接则停管线
	// （主动停止路径 stopLocked 已置空 pipeConn，此处不重复）。
	m.mu.Lock()
	if m.pipeConn == conn {
		m.stopLocked()
	}
	m.mu.Unlock()
}

// dispatchFrame 更新缓存并广播单帧（发送持锁，与 close 互斥）。
func (m *ScreenStreamManager) dispatchFrame(typ byte, payload []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch typ {
	case screenFrameKey:
		spspps, key := splitSPSPPS(payload)
		if len(spspps) > 0 {
			m.lastSPSPPS = spspps
		}
		if len(key) > 0 {
			m.lastKeyFrame = key
		} else if len(spspps) == 0 {
			m.lastKeyFrame = payload // 无 start code：整体视为 I 帧
		}
	case screenFrameState:
		m.state = string(payload)
	case screenFrameDims:
		if len(payload) >= 8 {
			m.width = int(int32(binary.LittleEndian.Uint32(payload[0:4])))
			m.height = int(int32(binary.LittleEndian.Uint32(payload[4:8])))
		}
	}
	m.broadcastLocked(typ, payload)
}

// broadcastLocked 非阻塞发送到所有订阅者；满则丢帧（慢观众等下个 I 帧）。
func (m *ScreenStreamManager) broadcastLocked(typ byte, payload []byte) {
	for _, ch := range m.subscribers {
		select {
		case ch <- ScreenFrame{Type: typ, Data: payload}:
		default:
		}
	}
}

// splitSPSPPS 从 I 帧 payload 中拆出 SPS/PPS NALU（type 7/8）与其余 NALU。
// 无 start code 时两者均为空。
func splitSPSPPS(nalus []byte) (spspps, rest []byte) {
	i := 0
	n := len(nalus)
	// nextStartCode 返回下一个 start code 之后的索引与 NALU 头位置。
	next := func(from int) (scEnd int, ok bool) {
		for j := from; j+2 < n; j++ {
			if nalus[j] == 0 && nalus[j+1] == 0 && nalus[j+2] == 1 {
				return j + 3, true
			}
		}
		return 0, false
	}
	for {
		scEnd, ok := next(i)
		if !ok {
			return spspps, rest
		}
		// 找该 NALU 的结尾（下一个 start code 起点，含 4B 变体）。
		end := n
		for j := scEnd; j+2 < n; j++ {
			if nalus[j] == 0 && nalus[j+1] == 0 && (nalus[j+2] == 1 || (nalus[j+2] == 0 && j+3 < n && nalus[j+3] == 1)) {
				end = j
				break
			}
		}
		nalu := nalus[scEnd-3 : end] // 含 3B start code
		switch nalu[3] & 0x1F {
		case 7, 8:
			spspps = append(spspps, nalu...)
		default:
			rest = append(rest, nalu...)
		}
		if end >= n {
			return spspps, rest
		}
		i = end
	}
}

// ScreenHandler 实现 session.Handler：一个 screen 会话 = 一个观众。
type ScreenHandler struct{ Manager *ScreenStreamManager }

// NewScreenHandler 构造使用全局单例管理器的处理器。
func NewScreenHandler() *ScreenHandler {
	return &ScreenHandler{Manager: GetScreenStreamManager()}
}

// Handle 承载一个观众直至 ctx 取消或管线停止：订阅 → SCREEN_BEGIN →
// （capturing 时）推送缓存 SPS/PPS + I 帧使新观众立即有画面 → 循环转发
// binary NALU / SCREEN_STATE。
func (h *ScreenHandler) Handle(ctx context.Context, ws *websocket.Conn, sessionID string, params json.RawMessage) {
	var p proto.ScreenParams
	_ = json.Unmarshal(params, &p)
	normalizeScreenParams(&p)

	// 单帧快照模式：helper --jpeg-single 截屏后以单个 binary JPEG 帧回送，
	// 随即收线——不进入流式管线（不启动共享 helper / named pipe）。
	if p.Snapshot {
		jpeg, err := h.Manager.Snapshot(p.Quality)
		if err != nil {
			h.Manager.log.Warn("screen snapshot failed", "err", err)
			writeScreenText(ctx, ws, proto.TypeError, proto.ErrorPayload{
				Code: "SNAPSHOT_FAILED", Message: err.Error(),
			})
			return
		}
		writeScreenText(ctx, ws, typeScreenBegin, proto.ScreenBegin{
			State: "capturing", Codec: "jpeg",
		})
		writeScreenBinary(ctx, ws, jpeg)
		_ = ws.Close(websocket.StatusNormalClosure, "snapshot done")
		return
	}

	ch := h.Manager.Subscribe(sessionID)
	defer h.Manager.Unsubscribe(sessionID)

	w, ht, state := h.Manager.State()
	writeScreenText(ctx, ws, typeScreenBegin, proto.ScreenBegin{
		Width: w, Height: ht, State: state, Codec: "h264",
	})
	if !h.Manager.Running() {
		return // helper 启动失败（或非 windows）：只报状态即收线
	}

	// 新观众立即出画面：缓存 SPS/PPS + 最新 I 帧。
	spspps, key := h.Manager.CachedKeyFrame()
	if state == "capturing" && len(spspps)+len(key) > 0 {
		frame := make([]byte, 0, len(spspps)+len(key))
		frame = append(frame, spspps...)
		frame = append(frame, key...)
		writeScreenBinary(ctx, ws, frame)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-ch:
			if !ok {
				return // 管线停止
			}
			switch f.Type {
			case screenFrameState:
				writeScreenText(ctx, ws, typeScreenState, proto.ScreenState{State: string(f.Data)})
			case screenFrameDims:
				// 分辨率已编入二进制流（0x04 广播仅驱动 manager 状态），无 WS 帧。
			default:
				writeScreenBinary(ctx, ws, f.Data)
			}
		}
	}
}

// normalizeScreenParams 就地补默认值并夹紧（防御性；server 已校验）。
func normalizeScreenParams(p *proto.ScreenParams) {
	if p.Fps <= 0 {
		p.Fps = screenDefaultFps
	}
	if p.Fps > 30 {
		p.Fps = 30
	}
	if p.Quality <= 0 {
		p.Quality = screenDefaultQuality
	}
	if p.MaxWidth <= 0 {
		p.MaxWidth = screenDefaultMaxWidth
	}
}

func writeScreenText(ctx context.Context, ws *websocket.Conn, typ string, payload any) {
	msg, err := proto.NewMsg(typ, payload)
	if err != nil {
		return
	}
	jb, err := json.Marshal(msg)
	if err != nil {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
	defer cancel()
	_ = ws.Write(wctx, websocket.MessageText, jb)
}

func writeScreenBinary(ctx context.Context, ws *websocket.Conn, data []byte) {
	wctx, cancel := context.WithTimeout(ctx, execWriteTimeout)
	defer cancel()
	_ = ws.Write(wctx, websocket.MessageBinary, data)
}
