// cmd_update.go — `xnc update`：CLI 自更新（下载最新 release 的 CLI 制
// 品并自替换）。Windows 自替换标准舞：运行中 exe 不可覆写但可改名——
// 当前版本 → xnc.exe.old，新版本就位，下次运行清扫 .old。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	"xnc/proto"
)

type cliLatestInfo struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Channel string `json:"channel"`
}

func newUpdateCmd() *cobra.Command {
	var channel string
	cmd := &cobra.Command{
		Use:   "update [--channel stable|dev]",
		Short: "Update the xnc CLI to the latest release",
		RunE: func(c *cobra.Command, _ []string) error {
			return runUpdate(c, channel)
		},
	}
	cmd.Flags().StringVar(&channel, "channel", "",
		"release channel (stable|dev; empty = saved preference or stable)")
	return cmd
}

func runUpdate(cmd *cobra.Command, channelFlag string) error {
	cl, usage := dial(cmd, true)
	if usage != "" {
		return failUsage(cmd, usage)
	}

	// 1. 元信息。
	// 频道解析：flag > 保存的偏好 > stable。非默认频道保存偏好。
	if channelFlag == "" {
		channelFlag = savedChannel()
	}
	if channelFlag == "" {
		channelFlag = "stable"
	}
	if channelFlag != "stable" && channelFlag != "dev" {
		return failUsage(cmd, "channel must be stable or dev")
	}
	saveChannel(channelFlag)

	var latest cliLatestInfo
	path := "/api/cli/latest?channel=" + channelFlag
	if err := cl.Do("GET", path, nil, &latest); err != nil {
		return failAPI(cmd, err)
	}
	if latest.Version == cliVersion {
		fmt.Fprintf(cmd.OutOrStdout(), "Already up to date (%s, %s)\n", cliVersion, channelFlag)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s -> %s [%s]\n", cliVersion, latest.Version, channelFlag)

	// 2. 下载（Bearer 认证的裸 HTTP）。
	body, err := downloadAuthenticated(cl, "/api/cli/download?channel="+channelFlag)
	if err != nil {
		return failAPI(cmd, proto.Err(0, "NETWORK", err.Error()))
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != latest.SHA256 {
		return failAPI(cmd, proto.Err(0, "NETWORK", "sha256 mismatch (download corrupted?)"))
	}

	// 3. 自替换（rename 舞）。
	exe, err := os.Executable()
	if err != nil {
		return failUsage(cmd, err.Error())
	}
	old := exe + ".old"
	_ = os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		return failUsage(cmd, "backup current: "+err.Error())
	}
	if err := os.WriteFile(exe, body, 0o755); err != nil {
		// 就位失败：恢复旧版（.old 还在）。
		_ = os.Rename(old, exe)
		return failUsage(cmd, "write new: "+err.Error())
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Updated to %s (old version backed up as %s, cleaned on next run)\n",
		latest.Version, old)
	return nil
}

// downloadAuthenticated 裸 HTTP GET（Bearer 头），返回响应体字节。
func downloadAuthenticated(cl *Client, path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, cl.Base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cl.Token)
	resp, err := cl.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e proto.APIError
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(b, &e) == nil && e.Code != "" {
			return nil, &e
		}
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 128<<20))
}

// cleanupOldCLI 启动时清扫自更新残留的 .old（幂等）。
func cleanupOldCLI() {
	if exe, err := os.Executable(); err == nil {
		_ = os.Remove(exe + ".old")
	}
}

// savedChannel 读取保存的频道偏好（config.json 的 channel 字段）。
func savedChannel() string {
	cfg, _ := LoadConfig()
	return cfg.Channel
}

// saveChannel 持久化频道偏好（幂等——写入 config.json 但不覆盖 token）。
func saveChannel(ch string) {
	cfg, _ := LoadConfig()
	if cfg.Channel == ch {
		return
	}
	cfg.Channel = ch
	_ = SaveConfig(cfg)
}
