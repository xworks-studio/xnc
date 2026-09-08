// ticket.go — RelayTicket 准入票据（relay-plane spec §3.1）。
//
// 双粒度（v0.2 评审修正）：host 张按 (node, relay) 铸造——不带 sid，同
// relay 跨会话字节等值（core StartCapture 的 cfg 等值复用判据依赖这一点，
// 同节点第二个 viewer 不得触发换血）；viewer 张按会话铸造，cap 由
// desktopStart 时的 RBAC 填入——策略演进只改 claims，relay 不发版。
//
// 形态：payloadB64url..sigB64url（ed25519 over canonical JSON）。作为不透明
// 字符串流通：host 侧装进既有 token 字段（hello{nodeId,token}），viewer
// 侧走 URL ?token=。可重放（host 崩溃重启由 core 原样重放 cfg 再注册）。
//
// 撤销 = relay 本地墓碑 + 控制连接重连对账（无全局 CRL）：单次断线挡不住
// 可重放票 + 客户端重连循环（hello 2s 周期重发），墓碑期内同键一律拒绝。
package rtv

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	ticketV1 = 1

	// TicketHost / TicketViewer 票据角色。
	TicketHost   = "host"
	TicketViewer = "viewer"

	// ticketLeeway 时钟偏差容忍（spec §3.1）：relay 以本地时钟验 exp，
	// 廉价 VPS 漂移不误拒；漂移观测（心跳携带时钟）由控制连接承担。
	ticketLeeway = 120 * time.Second

	// tombstoneHorizon 墓碑保留时长：覆盖最长票 exp（24h）+ leeway。
	tombstoneHorizon = 25 * time.Hour
)

var (
	ErrBadToken = errors.New("rtv: bad relay ticket")
	ErrExpired  = errors.New("rtv: relay ticket expired")
	ErrKilled   = errors.New("rtv: relay ticket revoked")
)

// ticketClaims 线格式（v1）。签发与验签共用本结构——Go struct 序列化
// 字段序确定，canonical 形态单点定义，杜绝两端漂移。
type ticketClaims struct {
	V   int        `json:"v"`
	Typ string     `json:"typ"`
	NID string     `json:"nid"`
	SID string     `json:"sid,omitempty"` // 仅 viewer 张
	RID string     `json:"rid"`
	Gen int        `json:"gen"`
	Cap *ticketCap `json:"cap,omitempty"` // 仅 viewer 张
	NM  string     `json:"nm,omitempty"`  // viewer 显示名（controlState 广播）
	Iat int64      `json:"iat"`
	Exp int64      `json:"exp"`
}

type ticketCap struct {
	Control bool `json:"control"`
	Input   bool `json:"input"`
}

// Ticket 验签后的可用视图。
type Ticket struct {
	Typ                        string
	NodeID, SessionID, RelayID string
	Gen                        int
	Control, Input             bool
	Name                       string
	ExpiresAt                  time.Time
}

// ---------------- 签发侧（宿主进程持私钥） ----------------

// Signer 票据签发器。HostTicketFor 内置 (node,relay) 缓存——同键跨会话
// 返回同一字符串（字节等值），这是 core 侧 host 复用（不换血）的前提。
type Signer struct {
	priv ed25519.PrivateKey

	mu        sync.Mutex
	hostCache map[string]string
}

// GenerateSigningKey 生成新的 ed25519 签名密钥（部署工具用）。
func GenerateSigningKey() (ed25519.PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	return priv, err
}

// NewSignerFromHex 从 hex（64 字节私钥）构造签发器。
func NewSignerFromHex(privHex string) (*Signer, error) {
	b, err := hex.DecodeString(strings.TrimSpace(privHex))
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("rtv: bad signing key (want %d-byte hex)", ed25519.PrivateKeySize)
	}
	return &Signer{priv: ed25519.PrivateKey(b), hostCache: map[string]string{}}, nil
}

