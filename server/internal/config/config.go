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
	PublicURL        string        // XNC_PUBLIC_URL，安装脚本/SHORT LINK 的基址

	// —— M1-Slice2 desktop/TURN。TURN 缺省为空 = desktop 会话 503
	// TURN_UNCONFIGURED（relay-only 无 TURN 不可用）；凭据绝不入日志。
	TurnURLs           []string      // XNC_TURN_URLS，逗号分隔（turn:/turns: URL）
	TurnUsername       string        // XNC_TURN_USERNAME（dev = lt-cred 静态用户）
	TurnCredential     string        // XNC_TURN_CREDENTIAL（M2 换 REST 时效凭据）
	DesktopPerNode     int           // XNC_DESKTOP_PER_NODE，默认 1（采集源单实例），0 = 不限
	DesktopIdleTimeout time.Duration // XNC_DESKTOP_IDLE，默认 5m（无信令活动即关），0 = 不限
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
		PublicURL:          env("XNC_PUBLIC_URL", "https://control.xnc.app"),
		TurnURLs:           envList("XNC_TURN_URLS"),
		TurnUsername:       os.Getenv("XNC_TURN_USERNAME"),
		TurnCredential:     os.Getenv("XNC_TURN_CREDENTIAL"),
		DesktopPerNode:     envInt("XNC_DESKTOP_PER_NODE", 1),
		DesktopIdleTimeout: envDur("XNC_DESKTOP_IDLE", 5*time.Minute),
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
