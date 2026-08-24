package api

// TURN 池（境内媒体中转）管理：pool 内每台 coturn 共享 realm/凭据，任一台可
// 服务任意会话。server 从池中 round-robin 分配单台（下发 udp 优先 + tcp 兜底
// 两个 URL），浏览器/节点连第一可达的；后台 STUN binding 探测维护健康状态，
// 全池不可用/池空时由调用方回落 XNC_TURN_URLS 全列表（见 desktop_handlers.go
// turnConfig）。设计见 docs/2026-08-24-turn-pool-architecture.md。

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"xnc/proto"
)

const (
	// turnDefaultPort 池项缺省 TURN 端口（coturn 标准监听端口）。
	turnDefaultPort = 3478
	// turnProbeInterval 健康探测周期（后台 goroutine）。
	turnProbeInterval = 30 * time.Second
	// turnProbeTimeout 单次 STUN binding 超时。
	turnProbeTimeout = 3 * time.Second
	// turnFailThreshold 连续失败 ≥2 次标记 unhealthy；1 次成功即恢复。
	turnFailThreshold = 2

	// stunMagicCookie RFC 5389 魔数。
	stunMagicCookie = 0x2112A442
)

// turnServer 池内单台 TURN 的解析与健康状态。Healthy 在首次探测前视为 true
// （未探测的机器先试跑，由探测在 ≤30s 内校正）。
type turnServer struct {
	IP         string
	Port       int
	Healthy    bool
	lastProbe  time.Time
	failStreak int
}

// probeFunc 探测函数：addr = "ip:port"，返回是否可达。生产 = stunProbe；
// 单测注入假实现（不发网络）。
type probeFunc func(addr string) bool

// TurnPoolManager 持有池内各台状态；Allocate 与健康探测并发安全（探测在锁外
// 发网络请求，仅短持锁更新状态；分配只读快照）。
type TurnPoolManager struct {
	mu         sync.Mutex
	servers    []*turnServer
	rr         int // round-robin 游标（恒指向下一分配起点）
	username   string
	credential string

	probe    probeFunc     // 可注入（测试替身）；默认 stunProbe
	interval time.Duration // 探测周期；默认 turnProbeInterval（测试可调小）

	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewTurnPoolManager 解析 pool（每项 ip[:port]，缺省端口 3478）构造 manager。
// 坏项（非 IP/端口非法）跳过；池全坏 → 空池（Allocate 恒 false，调用方回落）。
// username/credential 为池内统一凭据（与 XNC_TURN_URLS 路径共用）。
func NewTurnPoolManager(pool []string, username, credential string) *TurnPoolManager {
	m := &TurnPoolManager{
		username:   username,
		credential: credential,
		probe:      stunProbe,
		interval:   turnProbeInterval,
	}
	for _, entry := range pool {
		if s, ok := parseTurnServer(entry); ok {
			m.servers = append(m.servers, &s)
		}
	}
	return m
}

// parseTurnServer 解析单条池项：裸 IP（或带 [] 的 IPv6）→ 缺省端口；
// ip:port → 指定端口。非 IP 或端口越界 → 跳过（ok=false）。
func parseTurnServer(entry string) (turnServer, bool) {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return turnServer{}, false
	}
	host, portStr := entry, ""
	if h, p, err := net.SplitHostPort(entry); err == nil {
		host, portStr = h, p
	}
	host = strings.Trim(host, "[]")
	if net.ParseIP(host) == nil {
		return turnServer{}, false // 纯 IP 池（免备案）；hostname/坏项跳过
	}
	port := turnDefaultPort
	if portStr != "" {
		n, err := strconv.Atoi(portStr)
		if err != nil || n < 1 || n > 65535 {
			return turnServer{}, false
		}
		port = n
	}
	return turnServer{IP: host, Port: port, Healthy: true}, true
}

// addr 返回 "ip:port"（探测与 URL 构造共用）。
func (s *turnServer) addr() string {
	return net.JoinHostPort(s.IP, strconv.Itoa(s.Port))
}

// turnURLs 构造下发 URL 列表：udp 优先 + tcp 兜底（浏览器/节点取第一个可达的；
// IPv6 地址加方括号）。凭据由 manager 统一下发。
func (s *turnServer) turnURLs() []string {
	host := s.IP
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	p := strconv.Itoa(s.Port)
	return []string{
		"turn:" + host + ":" + p + "?transport=udp",
		"turn:" + host + ":" + p + "?transport=tcp",
	}
}

