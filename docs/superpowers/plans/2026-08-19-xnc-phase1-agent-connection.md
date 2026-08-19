# XNC Phase 1 — Agent Connection 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 从零仓库实现 Phase 1：Agent 注册 → 设备认证 → WSS 长连 → 心跳 → 节点 online/offline 可见（Web + CLI），并部署到 SRV。

**Architecture:** Go 单体 xnc-server（chi + pgx + goose + 内存 Hub 管理长连接）；Rust xnc-agent（tokio + tokio-tungstenite + ed25519-dalek + windows-service）；Go xnc-cli；React 最小 Web。JSON 控制协议双向镜像定义（Go proto 包 + Rust xnc-proto crate，黄金样本对齐）。

**Tech Stack:** Go 1.26+（chi v5、pgx v5、goose v3、golang-jwt v5、x/crypto、coder/websocket）、Rust stable msvc（tokio、tokio-tungstenite/rustls、ed25519-dalek、serde、reqwest/rustls、clap、windows-service、windows）、PostgreSQL 16、React + TS (Vite)。

**Spec:** `C:\Users\LABS\Desktop\XNC\spec.md`（本计划实现其 §53 Phase 1；协议见 §9/10/11/12/13，API 见 §27，测试见 §59）

## Global Constraints（摘自 spec，所有任务隐含遵守）

* 公网通信仅 TLS 1.2+；明文 HTTP/WS 仅允许 127.0.0.1 开发环境。
* REST 统一错误 envelope：`{"ok":false,"data":null,"error":{"code":"...","message":"..."}}`；成功：`{"ok":true,"data":...,"error":null}`。
* Enrollment Token：`xnc_enroll_` + 32 字节 CSPRNG 的 base64url（43 字符），库中只存 SHA-256 hash，默认 TTL 30m、max_uses 1，明文仅创建响应返回一次。
* 设备认证：Key Challenge + Ed25519；nonce 60s 有效、单次使用；认证通过才进 HELLO。
* 心跳 30s，90s 无心跳判 offline，均可配置（测试中缩短）。
* JWT HS256、12h 过期；密码 bcrypt。
* 命名：二进制 `xnc-server` / `xnc-agent.exe` / `xnc`，Windows 服务名 `XNCAgent`；模块/表名用直白英文小词。
* CLI 退出码：0 成功 / 2 用法 / 240 认证 / 244 不存在 / 250 内部；一切命令支持 `--json`。
* 数据库只 PostgreSQL（开发/CI 用容器）；Go 测试用 testcontainers-go；Rust 用 cargo test。
* 提交信息用 Conventional Commits（feat/fix/test/chore/docs）。
* 测试设备凭据读 `config.env`（SRV=control.xnc.app → 47.243.209.52，aliyun-intl）。
* 单实例 Server：NodeId→连接 只存内存，不引入 Redis。

---

### Task 1: 仓库骨架与本地依赖

**Files:**
- Create: `go.work`、`docker-compose.yml`、`Makefile`、`README.md`
- Create: `xnc-server/go.mod`、`xnc-cli/go.mod`（`go mod init github.com/terry/xnc-server` 等，模块路径按最终 repo 定，占位 `github.com/xnc/...`）
- Create: `proto/messages.md`（空契约文档，Task 9 填充）

**Interfaces:**
- Produces: 目录布局 `xnc-server/ xnc-agent/ xnc-cli/ web/ proto/ skills/ deploy/ scripts/`；`docker-compose.yml` 提供 `postgres:16` 于 `localhost:5432`（用户 xnc/密码 xnc/库 xnc）

- [ ] **Step 1: 初始化 git 与目录**

```bash
cd /c/Users/LABS/Desktop/XNC
git init
mkdir -p xnc-server/cmd/xnc-server xnc-server/internal/{config,db,auth,api,agenthub,proto,model} xnc-server/internal/db/migrations xnc-cli/cmd/xnc xnc-cli/internal/{client,output} xnc-agent/crates/{xnc-proto,xnc-agent}/src proto deploy scripts web
```

- [ ] **Step 2: 写 docker-compose.yml**

```yaml
services:
  postgres:
    image: postgres:16
    environment:
      POSTGRES_USER: xnc
      POSTGRES_PASSWORD: xnc
      POSTGRES_DB: xnc
    ports: ["5432:5432"]
    volumes: [pgdata:/var/lib/postgresql/data]
volumes:
  pgdata:
```

- [ ] **Step 3: 写 go.work 与 Makefile**

```
go 1.26

use (
	./xnc-server
	./xnc-cli
)
```

```makefile
.PHONY: db-up server cli agent web test
db-up:
	docker compose up -d postgres
server:
	cd xnc-server && go run ./cmd/xnc-server
cli:
	cd xnc-cli && go run ./cmd/xnc
agent:
	cd xnc-agent && cargo run -p xnc-agent -- run
web:
	cd web && npm run dev
test:
	cd xnc-server && go test ./...
	cd xnc-cli && go test ./...
	cd xnc-agent && cargo test --workspace
```

- [ ] **Step 4: 验证**

Run: `docker compose up -d postgres && docker ps`
Expected: postgres 容器运行。`go work init`（若手写 go.work 格式报错则用命令生成再补 use）。

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "chore: repo skeleton with go workspace, compose, makefile"
```

---

### Task 2: Server 配置加载与健康检查

**Files:**
- Create: `xnc-server/internal/config/config.go`
- Test: `xnc-server/internal/config/config_test.go`
- Create: `xnc-server/cmd/xnc-server/main.go`（骨架：Load → chi → /healthz → Listen）

**Interfaces:**
- Produces: `config.Config{Listen, DatabaseURL, JWTSecret string; BootstrapAdminEmail, BootstrapAdminPassword, BootstrapCluster string; HeartbeatTimeout time.Duration}`；`config.Load() (*Config, error)`（全部从 `XNC_*` 环境变量读取，Listen 默认 `:8080`，HeartbeatTimeout 默认 90s）

- [ ] **Step 1: 写失败测试**

```go
func TestLoadDefaults(t *testing.T) {
	t.Setenv("XNC_DB_URL", "postgres://xnc:xnc@localhost:5432/xnc")
	t.Setenv("XNC_JWT_SECRET", "s3cret")
	c, err := Load()
	if err != nil { t.Fatal(err) }
	if c.Listen != ":8080" { t.Errorf("Listen = %q", c.Listen) }
	if c.HeartbeatTimeout != 90*time.Second { t.Errorf("timeout = %v", c.HeartbeatTimeout) }
}