// NewSignerFromKey 从已有私钥构造（GenerateSigningKey 的配套）。
func NewSignerFromKey(priv ed25519.PrivateKey) *Signer {
	return &Signer{priv: priv, hostCache: map[string]string{}}
}

// PublicKeyHex 当前签名公钥（hex，供 relay 验签侧配置）。
func (s *Signer) PublicKeyHex() string {
	return hex.EncodeToString(s.priv.Public().(ed25519.PublicKey))
}

// HostTicketFor 返回（必要时铸造）(node,relay) 的 host 张票。签发后跨会话
// 字节等值；exp 24h（P1 无续期——过期走注册失败→会话重生路径，P2 经 host
// 控制流带外刷新，绝不经 agent→core cfg，防换血）。
func (s *Signer) HostTicketFor(nodeID, relayID string) string {
	key := nodeID + "\x00" + relayID
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.hostCache[key]; ok {
		return t
	}
	now := time.Now()
	tok, err := mint(&ticketClaims{
		V: ticketV1, Typ: TicketHost, NID: nodeID, RID: relayID, Gen: 1,
		Iat: now.Unix(), Exp: now.Add(24 * time.Hour).Unix(),
	}, s.priv)
	if err != nil {
		panic(err) // ed25519 签名失败不可恢复
	}
	s.hostCache[key] = tok
	return tok
}

// ViewerTicket 铸造会话粒度的 viewer 张票。name 为显示名（controlState
// 广播用，relay 无 DB，名字只能随票走）；ttl 上限由调用方治理。
func (s *Signer) ViewerTicket(sessionID, nodeID, relayID, name string, control, input bool, ttl time.Duration) (string, error) {
	now := time.Now()
	return mint(&ticketClaims{
		V: ticketV1, Typ: TicketViewer, NID: nodeID, SID: sessionID, RID: relayID,
		Gen: 1, Cap: &ticketCap{Control: control, Input: input}, NM: name,
		Iat: now.Unix(), Exp: now.Add(ttl).Unix(),
	}, s.priv)
}

func mint(c *ticketClaims, priv ed25519.PrivateKey) (string, error) {
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	sig := ed25519.Sign(priv, payload)
	return base64.RawURLEncoding.EncodeToString(payload) + ".." +
		base64.RawURLEncoding.EncodeToString(sig), nil
}

// ---------------- 验签侧（relay 持公钥集合） ----------------

// Verifier 离线验签 + 本地墓碑。keys 为公钥集合（hex，ed25519 64 字节）——
// 签名密钥轮换双窗口期同时容纳新旧两把。
type Verifier struct {
	keys []ed25519.PublicKey

	mu   sync.Mutex
	tomb map[string]time.Time // 墓碑 key → 失效时刻（有界：≤ tombstoneHorizon）
}

// NewVerifier 从公钥 hex 清单构造。空清单 = bootstrap 态（xnc-relay 在
// RELAY_CONFIG 下发前的占位——Verify 一律 ErrBadToken，此时无会话），
// SetKeys 注满后正常验签。
func NewVerifier(pubHexes []string) (*Verifier, error) {
	v := &Verifier{tomb: map[string]time.Time{}}
	for _, h := range pubHexes {
		b, err := hex.DecodeString(strings.TrimSpace(h))
		if err != nil || len(b) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("rtv: bad verifier public key")
		}
		v.keys = append(v.keys, ed25519.PublicKey(b))
	}
	return v, nil
}

