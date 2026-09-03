package main

// xnc upgrade（spec §10 / §9.1 / §9.5）：经 agentctl 管道触发 agent 立即
// 检查并静默应用更新——CLI 自身不替换文件。--channel 跨频道切换（改绑定
// 频道后按新频道检查；仅 stable|dev）。触发是异步的：管道应答
// {ok:true,triggered:true} 即返回，随后本命令阻塞轮询 {op:status} 打印
// 状态迁移（checking → applying → 新版本），完成打印当前版本。

import (
	"context"
	"time"

	"github.com/spf13/cobra"
)

// upgradePollInterval / upgradePollTimeout：触发后轮询 status 的节奏
// （下载/安装分钟级；测试注入缩短）。超时不是失败（触发已成功），仅告警。
var (
	upgradePollInterval = 2 * time.Second
	upgradePollTimeout  = 5 * time.Minute
)

func newUpgradeCmd() *cobra.Command {
	var channel string
	cmd := &cobra.Command{
		Use:   "upgrade [--channel stable|dev]",
		Short: "Trigger the local agent to check for and apply updates now",
		Long: `Ask the local agent service (over its control pipe) to check for an
update on the machine's channel and silently apply it. The agent downloads
and runs the installer itself; this command blocks showing progress until
the new version is online.

--channel switches the machine's update channel first (stable or dev); the
binding is rewritten atomically and the check runs against the new channel.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if channel != "" && channel != "stable" && channel != "dev" {
				return failUsage(cmd, "channel must be stable or dev")
			}
			return runUpgrade(cmd, channel)
		},
	}
	cmd.Flags().StringVar(&channel, "channel", "",
		"update channel (stable|dev; empty = keep the machine's current channel)")
	addJSONFlag(cmd)
	return cmd
}

// upgradePollStatus 单次 status 轮询（独立短时限：agent 停机重启窗口内
// 拨号失败按瞬态处理，由调用方决定重试）。
func upgradePollStatus() (agentctlResp, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return agentctlRoundTrip(ctx, agentctlReq{Op: agentctlOpStatus})
}

// upgradeOutcome 是轮询结论（--json envelope 的数据源）。
type upgradeOutcome struct {
	Triggered bool   `json:"triggered"`
	Updated   bool   `json:"updated"`
	Version   string `json:"version"`
	Channel   string `json:"channel"`
	State     string `json:"state,omitempty"`
	Note      string `json:"note,omitempty"`
}

func runUpgrade(cmd *cobra.Command, channel string) error {
	// 基线：当前版本/频道（完成判据 = 版本变化；顺带展示切换前状态）。
	base, err := agentctlCall(agentctlReq{Op: agentctlOpStatus})
	if err != nil {
		return failAPI(cmd, errPipeUnreachable(err))
	}
	if !base.OK {
		return failPipeOp(cmd, base)
	}

	resp, err := agentctlCall(agentctlReq{Op: agentctlOpUpgrade, Channel: channel})
	if err != nil {
		return failAPI(cmd, errPipeUnreachable(err))
	}
	if !resp.OK {
		return failPipeOp(cmd, resp)
	}
	out := upgradeOutcome{Triggered: resp.Triggered, Version: base.Version, Channel: base.Channel}
	if !resp.Triggered {
		// agent 拒绝触发（更新在途）发生在频道切换之前：绑定未改，
		// 不打印 switching，只渲染 note。
		out.Note = resp.Note
		if out.Note == "" {
			out.Note = "update already in progress"
		}
		outf(cmd, "%s\n", out.Note)
	} else if channel != "" {
		outf(cmd, "switching channel %s -> %s\n", base.Channel, channel)
		outf(cmd, "update triggered\n")
	} else {
		outf(cmd, "update triggered (channel %s)\n", base.Channel)
	}

	// 轮询进度（§9.1 应用是分钟级；状态迁移只打一次）。
	deadline := time.Now().Add(upgradePollTimeout)
	sawPhase, nilStreak, lastPhase, timedOut, upToDate := false, 0, "", false, false
	for {
		st, err := upgradePollStatus()
		if err == nil && !st.OK {
			// agent 显式报错（协议级）：透传退出。
			return failPipeOp(cmd, st)
		}
		if err == nil {
			out.Version, out.Channel, out.State = st.Version, st.Channel, st.State
			if st.Version != "" && st.Version != base.Version {
				out.Updated = true
				break
			}
			phase := ""
			if st.Update != nil {
				phase = st.Update.Phase
				if phase != lastPhase {
					switch phase {
					case "checking":
						outf(cmd, "checking for updates\n")
					case "applying":
						outf(cmd, "applying update %s -> %s\n", st.Update.From, st.Update.To)
					}
					lastPhase = phase
				}
			}
			if phase != "" {
				sawPhase, nilStreak = true, 0
			} else {
				// 进度消失且版本未变：触发侧检查已收线且未应用（无更新可
				// 应用/黑名单/降级保护）。连续两次确认（躲开竞态窗口）。
				nilStreak++
				if sawPhase || nilStreak >= 2 {
					upToDate = true
					break
				}
			}
		}
		if time.Now().After(deadline) {
			timedOut = true
			outf(cmd, "warning: update still in progress after %s - current version %s (poll: xnc status)\n",
				upgradePollTimeout.Round(time.Second), out.Version)
			break
		}
		time.Sleep(upgradePollInterval)
	}

	switch {
	case out.Updated:
		outf(cmd, "updated to %s", out.Version)
		if out.State == agentctlStateOnline {
			outf(cmd, " (online)\n")
		} else {
			outf(cmd, " (agent reconnecting)\n")
		}
	case upToDate:
		outf(cmd, "already up to date (%s)\n", out.Version)
	case timedOut:
		// 告警已在轮询处打印（触发已成功，超时不是失败）。
	}
	if jsonOut(cmd) {
		PrintJSON(true, out, nil)
	}
	return nil
}
