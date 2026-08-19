package main

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Config is the CLI credential file at ~/.xnc/config.json.
type Config struct {
	Server string `json:"server"`
	Token  string `json:"token"`
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
		}
	}
	return c, nil
}

// SaveConfig writes the config file with 0600 permissions.
func SaveConfig(c Config) error {
	if err := os.MkdirAll(filepath.Dir(configPath()), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(configPath(), b, 0o600)
}
