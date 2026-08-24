package api

// TURN 池单测：解析 / 轮询分配 / 健康剔除与恢复 / 回落 / 探测 goroutine
// （假探测函数注入，不发真实网络；stunProbe 本身用回环 UDP 应答器测试）。

import (
	"encoding/binary"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"xnc/server/internal/config"
)

// TestParseTurnServer：裸 IP 缺省 3478、ip:port 显式、IPv6、坏项跳过。
func TestParseTurnServer(t *testing.T) {
	cases := []struct {
		entry string
		want  turnServer
		ok    bool
	}{
		{"1.2.3.4", turnServer{IP: "1.2.3.4", Port: 3478, Healthy: true}, true},
		{" 5.6.7.8:3479 ", turnServer{IP: "5.6.7.8", Port: 3479, Healthy: true}, true},
		{"fe80::1", turnServer{IP: "fe80::1", Port: 3478, Healthy: true}, true},
		{"[::1]:5349", turnServer{IP: "::1", Port: 5349, Healthy: true}, true},
		{"", turnServer{}, false},
		{"  ", turnServer{}, false},
		{"not-an-ip", turnServer{}, false},
		{"turn.example.com", turnServer{}, false}, // 纯 IP 池；hostname 跳过
		{"999.1.1.1", turnServer{}, false},
		{"1.2.3.4:0", turnServer{}, false},
		{"1.2.3.4:70000", turnServer{}, false},
		{"1.2.3.4:notaport", turnServer{}, false},
	}
	for _, c := range cases {
		got, ok := parseTurnServer(c.entry)
		assert.Equal(t, c.ok, ok, "entry %q", c.entry)
		if c.ok {
			assert.Equal(t, c.want, got, "entry %q", c.entry)
		}
	}
}

// TestNewTurnPoolManagerParsing：坏项被跳过，剩余按序入池；URL 为 udp 优先 +
// tcp 兜底；IPv6 加方括号。
func TestNewTurnPoolManagerParsing(t *testing.T) {
	m := NewTurnPoolManager([]string{"1.2.3.4", "5.6.7.8:3479", "bad", "999.1.1.1", "fe80::1"}, "u", "p")
	require.Len(t, m.servers, 3)
	cfg, ok := m.Allocate()
	require.True(t, ok)
	assert.Equal(t, "u", cfg.Username)
	assert.Equal(t, "p", cfg.Credential)
	assert.Equal(t, []string{
		"turn:1.2.3.4:3478?transport=udp",
		"turn:1.2.3.4:3478?transport=tcp",
	}, cfg.URLs)
	assert.Equal(t, "turn:5.6.7.8:3479?transport=tcp", m.servers[1].turnURLs()[1])
	assert.Equal(t, "turn:[fe80::1]:3478?transport=udp", m.servers[2].turnURLs()[0])
}

// TestAllocateRoundRobin：未探测前全 healthy，2 台交替分配。
func TestAllocateRoundRobin(t *testing.T) {
	m := NewTurnPoolManager([]string{"10.0.0.1", "10.0.0.2"}, "u", "p")
	seen := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		cfg, ok := m.Allocate()
		require.True(t, ok)
		seen = append(seen, cfg.URLs[0])
	}
	assert.Equal(t, []string{
		"turn:10.0.0.1:3478?transport=udp",
		"turn:10.0.0.2:3478?transport=udp",
		"turn:10.0.0.1:3478?transport=udp",
		"turn:10.0.0.2:3478?transport=udp",
	}, seen)
	// rr 游标已推进：下一台从 10.0.0.1 起（状态未变）
	cfg, _ := m.Allocate()
	assert.Equal(t, "turn:10.0.0.1:3478?transport=udp", cfg.URLs[0])
}

// fakeProbe 注入用假探测：按 addr 决定成败；mu 保护可切换结果。
type fakeProbe struct {
	mu    sync.Mutex
	fails map[string]bool
}

func (f *fakeProbe) probe(addr string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return !f.fails[addr]
}

