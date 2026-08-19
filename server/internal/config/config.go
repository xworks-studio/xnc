package config

import (
	"fmt"
	"os"
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
}

func Load() (Config, error) {
	c := Config{
		ListenAddr:       env("XNC_LISTEN", ":8080"),
		DatabaseURL:      os.Getenv("XNC_DATABASE_URL"),
		JWTSecret:        []byte(os.Getenv("XNC_JWT_SECRET")),
		AdminEmail:       os.Getenv("XNC_ADMIN_EMAIL"),
		AdminPassword:    os.Getenv("XNC_ADMIN_PASSWORD"),
		HeartbeatTimeout: envDur("XNC_HEARTBEAT_TIMEOUT", 90*time.Second),
		EnrollTokenTTL:   envDur("XNC_ENROLL_TOKEN_TTL", 30*time.Minute),
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
