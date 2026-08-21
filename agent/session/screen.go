// screen.go — 会话 kind=screen 的 agent 侧架构：ScreenStreamManager 单例
// （单 helper 进程，多观众共享管线）+ FrameHub（named pipe 读循环解析帧协议
// 并广播）+ ScreenHandler（订阅 → SCREEN_BEGIN → 缓存 I 帧立即出画面 →
// H.264 NALU 以 binary 帧转发，状态变化以 SCREEN_STATE text 帧通知）。
//
// pipe 帧协议（helper → agent）：[1B 类型][4B 长度 LE][payload]
//
//	0x01 I 帧（含 SPS/PPS/IDR）/ 0x02 P 帧 / 0x03 状态 / 0x04 分辨率。
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
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"xnc/proto"
)

const (
	// typeScreenBegin / typeScreenState screen 会话 WS 的 text 帧类型
	// （kind 私有词汇，归属规则同 SHELL_BEGIN）。
	typeScreenBegin = "SCREEN_BEGIN"
	typeScreenState = "SCREEN_STATE"

	// typeScreenFeedback 观众 → agent 的回传（浏览器解码器错误等）：
	// 此前解码错误死在浏览器 console，是纯黑洞——现在 surfaced 到 agent
	// 日志，编码侧问题第一时间可见。
	typeScreenFeedback = "SCREEN_FEEDBACK"

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
	Type byte // 0x01 I 帧 / 0x02 P 帧 / 0x03 状态 / 0x04 分辨率
	Data []byte
}

