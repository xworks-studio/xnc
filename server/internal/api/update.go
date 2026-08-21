// update.go — 统一自更新的服务端逻辑：目标版本决策、下载令牌、OFFER 下发。
//
// 目标版本：COALESCE(nodes.target_release, 最新 release)。三个触发时机
// （设计文档）：HELLO 握手回执、心跳 ACK 搭车、rollout 强制 —— 全部收敛
// 到 maybeOfferUpdate。
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"xnc/proto"
	"xnc/server/internal/db/sqlc"
)

// bundleArtifactName / cliArtifactName — release 制品的固定命名。
const (
	bundleArtifactName = "bundle.tar.gz"
	cliArtifactName    = "xnc-windows-amd64.exe"
)

// downloadTokens — agent bundle 下载令牌（短时效、单次、绑定节点）。
// 单实例 server，进程内 map 足够；过期惰性清扫。
var downloadTokens = struct {
	sync.Mutex
	m map[string]downloadToken
}{m: map[string]downloadToken{}}

type downloadToken struct {
	NodeID    uuid.UUID
	ReleaseID uuid.UUID
	Expires   time.Time
}

func mintDownloadToken(nodeID, releaseID uuid.UUID) string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	downloadTokens.Lock()
	downloadTokens.m[tok] = downloadToken{NodeID: nodeID, ReleaseID: releaseID, Expires: time.Now().Add(15 * time.Minute)}
	// 顺带清扫过期项（量小，无需独立协程）。
	for k, v := range downloadTokens.m {
		if time.Now().After(v.Expires) {
			delete(downloadTokens.m, k)
		}
	}
	downloadTokens.Unlock()
	return tok
}

func consumeDownloadToken(tok string, nodeID uuid.UUID) (uuid.UUID, bool) {
	downloadTokens.Lock()
	defer downloadTokens.Unlock()
	v, ok := downloadTokens.m[tok]
	if !ok || time.Now().After(v.Expires) || v.NodeID != nodeID {
		return uuid.UUID{}, false
	}
	delete(downloadTokens.m, tok) // 单次
	return v.ReleaseID, true
}

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

// maybeOfferUpdate — 版本落后则经控制通道下发 UPDATE_OFFER。
// HELLO 握手、心跳、强制 rollout 三入口共用；幂等性由 agent 侧去重保证
// （同版本重复 OFFER 静默跳过），服务端不做去重以保持无状态。
func (h *handlers) maybeOfferUpdate(ctx context.Context, nodeID uuid.UUID, currentVersion string, send func(proto.Message) error) {
	if currentVersion == "" {
		return
	}
	rel, ok := h.targetReleaseFor(ctx, nodeID)
	if !ok || rel.Version == currentVersion {
		return
	}
	q := h.st.Q()
	art, err := q.GetArtifact(ctx, sqlc.GetArtifactParams{ReleaseID: rel.ID, Name: bundleArtifactName})
	if err != nil {
		slog.Warn("update: bundle artifact missing", "version", rel.Version, "err", err)
		return
	}
	tok := mintDownloadToken(nodeID, rel.ID)
	offer, _ := proto.NewMsg(proto.TypeUpdateOffer, proto.UpdateOffer{
		Version: rel.Version,
		URL:     "/api/agent/bundle?token=" + tok + "&node=" + nodeID.String(),
		SHA256:  art.Sha256,
	})
	if err := send(offer); err != nil {
		slog.Warn("update: offer send failed", "node", nodeID, "err", err)
	} else {
		slog.Info("update: offered", "node", nodeID, "from", currentVersion, "to", rel.Version)
	}
}
