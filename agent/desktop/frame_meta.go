// frame_meta.go — M3 Task 4:FrameMetaV1 遥测记录的纯编解码(spec
// §16.7:仅做关联,绝不成为可复用的内容指纹)。
//
// == wire 契约(T4/T5;Task 5 TS 侧逐字节平移黄金向量,两侧均禁手拼)==
//
//	"frame-meta"  unreliable/unordered agent→viewer:每帧最后一包 RTP
//	                          写出之后发送一条 56 字节定长记录;失败只
//	                          计数(telemetryDrops),绝不阻塞媒体。
//
// == 56 字节布局表(全部字段小端;任何变更 = 破坏性协议变更) ==
//
//	off  size  field            说明
//	---  ----  -----            ----
//	0    1     version          恒 1(FrameMetaV1)
//	1    1     flags            保留,恒 0
//	2    2     reserved         保留,恒 0
//	4    4     rtpTimestamp     本帧 RTP 包实际盖章的 90kHz 时戳
//	                            (per-viewer 时钟值——发送器逐 viewer 汇出)
//	8    8     codecEpoch       编码器代际(编码器重置前进)
//	16   8     contentId        采集内容稳定 id
//	24   8     encodeSeq        编码序号
//	32   8     sourceMonoUs     采集(host 单调钟)微秒
//	40   8     hash64           会话键控 hash(见 frameMetaHash64)
//	48   8     reserved         保留,恒 0(未来 flags 扩展位)
//
//	合计 1+1+2+4+8+8+8+8+8+8 = 56 字节。
//
// == 会话键控 hash(裁决 1;spec §16.7)==
//
//	hash64 = FNV-1a64( LE64(codecEpoch) || LE64(contentId) ||
//	                   LE64(encodeSeq) || LE64(sourceMonoUs) || LE64(key) )
//
//	key 是 Publisher 构造时随机生成的 64 位会话键(每 viewer 会话一个,
//	crypto/rand 抽取 —— 不可预测):同 key 同输入确定(会话内可关联);
//	异 key 时 hash 以可忽略概率相同 —— FNV-1a 并非单射,「跨会话不可
//	关联」是概率性成立而非必然(64 位输出对 40 字节预映像的碰撞概率
//	≈2^-64/条,远小于信道误码;因此仍不构成可复用的内容指纹)。
//	key 绝不落盘、绝不随像素数据入日志;
//	56 字节记录本身也不入日志。captureEpoch 不进记录/不进 hash(编码
//	代际已足以标识编码身份;采集重建语义由 0x020B/state 帧承载)。
//
// 本文件只含纯编解码(无 webrtc 依赖);通道创建与发送接线:发送器侧
// 帧边界 seam(viewer_sender.go 的 MakeFrameMeta/OnFrameSent——身份在
// 入队时绑定,本帧最后一包成功写出后恰一次汇出)+ Publisher 侧中继
// (transport.go 的 attachFrameMeta/frameMetaRelay)+ 建立时序
// (session.go 的 setupPublisher,HandleOffer 前随输入通道一同创建)。
package desktop

import (
	"encoding/binary"
	"fmt"
)

// dcLabelFrameMeta 是 frame-meta DataChannel 标签(T4/T5 契约)。
const dcLabelFrameMeta = "frame-meta"

// frameMetaV1 布局常量(见文件头布局表)。
const (
	frameMetaV1Version uint8 = 1
	frameMetaV1Size          = 56
)

// FNV-1a 64 参数(与标准库 hash/fnv.New64a 逐字节一致;内联实现避免
// per-frame 对象分配,测试侧用标准库交叉验证)。
const (
	fnv1a64OffsetBasis uint64 = 14695981039346656037
	fnv1a64Prime       uint64 = 1099511628211
)

// FrameMetaV1 是一条帧溯源遥测记录(字段见布局表;Flags/Hash64 由编解码
// 填充,其余来自帧身份)。
type FrameMetaV1 struct {
	Flags        uint8
	RTPTimestamp uint32
	CodecEpoch   uint64
	ContentID    uint64
	EncodeSeq    uint64
	SourceMonoUs uint64
	Hash64       uint64
}

