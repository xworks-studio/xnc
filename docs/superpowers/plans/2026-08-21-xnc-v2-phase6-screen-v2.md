# XNC v2 Phase 6（桌面流）Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 交付桌面流（screen streaming）：DXGI Desktop Duplication GPU 捕获 + H.264 硬件编码 + 多观众单管线 + 浏览器 WebCodecs 解码 + CLI `xnc screen`，自适应帧率（静止→0fps，变化→30fps），键鼠控制协议预留。

**Architecture:** Agent（Session 0）通过 CreateProcessAsUser 拉起唯一 helper 进程（用户会话），helper 用 DXGI Desktop Duplication 捕获 GPU 纹理（脏区检测）→ H.264 硬件编码（NVENC/QSV/AMF/MFT fallback）→ named pipe 推给 Agent 的 ScreenStreamManager（单例，管理 helper 生命周期 + FrameHub 广播）→ N 个 screen 会话各自推送 WS binary 帧 → 浏览器 WebCodecs 解码渲染到 canvas。Server 纯转发。

**Tech Stack:** `x/sys/windows`（WTS/CreateProcessAsUser/named pipe）+ `go-ghwdetect` 或手写 DXGI COM 调用 + `go.mdx/pkcs7` 不需要——H.264 编码用 Windows MFT（Media Foundation Transform）COM API，硬件加速通过 MFT 枚举自动选择。Go CGO 禁用下用 COM 纯 syscall。

**Spec:** `spec.md` §64（修订为桌面流）+ 本文档。

## Global Constraints

- **Session 0 桥接**：Agent（Session 0 服务）→ WTSQueryUserToken + CreateProcessAsUser → helper.exe（用户会话）→ DXGI 捕获 + H.264 编码 → named pipe → Agent。不变。
- **单管线多观众**：ScreenStreamManager 单例——首个观众启动 helper，最后一个离开停止。FrameHub 广播帧到 N 个订阅 session。
- **捕获技术**：DXGI Desktop Duplication（Win8+，GPU 加速，脏区检测）。GDI 仅作 fallback（WDDM 驱动不支持时）。
- **编码**：H.264 Baseline Profile（浏览器兼容性最好）。硬件优先（NVENC→QSV→AMF），软件 fallback（MFT H.264 Software Encoder）。YUV420P。
- **帧率自适应**：桌面静止 → 0fps（无 DXGI 帧到达即无输出）；少量变化 → ~5fps；频繁变化 → 上限 30fps。客户端可传 `fps` 上限参数。
- **帧类型**：I 帧（关键帧/全量）+ P 帧（增量/脏区）。关键帧间隔：每 2 秒或按需（新观众加入时立即发 I 帧）。
- **pipe 协议**：
  ```
  [1B 类型][4B 长度(payload 字节)][payload]
  类型：
    0x01 = H.264 I 帧（完整 NALU 序列，含 SPS/PPS/IDR）
    0x02 = H.264 P 帧（增量 NALU）
    0x03 = 状态变化（payload = ASCII：capturing/locked/no_session）
    0x04 = 分辨率通知（payload = 8B：width int32 LE + height int32 LE）
    0x05 = 输入事件（预留，Phase 6+ 键鼠控制）
  ```
- **会话 WS 协议**：
  ```
  SESSION_OPEN params: {"fps"?:15, "quality"?:60, "maxWidth"?:1920, "codec"?:"h264"}
  text  SCREEN_BEGIN {"width":W, "height":H, "state":"capturing", "codec":"h264"}
  text  SCREEN_STATE {"state":"capturing|locked|no_session"}
  binary H.264 NALU（I 帧含 SPS/PPS/IDR；P 帧仅增量数据）
  ```
  新观众加入 → FrameHub 立即推送缓存的最新 I 帧 + SPS/PPS → 解码器可立即初始化。
- **REST**：`POST /api/nodes/{id}/screen {"fps"?, "quality"?, "maxWidth"?}` → 202 统一响应。RBAC operator+。
- **CLI**：`xnc screen <node> --snapshot out.jpg`（收 I 帧 → 解码 → 写 JPEG）；`xnc screen <node> --open`（生成 HTML → 浏览器 canvas + WebCodecs）。
- **Web UI**：NodeDetail → [Screen Preview] → canvas + WebCodecs 实时流。
- **清理**：最后一个观众离开 → helper 进程退出 + named pipe 关闭 + GPU 资源释放。
- **三态**：capturing（正常出帧）/ locked（锁屏，OpenInputDesktop 失败）/ no_session（WTSGetActiveConsoleSessionId == 0xFFFFFFFF）。
- **带宽预算**：1080p 30fps 满屏变化 ≤ 2MB/s；静止桌面 ~0KB/s。
- **CPU 预算**：helper 进程 ≤ 5%（硬件编码时 ~1%）。
- 双 GOOS 编译门槛（Windows 代码 behind build tags）。

