// update.go — 统一自更新的服务端逻辑：目标版本决策、UPDATE_AVAILABLE 下发。
//
// 目标版本：COALESCE(nodes.target_release, 最新 release)。三个触发时机
// （设计文档）：HELLO 握手回执、心跳 ACK 搭车、rollout 强制 —— 全部收敛
// 到 maybeOfferUpdate。
package api

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"xnc/proto"
	"xnc/server/internal/db/sqlc"
)

// bundleArtifactName / cliArtifactName — release 制品的固定命名。
const (
	bundleArtifactName = "bundle.tar.gz"
	cliArtifactName    = "xnc-windows-amd64.exe"
)

// targetReleaseFor — 节点的目标版本：pin 优先，否则节点所在频道的最新。
// 无可用 release 时返回 false（不触发更新）。
func (h *handlers) targetReleaseFor(ctx context.Context, nodeID uuid.UUID) (sqlc.Release, bool) {
	q := h.st.Q()
	info, err := q.GetNodeTargetRelease(ctx, nodeID)
	if err != nil {
		return sqlc.Release{}, false
	}
	// admin 显式 pin 最高优先。
	if info.TargetRelease.Valid {
		if rel, err := q.GetReleaseByVersion(ctx, info.TargetRelease.String); err == nil {
			return rel, true
		}
	}
	// 节点所在频道的最新 release。
	channel := "stable"
	if info.Channel != "" {
		channel = info.Channel
	}
	rel, err := q.GetLatestReleaseByChannel(ctx, channel)
	if err != nil {
		return sqlc.Release{}, false
	}
	return rel, true
}

// maybeOfferUpdate — 版本落后则经控制通道推送 UPDATE_AVAILABLE。
// HELLO 握手、心跳、强制 rollout 三入口共用；幂等性由 agent 侧去重保证
// （同版本重复推送静默跳过），服务端不做去重以保持无状态。
//
// 推送载荷 {version,url,sha256} 本身即完整清单：agent 的
// updater.HandlePush 经已认证控制通道收到后直接编排，无需再拉
// setup.json——sha256 即信任根（下载后校验，不符入黑名单），下载 url
// 由 agent 强制与 server 同源；拉取 setup.json 是轮询路径（CheckNow）
// 的清单来源，推送路径不经过。
//
// 未上线直采终态（设计 §14）：更新唯一经安装器（setup.exe 制品）编排，
// 遗留 bundle 通道已移除——release 不含 setup.exe 制品时不推送（仅告警，
// 发布流水线漏传安装器属配置错误，应修复发布而非降级兜底）。
func (h *handlers) maybeOfferUpdate(ctx context.Context, nodeID uuid.UUID, currentVersion string, send func(proto.Message) error) {
	if currentVersion == "" {
		return
	}
	rel, ok := h.targetReleaseFor(ctx, nodeID)
	if !ok || rel.Version == currentVersion {
		return
	}
	art, err := h.st.Q().GetArtifact(ctx, sqlc.GetArtifactParams{ReleaseID: rel.ID, Name: setupArtifactName})
	if err != nil {
		slog.Warn("update: setup artifact missing; not offering", "version", rel.Version, "err", err)
		return
	}
	push, _ := proto.NewMsg(proto.TypeUpdateAvailable, proto.UpdateAvailable{
		Version: rel.Version,
		URL:     "/setup.exe?channel=" + rel.Channel,
		SHA256:  art.Sha256,
	})
	if err := send(push); err != nil {
		slog.Warn("update: push send failed", "node", nodeID, "err", err)
	} else {
		slog.Info("update: pushed", "node", nodeID, "from", currentVersion, "to", rel.Version)
	}
}
