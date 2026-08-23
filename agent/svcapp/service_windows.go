//go:build windows

// svcapp 将 agent.Agent 适配为 Windows 服务：svc.Handler 生命周期、
// 通过 mgr 安装/卸载服务（Automatic 启动）。服务名参数化（M2-Slice3
// Task 2）：默认 XNCAgent（生产行为不变）；dev 拓扑安装 XNCAgentDev
// （独立服务名 + 独立 state 目录 → 与生产完全隔离）。单二进制可服务
// 任意名字 —— 服务运行模式从 argv --service-name 读回自己的名字。
package svcapp

import (
	"context"
	"errors"
	"log/slog"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"xnc/agent"
)

// DefaultServiceName 是生产服务名（sc query XNCAgent）。dev 拓扑用
// XNCAgentDev（install --service-name XNCAgentDev）。
const DefaultServiceName = "XNCAgent"

var errExists = errors.New("service already exists")

// ServiceConfig 描述一次安装/运行的目标服务；零值字段回落默认
// （= 生产行为）。StateDir 不在此处 —— 它走 agent 自身 --state-dir
// flag（服务 argv 传递），隔离由安装方保证。
type ServiceConfig struct {
	Name        string
	DisplayName string
	Description string
}

// Normalize 补全空字段为生产默认。
func (c ServiceConfig) Normalize() ServiceConfig {
	if c.Name == "" {
		c.Name = DefaultServiceName
	}
	if c.DisplayName == "" {
		c.DisplayName = "XNC Agent"
	}
	if c.Description == "" {
		c.Description = "XNC node agent (outbound control connection)"
	}
	return c
}

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
// name 必须与 SCM 注册名一致（空 = XNCAgent）；服务 argv 自带
// --service-name <name>（install 时写入），运行侧据此分发。
func Run(a *agent.Agent, name string) error {
	return svc.Run(ServiceConfig{Name: name}.Normalize().Name, &handler{A: a})
}

// Install 创建服务（Automatic）并立即启动。
// args 的每个元素必须是独立的 argv 词（如 "run"、"--server=…"）：
// x/sys 对各元素分别做 syscall.EscapeArg 引用处理，拼好的整串会被
// 整体加引号成一个 argv 词，导致服务启动后 cobra 解析失败。
func Install(exePath string, cfg ServiceConfig, args ...string) error {
	cfg = cfg.Normalize()
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	if s, err := m.OpenService(cfg.Name); err == nil {
		_ = s.Close()
		return errExists
	}
	s, err := m.CreateService(cfg.Name, exePath, mgr.Config{
		StartType:   mgr.StartAutomatic,
		DisplayName: cfg.DisplayName,
		Description: cfg.Description,
	}, args...)
	if err != nil {
		return err
	}
	defer s.Close()
	return s.Start()
}

// Uninstall 停止并删除服务（不存在/未运行均视为可清理）。
func Uninstall(name string) error {
	m, err := mgr.Connect()
	if err != nil {
		return err
	}
	defer m.Disconnect()
	s, err := m.OpenService(ServiceConfig{Name: name}.Normalize().Name)
	if err != nil {
		return err
	}
	defer s.Close()
	_, _ = s.Control(svc.Stop)
	return s.Delete()
}