func TestLoadMissingDB(t *testing.T) {
	t.Setenv("XNC_DB_URL", "")
	if _, err := Load(); err == nil { t.Fatal("want error") }
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd xnc-server && go test ./internal/config/ -v`
Expected: FAIL（Load 未定义）

- [ ] **Step 3: 实现 config.go**

```go
package config

import (
	"fmt"
	"os"
	"time"
)

type Config struct {
	Listen                 string
	DatabaseURL            string
	JWTSecret              string
	BootstrapAdminEmail    string
	BootstrapAdminPassword string
	BootstrapCluster       string
	HeartbeatTimeout       time.Duration
}

func Load() (*Config, error) {
	c := &Config{
		Listen:               env("XNC_LISTEN", ":8080"),
		DatabaseURL:          os.Getenv("XNC_DB_URL"),
		JWTSecret:            os.Getenv("XNC_JWT_SECRET"),
		BootstrapAdminEmail:  os.Getenv("XNC_BOOTSTRAP_ADMIN_EMAIL"),
		BootstrapCluster:     env("XNC_BOOTSTRAP_CLUSTER", "lab"),
	}
	if c.DatabaseURL == "" { return nil, fmt.Errorf("XNC_DB_URL required") }
	if c.JWTSecret == "" { return nil, fmt.Errorf("XNC_JWT_SECRET required") }
	d, err := time.ParseDuration(env("XNC_HEARTBEAT_TIMEOUT", "90s"))
	if err != nil { return nil, err }
	c.HeartbeatTimeout = d
	c.BootstrapAdminPassword = os.Getenv("XNC_BOOTSTRAP_ADMIN_PASSWORD")
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" { return v }
	return def
}
```

依赖引入：`go get github.com/go-chi/chi/v5`。

- [ ] **Step 4: main.go 骨架（/healthz）**

```go
func main() {
	cfg, err := config.Load()
	if err != nil { log.Fatal(err) }
	r := chi.NewRouter()
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK); w.Write([]byte("ok"))
	})
	log.Printf("xnc-server listening on %s", cfg.Listen)
	log.Fatal(http.ListenAndServe(cfg.Listen, r))
}
```

- [ ] **Step 5: 跑测试通过 + 手工冒烟**

Run: `cd xnc-server && go test ./... && go run ./cmd/xnc-server & curl -s localhost:8080/healthz`
Expected: PASS；输出 `ok`

- [ ] **Step 6: Commit**

```bash
git add -A && git commit -m "feat(server): config loading and healthz skeleton"
```

---

### Task 3: 数据库迁移与连接

**Files:**
- Create: `xnc-server/internal/db/db.go`、`xnc-server/internal/db/migrations/0001_init.sql`
- Test: `xnc-server/internal/db/db_test.go`

**Interfaces:**
- Produces: `db.Connect(ctx context.Context, url string) (*pgxpool.Pool, error)`；`db.Migrate(pool *pgxpool.Pool) error`（goose embed，幂等）
- Produces: 表 `users(id uuid pk, email text unique not null, display_name text, password_hash text not null, created_at timestamptz default now())`、`clusters(id uuid pk, name text unique not null, owner_id uuid references users, created_at timestamptz)`、`cluster_members(cluster_id uuid, user_id uuid, role text not null, created_at timestamptz, primary key(cluster_id,user_id))`、`nodes(id uuid pk, cluster_id uuid not null references clusters, name text not null, machine_id text unique not null, hostname text, os_version text, agent_version text, shell_type text, public_key text not null, status text not null default 'offline', last_seen_at timestamptz, created_at timestamptz default now(), unique(cluster_id,name))`、`enrollment_tokens(id uuid pk, cluster_id uuid references clusters, token_hash text unique not null, expires_at timestamptz not null, max_uses int not null default 1, used_count int not null default 0, created_by uuid, created_at timestamptz default now())`、`audit_logs(id uuid pk, user_id uuid, cluster_id uuid, node_id uuid, action text not null, session_id text, metadata jsonb default '{}', created_at timestamptz default now())`

- [ ] **Step 1: 写 0001_init.sql（按上方 Interfaces 完整六表 DDL，略写于此——执行者照 Interfaces 逐字段写 SQL）**

- [ ] **Step 2: 写失败测试（testcontainers）**

```go
func TestMigrateIdempotent(t *testing.T) {
	ctx := context.Background()
	pg, err := postgres.Run(ctx, "postgres:16",
		postgres.WithDatabase("xnc"), postgres.BasicUser("xnc", "xnc"))
	if err != nil { t.Skip("docker unavailable:", err) }
	defer pg.Terminate(ctx)
	pool, err := db.Connect(ctx, pg.ConnectionString(ctx))
	if err != nil { t.Fatal(err) }
	if err := db.Migrate(pool); err != nil { t.Fatal(err) }
	if err := db.Migrate(pool); err != nil { t.Fatal("second migrate must be no-op:", err) }
	var n int
	pool.QueryRow(ctx, `select count(*) from information_schema.tables
		where table_name in ('users','clusters','cluster_members','nodes','enrollment_tokens','audit_logs')`).Scan(&n)
	if n != 6 { t.Errorf("tables = %d, want 6", n) }
}
```

依赖：`go get github.com/testcontainers/testcontainers-go github.com/testcontainers/testcontainers-go/modules/postgres github.com/pressly/goose/v3`

- [ ] **Step 3: 跑测试确认失败 → 实现 db.go**

```go
package db

import (
	"context"
	"embed"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	return pgxpool.New(ctx, url)
}