**不含**：键鼠注入（协议预留 0x05，实现归 Phase 6+ 增量）、音频流、多显示器选择（主显示器 only）、WebRTC（用 WebSocket + WebCodecs 替代，无需 STUN/TURN）。

---

## 文件结构总览

```text
proto/
├── session.go                 T1  +KindScreen/ScreenParams/ScreenBegin/ScreenState
└── session_test.go            T1
server/internal/api/
├── screen_handlers.go         T2  POST /screen（复用 startSession）
├── screen_handlers_test.go    T2
agent/session/
├── screen.go                  T3  ScreenStreamManager（单例）+ FrameHub + ScreenHandler
├── screen_test.go             T3  mock pipe 驱动
agent/screen-helper/
├── main.go                    T4  入口 + pipe 客户端 + 主循环
├── capture_windows.go         T5  DXGI Desktop Duplication（GPU 捕获 + 脏区）
├── encode_windows.go          T6  H.264 MFT 编码器（硬件枚举 + 编码循环）
├── pipe_windows.go            T4  named pipe 服务端
├── fallback_gdi_windows.go    T7  GDI fallback（WDDM 不支持时）
├── capture_other.go           T4  非 Windows 桩
├── encode_other.go            T6  非 Windows 桩
├── go.mod                     T4  独立模块（独立 exe）
agent/agent.go / mockagent     T8  注册 KindScreen
cli/
├── cmd_screen.go              T9  --snapshot（WS→I帧→JPEG）/ --open（HTML+WebCodecs）
├── cmd_screen_test.go         T9
web/src/pages/
├── ScreenPreview.tsx          T10 canvas + WebCodecs 实时流
web/src/pages/NodeDetail.tsx   T10 + [Screen Preview] 按钮
scripts/e2e_phase6.sh          T11 帧到达 + 多观众 + 无残留
```

---

### Task 1: proto — screen 词汇

同前——KindScreen + ScreenParams/ScreenBegin/ScreenState。codec 字段预留。

---

### Task 2: server — screen 端点

同前——复用 startSession，operator+ RBAC。

---

### Task 3: agent — ScreenStreamManager + FrameHub

**Files:**
- Create: `agent/session/screen.go`、`agent/session/screen_test.go`

**Interfaces:**

```go
// ScreenStreamManager 管理共享捕获管线。单例。
type ScreenStreamManager struct {
    mu          sync.Mutex
    helper      *exec.Cmd
    pipeConn    net.Conn  // named pipe 客户端连接
    subscribers map[string]chan ScreenFrame  // sessionID → 帧 channel
    lastKeyFrame []byte    // 最新 I 帧缓存（新观众立即推送）
    lastSPSPPS  []byte    // SPS/PPS 缓存
    width, height int
    state       string    // capturing/locked/no_session
    helperPath  string
    log         *slog.Logger
}

type ScreenFrame struct {
    Type    byte   // 0x01 I帧 / 0x02 P帧 / 0x03 状态 / 0x04 分辨率
    Data    []byte
}

func GetScreenStreamManager() *ScreenStreamManager  // 全局单例
func (m *ScreenStreamManager) Subscribe(sessionID string) <-chan ScreenFrame
func (m *ScreenStreamManager) Unsubscribe(sessionID string)
func (m *ScreenStreamManager) State() (width, height int, state string)
```

**生命周期**：
- Subscribe 时检查 helper 是否已运行——未运行则启动
- Unsubscribe 时检查剩余订阅者——归零则杀 helper + 关 pipe
- pipe 读取 goroutine：解析帧 → 更新缓存 → 广播到所有 subscriber channel

**Screen Handler**（实现 session.Handler 接口）：
```go
type ScreenHandler struct{ Manager *ScreenStreamManager }
func (h *ScreenHandler) Handle(ctx, ws, sessionID, params) {
    // 1. Manager.Subscribe(sessionID)
    // 2. 发 SCREEN_BEGIN {width, height, state, codec:"h264"}
    // 3. 推送缓存的 SPS/PPS + 最新 I 帧（新观众立即有画面）
    // 4. select ctx.Done / frame channel → WS binary
    // 5. defer Manager.Unsubscribe(sessionID)
}
```

测试用 mock pipe 写帧验证广播逻辑。

---

### Task 4: helper — 入口 + named pipe

**Files:**
- Create: `agent/screen-helper/main.go`、`agent/screen-helper/pipe_windows.go`、`agent/screen-helper/capture_other.go`、`agent/screen-helper/go.mod`

```
用法：xnc-screen-helper.exe --pipe xnc-screen-<hash> --max-width 1920 --quality 60
```