func (f *fakeProbe) setFails(addr string, fails bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fails[addr] = fails
}

func newFakeProbe() *fakeProbe { return &fakeProbe{fails: map[string]bool{}} }

// TestHealthExclusionAndRecovery：假探测经后台 goroutine 驱动——1 台失败×2 →
// 剔除，分配只回健康台；探测恢复 → 1 次成功重新纳入。
func TestHealthExclusionAndRecovery(t *testing.T) {
	m := NewTurnPoolManager([]string{"10.0.0.1", "10.0.0.2"}, "u", "p")
	fp := newFakeProbe()
	m.probe = fp.probe
	m.interval = 10 * time.Millisecond
	fp.setFails("10.0.0.1:3478", true) // A 台恒失败

	m.Start()
	t.Cleanup(m.Stop)

	// 连续 2 次失败后 A 被剔除：分配只回 10.0.0.2
	require.Eventually(t, func() bool {
		cfg, ok := m.Allocate()
		return ok && cfg.URLs[0] == "turn:10.0.0.2:3478?transport=udp"
	}, 3*time.Second, 20*time.Millisecond)
	assert.False(t, m.servers[0].Healthy)
	assert.True(t, m.servers[1].Healthy)

	// 探测恢复 → 1 次成功即重新纳入
	fp.setFails("10.0.0.1:3478", false)
	require.Eventually(t, func() bool {
		m.mu.Lock()
		h := m.servers[0].Healthy
		m.mu.Unlock()
		return h
	}, 3*time.Second, 20*time.Millisecond)
	cfg, ok := m.Allocate()
	require.True(t, ok)
	assert.True(t, cfg.URLs[0] == "turn:10.0.0.1:3478?transport=udp" ||
		cfg.URLs[0] == "turn:10.0.0.2:3478?transport=udp")
}

// TestAllocateFallback：池空 / 全不健康 → false（调用方回落旧路径）。
func TestAllocateFallback(t *testing.T) {
	// 空池（含全坏项）
	m := NewTurnPoolManager(nil, "u", "p")
	_, ok := m.Allocate()
	assert.False(t, ok)
	m2 := NewTurnPoolManager([]string{"bad", "1.2.3.4:0"}, "u", "p")
	require.Empty(t, m2.servers)
	_, ok = m2.Allocate()
	assert.False(t, ok)

	// 全不健康（假探测恒失败）
	m3 := NewTurnPoolManager([]string{"10.0.0.1", "10.0.0.2"}, "u", "p")
	fp := newFakeProbe()
	fp.setFails("10.0.0.1:3478", true)
	fp.setFails("10.0.0.2:3478", true)
	m3.probe = fp.probe
	m3.interval = 10 * time.Millisecond
	m3.Start()
	t.Cleanup(m3.Stop)
	require.Eventually(t, func() bool {
		_, ok := m3.Allocate()
		return !ok
	}, 3*time.Second, 20*time.Millisecond)
}

// TestStunProbe：真实 stunProbe 对回环 UDP 应答器成功；对未监听端口失败。
func TestStunProbe(t *testing.T) {
	// 回环应答器：收到 20B STUN binding request，原样回带 transaction id 的
	// binding success response（20B 头 + XOR-MAPPED-ADDRESS 8B = 28B）。
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	defer pc.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 64)
		for {
			_, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			resp := make([]byte, 28)
			binary.BigEndian.PutUint16(resp[0:2], 0x0101) // binding success
			binary.BigEndian.PutUint16(resp[2:4], 8)      // XOR-MAPPED-ADDRESS
			binary.BigEndian.PutUint32(resp[4:8], stunMagicCookie)
			copy(resp[8:20], buf[8:20]) // 复用请求 transaction id
			resp[20] = 0x00             // attr type XOR-MAPPED-ADDRESS
			resp[21] = 0x20
			resp[22], resp[23] = 0, 4 // length 4
			_, _ = pc.WriteTo(resp, addr)
			return
		}
	}()
	require.True(t, stunProbe(pc.LocalAddr().String()), "healthy echo server should pass")
	<-done

	// 未监听端口 → 失败路径（ICMP port unreachable 或超时，均判定不可达）
	free, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	closedPort := free.LocalAddr().(*net.UDPAddr).Port
	free.Close()
	assert.False(t, stunProbe(net.JoinHostPort("127.0.0.1", strconv.Itoa(closedPort))))
}

