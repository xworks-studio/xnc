package main

// xnc upgrade（spec §10）：经 agentctl 管道触发 agent 立即检查并静默应用
// 更新，随后轮询 status 显示进度（checking → applying → 新版本）。假管道
// 注入脚本化应答序列。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptPipe 按序消费脚本化应答（dial 次数超出脚本长度后重复最后一条）；
// failDialAt 在指定 0 基序号注入一次拨号失败（安装器执行期服务重启的瞬时
// 不可达回归）。
type scriptPipe struct {
	respondFn  func(req agentctlReq) agentctlResp
	script     []agentctlResp
	n          atomic.Int64
	failDialAt int64 // -1 = 从不
	mu         sync.Mutex
	reqs       []agentctlReq
}

func (s *scriptPipe) record(req agentctlReq) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reqs = append(s.reqs, req)
}

func (s *scriptPipe) ops() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ops := make([]string, len(s.reqs))
	for i, r := range s.reqs {
		ops[i] = r.Op
	}
	return ops
}

func (s *scriptPipe) dial(ctx context.Context, pipe string) (net.Conn, error) {
	i := s.n.Add(1) - 1
	if s.failDialAt == i {
		return nil, errors.New("pipe is being closed")
	}
	resp := s.script[len(s.script)-1]
	if i < int64(len(s.script)) {
		resp = s.script[i]
	}
	c1, c2 := net.Pipe()
	go func() {
		defer c2.Close()
		line, err := bufio.NewReader(c2).ReadString('\n')
		if err != nil {
			return
		}
		var req agentctlReq
		if json.Unmarshal([]byte(line), &req) != nil {
			return
		}
		s.record(req)
		b, err := json.Marshal(resp)
		if err != nil {
			return
		}
		_, _ = c2.Write(append(b, '\n'))
	}()
	return c1, nil
}

func (s *scriptPipe) install(t *testing.T) {
	t.Helper()
	old := agentctlDial
	agentctlDial = s.dial
	t.Cleanup(func() { agentctlDial = old })
}

func newScriptPipe(t *testing.T, script ...agentctlResp) *scriptPipe {
	t.Helper()
	s := &scriptPipe{script: script, failDialAt: -1}
	s.install(t)
	return s
}

func shortenUpgradePolling(t *testing.T) {
	t.Helper()
	oldI, oldT := upgradePollInterval, upgradePollTimeout
	upgradePollInterval, upgradePollTimeout = 5*time.Millisecond, 2*time.Second
	t.Cleanup(func() { upgradePollInterval, upgradePollTimeout = oldI, oldT })
}

func st(v, state, channel string, upd *agentctlUpdate) agentctlResp {
	return agentctlResp{OK: true, State: state, Version: v, Channel: channel, Update: upd}
}

// 全链路：基线 status（0.6.1/stable）→ upgrade 触发 → checking → applying
// （0.6.1→0.6.2）→ 新版本上线。打印状态迁移，收尾打印当前版本，exit 0。
func TestUpgradeHappyPath(t *testing.T) {
	isolatedHome(t)
	shortenUpgradePolling(t)
	pipe := newScriptPipe(t,
		st("0.6.1", "online", "stable", nil),
		agentctlResp{OK: true, Triggered: true},
		st("0.6.1", "online", "dev", &agentctlUpdate{Phase: "checking"}),
		st("0.6.1", "online", "dev", &agentctlUpdate{Phase: "applying", From: "0.6.1", To: "0.6.2"}),
		st("0.6.2", "online", "dev", nil),
	)

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"upgrade", "--channel", "dev"})
	})
	require.Equal(t, 0, code)
	assert.Contains(t, out, "checking")
	assert.Contains(t, out, "applying")
	assert.Contains(t, out, "0.6.1")
	assert.Contains(t, out, "0.6.2")
	assert.Contains(t, out, "updated")
	assert.Contains(t, out, "online")

	// wire：upgrade 请求携带 channel；触发前先取基线 status。
	require.Len(t, pipe.reqs, 5)
	assert.Equal(t, []string{"status", "upgrade", "status", "status", "status"}, pipe.ops())
	assert.Equal(t, "upgrade", pipe.reqs[1].Op)
	assert.Equal(t, "dev", pipe.reqs[1].Channel)
}