// Allocate 从 healthy 池 round-robin 分配一台，返回单 TURN 配置
// （URLs = udp+tcp 两个 URL，凭据共用）。池空或全不健康 → false。
// 探测未开始前所有台按 healthy 处理（初始值）。
func (m *TurnPoolManager) Allocate() (proto.DesktopTurnConfig, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.servers)
	if n == 0 {
		return proto.DesktopTurnConfig{}, false
	}
	for i := 0; i < n; i++ {
		idx := (m.rr + i) % n
		s := m.servers[idx]
		if s.Healthy {
			m.rr = (idx + 1) % n // 下一分配从这台之后起扫
			return proto.DesktopTurnConfig{
				URLs:       s.turnURLs(),
				Username:   m.username,
				Credential: m.credential,
			}, true
		}
	}
	return proto.DesktopTurnConfig{}, false
}

// Start 启动后台健康探测 goroutine（幂等）：先立即跑一轮（启动 ≤3s 内校正
// 状态），之后每 interval 一轮。
func (m *TurnPoolManager) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopCh != nil {
		return
	}
	m.stopCh = make(chan struct{})
	m.wg.Add(1)
	go m.probeLoop(m.stopCh)
}

// Stop 停止探测 goroutine（幂等）并等待退出。
func (m *TurnPoolManager) Stop() {
	m.mu.Lock()
	if m.stopCh == nil {
		m.mu.Unlock()
		return
	}
	close(m.stopCh)
	m.stopCh = nil
	m.mu.Unlock()
	m.wg.Wait()
}

func (m *TurnPoolManager) probeLoop(stop <-chan struct{}) {
	defer m.wg.Done()
	m.probeCycle() // 启动即校正一次（探测在锁外，不阻塞分配）
	t := time.NewTicker(m.interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			m.probeCycle()
		}
	}
}

// probeCycle 对池内每台发一次探测并更新健康状态。探测在网络层串行执行
// （池通常个位数，30s 周期内余量充足）；状态更新持锁极短。
func (m *TurnPoolManager) probeCycle() {
	m.mu.Lock()
	list := make([]*turnServer, len(m.servers))
	copy(list, m.servers)
	m.mu.Unlock()

	for _, s := range list {
		ok := m.probe(s.addr())
		m.mu.Lock()
		s.lastProbe = time.Now()
		prev := s.Healthy
		if ok {
			s.failStreak = 0
			s.Healthy = true // 1 次成功即恢复
		} else {
			s.failStreak++
			if s.failStreak >= turnFailThreshold {
				s.Healthy = false // 连续 2 次失败剔除
			}
		}
		transition := s.Healthy != prev
		m.mu.Unlock()
		if transition {
			slog.Info("turn pool health changed",
				"addr", s.addr(), "healthy", s.Healthy)
		}
	}
}

// stunProbe 真实 STUN binding over UDP：构造 20B binding request（type=0x0001,
// length=0, magic cookie, 12B 随机 transaction id），3s 超时；应答为合法的
// binding success response（type=0x0101 + 魔数 + 同 transaction id + ≥28B，
// coturn 典型应答 = 20B 头 + XOR-MAPPED-ADDRESS 8B = 28B，或附 SOFTWARE 等
// 属性更长）即视为 ok。纯 Go，无新依赖。
func stunProbe(addr string) bool {
	conn, err := net.DialTimeout("udp", addr, turnProbeTimeout)
	if err != nil {
		return false
	}
	defer conn.Close()
	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:2], 0x0001) // STUN binding request
	binary.BigEndian.PutUint16(req[2:4], 0)      // 无属性
	binary.BigEndian.PutUint32(req[4:8], stunMagicCookie)
	if _, err := rand.Read(req[8:20]); err != nil {
		return false
	}
	_ = conn.SetDeadline(time.Now().Add(turnProbeTimeout))
	if _, err := conn.Write(req); err != nil {
		return false
	}
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if err != nil {
		return false
	}
	return stunResponseOK(buf[:n], req[8:20])
}

// stunResponseOK 校验应答：binding success（0x0101）+ 魔数 + 与请求一致的
// transaction id + 至少 20B 头 + 1 个属性（XOR-MAPPED-ADDRESS）。
func stunResponseOK(resp, txID []byte) bool {
	if len(resp) < 28 { // 20B 头 + ≥8B 属性
		return false
	}
	if binary.BigEndian.Uint16(resp[0:2]) != 0x0101 { // binding success response
		return false
	}
	if binary.BigEndian.Uint32(resp[4:8]) != stunMagicCookie {
		return false
	}
	if !bytes.Equal(resp[8:20], txID) {
		return false
	}
	return true
}
