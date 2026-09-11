package main

// xnc register / deregister / status（spec §6.2 register 流程、§7 语义矩阵）。
// 本机注册态一律经 agent 控制管道 \\.\pipe\xnc-agentctl（§6.3）驱动——CLI
// 不读机器级文件；用户 JWT 只在 register 请求内内存传递（§5.1）。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"xnc/proto"
)

// ---- agentctl 管道客户端（wire 镜像，不 import agent 模块） ----

// agentctlPipeName 固定管道名（与 agent/agentctl.PipeName 一致）。
const agentctlPipeName = `\\.\pipe\xnc-agentctl`

// 协议 op 与 status 取值（镜像 agent/agentctl 常量）。
const (
	agentctlOpRegister   = "register"
	agentctlOpDeregister = "deregister"
	agentctlOpStatus     = "status"
	agentctlOpUpgrade    = "upgrade"

	agentctlStateUnregistered = "unregistered"
	agentctlStateRegistered   = "registered"
	agentctlStateOnline       = "online"
)

// agentctlReq / agentctlResp 镜像 agent/agentctl 的 Request/Response 字段
// tag（单行 JSON 一问一答；多余字段忽略，前向兼容）。两处必须同步修改。
type agentctlReq struct {
	Op        string `json:"op"`
	Server    string `json:"server,omitempty"`
	ClusterID string `json:"clusterId,omitempty"`
	JWT       string `json:"jwt,omitempty"`
	Channel   string `json:"channel,omitempty"` // upgrade：目标频道 stable|dev
	Action    string `json:"action,omitempty"`  // display：on | off | status
}

// agentctlUpdate 镜像 agent/agentctl.UpdateInfo（status 的在途更新进度，
// 仅更新在途时出现）。
type agentctlUpdate struct {
	Phase string `json:"phase"` // checking | applying
	From  string `json:"from,omitempty"`
	To    string `json:"to,omitempty"`
}

// agentctlDisplay 镜像 agent/agentctl.DisplayInfo（display op 状态载荷）。
type agentctlDisplay struct {
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

type agentctlResp struct {
	OK        bool             `json:"ok"`
	NodeID    string           `json:"nodeId,omitempty"`
	State     string           `json:"state,omitempty"`
	Server    string           `json:"server,omitempty"`
	ClusterID string           `json:"clusterId,omitempty"`
	Channel   string           `json:"channel,omitempty"`
	Version   string           `json:"version,omitempty"`
	Triggered bool             `json:"triggered"`         // upgrade：false = 已在途（非错误，见 note）
	Note      string           `json:"note,omitempty"`    // upgrade 在途说明
	Update    *agentctlUpdate  `json:"update,omitempty"`  // status：在途更新进度
	Display   *agentctlDisplay `json:"display,omitempty"` // display：状态载荷
	Error     string           `json:"error,omitempty"`
}

// agentctlDial 是拨号缝隙（测试换 net.Pipe 假服务端；生产 = winio）。
var agentctlDial = func(ctx context.Context, pipe string) (net.Conn, error) {
	return dialAgentCtlPipe(ctx, pipe)
}

const agentctlMaxLine = 1 << 20 // 应答行上限（agent 侧应答远小于此）

// agentctlRoundTrip 连接控制管道完成一次单行 JSON 请求/应答（每连接一问
// 一答后关闭）；ctx 控制拨号与收发时限。
func agentctlRoundTrip(ctx context.Context, req agentctlReq) (agentctlResp, error) {
	conn, err := agentctlDial(ctx, agentctlPipeName)
	if err != nil {
		return agentctlResp{}, err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	b, err := json.Marshal(req)
	if err != nil {
		return agentctlResp{}, err
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return agentctlResp{}, fmt.Errorf("write request: %w", err)
	}
	line, err := bufio.NewReader(io.LimitReader(conn, agentctlMaxLine)).ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return agentctlResp{}, fmt.Errorf("read response: %w", err)
	}
	var resp agentctlResp
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &resp); err != nil {
		return agentctlResp{}, fmt.Errorf("decode response: %w", err)
	}
	return resp, nil
}