// 不带 --channel：upgrade 请求省略 channel 字段（当前频道即时检查）。
func TestUpgradeNoChannelFlag(t *testing.T) {
	isolatedHome(t)
	shortenUpgradePolling(t)
	pipe := newScriptPipe(t,
		st("0.6.1", "online", "stable", nil),
		agentctlResp{OK: true, Triggered: true},
		st("0.6.1", "online", "stable", &agentctlUpdate{Phase: "checking"}),
		st("0.6.2", "online", "stable", nil),
	)
	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"upgrade"})
	})
	require.Equal(t, 0, code)
	assert.Contains(t, out, "0.6.2")
	for _, r := range pipe.reqs {
		if r.Op == "upgrade" {
			assert.Empty(t, r.Channel)
		}
	}
}

// 非法 channel：本地拒绝（usage），不触管道。
func TestUpgradeInvalidChannel(t *testing.T) {
	isolatedHome(t)
	shortenUpgradePolling(t)
	pipe := newScriptPipe(t, agentctlResp{OK: true})

	_, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"upgrade", "--channel", "beta"})
	})
	require.Equal(t, exitUsage, code)
	assert.Empty(t, pipe.ops())
}

// 在途应答（triggered=false + note）：渲染 note 后照常轮询到完成。
func TestUpgradeNoteWhenAlreadyInProgress(t *testing.T) {
	isolatedHome(t)
	shortenUpgradePolling(t)
	newScriptPipe(t,
		st("0.6.1", "online", "stable", nil),
		agentctlResp{OK: true, Triggered: false, Note: "update already in progress"},
		st("0.6.1", "online", "stable", &agentctlUpdate{Phase: "applying", From: "0.6.1", To: "0.6.2"}),
		st("0.6.2", "online", "stable", nil),
	)

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"upgrade"})
	})
	require.Equal(t, 0, code)
	assert.Contains(t, out, "update already in progress")
	assert.Contains(t, out, "0.6.2")
}

// 在途应答 + --channel：agent 在触碰频道绑定前即拒绝（pending/在途先于
// 切换），频道实际未变——不得打印 switching channel，只渲染 note。
func TestUpgradeNoteWhenAlreadyInProgressWithChannel(t *testing.T) {
	isolatedHome(t)
	shortenUpgradePolling(t)
	newScriptPipe(t,
		st("0.6.1", "online", "stable", nil),
		agentctlResp{OK: true, Triggered: false, Note: "update already in progress"},
		st("0.6.1", "online", "stable", &agentctlUpdate{Phase: "applying", From: "0.6.1", To: "0.6.2"}),
		st("0.6.2", "online", "stable", nil),
	)

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"upgrade", "--channel", "dev"})
	})
	require.Equal(t, 0, code)
	assert.Contains(t, out, "update already in progress")
	assert.NotContains(t, out, "switching channel")
	assert.Contains(t, out, "0.6.2")
}

// 检查完成但无更新可应用（update 字段消失、版本未变）：报 up to date，
// exit 0。
func TestUpgradeAlreadyUpToDate(t *testing.T) {
	isolatedHome(t)
	shortenUpgradePolling(t)
	newScriptPipe(t,
		st("0.6.2", "online", "stable", nil),
		agentctlResp{OK: true, Triggered: true},
		st("0.6.2", "online", "stable", &agentctlUpdate{Phase: "checking"}),
		st("0.6.2", "online", "stable", nil),
	)

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"upgrade"})
	})
	require.Equal(t, 0, code)
	assert.Contains(t, out, "up to date")
	assert.Contains(t, out, "0.6.2")
}

