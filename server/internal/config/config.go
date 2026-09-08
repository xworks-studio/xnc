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
	ShellPerNode     int           // XNC_SHELL_PER_NODE，默认 10，0 = 不限
	ShellIdleTimeout time.Duration // XNC_SHELL_IDLE，默认 30m，0 = 不限
	ShellMaxLifetime time.Duration // XNC_SHELL_MAX，默认 8h，0 = 不限

	// —— RTV 桌面中继（2026-09-08 重构，替代 TURN/WebRTC 面）。
	// RTVStreamEndpoint 缺省为空 = desktop 会话 503 RTV_UNCONFIGURED；
	// 形态 host:port（host 腿 QUIC，生产 xnc.app:4433）。凭据绝不入日志。
	RTVStreamEndpoint string // XNC_RTV_ENDPOINT
	// RTVHostAddr/RTVWTAddr 是两条 UDP 腿的容器内监听地址（空 = 不启对应
	// 腿；compose 映射 4433/udp 与 443/udp）。
	RTVHostAddr string // XNC_RTV_HOST_ADDR，默认 ":4433"
	RTVWTAddr   string // XNC_RTV_WT_ADDR，默认 ":443"
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
	// RTVWSOrigins WS 兜底腿的 Origin 白名单（空 = coder/websocket 同源
	// 校验；生产经 caddy 同源反代即正确语义，dev 跨源联调时配置）。
	RTVWSOrigins []string // XNC_RTV_WS_ORIGINS，逗号分隔

	DesktopPerNode     int           // XNC_DESKTOP_PER_NODE，默认 4（多 viewer 各自独立会话），0 = 不限
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
		ShellPerNode:          envInt("XNC_SHELL_PER_NODE", 10),
		ShellIdleTimeout:      envDur("XNC_SHELL_IDLE", 30*time.Minute),
		ShellMaxLifetime:      envDur("XNC_SHELL_MAX", 8*time.Hour),
		RTVStreamEndpoint:     env("XNC_RTV_ENDPOINT", ""),
		RTVHostAddr:           env("XNC_RTV_HOST_ADDR", ":4433"),
		RTVWTAddr:             env("XNC_RTV_WT_ADDR", ":443"),
		RTVCertFile:           os.Getenv("XNC_RTV_CERT_FILE"),
		RTVKeyFile:            os.Getenv("XNC_RTV_KEY_FILE"),
		RTVWSOrigins:          envList("XNC_RTV_WS_ORIGINS"),
		DesktopPerNode:        envInt("XNC_DESKTOP_PER_NODE", 4),
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