// Verify 校验并解析票据：签名（任一公钥）→ v/typ → exp（±leeway）→ 墓碑。
func (v *Verifier) Verify(token string) (Ticket, error) {
	dot := strings.Index(token, "..")
	if token == "" || dot <= 0 {
		return Ticket{}, ErrBadToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(token[:dot])
	if err != nil {
		return Ticket{}, ErrBadToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(token[dot+2:])
	if err != nil {
		return Ticket{}, ErrBadToken
	}
	var c ticketClaims
	if err := json.Unmarshal(payload, &c); err != nil || c.V != ticketV1 {
		return Ticket{}, ErrBadToken
	}
	valid := false
	for _, k := range v.keys {
		if ed25519.Verify(k, payload, sig) {
			valid = true
			break
		}
	}
	if !valid {
		return Ticket{}, ErrBadToken
	}
	now := time.Now()
	if now.After(time.Unix(c.Exp, 0).Add(ticketLeeway)) {
		return Ticket{}, ErrExpired
	}
	tk := Ticket{
		Typ: c.Typ, NodeID: c.NID, SessionID: c.SID, RelayID: c.RID, Gen: c.Gen,
		Control: c.Cap != nil && c.Cap.Control, Input: c.Cap != nil && c.Cap.Input,
		Name: c.NM, ExpiresAt: time.Unix(c.Exp, 0),
	}
	if killed := v.checkTomb(tombKey(c), now); killed {
		return Ticket{}, ErrKilled
	}
	return tk, nil
}

// SetKeys 热替换公钥集合（RELAY_CONFIG 下发时调用；双窗口期新旧并存。
// 换钥不触碰墓碑）。
func (v *Verifier) SetKeys(pubHexes []string) error {
	nv, err := NewVerifier(pubHexes)
	if err != nil {
		return err
	}
	v.mu.Lock()
	v.keys = nv.keys
	v.mu.Unlock()
	return nil
}

// Kill 记墓碑（撤销语义，spec §3.4）：
//   - sessionID 非空 = 会话级撤销：只记会话键（会话终局的常规路径——
//     viewer 票随会话废止）。**不碰节点键**：host 张票按 node 铸造且
//     跨会话复用，一次会话关闭就把节点拉黑 25h 是语义错误（真机验收
//     踩坑：host 全部 401、换血后也注册不上）。
//   - sessionID 为空且 nodeID 非空 = 节点级撤销（管理端禁用/删除节点）：
//     记节点键，挡下该节点全部 host 张。
//
// 保留期覆盖最长票 exp。
func (v *Verifier) Kill(nodeID, sessionID string) {
	now := time.Now()
	v.mu.Lock()
	defer v.mu.Unlock()
	if sessionID != "" {
		v.tomb["s:"+sessionID] = now.Add(tombstoneHorizon)
		return
	}
	if nodeID != "" {
		v.tomb["n:"+nodeID] = now.Add(tombstoneHorizon)
	}
}

func tombKey(c ticketClaims) string {
	if c.Typ == TicketHost {
		return "n:" + c.NID
	}
	return "s:" + c.SID
}

func (v *Verifier) checkTomb(key string, now time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	until, ok := v.tomb[key]
	if !ok {
		return false
	}
	if now.After(until) {
		delete(v.tomb, key) // 有界清理
		return false
	}
	return true
}

// ---------------- PlaneHost 最小实现 ----------------

// SimpleHost PlaneHost 的通用实现：验签+墓碑落在 Verifier；Touch/Event 经
// 函数字段注入（server 进程：Touch 接 session manager、事件落日志/审计；
// relay 独立进程：Touch no-op、事件转控制连接）。实现 Kill 以支撑
// Server.KillSession 的墓碑路径。
type SimpleHost struct {
	Verifier *Verifier
	TouchFn  func(session string)
	EventFn  func(typ, node, session string)
}

func (h *SimpleHost) VerifyToken(token string) (Ticket, error) {
	return h.Verifier.Verify(token)
}

func (h *SimpleHost) Touch(session string) {
	if h.TouchFn != nil {
		h.TouchFn(session)
	}
}

func (h *SimpleHost) SessionEvent(typ, node, session string) {
	if h.EventFn != nil {
		h.EventFn(typ, node, session)
	}
}

// Kill 墓碑（Server.KillSession 经接口断言调用）。
func (h *SimpleHost) Kill(node, session string) {
	h.Verifier.Kill(node, session)
}
