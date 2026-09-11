// Package agentctl 实现 agent 本地控制管道 \\.\pipe\xnc-agentctl（spec §6.3，
// 本规范唯一新本地面）：单行 JSON 请求 → 单行 JSON 应答（与 core 管道的
// 单行风格一致），每连接一问一答后关闭。管道只做转发与鉴权门控，业务在
// agent 编排层（Deps 注入）：
//
//	{op:"register", server, clusterId, jwt} → {ok:true, nodeId}
//	{op:"deregister"}                        → {ok:true}（要求连接方为管理员）
//	{op:"status"}                            → {ok:true, state, nodeId, server, clusterId, channel, version, update?}
//	{op:"upgrade", channel?}                 → {ok:true, triggered}（§9.1 手动触发；channel 见 §9.5）
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
	OpUpgrade    = "upgrade"
	OpDisplay    = "display"

	StateUnregistered = "unregistered"
	StateRegistered   = "registered"
	StateOnline       = "online"
)

// display op 的 action 取值（on = 手动开虚拟显示器并保持；off = 移除；
// status = 只读状态）。
const (
	DisplayActionOn     = "on"
	DisplayActionOff    = "off"
	DisplayActionStatus = "status"
)

// 更新频道（§9.5 跨频道切换仅经 upgrade op；白名单外的值 → bad_request）。
const (
	ChannelStable = "stable"
	ChannelDev    = "dev"
)

// ValidChannel 报告 ch 是否为合法频道（空 = 未指定，交由编排层按当前绑定）。
func ValidChannel(ch string) bool {
	return ch == "" || ch == ChannelStable || ch == ChannelDev
}

// update op 应答里 status.update.phase 的取值。
const (
	UpdatePhaseChecking = "checking" // 触发后拉清单/下载中（agent 侧进程内标记）
	UpdatePhaseApplying = "applying" // pending 存在 = 安装器执行中/新 agent 自检中
)

// Request 单行 JSON 请求（多余字段忽略，前向兼容）。
type Request struct {
	Op        string `json:"op"`
	Server    string `json:"server,omitempty"`    // register：控制面基址
	ClusterID string `json:"clusterId,omitempty"` // register：目标 cluster
	JWT       string `json:"jwt,omitempty"`       // register：用户 JWT（内存传递，用后即弃）
	Channel   string `json:"channel,omitempty"`   // upgrade：目标频道 stable|dev（空 = 当前频道）
	Action    string `json:"action,omitempty"`    // display：on | off | status
}

// Response 单行 JSON 应答。失败：{"ok":false,"error":"<code>: <message>"}；
// 错误码沿用服务端 proto 码（MACHINE_ID_CONFLICT 等），本地校验用小写码
// （bad_request / forbidden / internal / not_registered）。
//
// Triggered 仅 upgrade op 有意义（恒输出，false = 更新已在途而非错误，配
// note 说明）；Update 仅 status op 在更新在途时输出（进度信号，可缺省）。
type Response struct {
	OK        bool         `json:"ok"`
	NodeID    string       `json:"nodeId,omitempty"`
	State     string       `json:"state,omitempty"`
	Server    string       `json:"server,omitempty"`
	ClusterID string       `json:"clusterId,omitempty"`
	Channel   string       `json:"channel,omitempty"`
	Version   string       `json:"version,omitempty"`
	Triggered bool         `json:"triggered"`
	Note      string       `json:"note,omitempty"`
	Update    *UpdateInfo  `json:"update,omitempty"`
	Display   *DisplayInfo `json:"display,omitempty"`
	Error     string       `json:"error,omitempty"`
}

// DisplayInfo display op 的状态载荷（display.Manager.Snapshot 的镜像，
// agentctl 不 import 业务包，保持协议数据面独立）。
type DisplayInfo struct {
	DriverInstalled bool   `json:"driverInstalled"`
	DevicePresent   bool   `json:"devicePresent"`
	VirtualActive   bool   `json:"virtualActive"`
	PhysicalActive  bool   `json:"physicalActive"`
	LidClosed       bool   `json:"lidClosed"`
	LidKnown        bool   `json:"lidKnown"`
	ForceLid        string `json:"forceLid,omitempty"`
	AutoActive      bool   `json:"autoActive"`
	ManualActive    bool   `json:"manualActive"`
	Enabled         bool   `json:"enabled"`
}

// UpdateInfo status op 的在途更新进度（CLI upgrade 轮询信号，尽力而为：
// 仅覆盖手动触发与安装器执行窗口）。
type UpdateInfo struct {
	Phase string `json:"phase"` // checking | applying
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
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
	Channel   string // 生效频道（stable|dev；空绑定归一为 stable）
	Version   string // machineinfo.Version（agent bundle 版本）
	Update    *UpdateInfo
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
	// Upgrade 立即检查并静默应用更新（§9.1 手动触发；channel 非空且异于
	// 绑定频道时先原子切换 binding.channel，§9.5）。触发是异步的（编排器
	// 后台执行），本调用快速返回：triggered=false 表示已有更新在途（非
	// 错误，dispatch 配 note 应答）；错误（如未注册/绑定写失败）同步返回。
	Upgrade(ctx context.Context, channel string) (triggered bool, err error)
	// Display 虚拟显示器本地控制（on/off/status）；status 恒成功，on 在
	// 驱动未装时返回 not_installed 错误码。
	Display(ctx context.Context, action string) (*DisplayInfo, error)
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
	// 拨号认证，均给足余量；upgrade 为异步触发（秒级返回），status 恒快。
	// 超时以 internal 错误回给 CLI。
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
			Server: st.Server, ClusterID: st.ClusterID, Channel: st.Channel,
			Version: st.Version, Update: st.Update,
		}
	case OpUpgrade:
		// 频道白名单先于编排层（§9.5：仅 stable|dev，坏值不得触碰绑定）。
		if !ValidChannel(req.Channel) {
			return Errorf("bad_request: channel must be stable or dev")
		}
		// 无 admin 门：与 register 同级（任何交互用户可触发；改的是更新
		// 频道而非节点归属，绑定写入本身在 agent/ SYSTEM 侧完成）。
		triggered, err := s.Deps.Upgrade(ctx, req.Channel)
		if err != nil {
			return Errorf("%s", err.Error())
		}
		if !triggered {
			// 已在途（pending 存在或一次触发未收线）= 非错误；CLI 渲染
			// note 后转入 status 轮询（两端口径一致）。
			return Response{OK: true, Triggered: false, Note: "update already in progress"}
		}
		return Response{OK: true, Triggered: true}
	case OpDisplay:
		switch req.Action {
		case DisplayActionOn, DisplayActionOff, DisplayActionStatus:
		default:
			return Errorf("bad_request: action must be on, off or status")
		}
		info, err := s.Deps.Display(ctx, req.Action)
		if err != nil {
			return Errorf("%s", err.Error())
		}
		return Response{OK: true, Display: info}
	default:
		return Errorf("bad_request: unknown op %q", req.Op)
	}
}