func Migrate(pool *pgxpool.Pool) error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil { return err }
	return goose.Up(pool.DB, "migrations")
}
```

- [ ] **Step 4: 跑测试通过**（Run 同上，Expected: PASS）

- [ ] **Step 5: main.go 接入**（Load → Connect → Migrate → 再起路由；缺库时启动失败日志清晰）

- [ ] **Step 6: Commit**

```bash
git add -A && git commit -m "feat(server): postgres schema and goose migrations"
```

---

### Task 4: Bootstrap Admin 与默认 Cluster

**Files:**
- Create: `xnc-server/internal/model/model.go`（User/Cluster 结构体 + 常量 RoleOwner/Operator/Viewer）
- Create: `xnc-server/internal/api/bootstrap.go`
- Test: `xnc-server/internal/api/bootstrap_test.go`

**Interfaces:**
- Produces: `api.BootstrapAdmin(ctx, pool, cfg *config.Config) error`——users 为空且 cfg.BootstrapAdminEmail/Password 非空时：创建 owner 用户、创建默认 cluster、写 cluster_members(role='owner')、audit `bootstrap.admin`；users 非空则直接返回 nil

- [ ] **Step 1: 写失败测试**

```go
func TestBootstrapCreatesAdminOnce(t *testing.T) {
	// testcontainers 起 pg（抽 TestMain 共享，见 db 测试同法）
	ctx := context.Background()
	pool := testPool(t) // 辅助函数：起容器+迁移，返回 pool
	cfg := &config.Config{BootstrapAdminEmail: "a@b.c", BootstrapAdminPassword: "pw123456", BootstrapCluster: "lab"}
	if err := BootstrapAdmin(ctx, pool, cfg); err != nil { t.Fatal(err) }
	if err := BootstrapAdmin(ctx, pool, cfg); err != nil { t.Fatal("second call must be no-op") }
	var users, clusters, members int
	pool.QueryRow(ctx, `select count(*) from users`).Scan(&users)
	pool.QueryRow(ctx, `select count(*) from clusters`).Scan(&clusters)
	pool.QueryRow(ctx, `select count(*) from cluster_members where role='owner'`).Scan(&members)
	if users != 1 || clusters != 1 || members != 1 {
		t.Errorf("users=%d clusters=%d members=%d", users, clusters, members)
	}
}
```

- [ ] **Step 2: 确认失败 → 实现**（用 auth.HashPassword（Task 5 产出；本任务先临时内联 bcrypt，Task 5 落位后替换调用）——为避免依赖倒置，**将本任务排在 Task 5 之后执行亦可**；实现时直接调用 `auth.HashPassword`）

```go
func BootstrapAdmin(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config) error {
	var n int
	if err := pool.QueryRow(ctx, `select count(*) from users`).Scan(&n); err != nil { return err }
	if n > 0 || cfg.BootstrapAdminEmail == "" || cfg.BootstrapAdminPassword == "" { return nil }
	hash, err := auth.HashPassword(cfg.BootstrapAdminPassword)
	if err != nil { return err }
	tx, err := pool.Begin(ctx)
	if err != nil { return err }
	defer tx.Rollback(ctx)
	var userID, clusterID uuid.UUID
	if err := tx.QueryRow(ctx,
		`insert into users (email, display_name, password_hash) values ($1,$1,$2) returning id`,
		cfg.BootstrapAdminEmail, hash).Scan(&userID); err != nil { return err }
	if err := tx.QueryRow(ctx,
		`insert into clusters (name, owner_id) values ($1,$2) returning id`,
		cfg.BootstrapCluster, userID).Scan(&clusterID); err != nil { return err }
	if _, err := tx.Exec(ctx,
		`insert into cluster_members (cluster_id, user_id, role) values ($1,$2,'owner')`,
		clusterID, userID); err != nil { return err }
	_, err = tx.Exec(ctx, `insert into audit_logs (user_id, cluster_id, action) values ($1,$2,'bootstrap.admin')`, userID, clusterID)
	if err != nil { return err }
	return tx.Commit(ctx)
}
```

- [ ] **Step 3: 跑测试通过 → main.go 启动时调用**

- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "feat(server): bootstrap admin user and default cluster"
```

---

### Task 5: 密码哈希与 JWT

**Files:**
- Create: `xnc-server/internal/auth/password.go`、`xnc-server/internal/auth/jwt.go`
- Test: `xnc-server/internal/auth/auth_test.go`

**Interfaces:**
- Produces: `auth.HashPassword(pw string) (string, error)`（bcrypt cost 12）；`auth.CheckPassword(hash, pw string) bool`
- Produces: `auth.Claims{UserID, Email string, jwt.RegisteredClaims}`；`auth.IssueToken(secret, userID, email string) (string, error)`（HS256，exp 12h）；`auth.ParseToken(secret, token string) (*Claims, error)`

- [ ] **Step 1: 写失败测试**

```go
func TestPasswordRoundtrip(t *testing.T) {
	h, err := HashPassword("pw123456")
	if err != nil || CheckPassword(h, "pw123456") != true || CheckPassword(h, "wrong") != false {
		t.Fatalf("hash=%v err=%v", h, err)
	}
}

func TestTokenRoundtrip(t *testing.T) {
	tok, err := IssueToken("s", "uid-1", "a@b.c")
	if err != nil { t.Fatal(err) }
	c, err := ParseToken("s", tok)
	if err != nil || c.UserID != "uid-1" || c.Email != "a@b.c" { t.Fatalf("c=%v err=%v", c, err) }
	if _, err := ParseToken("bad", tok); err == nil { t.Fatal("wrong secret must fail") }
}
```

