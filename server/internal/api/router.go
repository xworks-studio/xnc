package api

import (
	"crypto/tls"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"xnc/server"
	"xnc/server/internal/auth"
	"xnc/server/internal/config"
	"xnc/server/internal/db"
	"xnc/server/internal/registry"
	"xnc/server/internal/rtv"
	"xnc/server/internal/session"
	"xnc/server/internal/version"
)

type handlers struct {
	st   *db.Store
	cfg  config.Config
	reg  *registry.Registry
	sess *session.Manager
	rtv  *rtv.Server // RTV 中继（desktop 媒体面；XNC_RTV_ENDPOINT 未配置时腿仍监听但会话 503）
}

// Close 停止 handler 的后台 worker（RTV 中继随进程生命周期，无独立停止面）。
// 生产经 NewApp().Close() 在 cmd/xnc-server/main.go 优雅停机时调用。
func (h *handlers) Close() error {
	return nil
}

// App 生产入口的应用句柄：内嵌 http.Handler（router），并携带停止后台 worker
// （TURN 池健康探测）的 Close。main.go 停机时先调用 Close（停探测 goroutine）
// 再排空 HTTP 连接。
type App struct {
	http.Handler
	close func()
}

// Close 停止后台 worker（幂等；未启动 worker 时 no-op）。
func (a *App) Close() error {
	if a.close != nil {
		a.close()
	}
	return nil
}

// NewApp 生产入口：与 NewRouter（sess=nil 自建）等价，但返回 *App 携带 Close，
// 优雅停机时先停 TURN 池探测 goroutine 再等 HTTP 排空。
func NewApp(st *db.Store, cfg config.Config, reg *registry.Registry) *App {
	r, closeFn := newRouterWithSession(st, cfg, reg, nil)
	return &App{Handler: r, close: closeFn}
}

func NewRouter(st *db.Store, cfg config.Config, reg *registry.Registry) http.Handler {
	return NewRouterWithSession(st, cfg, reg, nil)
}

// NewRouterWithSession 允许注入共享的 session manager（测试经 TestEnv.Sess 直接
// 驱动会话生命周期）；sess 为 nil 时自建并按 config 覆写 shell 治理参数
// （NewRouter/NewApp 即此生产路径；Manager.New 的默认值仅零值兜底）。
func NewRouterWithSession(st *db.Store, cfg config.Config, reg *registry.Registry, sess *session.Manager) http.Handler {
	r, _ := newRouterWithSession(st, cfg, reg, sess)
	return r
}

