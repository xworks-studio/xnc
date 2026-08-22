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
// 字段一一对应(core_windows.go 负责转换)。
type Frame struct {
	Key    bool
	MonoUs uint64 // host 单调钟微秒——RTP 时戳的真相来源
	AU     []byte
}

// HelloInfo 是 HOST_HELLO 内容镜像;gen 递增代表 capture 重建。
type HelloInfo struct {
	Gen     uint32
	W, H    uint32
	Fps     uint32
	MaxSubs uint32
}

// StateEvent 是 STATE 事件镜像(dxgi_access_denied / capture_rebuilt 等)。
type StateEvent struct {
	Code        string
	Recoverable bool
}

// Source 是一条已 ATTACH 的桌面帧订阅(见 desktoppipe.Sub)。
type Source interface {
	// RecvFrame 阻塞取下一视频帧;ok=false = 源终结(关闭/断连/ctx 取消)。
	RecvFrame(ctx context.Context) (Frame, bool)
	// RecvState 阻塞取下一状态事件;ok=false 同上。
	RecvState(ctx context.Context) (StateEvent, bool)
	// Hello 返回最近一次 HOST_HELLO(可为 nil——测试 fake 允许)。
	Hello() *HelloInfo
	// RequestKeyframe 请求 host 立即产新 IDR(reason 进 host 记账)。
	RequestKeyframe(reason string) error
	Close() error
}

// Starter 负责帧源生命周期:Start = 启动/复用采集并 ATTACH(wts=0 时取
// 活动控制台会话);Stop = 会话终结时的 best-effort 停采(真实实现按引用
// 计数,最后一个会话才真正停)。
type Starter interface {
	Start(ctx context.Context, wts uint32) (Source, error)
	Stop() error
}
