// Package desktop 实现 agent 侧「实时桌面」会话(M1-Slice2 Task 4):
// desktoppipe 帧源(core StartCapture spawn 的 xnc-desktop rt pipe)→
// Pion WebRTC publisher(relay-only)→ 会话 WS 信令中转到 viewer。
//
// source.go 定义跨平台的帧源抽象:transport/session/qos 全部只依赖此抽象,
// 真实 Windows 实现(core RPC + desktoppipe.Dial)在 core_windows.go,
// 非 Windows 为桩(core_other.go)——因此回环单测(session_loopback_test.go)
// 用 fake 源即可在任意平台驱动「真实 Handler + 真实双 PeerConnection」。
// Recv* 采用 ctx 式取帧而非裸通道:desktoppipe 的通道类型无法跨平台接口
// 暴露(其包是 windows-only),适配器就地 select 转换。
package desktop

import "context"

// Frame 是一帧已编码视频 AU(Annex-B,4 字节起始码),与 desktoppipe.Frame
// 字段一一对应(core_windows.go 负责转换)。v2(0x0205)身份字段是
// desktoppipe.FrameIdentity 的透传:M3 起 ViewerSender 用其做 epoch 感知的
// WAIT_IDR(0x020B 不连续的 wire 侧镜像);v1 帧身份全零 = 退化为纯关键帧
// 门控。W/H 不为发送侧消费,不透传。
type Frame struct {
	Key           bool
	PresentMonoUs uint64 // host 单调钟微秒——RTP 时戳的真相来源
	// CaptureEpoch/CodecEpoch:capture 重建 / 编码器重置的代际(v2 身份)。
	CaptureEpoch, CodecEpoch uint64
	// ContentID:一帧采集内容的稳定 id;EncodeSeq:编码序号(v2 身份)。
	ContentID, EncodeSeq uint64
	// SourceMonoUs:采集(而非呈现)时刻(v2 身份;发送侧仅诊断用)。
	SourceMonoUs uint64
	AU           []byte
}

// HelloInfo 是 HOST_HELLO 内容镜像;gen 递增代表 capture 重建。
// Displays 为 M2-S3 Task 5 的 displays[] 镜像(nil = 旧 host 未携带)。
type HelloInfo struct {
	Gen      uint32
	W, H     uint32
	Fps      uint32
	MaxSubs  uint32
	Displays []Display
}

// Display 是 HOST_HELLO displays[] 一项的镜像(M2-S3 Task 5)。
type Display struct {
	Index   uint32
	OriginX int32
	OriginY int32
	W, H    uint32
	Primary bool
}

// StateEvent 是 STATE 事件镜像(dxgi_access_denied / capture_rebuilt 等)。
type StateEvent struct {
	Code        string
	Recoverable bool
}

// CursorEvent 是 0x0109 光标事件镜像(HOST_HELLO 流空间逻辑像素;
// M1-Slice3 Task 3)。
type CursorEvent struct {
	X, Y    int32
	Visible bool
}

// DisplayChangedEvent 是 0x010A 事件镜像:统一 CaptureReset 改变了流几何
// (M2-Slice1 Task 2)。Gen 与随后 HOST_HELLO 的 gen 一致。
type DisplayChangedEvent struct {
	Gen    uint32
	W, H   uint32
	Reason string
}

// VideoConfig 是共享编码流的参数(M3 Task 3:QoSController 的决策载体,
// 经 Source.SetVideoConfig 下发 host 管控消息 SET_VIDEO_CONFIG 0x0129)。
// 语义:Bitrate(bps)与 FPS 热更新(编码器 Reconfigure);MaxW 变更走
// reset/dims 路径(codec epoch 前进——重建后的首帧即 IDR,发送侧经
// 0x020B/WAIT_IDR 自然恢复,无需显式关键帧请求)。
type VideoConfig struct {
	Bitrate uint32 // bps
	FPS     uint32
	MaxW    uint32 // 0 = 不缩(native 只缩不放)
}

// Source 是一条已 ATTACH 的桌面帧订阅(见 desktoppipe.Sub)。
type Source interface {
	// RecvFrame 阻塞取下一视频帧;ok=false = 源终结(关闭/断连/ctx 取消)。
	RecvFrame(ctx context.Context) (Frame, bool)
	// RecvState 阻塞取下一状态事件;ok=false 同上。
	RecvState(ctx context.Context) (StateEvent, bool)
	// RecvCursor 阻塞取下一光标事件(0x0109);ok=false 同上。
	RecvCursor(ctx context.Context) (CursorEvent, bool)
	// RecvDisplay 阻塞取下一显示变化事件(0x010A;M2-Slice1 Task 2);
	// ok=false 同上。
	RecvDisplay(ctx context.Context) (DisplayChangedEvent, bool)
	// Hello 返回最近一次 HOST_HELLO(可为 nil——测试 fake 允许)。
	Hello() *HelloInfo
	// RequestKeyframe 请求 host 立即产新 IDR(reason 进 host 记账)。
	RequestKeyframe(reason string) error
	// SetVideoConfig 下发共享编码参数(M3 Task 3;见 VideoConfig)。host
	// 无该能力(v1 wire / 未广告 capability)时返回
	// desktoppipe.ErrVideoConfigUnsupported 形态的错误,调用方记一次
	// "unsupported" 即停发(决策留在 agent 侧)。
	SetVideoConfig(VideoConfig) error
	// SubID 返回本订阅的 sub_id(0x0108 输入消息必须携带)。
	SubID() uint32
	// SendInput 发送一条已编码的 0x0108 payload([u32 sub_id][u64 seq]
	// [u8 type][payload'];sub_id 由调用方经 SubID 填充)。
	SendInput(payload []byte) error
	Close() error
}

// Starter 负责帧源生命周期:Start = 启动/复用采集并 ATTACH(wts=0 时取
// 活动控制台会话);Stop = 会话终结时的 best-effort 停采(真实实现按引用
// 计数,最后一个会话才真正停)。
type Starter interface {
	Start(ctx context.Context, wts uint32) (Source, error)
	Stop() error
}

// SasResult 是一次 SendSAS(core 0x0110)的结果镜像(M2-Slice1 Task 5)。
// OK=true 表示核心受理并调用了 SendSAS —— HR 是合成 HRESULT(0 = sas.dll
// 调用未抛异常,非「SAS 已送达」证明;验收以安全桌面出现为准,T6)。
// OK=false 时 Code 为稳定码:核心侧 SAS_DENIED(门控关)/ SAS_UNAVAILABLE
// (sas.dll 不可载)/ BAD_PAYLOAD;agent 侧 unsupported(Starter 无该
// 能力)/ core_unavailable(拨号失败)/ core_error(超时等传输错)。
type SasResult struct {
	OK   bool
	HR   uint32
	Code string
}

// SasCaller 是 Starter 的可选能力:经共享 core 连接触发 secure attention
// (0x0110;能力门控在 core 侧 --allow-sas)。未实现者(非 Windows 桩、
// 无 core 拓扑)收到 secure_attention 请求时按 unsupported 应答。
type SasCaller interface {
	SendSAS(reason string) SasResult
}

// DisplaySwitcher 是 Source 的可选能力(M2-Slice3 Task 5):切换采集
// 输出到 displays 表中 index 对应的显示器(0x0128;host 校验后统一
// reset 重建绑定)。未实现者(旧 host、fake)收到 switch_display 时按
// unsupported 应答。
type DisplaySwitcher interface {
	SwitchDisplay(index uint32) error
}
