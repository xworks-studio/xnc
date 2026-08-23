// xnc-agent：XNC 节点代理宿主 CLI。
// 命令树在本文件定义（跨平台共用）；Windows 专属的服务宿主逻辑
// 隔离在 main_windows.go / main_other.go 的平台钩子中。
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"xnc/agent/updater"

	"github.com/spf13/cobra"
)

func main() {
	// apply-update：自更新换文件子模式（updater.apply spawn 的第二自我。
	// 不走 cobra flag 树——参数位置式：--apply-update <stageDir> <parentPID>）。
	for _, arg := range os.Args[1:] {
		if arg == "--apply-update" && len(os.Args) >= 4 {
			logf := func(format string, args ...any) {
				fmt.Fprintf(os.Stderr, "apply-update: "+format+"\n", args...)
			}
			if err := updater.RunApply(os.Args[2], os.Args[3], logf); err != nil {
				logf("FAILED: %v", err)
				os.Exit(1)
			}
			return
		}
	}
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// newRootCmd 构建命令树（抽出供测试直接驱动 flag 解析路径）。
func newRootCmd() *cobra.Command {
	var server, token, stateDir, serviceName, corePipe, coreSecretHex string
	var devName, devLogFile, devCorePipe, devCoreSecretHex string
	root := &cobra.Command{
		Use:          "xnc-agent",
		Short:        "XNC Windows node agent",
		SilenceUsage: true,
	}
	run := &cobra.Command{
		Use:   "run",
		Short: "run the agent (foreground debug entry; service mode when launched by the SCM)",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runAgent(server, token, stateDir, serviceName, corePipe, coreSecretHex)
		},
	}
	// run-dev-console：一次性开发拓扑宿主——不起 SCM、状态全在
	// %TEMP%\xnc-dev-agent-<pid>（退出即清）、server/token 全来自命令行。
	runDev := &cobra.Command{
		Use:   "run-dev-console",
		Short: "throwaway foreground dev agent: fresh %TEMP% state, no SCM, Ctrl+C exits clean",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runDevConsole(server, token, devName, devLogFile, devCorePipe, devCoreSecretHex)
		},
	}
	install := &cobra.Command{
		Use:   "install",
		Short: "install and start the XNCAgent Windows service",
		RunE: func(_ *cobra.Command, _ []string) error {
			return installService(server, token, stateDir, serviceName, corePipe, coreSecretHex)
		},
	}
	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "stop and remove the XNCAgent Windows service",
		RunE: func(_ *cobra.Command, _ []string) error {
			return uninstallService(serviceName)
		},
	}
	for _, c := range []*cobra.Command{run, install} {
		c.Flags().StringVar(&server, "server", "", "control server URL (required)")
		c.Flags().StringVar(&token, "token", "", "enrollment token (first run)")
		c.Flags().StringVar(&stateDir, "state-dir", defaultStateDir(), "state directory")
		// M2-Slice3 Task 2: dev 服务化 —— 服务名参数化（XNCAgentDev）+
		// dev desktop core pipe 凭据入服务 argv（SCM 无交互环境；与 core
		// --smoke-secret 同一 dev-only plaintext 先例）。默认值 = 生产行为
		// 完全不变。
		c.Flags().StringVar(&serviceName, "service-name", "", "Windows service name (default XNCAgent)")
		c.Flags().StringVar(&corePipe, "desktop-core-pipe", "",
			"desktop sessions: core XNIP pipe (full \\\\.\\pipe\\name)")
		c.Flags().StringVar(&coreSecretHex, "desktop-core-secret-hex", "",
			"desktop sessions: core pipe secret (64 hex chars; never logged)")
		_ = c.MarkFlagRequired("server")
	}
	uninstall.Flags().StringVar(&serviceName, "service-name", "", "Windows service name (default XNCAgent)")
	runDev.Flags().StringVar(&server, "server", "", "control server URL (required)")
	runDev.Flags().StringVar(&token, "token", "", "enrollment token (required; fresh identity every run)")
	runDev.Flags().StringVar(&devName, "name", "", "node name override (default <hostname>-DEV)")
	runDev.Flags().StringVar(&devLogFile, "log-file", "", "also append logs to this file")
	// dev 桌面采集 seam(T6):等价设置 XNC_DESKTOP_CORE_PIPE /
	// XNC_DESKTOP_CORE_SECRET_HEX —— schtasks /TR 里 cmd set 包装太脆,
	// flag 交给 run-dev-console 自己 Setenv。两者齐全才注册 desktop kind。
	runDev.Flags().StringVar(&devCorePipe, "desktop-core-pipe", "",
		"dev desktop: core XNIP pipe (full \\\\.\\pipe\\name), enables desktop sessions")
	runDev.Flags().StringVar(&devCoreSecretHex, "desktop-core-secret-hex", "",
		"dev desktop: core pipe secret (64 hex chars; never logged)")
	_ = runDev.MarkFlagRequired("server")
	_ = runDev.MarkFlagRequired("token")
	root.AddCommand(run, runDev, install, uninstall)
	return root
}

// cmdContext 将 Ctrl+C / SIGTERM 转为 ctx 取消（前台调试模式用）。
func cmdContext() context.Context {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	_ = stop // 进程随 main 退出，无需显式释放
	return ctx
}
