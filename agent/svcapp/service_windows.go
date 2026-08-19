//go:build windows

// svcapp 将 agent.Agent 适配为 Windows 服务：svc.Handler 生命周期、
// 通过 mgr 安装/卸载 XNCAgent 服务（Automatic 启动）。
package svcapp

import (
	"context"
	"errors"
	"log/slog"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"xnc/agent"
)

// ServiceName 是 Windows 服务管理器中的服务名（sc query XNCAgent）。
const ServiceName = "XNCAgent"

var errExists = errors.New("service already exists")

type handler struct{ A *agent.Agent }

func (h *handler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	s <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- h.A.Run(ctx) }()
	s <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				s <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s <- svc.Status{State: svc.StopPending}
				cancel()
				<-errCh // 等待控制连接清理完成后再向 SCM 报告退出
				return false, 0
			}
		case err := <-errCh:
			if err != nil {
				slog.Error("agent exited", "err", err)
				return false, 1
			}
			return false, 0
		}
	}
}

// IsService 报告当前进程是否由 Windows 服务控制管理器启动。
func IsService() bool {
	is, _ := svc.IsWindowsService()
	return is
}

// Run 将 agent 交给 SCM 调度（服务模式入口，阻塞直至服务停止）。
func Run(a *agent.Agent) error { return svc.Run(ServiceName, &handler{A: a}) }

// Install 创建 XNCAgent 服务（Automatic）并立即启动。
// args 的每个元素必须是独立的 argv 词（如 "run"、"--server=…"）：
// x/sys 对各元素分别做 syscall.EscapeArg 引用处理，拼好的整串会被
// 整体加引号成一个 argv 词，导致服务启动后 cobra 解析失败。
func Install(exePath string, args ...string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	if s, err := m.OpenService(ServiceName); err == nil {
		_ = s.Close()
		return errExists
	}
	s, err := m.CreateService(ServiceName, exePath, mgr.Config{
		StartType:   mgr.StartAutomatic,
		DisplayName: "XNC Agent",
		Description: "XNC node agent (outbound control connection)",
	}, args...)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Start()
}

// Uninstall 停止并删除 XNCAgent 服务（服务不存在/未运行均视为可清理）。
func Uninstall() error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceName)
	if err != nil {
		return err
	}
	defer s.Close()
	_, _ = s.Control(svc.Stop)
	return s.Delete()
}