- [ ] **Step 2: 确认失败 → 实现**（golang-jwt/v5 + golang.org/x/crypto/bcrypt，照 Interfaces 签名实现，约 40 行）
- [ ] **Step 3: 跑测试通过**
- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "feat(server): bcrypt password and jwt issue/parse"
```

---

### Task 6: 登录 API、错误 envelope、JWT 中间件

**Files:**
- Create: `xnc-server/internal/api/router.go`、`login.go`、`errors.go`、`middleware.go`
- Test: `xnc-server/internal/api/api_test.go`

**Interfaces:**
- Produces: `api.NewRouter(pool *pgxpool.Pool, cfg *config.Config) *chi.Mux`
- Produces: `api.WriteErr(w, code string, status int, msg string)`（写统一 envelope）；`api.WriteOK(w, data any)`
- Produces: `api.RequireAuth(secret string) func(http.Handler) http.Handler`（解析 Bearer，ctx 放 userID/email）；`api.CtxUser(ctx) (userID, email string)`
- Route: `POST /api/auth/login`（body `{"email","password"}` → `{"ok":true,"data":{"token":"..."}}`；错误码 UNAUTHORIZED/400）

- [ ] **Step 1: 写失败测试**

```go
func TestLoginFlow(t *testing.T) {
	pool := testPool(t) // 容器+迁移+bootstrap（复用 Task 3/4 辅助）
	r := NewRouter(pool, testCfg())
	srv := httptest.NewServer(r); defer srv.Close()

	// 错误密码 → 401 + envelope
	resp, _ := http.Post(srv.URL+"/api/auth/login", "application/json",
		strings.NewReader(`{"email":"a@b.c","password":"wrong"}`))
	if resp.StatusCode != 401 { t.Fatalf("status=%d", resp.StatusCode) }
	var e struct{ Ok bool; Error struct{ Code string } }
	json.NewDecoder(resp.Body).Decode(&e)
	if e.Ok || e.Error.Code != "UNAUTHORIZED" { t.Fatalf("envelope=%+v", e) }

	// 正确登录 → token
	resp, _ = http.Post(srv.URL+"/api/auth/login", "application/json",
		strings.NewReader(`{"email":"a@b.c","password":"pw123456"}`))
	var ok struct{ Ok bool; Data struct{ Token string } }
	json.NewDecoder(resp.Body).Decode(&ok)
	if !ok.Ok || ok.Data.Token == "" { t.Fatalf("envelope=%+v", ok) }
}
```

- [ ] **Step 2: 确认失败 → 实现 errors.go / middleware.go / login.go / router.go**（login 查 users by email → CheckPassword → IssueToken → WriteOK；audit 写 `auth.login`）
- [ ] **Step 3: 跑测试通过**；main.go 改用 NewRouter
- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "feat(server): login endpoint, error envelope, jwt middleware"
```

---

### Task 7: Enrollment Token 创建 API

**Files:**
- Create: `xnc-server/internal/api/tokens.go`
- Test: `xnc-server/internal/api/tokens_test.go`

**Interfaces:**
- Consumes: Task 6 的 RequireAuth/CtxUser/WriteOK/WriteErr
- Produces: `POST /api/clusters/{clusterId}/enrollment-tokens`（body 可选 `{"ttlMinutes":30,"maxUses":1}`）→ data `{"token":"xnc_enroll_...","clusterId":"...","expiresAt":"..."}`；权限：cluster_members 中该用户 role in (owner,operator) 否则 403 FORBIDDEN；cluster 不存在 404 CLUSTER_NOT_FOUND
- Produces: 内部函数 `genEnrollToken() (plaintext, hash string, err error)`（32B CSPRNG → base64url 无填充；hash=SHA-256 hex）

- [ ] **Step 1: 写失败测试**

```go
func TestCreateEnrollmentToken(t *testing.T) {
	pool, r, jwtTok := bootAPI(t) // 容器+迁移+bootstrap+router+登录 token
	clusterID := firstClusterID(t, pool)

	// 未认证 → 401
	resp, _ := http.Post(srvURL(t,r)+"/api/clusters/"+clusterID+"/enrollment-tokens",
		"application/json", nil)
	if resp.StatusCode != 401 { t.Fatalf("status=%d", resp.StatusCode) }

	// 正常创建
	req, _ := http.NewRequest("POST", srvURL(t,r)+"/api/clusters/"+clusterID+"/enrollment-tokens",
		strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+jwtTok)
	resp, _ = http.DefaultClient.Do(req)
	var ok struct{ Ok bool; Data struct{ Token string } }
	json.NewDecoder(resp.Body).Decode(&ok)
	if !ok.Ok || !strings.HasPrefix(ok.Data.Token, "xnc_enroll_") || len(ok.Data.Token) != len("xnc_enroll_")+43 {
		t.Fatalf("token=%q", ok.Data.Token)
	}
	// 库中无明文：hash 查询命中、明文查询不命中
	var n int
	pool.QueryRow(ctx, `select count(*) from enrollment_tokens where token_hash = sha256hex(ok.Data.Token)`).Scan(&n) // 用辅助计算
	// 断言 n==1 且按明文查 enrollment_tokens.token_hash 无命中
}
```

- [ ] **Step 2: 确认失败 → 实现 tokens.go**（含 audit `token.create`）
- [ ] **Step 3: 跑测试通过**
- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "feat(server): enrollment token creation api"
```

---

### Task 8: Agent Enroll API

**Files:**
- Create: `xnc-server/internal/api/agent_enroll.go`
- Test: `xnc-server/internal/api/agent_enroll_test.go`

**Interfaces:**
- Consumes: Task 7 的 token 存储
- Produces: `POST /api/agent/enroll`（**无 JWT**，body `{"token","hostname","machineId","osVersion","agentVersion","publicKey"}`，publicKey 为 Ed25519 公钥 base64）
  - token hash 不存在 → 400 ENROLLMENT_TOKEN_INVALID
  - expires_at 已过 → 400 ENROLLMENT_TOKEN_EXPIRED
  - used_count >= max_uses → 400 ENROLLMENT_TOKEN_INVALID（重放）
  - machineId 已存在 → 更新该 node 的 publicKey/hostname/agent_version（重装场景），返回原 nodeId
  - 成功 → used_count++（事务内）、创建 node（status='offline'，name 默认 hostname 小写）、audit `node.enroll`、返回 `{"ok":true,"data":{"nodeId":"...","clusterId":"..."}}`

- [ ] **Step 1: 写失败测试**（五种情形各一个子测试：happy、无效、过期、重放、重装复用 machineId；过期 token 用 `insert ... expires_at = now()-interval '1 minute'` 直插库构造）
- [ ] **Step 2: 确认失败 → 实现 agent_enroll.go**（全部在一个数据库事务里校验+创建+计数+审计）
- [ ] **Step 3: 跑测试通过**
- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "feat(server): agent enrollment endpoint"
```

