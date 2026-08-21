// capturer.go — 采集器抽象（平台无关）：captureLoop 只依赖此接口，
// DDA（xnc-dda.dll，主路径）与 WGC（回退）各自实现。
package main

// screenCapturer 是采集源的统一契约（BGRA top-down 全帧）。
//
//	AcquireFrame 桌面静止返回 ErrTimeout；会话/设备级失败返回其他错误
//	           （调用方整体重建）；光标-only 更新由实现自行合成后按
//	           内容帧返回。
//	Dims        当前源分辨率（重建后可能变化）。
type screenCapturer interface {
	AcquireFrame(timeoutMs uint) ([]byte, error)
	Dims() (int, int)
	Close()
}
