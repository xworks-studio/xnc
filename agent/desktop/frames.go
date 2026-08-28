// frames.go — desktop 会话的帧泵/事件泵 goroutine 主体(信令循环之外的
// 全部泵;由 session.go 的 Handle/setupPublisher 以 go 启动)。
package desktop

import (
	"context"
	"log/slog"
)

// pumpStateEvents 状态事件泵:STATE → {"type":"state"}(M2-Slice1 Task 2;
// 源终结即退出)。
func pumpStateEvents(ctx context.Context, w *wsWriter, dyn Source) {
	for {
		ev, ok := dyn.RecvState(ctx)
		if !ok {
			return
		}
		w.write(ctx, stateFrame{Type: vocabState, Code: ev.Code, Recoverable: ev.Recoverable})
	}
}

// pumpDisplayEvents 显示器事件泵:0x010A → {"type":"display_changed"}
// (M2-Slice1 Task 2;几何/代际变化;源终结即退出)。
func pumpDisplayEvents(ctx context.Context, w *wsWriter, dyn Source) {
	for {
		ev, ok := dyn.RecvDisplay(ctx)
		if !ok {
			return
		}
		w.write(ctx, displayChangedFrame{
			Type: vocabDisplayChanged, Generation: ev.Gen, W: ev.W, H: ev.H, Reason: ev.Reason})
	}
}

// qosObservingSource 在帧汇出点(source → publisher 帧泵)喂养共享 QoS
// 的帧观测(M4 修正):透传全部 Source 行为,仅对每帧回调一次 —— host
// 重置后的新代帧流由此解除 qos 控制器的 reset-recovery grace。onFrame
// 为 nil 时纯透传。
type qosObservingSource struct {
	Source
	onFrame func()
}

func (s qosObservingSource) RecvFrame(ctx context.Context) (Frame, bool) {
	f, ok := s.Source.RecvFrame(ctx)
	if ok && s.onFrame != nil {
		s.onFrame()
	}
	return f, ok
}

// pumpFrames 帧泵:pipe → RTP。源终结(RecvFrame false)或写失败(PC 已死)
// 即退出——会话由信令主循环的 WS 错误路径统一收线。
func pumpFrames(ctx context.Context, log *slog.Logger, src Source, pub *Publisher) {
	for {
		f, ok := src.RecvFrame(ctx)
		if !ok {
			return
		}
		if err := pub.WriteFrame(f); err != nil {
			log.Info("desktop frame pump stopped", "err", err)
			return
		}
	}
}

// pumpCursor 光标泵:pipe 0x0109 → cursor DataChannel(不可靠,最新即准)。
func pumpCursor(ctx context.Context, src Source, ictl *inputController) {
	for {
		ev, ok := src.RecvCursor(ctx)
		if !ok {
			return
		}
		ictl.forwardCursor(ev)
	}
}
