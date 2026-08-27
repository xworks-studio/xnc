// qos_min.go — 本片的最小 QoS 面(plan T4:TWCC 开;PLI→RequestKeyframe;
// 仅透传统计)。发送侧拦截器(NACK 重发、RTCP SenderReport、TWCC 出向头
// 扩展)由 newDesktopAPI 的 RegisterDefaultInterceptors +
// ConfigureTWCCHeaderExtensionSender 装配;本文件只做:
//
//   - RTCP 泵:消费 viewer 回传的 PLI/FIR/NACK/TWCC 反馈——PLI/FIR 触发
//     OnKeyRequest(reason)→ session 侧经 KeyframeCoordinator(M3 Task 2)
//     合并后转 Source.RequestKeyframe(reason="pli"/"fir",≤31B);
//   - keyRequestIsUrgent:Publisher seam 各 reason 的 urgent 判定(connect=
//     新订阅绕过 250ms 冷却;其余常规);
//   - 计数器透传:PubStats 快照供 session/日志/e2e 观测(不含任何凭据)。
//
// Slice3 扩展位已由 M3 Task 3 落地:viewer_feedback(信令帧,session.go
// 解析)→ QoSController(qos_controller.go)驱动码率/fps/max_w 阶梯与
// 旁观者暂停,决策经 SET_VIDEO_CONFIG 0x0129(desktoppipe)下发 host;
// RTCP 面的 TWCC 计数仍在此透传(估计带宽由 viewer 侧 getStats 汇总上报)。
package desktop

import (
	"sync/atomic"

	"github.com/pion/rtcp"
)

// pubStats 是 publisher 的原子计数器集(透传统计)。帧/字节/抑制计数
// 已随发包状态下沉到 ViewerSender(viewer_sender.go 的 ViewerStats)。
type pubStats struct {
	preConnDropped atomic.Uint64 // 连接就绪前丢弃帧数(sender 未启动,写了也丢)
	pli            atomic.Uint64 // 收到 RTCP PLI
	fir            atomic.Uint64 // 收到 RTCP FIR
	nack           atomic.Uint64 // 收到 RTCP NACK(拦截器已本地重发)
	twcc           atomic.Uint64 // 收到 TWCC 反馈包
}

// PubStats 是 Stats() 的快照形态。
type PubStats struct {
	FramesWritten  uint64
	BytesWritten   uint64
	PreConnDropped uint64
	PreKeyDropped  uint64
	PLI            uint64
	FIR            uint64
	NACK           uint64
	TWCC           uint64
}

// rtcpLoop 是 publisher 的 RTCP 读泵:RTPSender.ReadRTCP 直到连接关闭
// (Close 后 Read 返回 ErrClosedPipe)。包级分派只看类型;SSRC 等细节
// 本片不消费。panic 不可能(纯分派),错误即退出。
func (p *Publisher) rtcpLoop() {
	defer p.wg.Done()
	for {
		pkts, _, err := p.sender.ReadRTCP()
		if err != nil {
			return // 连接关闭(主动 Close / 对端断开)
		}
		for _, pkt := range pkts {
			switch pt := pkt.(type) {
			case *rtcp.PictureLossIndication:
				p.stats.pli.Add(1)
				p.fireKeyRequest("pli")
			case *rtcp.FullIntraRequest:
				p.stats.fir.Add(1)
				p.fireKeyRequest("fir")
			case *rtcp.TransportLayerNack:
				p.stats.nack.Add(1)
			case *rtcp.TransportLayerCC:
				p.stats.twcc.Add(1)
			default:
				_ = pt
			}
		}
	}
}

// fireKeyRequest 触发注册的关键帧回调(session 侧接 KeyframeCoordinator:
// RTCP PLI/FIR、连接就绪 connect 与发送器 overflow/pacer/resume 全部经此
// seam 汇入协调器,由它合并/冷却后打 host)。
func (p *Publisher) fireKeyRequest(reason string) {
	if fn := p.keyFn; fn != nil {
		fn(reason)
	}
}

// keyRequestIsUrgent 判定 Publisher seam 上一个 reason 是否 urgent(裁决 2:
// 绕过 250ms 常规冷却,但在途请求仍不复制):connect=新订阅——ICE+DTLS
// 就绪需要立即产 fresh IDR(首帧语义);epoch 变更的 urgent 请求由 Task 3
// 的 QoS 动作直接走 coordinator API。其余(pli/fir/overflow/pacer/resume)
// 一律常规冷却。
func keyRequestIsUrgent(reason string) bool {
	return reason == "connect"
}