---

### Task 9: 协议消息定义（Go）与契约文档

**Files:**
- Create: `xnc-server/internal/proto/messages.go`
- Create: `proto/messages.md`
- Test: `xnc-server/internal/proto/messages_test.go`

**Interfaces:**
- Produces: `proto.Envelope{Type string \`json:"type"\`; RequestID string \`json:"requestId,omitempty"\`; SessionID string \`json:"sessionId,omitempty"\`; Payload json.RawMessage \`json:"payload,omitempty"\`}`
- Produces（payload 类型 + 常量）：`MSG_CHALLENGE="CHALLENGE"`（Payload `Challenge{Nonce string \`json:"nonce"\`; ExpiresAt time.Time \`json:"expiresAt"\`}`，nonce 为 16B 随机 base64）、`MSG_CHALLENGE_RESPONSE`（`ChallengeResponse{NodeID, Signature string}`）、`MSG_HELLO`（`Hello{NodeID, Hostname, AgentVersion string}`）、`MSG_HELLO_ACK`、`MSG_HEARTBEAT`（空 payload）、`MSG_HEARTBEAT_ACK`、`MSG_ERROR`（`ErrPayload{Code, Message string}`）
- Produces: 二进制帧头约定（Phase 3 用，文档化）：`[1B frameType][16B sessionId][payload]`

- [ ] **Step 1: 写黄金样本测试**

```go
func TestEnvelopeGoldenJSON(t *testing.T) {
	e := Envelope{Type: MSG_CHALLENGE, Payload: mustJSON(Challenge{Nonce: "nw==", ExpiresAt: time.Unix(0,0).UTC()})}
	b, _ := json.Marshal(e)
	want := `{"type":"CHALLENGE","payload":{"nonce":"nw==","expiresAt":"1970-01-01T00:00:00Z"}}`
	if string(b) != want { t.Fatalf("got %s", b) }
}
```

- [ ] **Step 2: 确认失败 → 实现 messages.go；把黄金样本同步进 `proto/messages.md`（Rust 侧 Task 13 以此为对齐基准）**
- [ ] **Step 3: 跑测试通过**
- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "feat(proto): json control message envelope and golden samples"
```

---

### Task 10: Agent Hub 与 WebSocket 挑战认证

**Files:**
- Create: `xnc-server/internal/agenthub/hub.go`、`xnc-server/internal/api/agent_connect.go`
- Test: `xnc-server/internal/agenthub/hub_test.go`

**Interfaces:**
- Consumes: Task 9 proto；Task 3 nodes.public_key
- Produces: `agenthub.Hub`——`NewHub(heartbeatTimeout time.Duration) *Hub`；`(*Hub) Register(nodeID string, c *Conn)`（同 node 旧连接被 Close）；`(*Hub) Unregister(nodeID string, c *Conn)`；`(*Hub) SetSeen(nodeID)`；`(*Hub) IsOnline(nodeID) bool`；`(*Hub) Start(ctx, pool)`（后台每 10s sweep：连接已断且 last_seen 超时 → UPDATE nodes SET status='offline'）
- Produces: `agenthub.Conn`——包装 websocket 连接 + `LastSeen atomic.Int64`；`api.HandleAgentConnect(hub, pool, secret-less)` 挂在 `GET /api/agent/connect`（**无 JWT**）：
  1. Accept WS（coder/websocket），10s 读超时
  2. 发 CHALLENGE（nonce 16B，expiresAt=now+60s，nonce 存内存 set 单次使用）
  3. 收 CHALLENGE_RESPONSE{nodeId, signature}：查 node.public_key → ed25519.Verify(nonce, sig)；失败/过期/nonce 重用 → 发 ERROR{code:"UNAUTHORIZED"} 并 Close
  4. 收 HELLO{nodeId,...}：HELLO.nodeId 必须等于挑战响应的 nodeId，agentVersion 记库；不匹配 → ERROR{code:"UNAUTHORIZED"} Close
  5. 发 HELLO_ACK，`UPDATE nodes SET status='online', last_seen_at=now()`，hub.Register，进入读循环：每条 HEARTBEAT → SetSeen + 节流写 last_seen_at（≥30s 一次）+ 回 HEARTBEAT_ACK；Read 出错/断开 → Unregister + status='offline' + last_seen_at=now()

- [ ] **Step 1: 写失败测试**（用 coder/websocket 的客户端拨号 httptest server；Go 生成 ed25519 测试密钥模拟 agent）

```go
func TestChallengeRejectsWrongKey(t *testing.T) {
	h := agenthub.NewHub(time.Minute)
	nodeID := seedNode(t, pool, testPubKey)     // 直插 node 行，public_key 为正确测试钥
	srv := httptest.NewServer(http.HandlerFunc(func(w, r) {
		api.HandleAgentConnect(h, pool)(w, r)
	}))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/agent/connect"
	c, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil { t.Fatal(err) }
	defer c.CloseNow()
	wsjson.Write(ctx, c, proto.BadChallengeReply(nodeID, wrongKeySign)) // 辅助：错误钥签名
	var env proto.Envelope
	wsjson.Read(ctx, c, &env)
	if env.Type != proto.MSG_ERROR { t.Fatalf("want ERROR got %s", env.Type) }
}

func TestFullHandshake(t *testing.T) {
	// 正确签名 → CHALLENGE 通过 → HELLO → HELLO_ACK → hub.IsOnline(nodeID)==true
	// → 客户端 Close 后 IsOnline==false
}
```

- [ ] **Step 2: 确认失败 → 实现 hub.go 与 agent_connect.go**
- [ ] **Step 3: 跑测试通过**；router 挂载 /api/agent/connect；main.go 起 hub.Start
- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "feat(server): agent ws with ed25519 challenge auth and hub"
```

