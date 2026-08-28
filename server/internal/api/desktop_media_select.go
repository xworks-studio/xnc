package api

// desktop_media_select.go — M4 Task 4：desktop 媒体管线版本的 server 侧
// per-session 选择（纯函数，无 DB/容器依赖；表驱动单测见
// desktop_media_select_test.go）。
//
// 生产控制点在 server（绑定裁决 1/5）：节点 allowlist + 百分比 + 回滚开关
// （XNC_DESKTOP_MEDIA_V2_*），选择发生在 desktopStart 内、startSession 之前
// ——即 Host/Publisher 启动之前——并随 SESSION_OPEN params 一次性快照；
// live 会话绝不重选。host 本机的 XNC_DESKTOP_PIPELINE_V2 仍是节点级 force
//（dev/自验），与 server 选择不一致时由 agent 按会话钉子响亮拒绝。
//
// 优先级（高 → 低）：
//  1. 回滚开关：一切新会话 → v1（含 allowlist 内节点——"all new sessions"）；
//  2. 显式节点 allowlist：命中 → v2（胜过百分比，百分比 0 也选 v2）；
//  3. 百分比：越界（<0 或 >100，视为未知输入）fail closed 到 v1；合法时按
//     节点 UUID 的 FNV-1a 桶确定性判定（桶 < 百分比 → v2）。
//
// 桶按**节点**（而非会话）键控：rt-pipe host 每节点共享（max_subs=4 多
// viewer），其管线版本在 host 进程启动时定死——同一节点的并发会话必须拿到
// 同一选择，否则 agent 的会话钉子必然与邻居会话冲突。节点键还使百分比移动
// 整节点队列、跨 server 重启稳定。
import (
	"hash/fnv"
	"strings"

	"xnc/proto"
)

// MediaRollout 是一次选择所依据的 rollout 配置快照（来自 config.Config 的
// DesktopMediaV2* 字段；纯数据，测试可直接构造）。
type MediaRollout struct {
	// Percent ∈ [0,100]：命中桶的节点比例；越界按 fail closed 处理（→ 0）。
	Percent int
	// Allowlist：显式选 v2 的节点 UUID 列表（大小写不敏感——env 输入可能
	// 大写，节点 UUID 规范形态小写）。
	Allowlist []string
	// Rollback：true = 一切新会话 v1（回滚开关，胜过 allowlist/百分比）。
	Rollback bool
}

// selectMediaProtocol 为 nodeID 上的一个**新**会话选择媒体协议版本。
// 纯函数：同一 (roll, nodeID) 恒同值——会话级快照由调用方在打开时一次性
// 落进 params（desktopStart），此后 live 会话与后续配置变化互不影响。
func selectMediaProtocol(roll MediaRollout, nodeID string) string {
	if roll.Rollback {
		return proto.MediaProtocolV1
	}
	if mediaNodeAllowlisted(roll.Allowlist, nodeID) {
		return proto.MediaProtocolV2
	}
	if roll.Percent < 0 || roll.Percent > 100 {
		return proto.MediaProtocolV1 // 未知输入 fail closed
	}
	return mediaBucketProtocol(nodeID, roll.Percent)
}

// mediaNodeAllowlisted 大小写不敏感匹配节点 UUID。
func mediaNodeAllowlisted(allowlist []string, nodeID string) bool {
	n := strings.ToLower(nodeID)
	for _, a := range allowlist {
		if strings.ToLower(a) == n {
			return true
		}
	}
	return false
}

// mediaBucketProtocol 按节点 UUID 的 FNV-1a 哈希桶判定（0-99 < percent →
// v2）；确定性、跨重启稳定。percent 已由调用方夹取到 [0,100]。
func mediaBucketProtocol(nodeID string, percent int) string {
	if percent == 0 {
		return proto.MediaProtocolV1 // 快路径（缺省）
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(nodeID)))
	if int(h.Sum32()%100) < percent {
		return proto.MediaProtocolV2
	}
	return proto.MediaProtocolV1
}