// newFrameMetaV1 组装一帧的记录:rtpTimestamp 必须是该帧 RTP 包实际
// 盖章的 per-viewer 时戳(发送器出口处取自包,而非旁路推算);hash64
// 以会话键对身份字段键控混合。
func newFrameMetaV1(key uint64, f Frame, rtpTimestamp uint32) FrameMetaV1 {
	m := FrameMetaV1{
		RTPTimestamp: rtpTimestamp,
		CodecEpoch:   f.CodecEpoch,
		ContentID:    f.ContentID,
		EncodeSeq:    f.EncodeSeq,
		SourceMonoUs: f.SourceMonoUs,
	}
	m.Hash64 = frameMetaHash64(key, m)
	return m
}

// frameMetaHash64 计算会话键控 hash(预映像与算法见文件头)。零分配
//(栈上 40 字节预映像)。
func frameMetaHash64(key uint64, m FrameMetaV1) uint64 {
	var pre [40]byte
	binary.LittleEndian.PutUint64(pre[0:], m.CodecEpoch)
	binary.LittleEndian.PutUint64(pre[8:], m.ContentID)
	binary.LittleEndian.PutUint64(pre[16:], m.EncodeSeq)
	binary.LittleEndian.PutUint64(pre[24:], m.SourceMonoUs)
	binary.LittleEndian.PutUint64(pre[32:], key)
	h := fnv1a64OffsetBasis
	for _, b := range pre {
		h = (h ^ uint64(b)) * fnv1a64Prime
	}
	return h
}

// appendFrameMetaV1 把 56 字节记录追加到 dst(保留区写零)。
func appendFrameMetaV1(dst []byte, m FrameMetaV1) []byte {
	var rec [frameMetaV1Size]byte
	rec[0] = frameMetaV1Version
	rec[1] = m.Flags
	binary.LittleEndian.PutUint32(rec[4:], m.RTPTimestamp)
	binary.LittleEndian.PutUint64(rec[8:], m.CodecEpoch)
	binary.LittleEndian.PutUint64(rec[16:], m.ContentID)
	binary.LittleEndian.PutUint64(rec[24:], m.EncodeSeq)
	binary.LittleEndian.PutUint64(rec[32:], m.SourceMonoUs)
	binary.LittleEndian.PutUint64(rec[40:], m.Hash64)
	return append(dst, rec[:]...)
}

// encodeFrameMetaV1 产出一个新的 56 字节记录。每帧恰好一次小分配:记录
// 缓冲被中继通道/SCTP 出口持有(pion sctp 重传缓冲引用用户字节),
// 不可跨帧复用同一缓冲。
func encodeFrameMetaV1(m FrameMetaV1) []byte {
	return appendFrameMetaV1(make([]byte, 0, frameMetaV1Size), m)
}

// decodeFrameMetaV1 解码并强制长度 56 + version 1(其余形态一律拒绝)。
func decodeFrameMetaV1(b []byte) (FrameMetaV1, error) {
	if len(b) != frameMetaV1Size {
		return FrameMetaV1{}, fmt.Errorf("desktop: frame-meta: length %d, want %d", len(b), frameMetaV1Size)
	}
	if b[0] != frameMetaV1Version {
		return FrameMetaV1{}, fmt.Errorf("desktop: frame-meta: version %d, want %d", b[0], frameMetaV1Version)
	}
	return FrameMetaV1{
		Flags:        b[1],
		RTPTimestamp: binary.LittleEndian.Uint32(b[4:]),
		CodecEpoch:   binary.LittleEndian.Uint64(b[8:]),
		ContentID:    binary.LittleEndian.Uint64(b[16:]),
		EncodeSeq:    binary.LittleEndian.Uint64(b[24:]),
		SourceMonoUs: binary.LittleEndian.Uint64(b[32:]),
		Hash64:       binary.LittleEndian.Uint64(b[40:]),
	}, nil
}