---

### Task 11: Nodes API

**Files:**
- Create: `xnc-server/internal/api/nodes.go`
- Test: `xnc-server/internal/api/nodes_test.go`

**Interfaces:**
- Consumes: Task 6 RequireAuth、Task 10 hub.IsOnline
- Produces: `GET /api/nodes`（JWT；只返回请求用户所在 cluster 的节点；data 为数组：`{id, name, clusterId, clusterName, hostname, osVersion, agentVersion, shellType, status, lastSeenAt}`，status 以 `hub.IsOnline || db.status` 合成，online 优先）；`GET /api/nodes/{id}`（404 NODE_NOT_FOUND / 403）；`POST /api/nodes/{id}/disable|enable`（owner+，置 status='disabled'，audit `node.disable`/`node.enable`）

- [ ] **Step 1: 写失败测试**（列表需 JWT；在线合成：直插 offline 行 + hub.Register 后 status 变 online；disable 后 enable 恢复；viewer 403——Phase 1 无第二用户，用直插 cluster_members(role='viewer') 用户登录测）
- [ ] **Step 2: 确认失败 → 实现 nodes.go**
- [ ] **Step 3: 跑测试通过**
- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "feat(server): nodes list/detail/disable/enable api"
```

---

### Task 12: Rust workspace 与 xnc-proto

**Files:**
- Create: `xnc-agent/Cargo.toml`（workspace: crates/xnc-proto、crates/xnc-agent）、`xnc-agent/crates/xnc-proto/Cargo.toml`、`xnc-agent/crates/xnc-proto/src/lib.rs`
- Test: `xnc-agent/crates/xnc-proto/src/lib.rs` 内 `#[cfg(test)]`

**Interfaces:**
- Consumes: `proto/messages.md` 黄金样本（Task 9）
- Produces: Rust 镜像类型（serde，`#[serde(tag = "type")]`）：

```rust
#[derive(Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "SCREAMING_SNAKE_CASE")]
pub enum ServerMsg {
    Challenge { nonce: String, expires_at: DateTime<Utc> },
    HelloAck,
    HeartbeatAck,
    Error { code: String, message: String },
}

#[derive(Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "SCREAMING_SNAKE_CASE")]
pub enum AgentMsg {
    ChallengeResponse { node_id: String, signature: String }, // payload 扁平字段
    Hello { node_id: String, hostname: String, agent_version: String },
    Heartbeat,
}
```

注意：Go 侧 Envelope 是 `{type, payload:{...}}` 两层，Rust 侧用 `#[serde(tag="type")]` + `payload` 内联自定义 serde 或直接手写 `Envelope { r#type, payload: Value }` + 手动分发——**以黄金样本测试为准**，两边序列化字节必须一致。

- [ ] **Step 1: 写失败测试**（把 Task 9 的黄金 JSON 字符串硬编码进 Rust 测试，assert 反序列化+再序列化相等）
- [ ] **Step 2: 确认失败 → 实现 lib.rs**
- [ ] **Step 3: `cargo test -p xnc-proto` 通过**
- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "feat(agent): rust workspace and proto mirror with golden tests"
```

---

### Task 13: Agent 身份与凭据存储

**Files:**
- Create: `xnc-agent/crates/xnc-agent/src/{identity.rs, credential.rs, machineid.rs}`
- Test: 同目录 `#[cfg(test)]`

**Interfaces:**
- Produces: `identity::load_or_create(dir: &Path) -> Identity`——`Identity { keypair: ed25519_dalek::SigningKey, node_id: Option<String> }`；私钥/公钥与 node_id 以 JSON 序列化后 DPAPI `CryptProtectData`（`CRYPTPROTECT_LOCAL_MACHINE`）写入 `<dir>/credential.bin`；加载失败（损坏/首次）则重建并覆盖
- Produces: `machineid::get() -> String`（注册表 `HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid`，windows crate RegGetValueW）
- Produces: `identity::public_key_b64(&Identity) -> String`

- [ ] **Step 1: 写失败测试**（tempdir：load_or_create 两次返回同一公钥；credential.bin 首字节非明文 JSON——断言文件内容不含 `"keypair"` 明文子串）
- [ ] **Step 2: 确认失败 → 实现**（windows crate feature `Win32_Security_Cryptography`、`Win32_System_Registry`）
- [ ] **Step 3: `cargo test -p xnc-agent` 通过**
- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "feat(agent): ed25519 identity with dpapi-protected credential store"
```

---

### Task 14: Agent Enroll 客户端

**Files:**
- Create: `xnc-agent/crates/xnc-agent/src/enroll.rs`
- Test: `xnc-agent/crates/xnc-agent/tests/enroll.rs`（wiremock）

**Interfaces:**
- Consumes: Task 13 Identity
- Produces: `enroll::run(server: &str, token: &str, id: &mut Identity) -> Result<String>`：POST `{server}/api/agent/enroll`，body `{"token","hostname","machineId","osVersion","agentVersion","publicKey"}`（hostname=env COMPUTERNAME，osVersion 用 `sysinfo` 或 `os_info` crate，agentVersion=env!("CARGO_PKG_VERSION")）；成功解析 `data.nodeId` 写回 Identity 并持久化；HTTP 非 2xx 时返回携带 error.code 的错误

- [ ] **Step 1: 写失败测试**（wiremock：200 成功路径持久化 node_id；400 ENROLLMENT_TOKEN_INVALID 时 Err 含 code）
- [ ] **Step 2: 确认失败 → 实现**（reqwest rustls-tls；danger_accept_invalid_certs 仅当 server 为 https://127.0.0.1 或 XNC_DEV_INSECURE=1）
- [ ] **Step 3: `cargo test -p xnc-agent` 通过**
- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "feat(agent): enrollment client"
```

---

### Task 15: Agent 连接、挑战与 HELLO

**Files:**
- Create: `xnc-agent/crates/xnc-agent/src/connect.rs`
- Test: `xnc-agent/crates/xnc-agent/tests/connect.rs`