// agentctlCall 以默认时限完成一次管道往返（register 含 agent 内部 HTTP
// 往返，给足 opTimeout 同量级的余量）。
func agentctlCall(req agentctlReq) (agentctlResp, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 65*time.Second)
	defer cancel()
	return agentctlRoundTrip(ctx, req)
}

// pipeAPIError 把管道错误串 "<code>: <message>" 还原为 APIError——code 保持
// 管道侧原样（本地码小写如 forbidden，服务端透传码大写如
// MACHINE_ID_CONFLICT），Error() 文本与管道输出逐字一致；无码前缀按
// INTERNAL 处理。
func pipeAPIError(pipeErr string) *proto.APIError {
	if code, msg, ok := strings.Cut(pipeErr, ": "); ok {
		return proto.Err(0, code, msg)
	}
	return proto.Err(0, proto.CodeInternal, pipeErr)
}

// pipeExitCode 管道错误码 → 进程退出码：小写本地码归一大写后走 ExitCode
// 映射（forbidden→241、MACHINE_ID_CONFLICT→244 等），未知码落 INTERNAL。
func pipeExitCode(pipeErr string) int {
	code, _, ok := strings.Cut(pipeErr, ": ")
	if !ok {
		return exitInternal
	}
	return ExitCode(proto.Err(0, strings.ToUpper(code), ""))
}

// errPipeUnreachable 统一管道拨号失败的报错（NETWORK → exit 245）。
func errPipeUnreachable(err error) *proto.APIError {
	return proto.Err(0, "NETWORK", fmt.Sprintf(
		"agent service not reachable: %v (is XNC installed and the XNCAgent service running?)", err))
}

// failPipeOp 原样呈现管道 op 错误（错误串逐字透传）；forbidden 补提权
// 控制台提示（§7 反注册要求 admin），MACHINE_ID_CONFLICT 补 --force 提示
// （§6.4 409）。退出码按归一化错误码映射。
func failPipeOp(cmd *cobra.Command, resp agentctlResp) error {
	switch strings.ToUpper(pipeAPIError(resp.Error).Code) {
	case proto.CodeForbidden:
		fmt.Fprintln(os.Stderr, "deregister requires an administrator console - re-run from an elevated terminal")
	case proto.CodeMachineIDConflict:
		fmt.Fprintln(os.Stderr, "this machine is registered to another cluster - re-run with --force to rebind it")
	}
	if jsonOut(cmd) {
		PrintJSON(false, nil, pipeAPIError(resp.Error))
	} else {
		fmt.Fprintln(os.Stderr, "xnc: "+resp.Error)
	}
	return &exitError{code: pipeExitCode(resp.Error), msg: resp.Error}
}

// ---- register ----

// clusterRef 是 GET /api/clusters 的条目（与 cluster list 同形）。
type clusterRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// registerPollInterval / registerPollTimeout：注册后轮询节点 online 的节奏
// （spec §6.2 第 4 步 ≤10s；测试注入缩短）。
var (
	registerPollInterval = 500 * time.Millisecond
	registerPollTimeout  = 10 * time.Second
)