// 管道不可达：exit 245 + agent 服务提示。
func TestUpgradePipeUnreachable(t *testing.T) {
	isolatedHome(t)
	unreachablePipe(t)

	_, code := captureStderr(t, func() int {
		return runCLI(t.Context(), []string{"upgrade"})
	})
	require.Equal(t, exitNet, code)
}

// 管道 op 错误（未注册）：错误串透传，exit 250（未知码归 INTERNAL）。
func TestUpgradePipeError(t *testing.T) {
	isolatedHome(t)
	shortenUpgradePolling(t)
	newScriptPipe(t,
		st("0.6.1", "unregistered", "", nil),
		agentctlResp{OK: false, Error: "not_registered: no binding"},
	)

	stderr, code := captureStderr(t, func() int {
		return runCLI(t.Context(), []string{"upgrade"})
	})
	require.Equal(t, exitInternal, code)
	assert.Contains(t, stderr, "not_registered: no binding")
}

// 轮询期瞬时管道失败（安装器停服务窗口）：容忍并在恢复后继续。
func TestUpgradeToleratesTransientPollErrors(t *testing.T) {
	isolatedHome(t)
	shortenUpgradePolling(t)
	s := newScriptPipe(t,
		st("0.6.1", "online", "stable", nil),
		agentctlResp{OK: true, Triggered: true},
		st("0.6.1", "online", "stable", &agentctlUpdate{Phase: "checking"}),
		st("0.6.1", "registered", "stable", &agentctlUpdate{Phase: "applying", From: "0.6.1", To: "0.6.2"}),
		st("0.6.2", "online", "stable", nil),
	)
	s.failDialAt = 3 // applying 阶段的一次拨号失败

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"upgrade"})
	})
	require.Equal(t, 0, code)
	assert.Contains(t, out, "0.6.2")
}

// 轮询超时（更新一直在 applying）：告警不失败（触发已成功），exit 0。
func TestUpgradeTimeoutWarns(t *testing.T) {
	isolatedHome(t)
	oldI, oldT := upgradePollInterval, upgradePollTimeout
	upgradePollInterval, upgradePollTimeout = 5*time.Millisecond, 30*time.Millisecond
	t.Cleanup(func() { upgradePollInterval, upgradePollTimeout = oldI, oldT })

	newScriptPipe(t,
		st("0.6.1", "online", "stable", nil),
		agentctlResp{OK: true, Triggered: true},
		st("0.6.1", "online", "stable", &agentctlUpdate{Phase: "applying", From: "0.6.1", To: "0.6.2"}),
	)

	out, code := captureStdout(t, func() int {
		return runCLI(t.Context(), []string{"upgrade"})
	})
	require.Equal(t, 0, code)
	assert.Contains(t, out, "still in progress")
}

// --json：stdout 单行 envelope；进度行走 stderr。
func TestUpgradeJSONEnvelope(t *testing.T) {
	isolatedHome(t)
	shortenUpgradePolling(t)
	newScriptPipe(t,
		st("0.6.1", "online", "stable", nil),
		agentctlResp{OK: true, Triggered: true},
		st("0.6.1", "online", "dev", &agentctlUpdate{Phase: "applying", From: "0.6.1", To: "0.6.2"}),
		st("0.6.2", "online", "dev", nil),
	)

	stdout, stderr, code := runCLICaptureBoth(t, func() int {
		return runCLI(t.Context(), []string{"upgrade", "--channel", "dev", "--json"})
	})
	require.Equal(t, 0, code)
	assert.Contains(t, stderr, "applying", "progress lines go to stderr in json mode")
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Triggered bool   `json:"triggered"`
			Updated   bool   `json:"updated"`
			Version   string `json:"version"`
			Channel   string `json:"channel"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &env))
	assert.True(t, env.OK)
	assert.True(t, env.Data.Triggered)
	assert.True(t, env.Data.Updated)
	assert.Equal(t, "0.6.2", env.Data.Version)
	assert.Equal(t, "dev", env.Data.Channel)
}