**Interfaces:**
- Consumes: Task 12 proto、Task 13 Identity
- Produces: `connect::handshake(ws: WebSocketStream<...>, id: &Identity) -> Result<Conn>`：收 CHALLENGE → 校验 expires_at（>now）→ 对 nonce 的 UTF-8 字节 ed25519 签名（base64）→ 发 ChallengeResponse{node_id, signature} → 收 HELLO_ACK（若先收 Error 则 Err(code)）→ 发 Hello{node_id, hostname, agent_version}（顺序按 spec §13：先 HELLO 后等待——**以 spec 为准：challenge 通过后 Agent 发 HELLO，Server 回 HELLO_ACK**；测试按此断言顺序）
- Produces: `connect::connect_once(server: &str, id: &Identity) -> Result<Conn>`（URL: `{server}/api/agent/connect` 的 wss 形式，tokio-tungstenite rustls）

- [ ] **Step 1: 写失败测试**（tokio 内起 tungstenite TCP listener 假 server：发 CHALLENGE(nonce="aGVsbG8=") → 期待收到 ChallengeResponse（用测试钥独立验签）→ 期待 Hello → 回 HelloAck → 断言 Ok；另测：假 server 发 Error → Err）
- [ ] **Step 2: 确认失败 → 实现 connect.rs**
- [ ] **Step 3: `cargo test -p xnc-agent` 通过**
- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "feat(agent): ws connect with challenge signature and hello"
```

---

### Task 16: 心跳与重连循环

**Files:**
- Create: `xnc-agent/crates/xnc-agent/src/run.rs`
- Test: `xnc-agent/crates/xnc-agent/tests/run.rs`

**Interfaces:**
- Consumes: Task 14 enroll、Task 15 connect
- Produces: `run::run_agent(cfg: AgentConfig) -> !`：无 node_id → enroll；循环 { connect_once → 每 30s 发 Heartbeat、收 HeartbeatAck/Error；断开 → backoff 1s,2s,5s,10s,30s(封顶) 重连 }；心跳间隔与 backoff 表可注入（测试用 50ms/10ms）
- Produces: `AgentConfig { server: String, token: Option<String>, heartbeat: Duration, backoffs: Vec<Duration>, data_dir: PathBuf }`

- [ ] **Step 1: 写失败测试**（假 server：完成握手后主动断开 → 断言 agent 在缩短 backoff 内重连 ≥2 次；假 server 连续回 HeartbeatAck → 断言收到 ≥3 次心跳）
- [ ] **Step 2: 确认失败 → 实现 run.rs**（tokio::select! 心跳定时器 vs 读循环；读循环退出即断线）
- [ ] **Step 3: `cargo test -p xnc-agent` 通过**
- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "feat(agent): heartbeat loop with exponential backoff reconnect"
```

---

### Task 17: Windows Service 宿主与 CLI 参数

**Files:**
- Create: `xnc-agent/crates/xnc-agent/src/{main.rs, service.rs, cli.rs}`
- Test: `xnc-agent/crates/xnc-agent/src/cli.rs` 内 `#[cfg(test)]`

**Interfaces:**
- Consumes: Task 16 run_agent
- Produces: clap 命令：`xnc-agent.exe install --server <url> [--token <t>] [--data-dir C:\ProgramData\XNC]`（写 `<data-dir>\agent.json`，`sc create XNCAgent binPath= <exe> run start= auto`）、`remove`、`run`（服务入口/前台均可）、`upgrade`（占位：打印"manual upgrade"退出 0）、`status`
- Produces: `AgentConfig::load(path) -> Result<AgentConfig>`；service.rs 用 windows-service crate 定义 `XNCAgent` 服务（Automatic，失败恢复 restart）

- [ ] **Step 1: 写失败测试**（cli 解析：`install --server https://x --token t` → 子命令与 flag 正确；AgentConfig 序列化/加载 roundtrip 用 tempdir）
- [ ] **Step 2: 确认失败 → 实现 main/service/cli**
- [ ] **Step 3: `cargo test -p xnc-agent` 通过；`cargo build --release` 产出 exe（msvc target）**
- [ ] **Step 4: 真机手工冒烟（NODE_MAIN，管理员 PowerShell）**

```powershell
.\xnc-agent.exe install --server https://control.xnc.app --token <t>
Get-Service XNCAgent   # Running
```

- [ ] **Step 5: Commit**

```bash
git add -A && git commit -m "feat(agent): windows service host and install cli"
```

---

### Task 18: xnc CLI（login / token create / node list）

**Files:**
- Create: `xnc-cli/cmd/xnc/main.go`、`xnc-cli/internal/client/client.go`、`xnc-cli/internal/output/output.go`
- Test: `xnc-cli/internal/client/client_test.go`、`output_test.go`

**Interfaces:**
- Produces: `client.APIClient{BaseURL, Token string}`——`Login(email, pw) (token string, err)`、`CreateEnrollmentToken(clusterID string) (*TokenResp, error)`、`ListNodes() ([]Node, error)`（解析 server envelope，error 携带 code）
- Produces: `output.PrintJSON(v)`、`output.PrintNodesTable(nodes)`；退出码 helper：`exit.ErrCode(err)` 映射 UNAUTHORIZED→240、FORBIDDEN→241、*_NOT_FOUND→244、其他→250
- Produces: 命令 `xnc login --server <url> <email>`（提示输密码，存 `%USERPROFILE%\.xnc\config.json`）、`xnc token create <clusterId>`、`xnc node list [--json]`；全局 `--server/--token/--output` 覆盖 env `XNC_SERVER/XNC_TOKEN`
- Produces: `Node{Name, ClusterName, Status, Hostname, OsVersion, AgentVersion, LastSeenAt string}` JSON tags 与 server API 字段一致

