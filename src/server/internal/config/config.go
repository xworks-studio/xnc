package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr       string
	DatabaseURL      string
	JWTSecret        []byte
	AdminEmail       string
	AdminPassword    string
	HeartbeatTimeout time.Duration
	EnrollTokenTTL   time.Duration
	// MaxClustersPerUser 自助建 cluster 的每用户限额（XNC_MAX_CLUSTERS_PER_USER，
	// 默认 20，0 视为默认）：只数"我任 owner 的存活 cluster"，防刷。
	MaxClustersPerUser int
	ShellPerNode       int           // XNC_SHELL_PER_NODE，默认 10，0 = 不限
	ShellIdleTimeout   time.Duration // XNC_SHELL_IDLE，默认 30m，0 = 不限
	ShellMaxLifetime   time.Duration // XNC_SHELL_MAX，默认 8h，0 = 不限

	// —— RTV 桌面中继（2026-09-08 重构，替代 TURN/WebRTC 面）。
	// RTVEmbedded（XNC_RTV_EMBEDDED，默认 false，2026-09-11 主站缩减决策）：
	// false = 主站不跑媒体面——不创建内嵌 rtv.Server、不挂 /ws，桌面会话只
	// 经外置 relay（rtvpool），无可用 relay 时 503 RTV_NO_RELAY。true = 旧单机
	// 全内嵌模式（dev-compose/CI/测试装置显式开启），此时 RTVStreamEndpoint
	// 恢复旧语义（host:port，空 = desktop 503 RTV_UNCONFIGURED）。
	RTVEmbedded bool // XNC_RTV_EMBEDDED
	// RTVStreamEndpoint 仅内嵌模式的 host 腿公告地址（host:port，生产
	// xnc.app:4433）。embedded=false 时忽略（告警提示）。凭据绝不入日志。
	RTVStreamEndpoint string // XNC_RTV_ENDPOINT
	// RTVHostAddr/RTVWTAddr 是两条 UDP 腿的容器内监听地址（空 = 不启对应
	// 腿；compose 映射 4433/udp 与 443/udp）。
	RTVHostAddr string // XNC_RTV_HOST_ADDR，默认 ":4433"
	RTVWTAddr   string // XNC_RTV_WT_ADDR，默认 ":443"
	// RTVWTPublicPort：浏览器 WT URL 的公网端口（XNC_RTV_WT_PORT；空 = 443
	// 规范端口。UDP443 被安全组拦截等过渡期用非规范端口，如 14433 映射
	// 到容器 443）。
	RTVWTPublicPort string // XNC_RTV_WT_PORT
	// RTV 证书：ACME DNS-01（lego + Aliyun DNS）为主——浏览器 WT 走标准
	// Web PKI（真实 CA 证书，无 serverCertificateHashes 层）。ACME 未配置时
	// 回落 XNC_RTV_CERT_FILE/KEY_FILE 文件对；再缺 = 进程内自签 dev 证书
	// （高声告警，WT 腿浏览器不可用，WS 兜底腿经 caddy 仍可用）。
	RTVACMEDomain  string // XNC_ACME_DOMAIN（如 xnc.app；空 = ACME 关闭）
	RTVACMEEmail   string // XNC_ACME_EMAIL
	RTVACMEDir     string // XNC_ACME_CERT_DIR，证书+账号密钥的卷持久化目录
	RTVACMEStaging bool   // XNC_ACME_STAGING，LE staging 目录（联调防配额烧穿）
	AlidnsKey      string // ALIDNS_ACCESS_KEY（deploy/.env 唯一源）
	AlidnsSecret   string // ALIDNS_SECRET_KEY
	RTVCertFile    string // XNC_RTV_CERT_FILE（ACME 关闭时的文件形态）
	RTVKeyFile     string // XNC_RTV_KEY_FILE
	// RTVInsecureTLS（dev-only）：host 腿跳过证书校验（自签 dev 栈）。
	// 生产绝不开（配置即高声告警）。
	RTVInsecureTLS bool // XNC_RTV_INSECURE_TLS
	// RTVWSOrigins WS 兜底腿的 Origin 白名单（空 = coder/websocket 同源
	// 校验；生产经 caddy 同源反代即正确语义，dev 跨源联调时配置）。
	RTVWSOrigins []string // XNC_RTV_WS_ORIGINS，逗号分隔
	// RTVSigningKey（relay-plane）：RelayTicket 签名私钥（ed25519 64B hex）。
	// 空 = 进程内临时生成并告警（重启作废——relay-0 会话本就随进程消亡，
	// 可接受；部署外部 relay 前必须显式配置，否则 server 重启后外部 relay
	// 需重连控制连接同步新公钥）。
	RTVSigningKey string // XNC_RTV_SIGNING_KEY
	// RTVRelayAllowlist（relay-plane 准入）：ed25519 公钥 hex 清单（逗号
	// 分隔）。命中 → 注册即 active；未命中 → pending 待管理端审批。
	RTVRelayAllowlist []string // XNC_RTV_RELAY_ALLOWLIST

	DesktopPerNode     int           // XNC_DESKTOP_PER_NODE，默认 8（多 viewer 各自独立会话），0 = 不限
	DesktopIdleTimeout time.Duration // XNC_DESKTOP_IDLE，默认 5m（无信令活动即关），0 = 不限

	// —— installer 分发同步（installersync）：GitHub Releases 是唯一事实
	// 源，server 定时拉回本地 release store（/installer 服务路径不变）。
	InstallerSyncRepo     string        // XNC_INSTALLER_SYNC_REPO，默认 xworks-studio/xnc；空 = 关闭同步
	InstallerSyncInterval time.Duration // XNC_INSTALLER_SYNC_INTERVAL，默认 5m，下限 1m（保护 API 配额）
	GitHubToken           string        // XNC_GITHUB_TOKEN，可选：匿名 60 req/h 已足够，防限额/私有库时配置
}