pipe_windows.go：CreateNamedPipe 服务端 + 等待连接 + Write 帧协议。

主循环（在 capture/encode 就位前先跑通 pipe 通信）：
```go
for {
    time.Sleep(time.Second)  // 占位——T5/T6 替换为真实捕获+编码
    writeFrame(w, 0x03, []byte("capturing"))
}
```

---

### Task 5: helper — DXGI Desktop Duplication 捕获

**Files:**
- Create: `agent/screen-helper/capture_windows.go`

```go
// DXGI Desktop Duplication COM 调用（纯 syscall，无 CGO）
// 流程：
// 1. CoInitializeEx
// 2. CreateDXGIFactory1 → EnumAdapters → EnumOutputs → GetDesc → QueryInterface(IDXGIDuplicateOutput)
// 3. 循环 AcquireNextFrame（阻塞等待变化，timeout 控制帧率上限）
// 4. 获取 GPU 纹理 → 脏区检测 → 转换为可编码格式
// 5. ReleaseFrame

type DXGICapturer struct {
    dup      *IDXGIOutputDuplication  // COM 接口
    device   *ID3D11Device
    context  *ID3D11DeviceContext
    width, height int
}

func NewDXGICapturer() (*DXGICapturer, error)
func (c *DXGICapturer) AcquireFrame(timeoutMs uint) (texture *ID3D11Texture2D, dirtyRects []RECT, err error)
func (c *DXGICapturer) ReleaseFrame()
func (c *DXGICapturer) Close()

// GPU 纹理 → 系统内存（用于编码器输入）
// CopyResource 到 staging texture → Map → RGBA 字节
// 如果有脏区，只拷贝脏区的像素
```

**COM syscall 层**：手写 `IDXGIDuplicateOutput_AcquireNextFrame` 等的 syscall。参考项目 `github.com/kbinani/screenshot` 或直接用 GUID + vtable 调用。

**锁屏检测**：AcquireNextFrame 在锁屏时返回 DXGI_ERROR_ACCESS_LOST → 切换到 locked 状态。恢复后重建 duplication。

---

### Task 6: helper — H.264 MFT 编码器

**Files:**
- Create: `agent/screen-helper/encode_windows.go`、`agent/screen-helper/encode_other.go`

```go
// Windows Media Foundation Transform H.264 编码器
// 流程：
// 1. CoCreateInstance(CLSID_CMSH264EncoderMFT) 或枚举硬件 MFT
// 2. 设置输入类型：NV12 或 RGB32 → 转换为 YUV420P
// 3. 设置输出类型：H.264 Baseline, 指定比特率/关键帧间隔
// 4. 编码循环：喂入帧 → ProcessInput → ProcessOutput → 收集 NALU

type H264Encoder struct {
    mft        *IMFTransform
    inputStream *IMFMediaBuffer
    width, height int
    bitrate    uint32   // e.g. 2_000_000 (2Mbps)
    gopSize    uint32   // 关键帧间隔（帧数），30fps × 2s = 60
}

func NewH264Encoder(width, height, bitrate, gopSize int) (*H264Encoder, error)
func (e *H264Encoder) Encode(frame []byte, isKeyFrame bool) (nalus []byte, err error)
func (e *H264Encoder) Close()

// GetParameters：返回 SPS/PPS（首次编码后可用）
func (e *H264Encoder) SPSPPS() []byte
```

**硬件枚举**：MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODATOR, MFT_ENUM_FLAG_HARDWARE) → 优先选硬件 MFT。无硬件 → fallback 到 CMSH264EncoderMFT（Microsoft 软件 H.264 编码器）。

**颜色空间转换**：DXGI 输出 BGRA → 编码器需要 NV12/YUV420P。CPU 转换（SIMD 可选，BGRA→YUV420P ~2ms @1080p）。

---

### Task 7: helper — GDI fallback + 整合主循环

**Files:**
- Create: `agent/screen-helper/fallback_gdi_windows.go`
- Modify: `agent/screen-helper/main.go`（整合完整流程）

```go
// 完整主循环：
func main() {
    // 1. 尝试 DXGI
    capturer, err := NewDXGICapturer()
    if err != nil {
        // fallback 到 GDI（旧驱动/WDDM 不支持）
        capturer = NewGDICapturer()
    }
    defer capturer.Close()

    // 2. 创建编码器
    encoder, err := NewH264Encoder(width, height, bitrate, gopSize)
    defer encoder.Close()

    // 3. named pipe 服务端
    pipe := NewPipeServer(pipeName)
    defer pipe.Close()
    pipe.WaitForConnection()

    // 4. 发分辨率
    writeFrame(pipe, 0x04, packDim(width, height))

    // 5. 编码循环
    for {
        // 捕获（阻塞等待变化，DXGI 超时 100ms 控制帧率上限）
        frame, dirty, err := capturer.AcquireFrame(100)
        if err == ErrAccessLost {
            // 锁屏/切换用户 → 状态
            writeFrame(pipe, 0x03, []byte("locked"))
            continue
        }
        if err == ErrTimeout {
            // 无变化 → 不编码不出帧（自适应 0fps）
            continue
        }

        // 有变化 → 编码
        nalu, err := encoder.Encode(frame, needKeyFrame)
        if err != nil { continue }

        // 推送
        if isKeyFrame {
            writeFrame(pipe, 0x01, spsPPS+nalu)
        } else {
            writeFrame(pipe, 0x02, nalu)
        }
    }
}
```