- [ ] **Step 1: 写失败测试**（httptest 假 server：Login 解析 envelope；ListNodes 401 → err.Code==UNAUTHORIZED → exit 240；golden：PrintJSON 输出与期望字符串相等）
- [ ] **Step 2: 确认失败 → 实现**（cobra；密码读取 golang.org/x/term.ReadPassword）
- [ ] **Step 3: `go test ./...` 通过；`go run ./cmd/xnc node list --json` 对本地 server 冒烟**
- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "feat(cli): login, token create, node list with json envelope"
```

---

### Task 19: Web 最小界面（登录 + 节点列表）

**Files:**
- Create: `web/`（`npm create vite@latest web -- --template react-ts`）、`web/src/api.ts`、`web/src/pages/{Login,Nodes}.tsx`、`web/src/App.tsx`、`web/vite.config.ts`（proxy `/api` → `http://127.0.0.1:8080`）

**Interfaces:**
- Consumes: Task 6/11 REST API
- Produces: `/login`（email+password → 存 localStorage `xnc_token`）；`/`（Nodes 表格：Name/Cluster/Status 徽标/Agent/LastSeen，5s 轮询，401 跳回 login）

- [ ] **Step 1: 脚手架与 api.ts**

```typescript
export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const r = await fetch(path, {
    ...init,
    headers: { "Content-Type": "application/json", Authorization: `Bearer ${localStorage.getItem("xnc_token") ?? ""}`, ...init?.headers },
  });
  const body = await r.json();
  if (!body.ok) throw Object.assign(new Error(body.error.message), { code: body.error.code });
  return body.data as T;
}
export const login = (email: string, password: string) =>
  api<{ token: string }>("/api/auth/login", { method: "POST", body: JSON.stringify({ email, password }) });
export const listNodes = () => api<Node[]>("/api/nodes");
```

- [ ] **Step 2: Login/Nodes 页面 + 路由 + 轮询（setInterval 5000，组件卸载清理）**
- [ ] **Step 3: 手工验证**：`make db-up && make server && make web` → 登录 → 列表空态；启动 agent 后 5s 内出现 online
- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "feat(web): minimal login and node list"
```

---

### Task 20: 本地 E2E 与 SRV 部署

**Files:**
- Create: `scripts/dev-up.sh`（db-up + server + 提示）、`deploy/xnc-server.service`、`deploy/Caddyfile`、`deploy/srv-init.sh`、`docs/dev.md`

**Interfaces:**
- Consumes: 全部前序任务；`config.env`（SRV_HOST/SRV_SSH_USER/SRV_SSH_PASSWORD/SRV_DOMAIN；应用账号 XNC_ADMIN_*）

- [ ] **Step 1: 本地 E2E（Scenario A/G 本地版）**

```bash
make db-up
XNC_DB_URL=postgres://xnc:xnc@localhost:5432/xnc XNC_JWT_SECRET=devsecret \
XNC_BOOTSTRAP_ADMIN_EMAIL=i@terry.ee XNC_BOOTSTRAP_ADMIN_PASSWORD=tian8149 make server
xnc login --server http://127.0.0.1:8080 i@terry.ee
xnc token create <clusterId>
.\xnc-agent.exe install --server http://127.0.0.1:8080 --token xnc_enroll_...
xnc node list    # web-01... ONLINE
# 停服务：sc stop XNCAgent → 90s 内 xnc node list 显示 offline → sc start 恢复 online
```

Expected: 对应 spec Scenario A（注册上线）与 G（断连/恢复）本地全通过。

- [ ] **Step 2: SRV 部署文件**

```ini
# deploy/xnc-server.service
[Unit]
Description=XNC Control Server
After=network-online.target postgresql.service
Wants=network-online.target
[Service]
Environment=XNC_DB_URL=postgres://xnc:REPLACE@127.0.0.1:5432/xnc
Environment=XNC_JWT_SECRET=REPLACE
Environment=XNC_LISTEN=127.0.0.1:8080
Environment=XNC_BOOTSTRAP_ADMIN_EMAIL=i@terry.ee
Environment=XNC_BOOTSTRAP_ADMIN_PASSWORD=REPLACE
ExecStart=/usr/local/bin/xnc-server
Restart=always
User=xnc
[Install]
WantedBy=multi-user.target
```

```caddyfile
# deploy/Caddyfile
control.xnc.app {
    reverse_proxy 127.0.0.1:8080
}
```

`deploy/srv-init.sh`：apt 装 postgresql + caddy；建库/用户（随机密码写入 unit）；`GOOS=linux GOARCH=amd64 go build` 上传二进制；systemctl enable --now xnc-server caddy。

- [ ] **Step 3: 真机全链路验收（读 config.env 凭据 SSH 上 SRV 执行）**

```bash
ssh root@control.xnc.app 'systemctl status xnc-server caddy --no-pager'
curl -s https://control.xnc.app/healthz     # ok
```

然后 NODE_MAIN：`xnc-agent.exe install --server https://control.xnc.app --token <生产 token>` → `xnc login --server https://control.xnc.app i@terry.ee` → `xnc node list` 显示 online。防火墙确认：SRV 仅开 22/443。

Expected: 公网 TLS 全链路通；节点公网零入站（Scenario B）。

- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "feat(deploy): srv systemd/caddy deployment and e2e scripts"
```

---

## Self-Review 结论

* **Spec 覆盖**（§53 Phase 1 条目 → 任务）：Bootstrap Admin/JWT Login→T4/5/6；Windows Service→T17；Enrollment→T7/8/14；Device Identity→T9/10/13/15；WebSocket Connection→T10/15；Heartbeat→T11/16；online/offline→T10/11；Node 列表（Web+CLI）→T11/18/19。✓
* **无占位符**：所有步骤含具体命令/代码/期望输出；T3 的 DDL 以 Interfaces 字段清单形式给出（执行者逐字段落 SQL，非 TBD）。✓
* **类型一致**：Envelope/CHALLENGE/HELLO 字段名在 Go（T9）与 Rust（T12）经黄金样本强制一致；`Hub.Register/IsOnline`（T10）与 nodes.go（T11）调用一致；`AgentConfig`（T16）在 T17 复用。✓
* **顺序依赖**：T4 依赖 T5 的 HashPassword（计划已注明可对调执行顺序，实现时按 T5 先）。