func Load() (Config, error) {
	c := Config{
		ListenAddr:            env("XNC_LISTEN", ":8080"),
		DatabaseURL:           os.Getenv("XNC_DATABASE_URL"),
		JWTSecret:             []byte(os.Getenv("XNC_JWT_SECRET")),
		AdminEmail:            os.Getenv("XNC_ADMIN_EMAIL"),
		AdminPassword:         os.Getenv("XNC_ADMIN_PASSWORD"),
		HeartbeatTimeout:      envDur("XNC_HEARTBEAT_TIMEOUT", 90*time.Second),
		EnrollTokenTTL:        envDur("XNC_ENROLL_TOKEN_TTL", 30*time.Minute),
		MaxClustersPerUser:    envInt("XNC_MAX_CLUSTERS_PER_USER", 20),
		ShellPerNode:          envInt("XNC_SHELL_PER_NODE", 10),
		ShellIdleTimeout:      envDur("XNC_SHELL_IDLE", 30*time.Minute),
		ShellMaxLifetime:      envDur("XNC_SHELL_MAX", 8*time.Hour),
		RTVEmbedded:           envBool("XNC_RTV_EMBEDDED", false),
		RTVStreamEndpoint:     env("XNC_RTV_ENDPOINT", ""),
		RTVHostAddr:           env("XNC_RTV_HOST_ADDR", ":4433"),
		RTVWTPublicPort:       os.Getenv("XNC_RTV_WT_PORT"),
		RTVWTAddr:             env("XNC_RTV_WT_ADDR", ":443"),
		RTVCertFile:           os.Getenv("XNC_RTV_CERT_FILE"),
		RTVKeyFile:            os.Getenv("XNC_RTV_KEY_FILE"),
		RTVInsecureTLS:        envBool("XNC_RTV_INSECURE_TLS", false),
		RTVWSOrigins:          envList("XNC_RTV_WS_ORIGINS"),
		RTVSigningKey:         os.Getenv("XNC_RTV_SIGNING_KEY"),
		RTVRelayAllowlist:     envList("XNC_RTV_RELAY_ALLOWLIST"),
		DesktopPerNode:        envInt("XNC_DESKTOP_PER_NODE", 8),
		DesktopIdleTimeout:    envDur("XNC_DESKTOP_IDLE", 5*time.Minute),
		InstallerSyncRepo:     env("XNC_INSTALLER_SYNC_REPO", "xworks-studio/xnc"),
		InstallerSyncInterval: envDur("XNC_INSTALLER_SYNC_INTERVAL", 5*time.Minute),
		GitHubToken:           os.Getenv("XNC_GITHUB_TOKEN"),
	}
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("XNC_DATABASE_URL is required")
	}
	if len(c.JWTSecret) < 32 {
		return c, fmt.Errorf("XNC_JWT_SECRET must be at least 32 bytes")
	}
	// 下限钳制：更快的轮询只会烧 API 配额（匿名 60 req/h），同步收益为零。
	if c.InstallerSyncInterval < time.Minute {
		c.InstallerSyncInterval = time.Minute
	}
	return c, nil
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envDur(k string, d time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if p, err := time.ParseDuration(v); err == nil {
			return p
		}
	}
	return d
}

// envInt 解析整型 env；缺失或非法（Atoi 失败/零值空串）回默认。
func envInt(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return d
}

// envList 解析逗号分隔列表 env（XNC_TURN_URLS）；缺失/全空白 → nil。
func envList(k string) []string {
	v := os.Getenv(k)
	if v == "" {
		return nil
	}
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// envBool 解析布尔 env（strconv.ParseBool：1/t/T/true/TRUE…）；缺失或非法
// 回默认（回滚开关缺省 false = fail closed 到 v1）。
func envBool(k string, d bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return d
}
