// Package agentctl 实现 agent 本地控制管道 \\.\pipe\xnc-agentctl（spec §6.3，
// 本规范唯一新本地面）：单行 JSON 请求 → 单行 JSON 应答（与 core 管道的
// 单行风格一致），每连接一问一答后关闭。管道只做转发与鉴权门控，业务在
// agent 编排层（Deps 注入）：
//
//	{op:"register", server, clusterId, jwt} → {ok:true, nodeId}
//	{op:"deregister"}                        → {ok:true}（要求连接方为管理员）
//	{op:"status"}                            → {ok:true, state, nodeId, server, clusterId, version}
//
// jwt 仅在内存中经手（请求 → 编排层 HTTP 头），不落盘、不写日志；应答与
// 错误串均不含 jwt。
package agentctl

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// PipeName 固定管道名（CLI/安装器按此连接）。
const PipeName = `\\.\pipe\xnc-agentctl`

// 协议 op 与 status 取值。
const (
	OpRegister   = "register"
	OpDeregister = "deregister"
	OpStatus     = "status"

	StateUnregistered = "unregistered"
	StateRegistered   = "registered"
	StateOnline       = "online"
)

// Request 单行 JSON 请求（多余字段忽略，前向兼容）。
type Request struct {
	Op        string `json:"op"`
	Server    string `json:"server,omitempty"`    // register：控制面基址
	ClusterID string `json:"clusterId,omitempty"` // register：目标 cluster
	JWT       string `json:"jwt,omitempty"`       // register：用户 JWT（内存传递，用后即弃）
}

// Response 单行 JSON 应答。失败：{"ok":false,"error":"<code>: <message>"}；
// 错误码沿用服务端 proto 码（MACHINE_ID_CONFLICT 等），本地校验用小写码
// （bad_request / forbidden / internal / not_registered）。
type Response struct {
	OK        bool   `json:"ok"`
	NodeID    string `json:"nodeId,omitempty"`
	State     string `json:"state,omitempty"`
	Server    string `json:"server,omitempty"`
	ClusterID string `json:"clusterId,omitempty"`
	Version   string `json:"version,omitempty"`
	Error     string `json:"error,omitempty"`
}

// Errorf 构造失败应答（错误串格式 "<code>: <message>"）。
func Errorf(format string, args ...any) Response {
	return Response{OK: false, Error: fmt.Sprintf(format, args...)}
}

// Status 是 status op 的数据源（agent 编排层按 binding + 连接状态填充）。
type Status struct {
	State     string // unregistered | registered | online（WS 已建立）
	NodeID    string
	Server    string
	ClusterID string
	Version   string // machineinfo.Version（agent bundle 版本）
}

// Deps 管道操作到 agent 编排层的依赖注入（agent.Agent 实现）。实现自行保证
// jwt 不落盘、不写日志。
type Deps interface {
	// Register 以用户 JWT 调 POST /api/clusters/{id}/nodes/register（Task 2
	// 端点）注册本机；成功返回 nodeId 并落 binding、唤醒连接路径。
	Register(ctx context.Context, server, clusterID, jwt string) (string, error)
	// Deregister 机器自注销：WS 控制通道 NODE_DELETE + 删 binding.json
	// （保留 identity.json 供重注册复用）。
	Deregister(ctx context.Context) error
	// Status 本机注册/在线状态。
	Status() Status
}

// Server 控制管道服务端。Name 空 = PipeName；IsAdminConn 空 = 平台实现
// （Windows：连接方进程令牌的 Administrators 启用成员检查；测试可注入）。
type Server struct {
	Name        string
	Deps        Deps
	Log         *slog.Logger
	IsAdminConn func(net.Conn) (bool, error)

	mu sync.Mutex // 串行化 op（一次一个；连接本身也顺序 accept）
}

func (s *Server) name() string {
	if s.Name == "" {
		return PipeName
	}
	return s.Name
}

func (s *Server) logger() *slog.Logger {
	if s.Log == nil {
		return slog.Default()
	}
	return s.Log
}

const (
	// connTimeout 单连接读/写各自的时限（dispatch 另有 opTimeout）：防慢
	// 客户端占住唯一 accept 槽位。
	connTimeout = 30 * time.Second
	// opTimeout 单操作时限：register 含 HTTP 往返，deregister 含一次 WS
	// 拨号认证，均给足余量；超时以 internal 错误回给 CLI。
	opTimeout = 60 * time.Second
	// maxRequestLine 请求行上限（JWT 约 1KB；64KB 余量充足）。
	maxRequestLine = 64 * 1024
)

// dispatch 处理一条已解析请求（平台无关；Windows Listen 与测试共用）。
func (s *Server) dispatch(ctx context.Context, conn net.Conn, req Request) Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch req.Op {
	case OpRegister:
		if req.Server == "" || req.ClusterID == "" || req.JWT == "" {
			return Errorf("bad_request: server, clusterId and jwt required")
		}
		nodeID, err := s.Deps.Register(ctx, req.Server, req.ClusterID, req.JWT)
		if err != nil {
			return Errorf("%s", err.Error())
		}
		return Response{OK: true, NodeID: nodeID}
	case OpDeregister:
		if s.IsAdminConn == nil {
			return Errorf("internal: admin check unavailable")
		}
		admin, err := s.IsAdminConn(conn)
		if err != nil {
			return Errorf("internal: admin check: %v", err)
		}
		if !admin {
			return Errorf("forbidden: admin required")
		}
		if err := s.Deps.Deregister(ctx); err != nil {
			return Errorf("%s", err.Error())
		}
		return Response{OK: true}
	case OpStatus:
		st := s.Deps.Status()
		return Response{
			OK: true, State: st.State, NodeID: st.NodeID,
			Server: st.Server, ClusterID: st.ClusterID, Version: st.Version,
		}
	default:
		return Errorf("bad_request: unknown op %q", req.Op)
	}
}
