package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"xnc/agent/connect"
	"xnc/agent/enroll"
	"xnc/agent/identity"
	"xnc/agent/machineinfo"
	"xnc/agent/session"
	"xnc/proto"
)

func main() {
	// E2E 捕获日志：默认 logger 指向 stdout（JSON），所有节点共用。
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	server := flag.String("server", "", "control server URL (required)")
	token := flag.String("token", "",
		"enrollment token, used only when a node has no identity yet; "+
			"with -count N the SAME token pays for N enrolls, so create it with maxUses >= N "+
			"(POST /api/clusters/{cluster}/enrollment-tokens)")
	dir := flag.String("identity-dir", filepath.Join(os.TempDir(), "xnc-mockagent"),
		"identity dir; per-node key file node-N.json")
	count := flag.Int("count", 1, "number of simulated nodes (each: own key + enroll + connection)")
	first := flag.Int("first", 0, "starting node index: nodes are numbered first..first+count-1 "+
		"(identity files, MOCK-xxx names, machine IDs) — enables multi-wave load runs sharing one identity dir")
	beat := flag.Duration("beat", 30*time.Second, "heartbeat interval")
	disc := flag.Duration("disconnect-after", 0,
		"accepted but a no-op: E2E simulates drops by killing the process")
	once := flag.Bool("once", false,
		"exit after the first successful connection (dial, auth, HELLO_ACK) per node")
	flag.Parse()
	if *server == "" {
		fmt.Fprintln(os.Stderr, "--server required")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var failed atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < *count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			idx := *first + i
			if err := runOne(ctx, *server, *token, *dir, idx, *beat, *disc, *once); err != nil {
				slog.Error("node failed", "node", idx, "err", err)
				failed.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if failed.Load() > 0 {
		os.Exit(1)
	}
}

// runOne 跑一个模拟节点：身份加载（缺失且给了 token 则 enroll）→ 控制连接。
func runOne(ctx context.Context, server, token, dir string, i int, beat, disc time.Duration, once bool) error {
	_ = disc // --disconnect-after：兼容性 no-op，断线场景由 E2E kill 进程模拟
	log := slog.With("node", i)

	path := filepath.Join(dir, fmt.Sprintf("node-%d.json", i))
	k, err := identity.Load(path)
	if err != nil || k.NodeID == "" {
		if token == "" {
			return fmt.Errorf("node %d: no identity and no token", i)
		}
		k = identity.Generate()
		info := mockInfo(i)
		if err := enroll.Enroll(ctx, server, token, k, info); err != nil {
			return err
		}
		if err := identity.Save(k, path); err != nil {
			return err
		}
		log.Info("enrolled", "nodeID", k.NodeID)
	} else {
		log.Info("identity loaded", "nodeID", k.NodeID)
	}

	c := connect.NewClient(server, k, mockInfo(i))
	c.Beat = beat
	c.Log = log
	// 每次连接就绪（含重连）重建 engine：旧 engine 的 sendControl 绑定旧连接，
	// 其 active 会话已随断连作废，重建即正确语义（与 agent.Run 相同装配）。
	c.OnReady = func(sendControl func(m proto.Message) error) {
		engine := session.NewEngine(log, sendControl)
		engine.Register(proto.KindExec, session.NewExec(log))
		engine.Register(proto.KindShell, session.NewShell(log))
		engine.Register(proto.KindFile, session.NewFile(log))
		engine.Register(proto.KindTunnel, session.NewTunnel(log))
		engine.Register(proto.KindScreen, session.NewScreenHandler(""))
		c.Handler = engine
	}

	if once {
		return c.RunOnce(ctx)
	}
	return c.Run(ctx)
}

// mockInfo 生成确定性节点环境信息：同一 i 总得到同一身份特征。
func mockInfo(i int) machineinfo.Info {
	return machineinfo.Info{
		Hostname:     fmt.Sprintf("MOCK-%03d", i),
		MachineID:    fmt.Sprintf("mock-machine-%03d", i),
		OSVersion:    "MockOS",
		AgentVersion: "0.1.0-mock",
		ShellType:    "pwsh",
	}
}