// TestStunResponseOK：非法应答（类型错/魔数错/transaction id 不符/过短）全部
// 拒绝；28B 合法应答（coturn 典型形态）通过。
func TestStunResponseOK(t *testing.T) {
	txID := make([]byte, 12)
	valid := make([]byte, 28)
	binary.BigEndian.PutUint16(valid[0:2], 0x0101)
	binary.BigEndian.PutUint16(valid[2:4], 8)
	binary.BigEndian.PutUint32(valid[4:8], stunMagicCookie)
	copy(valid[8:20], txID)
	assert.True(t, stunResponseOK(valid, txID))

	bad := func(mutate func(b []byte)) []byte {
		b := append([]byte(nil), valid...)
		mutate(b)
		return b
	}
	assert.False(t, stunResponseOK(valid[:20], txID), "过短（无属性）")
	assert.False(t, stunResponseOK(bad(func(b []byte) { binary.BigEndian.PutUint16(b[0:2], 0x0111) }), txID), "类型错")
	assert.False(t, stunResponseOK(bad(func(b []byte) { b[5] ^= 0xFF }), txID), "魔数错")
	otherTx := make([]byte, 12)
	otherTx[0] = 1
	assert.False(t, stunResponseOK(valid, otherTx), "transaction id 不符")
}

// TestTurnConfigPoolPriority：turnConfig() 池优先——池分配成功返回池内单台；
// 池空/全不健康回落到 XNC_TURN_URLS 全列表；无池走旧路径。
func TestTurnConfigPoolPriority(t *testing.T) {
	cfgURLs := []string{"turn:relay.xnc.app:3478?transport=tcp"}

	// 无池：旧路径
	h := &handlers{cfg: newTurnCfg(cfgURLs)}
	got := h.turnConfig()
	require.NotNil(t, got)
	assert.Equal(t, cfgURLs, got.URLs)

	// 池分配成功：池内单台（udp+tcp），凭据共用
	m := NewTurnPoolManager([]string{"10.0.0.1", "10.0.0.2"}, "pooluser", "poolcred")
	h = &handlers{cfg: newTurnCfg(cfgURLs), turnPool: m}
	got = h.turnConfig()
	require.NotNil(t, got)
	assert.Equal(t, []string{"turn:10.0.0.1:3478?transport=udp", "turn:10.0.0.1:3478?transport=tcp"}, got.URLs)
	assert.Equal(t, "pooluser", got.Username)
	assert.Equal(t, "poolcred", got.Credential)

	// 池全不健康：回落旧路径（全列表 + 池统一凭据仍来自 config）
	m2 := NewTurnPoolManager([]string{"10.0.0.3"}, "u", "p")
	fp := newFakeProbe()
	fp.setFails("10.0.0.3:3478", true)
	m2.probe = fp.probe
	m2.interval = 10 * time.Millisecond
	m2.Start()
	t.Cleanup(m2.Stop)
	require.Eventually(t, func() bool {
		m2.mu.Lock()
		defer m2.mu.Unlock()
		return !m2.servers[0].Healthy
	}, 3*time.Second, 20*time.Millisecond)
	h = &handlers{cfg: newTurnCfg(cfgURLs), turnPool: m2}
	got = h.turnConfig()
	require.NotNil(t, got)
	assert.Equal(t, cfgURLs, got.URLs)
}

func newTurnCfg(urls []string) config.Config {
	return config.Config{TurnURLs: urls, TurnUsername: "testuser", TurnCredential: "testcred"}
}