// newRouterWithSession 共享构造：返回 router 与停止后台 worker 的闭包（生产经
// NewApp().Close() 调用；测试直接经 manager.Stop() 治理）。
func newRouterWithSession(st *db.Store, cfg config.Config, reg *registry.Registry, sess *session.Manager) (http.Handler, func()) {
	if sess == nil {
		sess = session.New(reg, slog.Default())
		sess.ShellPerNode = cfg.ShellPerNode
		sess.ShellIdleTimeout = cfg.ShellIdleTimeout
		sess.ShellMaxLifetime = cfg.ShellMaxLifetime
		sess.DesktopPerNode = cfg.DesktopPerNode
		sess.DesktopIdleTimeout = cfg.DesktopIdleTimeout
	}
	h := &handlers{st: st, cfg: cfg, reg: reg, sess: sess}
	// RTV 中继（desktop 媒体面）：host QUIC + WT 两腿绑 UDP；WS 兜底腿挂主
	// mux（/ws，经 caddy TCP443 反代）。viewer 鉴权 = 会话 client token；
	// input 门控 = per-node 输入活约（janitor TTL 撤约语义不变）。
	var tlsProv func([]string) *tls.Config
	if cfg.RTVCertFile != "" && cfg.RTVKeyFile != "" {
		tlsProv = rtv.CertFiles(cfg.RTVCertFile, cfg.RTVKeyFile)
	} else {
		tlsProv = rtv.DevSelfSigned()
	}
	h.rtv = rtv.New(rtv.Options{HostAddr: cfg.RTVHostAddr, WTAddr: cfg.RTVWTAddr},
		tlsProv,
		func(token string) (rtv.ViewerBinding, *rtv.AuthError) {
			node, sid, aerr := sess.AttachClientRTV(token)
			if aerr != nil {
				return rtv.ViewerBinding{}, &rtv.AuthError{Status: aerr.Status, Message: aerr.Message}
			}
			return rtv.ViewerBinding{Node: node.String(), Session: sid}, nil
		},
		cfg.RTVWSOrigins)
	h.rtv.Touch = sess.TouchActivity
	h.rtv.Hub.SetInputGate(func(node, sessionID string) bool {
		n, err := uuid.Parse(node)
		if err != nil {
			return false
		}
		holder, _ := sess.DesktopLeaseOf(n)
		return holder != "" && holder == sessionID
	})
	if err := h.rtv.Start(); err != nil {
		slog.Error("rtv legs failed to start", "err", err)
	}
	r := chi.NewRouter()

	r.Get("/api/health", func(w http.ResponseWriter, _ *http.Request) {
		respondJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": version.Version})
	})

	r.Route("/api/auth", func(ar chi.Router) {
		ar.Post("/login", h.login)
		ar.Group(func(g chi.Router) {
			g.Use(auth.Middleware(cfg.JWTSecret, st))
			g.Get("/me", h.me)
			g.Patch("/me", h.updateMe)
			g.Post("/password", h.changePassword)
		})
	})

	r.Route("/api/clusters", func(cr chi.Router) {
		cr.Use(auth.Middleware(cfg.JWTSecret, st))
		cr.Get("/", h.listClusters)
		cr.Post("/", h.createCluster)
		// 软删除：owner-only，有节点 409（handler 内判定）
		cr.Delete("/{id}", h.deleteCluster)
		// 用户 JWT 授权的节点注册（spec §6.4）：任一成员；409 含冲突 cluster 名
		cr.Post("/{id}/nodes/register", h.userRegisterNode)
	})

	// 用户管理：无自注册，仅 admin（任一 cluster owner）可创建/列出。
	r.Route("/api/users", func(ur chi.Router) {
		ur.Use(auth.Middleware(cfg.JWTSecret, st))
		ur.Post("/", h.createUser)
		ur.Get("/", h.listUsers)
	})

	// RTV 中继观测面（管理端；原 MVP /statsz 的收权版本）。
	r.Route("/api/rtv", func(tr chi.Router) {
		tr.Use(auth.Middleware(cfg.JWTSecret, st))
		tr.Get("/stats", h.rtvStats)
	})

	// 审计查询：admin-only，可选过滤 + 分页（handler 内判定）。
	r.Route("/api/audit", func(ar chi.Router) {
		ar.Use(auth.Middleware(cfg.JWTSecret, st))
		ar.Get("/", h.listAudit)
	})

	r.Route("/api/clusters/{id}/enrollment-tokens", func(tr chi.Router) {
		tr.Use(auth.Middleware(cfg.JWTSecret, st))
		tr.Post("/", h.createEnrollToken)
	})
	// membership 管理：列表任一成员可用；增删仅 owner（handler 内判定）。
	r.Route("/api/clusters/{id}/members", func(mr chi.Router) {
		mr.Use(auth.Middleware(cfg.JWTSecret, st))
		mr.Get("/", h.listMembers)
		mr.Post("/", h.addMember)
		mr.Delete("/{userId}", h.removeMember)
	})
	r.Post("/api/agent/enroll", h.agentEnroll)
	r.Get("/api/agent/connect", h.agentConnect)
	// 会话 WS（两侧均 token 即凭证，不走 JWT）
	r.Get("/api/session/{id}", h.clientSessionWS)
	r.Get("/api/agent/session", h.agentSessionWS)
	// RTV WS 兜底腿（token 即凭证；经 caddy TCP443 → 主 mux）
	r.Get("/ws", h.rtv.WSHandler())

	// 自更新管理面（admin：部署流水线上传制品 + 灰度 pin/强制下发）
	r.Route("/api/admin", func(ar chi.Router) {
		ar.Use(auth.Middleware(cfg.JWTSecret, st))
		ar.Post("/releases", h.adminUploadRelease)
		ar.Get("/releases", h.adminListReleases)
		ar.Delete("/releases/{id}", h.adminDeleteRelease)
		ar.Post("/rollout", h.adminRollout)
	})
	// CLI 自更新（用户 JWT）：元信息 + 二进制
	r.Route("/api/cli", func(cr chi.Router) {
		cr.Use(auth.Middleware(cfg.JWTSecret, st))
		cr.Get("/latest", h.cliLatest)
		cr.Get("/download", h.cliDownload)
	})

	// 安装器分发（设计 §4，无认证——产品首次下载入口）：频道最新 setup.exe
	// 直流 + 动态版本清单 setup.json（供 xnc upgrade --check / CI 消费）。
	// 安装器是唯一安装入口（未上线直采终态，设计 §14）：历史的一行流
	// （/a/* /c* /install/*.ps1）已在上线前整体移除。
	r.Get("/installer", h.setupDownload)
	r.Get("/installer.json", h.setupManifest)

	r.Route("/api/nodes", func(nr chi.Router) {
		nr.Use(auth.Middleware(cfg.JWTSecret, st))
		nr.Get("/", h.listNodes)
		nr.Get("/{id}", h.getNode)
		// exec：鉴权 + membership 通过后创建会话并下发 SESSION_OPEN，202 异步语义
		nr.Post("/{id}/exec", h.execStart)
		// shell：同一会话创建路径（startSession），kind=shell，ConPTY 交互式终端
		nr.Post("/{id}/shell", h.shellStart)
		// file：上传（path+size+sha256）/下载（path）会话，kind=file，startSession 路径
		nr.Post("/{id}/files/upload", h.fileUpload)
		nr.Post("/{id}/files/download", h.fileDownload)
		// tunnel：RDP 等端口隧道，kind=tunnel，白名单 target 解析 host/port
		nr.Post("/{id}/tunnel", h.tunnelStart)
		// screen：桌面流会话，kind=screen（Phase 6，DXGI+H.264），startSession 路径
		nr.Post("/{id}/screen", h.screenStart)
		// desktop：实时桌面会话（RTV 中继），startSession 路径；
		// XNC_RTV_ENDPOINT 未配置 → 503 RTV_UNCONFIGURED，每节点并发上限 + idle 治理
		nr.Post("/{id}/desktop", h.desktopStart)
		// 管理动作：owner-only（handler 内经 requireMinRoleIgnoreDisabled 判定）
		nr.Post("/{id}/disable", h.nodeDisable)
		nr.Post("/{id}/enable", h.nodeEnable)
		// 管理端硬删除节点：admin-only（handler 内 isAdminUser 判定），在线则
		// 同时逐出其控制连接
		nr.Delete("/{id}", h.adminDeleteNode)
	})

	// 内嵌 Web UI 兜底（Phase 7）：作为最后一条注册，chi 静态路由
	// （/api/*）优先命中，其余路径走 SPA——静态文件直出，未命中回落
	// index.html（react-router 客户端路由）；/api/* 前缀在 handler 内
	// 保持 404，不被 SPA 吞掉。
	r.Mount("/", server.SPAHandler())
	return r, func() { _ = h.Close() }
}
