package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config is the CLI credential file at ~/.xnc/config.json.
type Config struct {
	Server          string `json:"server"`
	Token           string `json:"token"`
	RememberedEmail string `json:"remembered_email,omitempty"` // login 交互提示的默认邮箱
	Channel         string `json:"channel,omitempty"`          // update 频道偏好（stable|dev）
}

func configPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".xnc", "config.json")
}

// LoadConfig merges environment (XNC_SERVER/XNC_TOKEN) over the config file.
// Flags are applied on top by the caller: flag > env > file.
func LoadConfig() (Config, error) {
	var c Config
	if v := os.Getenv("XNC_SERVER"); v != "" {
		c.Server = v
	}
	if v := os.Getenv("XNC_TOKEN"); v != "" {
		c.Token = v
	}
	if c.Server != "" && c.Token != "" {
		return c, nil
	}
	b, err := os.ReadFile(configPath())
	if err == nil {
		var f Config
		if json.Unmarshal(b, &f) == nil {
			if c.Server == "" {
				c.Server = f.Server
			}
			if c.Token == "" {
				c.Token = f.Token
			}
			if c.RememberedEmail == "" {
				c.RememberedEmail = f.RememberedEmail
			}
		}
	}
	return c, nil
}

// readConfigFile reads the config file only — no env merge. Logout uses it to
// detect a *file* token and to rewrite the file fields as-is (XNC_TOKEN in the
// environment must not leak into the saved config).
func readConfigFile() (Config, bool) {
	b, err := os.ReadFile(configPath())
	if err != nil {
		return Config{}, false
	}
	var c Config
	if json.Unmarshal(b, &c) != nil {
		return Config{}, false
	}
	return c, true
}

// SaveConfig writes the config file with 0600 permissions.
func SaveConfig(c Config) error {
	if err := os.MkdirAll(filepath.Dir(configPath()), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(configPath(), b, 0o600)
}