// ScreenStreamManager 管理共享捕获管线。单例——无论观众数，仅一个 helper
// 进程：首个 Subscribe 启动，最后一个 Unsubscribe 停止。
type ScreenStreamManager struct {
	mu            sync.Mutex
	helperCmd     *helperProc
	pipeConn      net.Conn
	helperLog     *os.File                    // helper stderr 日志文件（句柄继承给子进程）
	subscribers   map[string]chan ScreenFrame // sessionID → 帧 channel
	lastKeyFrame  []byte                      // 最新 I 帧缓存（新观众立即推送）
	lastSPSPPS    []byte                      // SPS/PPS 缓存（解码器初始化）
	width, height int
	state         string // capturing / locked / no_session
	helperPath    string
	// viewerParams 首个订阅者的捕获参数（helper 启动参数 --fps/--quality/
	// --max-width 的来源）；管线已运行时后续订阅者的参数不生效（共享管线）。
	viewerParams proto.ScreenParams
	log          *slog.Logger
	stopCh       chan struct{}
	running      bool

	// starter 可注入替换 helper 启动路径（测试用 net.Pipe 模拟）；nil 时
	// 走 launchHelperLocked（真实 helper 进程 + named pipe）。
	starter func(m *ScreenStreamManager) (net.Conn, *helperProc, error)
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

// helperPathFromExe 返回与 agent 同目录的 xnc-screen-helper 路径（Windows
// 上带 .exe 后缀；跨平台文件名一致，部署脚本与 bin/ 产物同名）。
func helperPathFromExe(agentExe string) string {
	if agentExe == "" {
		return "xnc-screen-helper"
	}
	name := "xnc-screen-helper"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(filepath.Dir(agentExe), name)
}

// Subscribe 添加一个观众；helper 未运行则以该观众的捕获参数启动（fps/
// quality/maxWidth 一次定终身——共享管线，后续观众参数不生效）。返回帧
// 广播 channel（manager 停止时关闭）。
func (m *ScreenStreamManager) Subscribe(sessionID string, p proto.ScreenParams) <-chan ScreenFrame {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.running {
		m.viewerParams = p
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
	if m.helperCmd != nil {
		_ = m.helperCmd.Kill()
		_ = m.helperCmd.Wait()
		m.helperCmd = nil
	}
	if m.pipeConn != nil {
		_ = m.pipeConn.Close()
		m.pipeConn = nil
	}
	if m.helperLog != nil {
		_ = m.helperLog.Close()
		m.helperLog = nil
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
// 读回字节。不触碰流式管线（helper 截完即退出，无 named pipe）。helper 同样
// 经 launchHelperAsUser 桥接进用户会话（Session 0 服务的 GDI 截屏同样需要）。
func (m *ScreenStreamManager) Snapshot(quality int) ([]byte, error) {
	m.mu.Lock()
	helperPath, log := m.helperPath, m.log
	m.mu.Unlock()
	if quality <= 0 {
		quality = screenDefaultQuality
	}
	// 在 helper 同目录（agent 部署目录，两进程均可读写）创建快照临时文件，
	// 而非 os.CreateTemp（SYSTEM 服务的 %TEMP% 对 console 用户不可写）。
	snapDir := filepath.Dir(helperPath)
	tmp, err := os.CreateTemp(snapDir, "xnc-snapshot-*.jpg")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	var out bytes.Buffer
	cmd, err := launchHelperAsUser(log, helperPath, &out,
		"--jpeg-single", tmpPath, "--quality", strconv.Itoa(quality))
	if err != nil {
		return nil, err
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("screen helper: %v: %s", err, bytes.TrimSpace(out.Bytes()))
	}
	return os.ReadFile(tmpPath)
}

// startPipelineLocked 启动管线（注入或真实 helper），成功即置 running 并
// 拉起读循环。
func (m *ScreenStreamManager) startPipelineLocked() error {
	m.stopCh = make(chan struct{})
	var (
		conn net.Conn
		cmd  *helperProc
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
// PID 确定性生成，同一 agent 运行期内不变）。启动经 launchHelperAsUser
// （Session 0 桥接，平台实现见 screen_windows.go / screen_other.go），并按
// 首个订阅者的参数转发 --fps/--quality/--max-width。stderr 落盘到 helper
// 同目录 screen-helper.log（agent/SYSTEM 打开、句柄继承给子进程——子进程
// 无需目录写权限），stopLocked 时关闭。
func (m *ScreenStreamManager) launchHelperLocked() (net.Conn, *helperProc, error) {
	pipeName := screenPipeName()
	logFile, lerr := os.OpenFile(filepath.Join(filepath.Dir(m.helperPath), "screen-helper.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if lerr != nil {
		m.log.Warn("screen helper log file unavailable, stderr discarded", "err", lerr)
	}
	var stderr io.Writer
	if logFile != nil {
		stderr = logFile
	}
	// 启动梯子：SYSTEM-in-session（DDA + 安全桌面/UAC 捕获）→ 用户令牌
	// （WGC 回退路径）。helper 在 SYSTEM 下采集不可用时以 exit 3 自退，
	// 此处短暂等待该信号后回退；XNC_USER_HELPER=1 可强制用户模式。
	var cmd *helperProc
	var err error
	if os.Getenv("XNC_USER_HELPER") == "" {
		cmd, err = launchHelperSystem(m.log, m.helperPath, stderr, helperArgs(pipeName, m.viewerParams)...)
		if err == nil {
			if code, exited := cmd.helperExitCode(2500 * time.Millisecond); exited {
				m.log.Warn("screen helper exited early under SYSTEM token, falling back to user token",
					"code", code)
				cmd = nil
			}
		} else {
			m.log.Warn("screen helper SYSTEM launch failed, falling back to user token", "err", err)
		}
	}
	if cmd == nil {
		cmd, err = launchHelperAsUser(m.log, m.helperPath, stderr, helperArgs(pipeName, m.viewerParams)...)
	}
	if err != nil {
		if logFile != nil {
			logFile.Close()
		}
		return nil, nil, err
	}
	m.helperLog = logFile
	conn, err := dialPipe(pipeName)
	if err != nil {
		_ = cmd.Kill()
		_ = cmd.Wait()
		if logFile != nil {
			logFile.Close()
			m.helperLog = nil
		}
		return nil, nil, err
	}
	return conn, cmd, nil
}

// helperArgs 组装 helper 启动参数：pipe 名 + 首个订阅者的捕获参数（0 值省
// 略——helper 侧 flag 自带默认值）。
func helperArgs(pipeName string, p proto.ScreenParams) []string {
	args := []string{"--pipe", pipeName}
	if p.Fps > 0 {
		args = append(args, "--fps", strconv.Itoa(p.Fps))
	}
	if p.Quality > 0 {
		args = append(args, "--quality", strconv.Itoa(p.Quality))
	}
	if p.MaxWidth > 0 {
		args = append(args, "--max-width", strconv.Itoa(p.MaxWidth))
	}
	return args
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
		writeScreenBinary(ctx, ws, append([]byte{proto.ScreenBinJPEG}, jpeg...))
		_ = ws.Close(websocket.StatusNormalClosure, "snapshot done")
		return
	}

	ch := h.Manager.Subscribe(sessionID, p)
	defer h.Manager.Unsubscribe(sessionID)

	// 观众回传：消费客户端 → agent 方向的帧（SCREEN_FEEDBACK 等）并落日
	// 志。必须持续读取——server pump 双向转发，不读会反压观众的发送。
	// 生命周期：随 ctx 取消 / 连接关闭（Reader 出错）自然退出；不 join——
	// 引擎在 Handle 返回后才关闭连接，join 会死锁。
	go func() {
		for {
			_, r, err := ws.Reader(ctx)
			if err != nil {
				return
			}
			b, err := io.ReadAll(io.LimitReader(r, 4096))
			if err != nil {
				return
			}
			var m proto.Message
			if json.Unmarshal(b, &m) != nil || m.Type != typeScreenFeedback {
				continue
			}
			h.Manager.log.Warn("screen viewer feedback", "session", sessionID, "payload", string(m.Payload))
		}
	}()

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
		frame := make([]byte, 0, 1+len(spspps)+len(key))
		frame = append(frame, proto.ScreenBinKey)
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
				// 帧类型随帧走（消费端免嗅探）：0x01 key / 0x02 delta。
				sub := proto.ScreenBinDelta
				if f.Type == screenFrameKey {
					sub = proto.ScreenBinKey
				}
				writeScreenBinary(ctx, ws, append([]byte{sub}, f.Data...))
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