func newRegisterCmd() *cobra.Command {
	var email string
	var yes, force bool
	cmd := &cobra.Command{
		Use:   "register",
		Short: "Register this machine as a node (login if needed, pick a cluster)",
		Long: `Bind this machine to an XNC cluster via the local agent service.

Logs in first if no session token is configured (interactive email+password,
or --email with the password on stdin), lists the clusters visible to the
account, then drives the agent service over its control pipe to enroll this
machine. Polls until the node is online and prints the panel URL.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, _ := LoadConfig()
			server, token := resolveServer(cmd, cfg), resolveToken(cmd, cfg)

			// 第 1 步：确保用户 JWT（§6.2）。
			if token == "" {
				s, t, err := registerLogin(cmd, cfg, server, email)
				if err != nil {
					return err
				}
				server, token = s, t
			} else {
				if cfg.RememberedEmail != "" {
					outf(cmd, "using session %s\n", cfg.RememberedEmail)
				}
			}
			cl := NewClient(server, token)

			// 第 2 步：管道预检——已注册且未 --force 时提示当前绑定（§6.2）。
			st, err := agentctlCall(agentctlReq{Op: agentctlOpStatus})
			if err != nil {
				return failAPI(cmd, errPipeUnreachable(err))
			}
			if !st.OK {
				return failPipeOp(cmd, st)
			}
			if (st.State == agentctlStateRegistered || st.State == agentctlStateOnline) && !force {
				return failAPI(cmd, proto.Err(409, proto.CodeNodeAlreadyEnrolled,
					fmt.Sprintf("machine already registered to cluster %s (node %s) at %s - re-run with --force to re-register",
						st.ClusterID, st.NodeID, st.Server)))
			}

			// 第 3 步：列出并选择 cluster。
			var clusters []clusterRef
			if e := cl.Do("GET", "/api/clusters", nil, &clusters); e != nil {
				return failAPI(cmd, e)
			}
			var cluster clusterRef
			switch len(clusters) {
			case 0:
				return failAPI(cmd, proto.Err(404, proto.CodeClusterNotFound,
					"no clusters visible to this account - ask an admin to add you to a cluster first"))
			case 1:
				cluster = clusters[0]
				outf(cmd, "cluster: %s (%s)\n", cluster.Name, cluster.ID)
				if !yes && !confirmRegister(cmd, cluster) {
					return failUsage(cmd, "aborted")
				}
			default:
				cluster, err = selectCluster(cmd, clusters)
				if err != nil {
					return err
				}
			}

			// 第 4 步：--force 先反注册（忽略 not_registered），再注册。
			if force {
				resp, err := agentctlCall(agentctlReq{Op: agentctlOpDeregister})
				if err != nil {
					return failAPI(cmd, errPipeUnreachable(err))
				}
				if !resp.OK && !strings.HasPrefix(resp.Error, "not_registered") {
					return failPipeOp(cmd, resp)
				}
				if resp.OK {
					outf(cmd, "deregistered previous binding\n")
				}
			}
			resp, err := agentctlCall(agentctlReq{
				Op: agentctlOpRegister, Server: server, ClusterID: cluster.ID, JWT: token})
			if err != nil {
				return failAPI(cmd, errPipeUnreachable(err))
			}
			if !resp.OK {
				return failPipeOp(cmd, resp)
			}

			// 第 5 步：轮询节点 online 后打印节点与面板入口。
			node, apiErr, online := pollNodeOnline(cl, resp.NodeID)
			if apiErr != nil {
				return failAPI(cmd, apiErr)
			}
			if jsonOut(cmd) {
				PrintJSON(true, map[string]any{
					"nodeId": resp.NodeID, "name": node.Name,
					"cluster": cluster.Name, "clusterId": cluster.ID,
					"server": server, "online": online}, nil)
				return nil
			}
			outf(cmd, "registered %s (node %s) to cluster %s (%s)\n",
				node.Name, resp.NodeID, cluster.Name, cluster.ID)
			if !online {
				outf(cmd, "warning: node not online yet - the agent may still be connecting\n")
			}
			outf(cmd, "panel: %s\n", server)
			return nil
		},
	}
	cmd.Flags().StringVar(&email, "email", "", "account email for the inline login")
	cmd.Flags().BoolVar(&yes, "yes", false, "skip the single-cluster confirmation prompt")
	cmd.Flags().BoolVar(&force, "force", false, "deregister first, then register (rebind; requires admin)")
	addJSONFlag(cmd)
	return cmd
}

// registerLogin 是 register 内联的会话获取：镜像 login 命令的两种形态
// （TTY 交互流；非交互严格 flag + stdin 一行密码），成功后按现行约定存
// config。密码经共享 stdin 行读取器（后续 cluster 选择仍能读到下一行）。
func registerLogin(cmd *cobra.Command, cfg Config, server, email string) (string, string, error) {
	var token string
	var user userDTO
	if term.IsTerminal(int(os.Stdin.Fd())) {
		deps := loginDeps{
			promptEmail:    func() string { return promptLine("Email", cfg.RememberedEmail) },
			promptPassword: promptPassword,
			doLogin:        doLogin,
		}
		s, t, u, err := runLoginFlow(deps, server, email)
		if err != nil {
			return "", "", loginFlowError(cmd, err)
		}
		server, token, user = s, t, u
	} else {
		if email == "" {
			return "", "", failUsage(cmd, "--email required in non-interactive mode")
		}
		password := stdinLine()
		if password == "" {
			return "", "", failUsage(cmd, "password expected on stdin")
		}
		t, u, apiErr := doLogin(server, email, password)
		if apiErr != nil {
			return "", "", failAPI(cmd, apiErr)
		}
		token, user = t, u
	}
	if err := SaveConfig(Config{Server: server, Token: token,
		RememberedEmail: user.Email, Channel: savedChannel()}); err != nil {
		return "", "", failAPI(cmd, proto.Err(0, proto.CodeInternal, "save config: "+err.Error()))
	}
	outf(cmd, "logged in as %s\n", user.Email)
	return server, token, nil
}

// confirmRegister 单 cluster 确认（空输入/EOF 默认继续；§6.2「仅一个时
// 确认即过」）。
func confirmRegister(cmd *cobra.Command, cluster clusterRef) bool {
	outf(cmd, "register this machine to cluster %s (%s)? [Y/n]: ", cluster.Name, cluster.ID)
	switch strings.ToLower(stdinLine()) {
	case "", "y", "yes":
		return true
	default:
		return false
	}
}

// selectCluster 多 cluster 编号选择：非法输入重试，EOF/空输入中止。
func selectCluster(cmd *cobra.Command, clusters []clusterRef) (clusterRef, error) {
	for i, c := range clusters {
		outf(cmd, "%d) %s (%s)\n", i+1, c.Name, c.ID)
	}
	for {
		outf(cmd, "select cluster [1-%d]: ", len(clusters))
		line := stdinLine()
		if line == "" {
			return clusterRef{}, failUsage(cmd, "cluster selection aborted")
		}
		n, err := strconv.Atoi(line)
		if err == nil && n >= 1 && n <= len(clusters) {
			return clusters[n-1], nil
		}
		outf(cmd, "xnc: invalid choice %q\n", line)
	}
}

// pollNodeOnline 轮询 GET /api/nodes/{id} 至 status online（§6.2 第 4 步）；
// API 错误视为瞬态重试至时限，超时后返回最后状态（注册已成功，不据此失败）。
func pollNodeOnline(cl *Client, nodeID string) (nodeDTO, *proto.APIError, bool) {
	deadline := time.Now().Add(registerPollTimeout)
	for {
		var n nodeDTO
		e := cl.Do("GET", "/api/nodes/"+url.PathEscape(nodeID), nil, &n)
		if e == nil && n.Status == "online" {
			return n, nil, true
		}
		if time.Now().After(deadline) {
			if e != nil {
				return nodeDTO{}, e, false
			}
			return n, nil, false
		}
		time.Sleep(registerPollInterval)
	}
}

// ---- deregister ----

func newDeregisterCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deregister",
		Short: "Remove this machine's node registration (requires an admin console)",
		Long: `Unregister this machine via the local agent service.

The agent disconnects from the server, deletes the node server-side and drops
the machine binding (the node identity key is kept for re-registration). The
control pipe requires the caller to be an administrator; run this from an
elevated console.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			resp, err := agentctlCall(agentctlReq{Op: agentctlOpDeregister})
			if err != nil {
				return failAPI(cmd, errPipeUnreachable(err))
			}
			if !resp.OK {
				return failPipeOp(cmd, resp)
			}
			if jsonOut(cmd) {
				PrintJSON(true, map[string]any{"deregistered": true}, nil)
				return nil
			}
			fmt.Println("deregistered")
			return nil
		},
	}
	addJSONFlag(cmd)
	return cmd
}

