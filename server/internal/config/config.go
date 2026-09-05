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

	// —— M1-Slice2 desktop/TURN。TURN 缺省为空 = desktop 会话 503
	// TURN_UNCONFIGURED（relay-only 无 TURN 不可用）；凭据绝不入日志。
	TurnURLs           []string      // XNC_TURN_URLS，逗号分隔（turn:/turns: URL）
	TurnUsername       string        // XNC_TURN_USERNAME（dev = lt-cred 静态用户）
	TurnCredential     string        // XNC_TURN_CREDENTIAL（M2 换 REST 时效凭据）
	// TurnPool 境内 TURN 中转池（XNC_TURN_POOL，逗号分隔 ip[:port]，缺省端口
	// 3478）。空 = 未配置 → 沿用 TurnURLs 全列表（现状）。非空时 desktop 会话
	// 从池中 round-robin 分配单台（udp 优先 + tcp 兜底两个 URL），凭据与
	// TurnUsername/TurnCredential 共用。
	TurnPool []string
	DesktopPerNode     int           // XNC_DESKTOP_PER_NODE，默认 4（对齐 agent host max_subs=4，多 viewer），0 = 不限
	DesktopIdleTimeout time.Duration // XNC_DESKTOP_IDLE，默认 5m（无信令活动即关），0 = 不限

	// DesktopICEPolicy（M4 Task 7 LAN 直连）：desktop 会话 ICE transport
	// policy 的 server 侧总开关。"relay"（缺省，spec 强约束）= 不下发字段，
	// agent 强制 relay-only；"all"（XNC_DESKTOP_ICE_POLICY，LAN 场景放开）
	// = SESSION_OPEN params 携带 proto.DesktopIceAll，允许 host/srflx 直连
	// 候选（绕开公网 TURN 中继的同网回环丢包）。归一化后只有这两个值：
	// 空/未知值一律 fail closed 回 "relay"（不配置 = 行为不变）。
	DesktopICEPolicy string // XNC_DESKTOP_ICE_POLICY，"relay"（缺省）| "all"

	// —— M4 Task 4：desktop 媒体管线 v2 canary（server 侧 per-session
	// 选择 = 生产控制点；节点本机 XNC_DESKTOP_PIPELINE_V2 仍是 host 侧
	// force，二者不一致时 agent 按会话钉子响亮拒绝）。百分比桶按节点 UUID
	// 哈希（rt-pipe host 每节点共享，一个节点的并发会话必须同版）。
	DesktopMediaV2Percent   int      // XNC_DESKTOP_MEDIA_V2_PERCENT，0-100，默认 0（全 v1）；越界视为 0
	DesktopMediaV2Allowlist []string // XNC_DESKTOP_MEDIA_V2_ALLOWLIST，逗号分隔节点 UUID（显式胜百分比）
	DesktopMediaV2Rollback  bool     // XNC_DESKTOP_MEDIA_V2_ROLLBACK，true = 一切新会话钉回 v1（回滚开关）
}

func Load() (Config, error) {
	c := Config{
		ListenAddr:         env("XNC_LISTEN", ":8080"),
		DatabaseURL:        os.Getenv("XNC_DATABASE_URL"),
		JWTSecret:          []byte(os.Getenv("XNC_JWT_SECRET")),
		AdminEmail:         os.Getenv("XNC_ADMIN_EMAIL"),
		AdminPassword:      os.Getenv("XNC_ADMIN_PASSWORD"),
		HeartbeatTimeout:   envDur("XNC_HEARTBEAT_TIMEOUT", 90*time.Second),
		EnrollTokenTTL:     envDur("XNC_ENROLL_TOKEN_TTL", 30*time.Minute),
		ShellPerNode:       envInt("XNC_SHELL_PER_NODE", 10),
		ShellIdleTimeout:   envDur("XNC_SHELL_IDLE", 30*time.Minute),
		ShellMaxLifetime:   envDur("XNC_SHELL_MAX", 8*time.Hour),
		TurnURLs:           envList("XNC_TURN_URLS"),
		TurnUsername:       os.Getenv("XNC_TURN_USERNAME"),
		TurnCredential:     os.Getenv("XNC_TURN_CREDENTIAL"),
		TurnPool:           envList("XNC_TURN_POOL"),
		DesktopPerNode:     envInt("XNC_DESKTOP_PER_NODE", 4),
		DesktopIdleTimeout: envDur("XNC_DESKTOP_IDLE", 5*time.Minute),
		DesktopICEPolicy:   icePolicy(os.Getenv("XNC_DESKTOP_ICE_POLICY")),
		DesktopMediaV2Percent:   envInt("XNC_DESKTOP_MEDIA_V2_PERCENT", 0),
		DesktopMediaV2Allowlist: envList("XNC_DESKTOP_MEDIA_V2_ALLOWLIST"),
		DesktopMediaV2Rollback:  envBool("XNC_DESKTOP_MEDIA_V2_ROLLBACK", false),
	}
	if c.DatabaseURL == "" {
		return c, fmt.Errorf("XNC_DATABASE_URL is required")
	}
	if len(c.JWTSecret) < 32 {
		return c, fmt.Errorf("XNC_JWT_SECRET must be at least 32 bytes")
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

// icePolicy 归一化 XNC_DESKTOP_ICE_POLICY：仅 "all" 视为放开直连，其余
//（空/未知值）fail closed 回 "relay"——缺省行为与旧版完全一致（不配置 =
// agent 侧强制 relay）。字面量对应 proto.DesktopIceAll（"all"），此处不引
// proto 依赖以保持 config 只依赖标准库。
func icePolicy(v string) string {
	if v == "all" {
		return "all"
	}
	return "relay"
}