GDI fallback：BitBlt 全帧捕获 → BGRA → 编码器。帧率上限 10fps（CPU 限制）。

---

### Task 8: 注册

agent.go / mockagent/main.go 注册 KindScreen → ScreenHandler（使用全局 GetScreenStreamManager()）。

Helper 部署：构建产物 `bin/xnc-screen-helper.exe`，与 agent 同目录部署。

---

### Task 9: CLI — xnc screen

```text
xnc screen <node> --snapshot out.jpg
  → POST /screen → WS → 等 SCREEN_BEGIN + SPS/PPS + 首个 I 帧
  → 本地解码 H.264 I 帧（需要嵌入解码器——用 Go image/jpeg 不行，H.264 需要解码器）
  → 简化方案：--snapshot 收到 H.264 I 帧后保存为 .h264 文件（用户用 ffmpeg/vlc 查看）
  → 或者：helper 在收到 --snapshot 特殊参数时输出 JPEG 而非 H.264
  → 选后者：helper 增加 --jpeg-mode 参数，单帧 GDI 捕获 + GDI+ JPEG 输出

xnc screen <node> --open
  → POST /screen → 生成本地 HTML（WebCodecs + canvas）→ 打开浏览器
  → HTML 内嵌 WS URL + token（60s 有效）
```

**--snapshot 简化**：helper 支持 `--jpeg-single <output-path>` 模式——直接 GDI 截屏 + GDI+ JPEG 编码 + 写文件，不走 pipe/H.264。Agent 收到 `--snapshot` 请求时拉起 helper 的这个模式。

---

### Task 10: Web UI — ScreenPreview（canvas + WebCodecs）

```tsx
// ScreenPreview.tsx 核心逻辑
const decoder = new VideoDecoder({
  output: (frame) => {
    ctx.drawImage(frame, 0, 0, canvas.width, canvas.height);
    frame.close();
  },
  error: (e) => setError(e.message),
});
decoder.configure({ codec: 'avc1.42E01E' }); // Baseline

ws.onmessage = (ev) => {
  if (typeof ev.data === 'string') {
    // SCREEN_BEGIN / SCREEN_STATE
    const msg = JSON.parse(ev.data);
    if (msg.type === 'SCREEN_BEGIN') { setSize(msg.payload.width, msg.payload.height); }
    if (msg.type === 'SCREEN_STATE') { setState(msg.payload.state); }
  } else {
    // H.264 NALU → 解码
    const data = new Uint8Array(ev.data);
    const nalType = data[4] & 0x1F; // NAL header after start code
    const isKey = nalType === 5 || nalType === 7; // IDR or SPS
    decoder.decode(new EncodedVideoChunk({
      type: isKey ? 'key' : 'delta',
      timestamp: performance.now(),
      data: data,
    }));
  }
};
```

UI：全屏面板，顶部状态栏（节点名 + state 徽章 + codec + 分辨率），中间 canvas。

---

### Task 11: E2E

脚本流程：
1. Dev 栈 + mockagent 上线
2. `xnc screen MOCK-xxx --snapshot out.jpg` → JPEG 文件非空 + 状态正确
3. 验证 helper 无残留：`tasklist /FI "IMAGENAME eq xnc-screen-helper.exe"` → 空
4. Web UI 预览面板打开 → 收到帧（用 shellsmoke 或手动验证）
5. 清理

真机验收（合并后）：TB16G7 三种状态 + 画面流畅度 + 带宽测量。

---

## 完成定义

```text
全模块测试绿 + 双 GOOS 编译
bash scripts/e2e_phase6.sh 全绿
bin/xnc-screen-helper.exe 构建产出（与 agent 同部署）
真机 TB16G7：
  打开 Web UI 预览 → 连续画面（>10fps 变化场景）
  桌面静止 → 带宽 ~0 KB/s
  锁屏 → 状态 "locked"（非黑屏）
  注销 → 状态 "no_session"
  关闭后 tasklist 无 helper 进程残留
  2 人同时预览 → 1 份 helper 进程
CLI xnc screen --snapshot → 可查看 JPEG
```