// ---- status（本机块 + 会话块，§7） ----

func newStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show local registration state and the current user session",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// 本机块：管道 status（短时限；不可达是提示而非错误——已安装
			// 未注册/未安装的机器仍要能看到会话块，§6.1）。
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			local := map[string]any{"reachable": false}
			var pairs [][2]string
			resp, err := agentctlRoundTrip(ctx, agentctlReq{Op: agentctlOpStatus})
			switch {
			case err != nil:
				pairs = [][2]string{
					{"state", "agent service not reachable"},
					{"hint", "is XNC installed and the XNCAgent service running?"},
				}
			case !resp.OK:
				return failPipeOp(cmd, resp)
			default:
				local["reachable"] = true
				local["state"] = resp.State
				pairs = append(pairs, [2]string{"state", resp.State})
				for _, p := range [][2]string{
					{"nodeId", resp.NodeID}, {"clusterId", resp.ClusterID},
					{"server", resp.Server}, {"channel", resp.Channel},
					{"version", resp.Version},
				} {
					if p[1] != "" {
						local[p[0]] = p[1]
						pairs = append(pairs, p)
					}
				}
				if resp.Update != nil {
					u := map[string]any{"phase": resp.Update.Phase}
					line := resp.Update.Phase
					if resp.Update.From != "" || resp.Update.To != "" {
						u["from"], u["to"] = resp.Update.From, resp.Update.To
						line = resp.Update.Phase + " " + resp.Update.From + " -> " + resp.Update.To
					}
					local["update"] = u
					pairs = append(pairs, [2]string{"update", line})
				}
			}

			// 会话块：当前 config（flag/env 合并前的文件+env 视图）。
			cfg, _ := LoadConfig()
			session := map[string]any{
				"server": cfg.Server, "email": cfg.RememberedEmail,
				"token_present": cfg.Token != "",
			}
			if jsonOut(cmd) {
				PrintJSON(true, map[string]any{"local": local, "session": session}, nil)
				return nil
			}
			fmt.Println("local:")
			printKV(pairs)
			fmt.Println("session:")
			printKV([][2]string{
				{"server", emptyDash(cfg.Server)},
				{"email", emptyDash(cfg.RememberedEmail)},
				{"token", map[bool]string{true: "present", false: "none"}[cfg.Token != ""]},
			})
			return nil
		},
	}
	addJSONFlag(cmd)
	return cmd
}

// emptyDash renders empty strings as "-" for the session table.
func emptyDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---- login 尾行提示（§10） ----

// printRegisterHint 经管道查注册态，unregistered 则提示 register；管道不可
// 达静默（login 不因 agent 缺席而失败/加行噪声以外的副作用）。
func printRegisterHint(cmd *cobra.Command) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := agentctlRoundTrip(ctx, agentctlReq{Op: agentctlOpStatus})
	if err != nil || !resp.OK || resp.State != agentctlStateUnregistered {
		return
	}
	outf(cmd, "node not registered - run: xnc register\n")
}

// ---- stdin 行读取（同一次命令运行内共享缓冲，跨 prompt 不丢行） ----

var (
	stdinReader     *bufio.Reader
	stdinReaderFile *os.File
)

// stdinLine 读取一行 stdin（TrimSpace）；os.Stdin 变化（测试替换）时重建
// 共享 reader。EOF 返回空串。
func stdinLine() string {
	if stdinReader == nil || stdinReaderFile != os.Stdin {
		stdinReader, stdinReaderFile = bufio.NewReader(os.Stdin), os.Stdin
	}
	line, _ := stdinReader.ReadString('\n')
	return strings.TrimSpace(line)
}
