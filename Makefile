# 代码模块统一在 src/ 下（2026-09-09 重构）；bin/（产物）、deploy/（运维）
# 留在仓库根。
MODULES := src/proto src/server src/agent src/cli src/mockagent src/shellsmoke

# 版本注入（版本单一来源）：构建时经 -ldflags 注入，缺省回落 0.0.0-dev。
# 例: make build-prod VERSION=0.4.6   （安装器打包版本须与注入值一致）
VERSION ?= 0.0.0-dev
AGENT_LDFLAGS := -X xnc/agent/machineinfo.Version=$(VERSION)
SERVER_LDFLAGS := -X xnc/server/internal/version.Version=$(VERSION)

.PHONY: build build-prod test fmt sqlc
build:
	@for m in $(MODULES); do (cd $$m && go build ./...); done
build-prod: build-agent build-server
build-agent:
	cd src/agent && go build -ldflags "$(AGENT_LDFLAGS)" -o ../../bin/xnc-agent$(EXE) ./cmd/xnc-agent
build-server:
	cd src/server && go build -ldflags "$(SERVER_LDFLAGS)" -o ../../bin/xnc-server$(EXE) ./cmd/xnc-server

# 安装器频道（设计 §3.1）：stable 产 xnc-setup-<ver>.exe，dev 产 -dev- 变体。
CHANNEL ?= stable
# 安装器（设计 §3/§12）：五二进制 → ISCC 打包 bin/xnc-setup[-dev]-<ver>.exe，
# sha256 输出到 stdout + sidecar。build.ps1 内含各二进制的既有构建路径
# （build-agent 旗标 / cli、shellhost go build / native build.bat）。
.PHONY: installer
installer:
	powershell -NoProfile -ExecutionPolicy Bypass -File src/installer/build.ps1 -Version $(VERSION) -Channel $(CHANNEL)
test:
	@for m in $(MODULES); do (cd $$m && go test ./...); done
fmt:
	@for m in $(MODULES); do (cd $$m && gofmt -l -w .); done
sqlc:
	cd src/server && sqlc generate

.PHONY: dev-up dev-down
dev-up:
	cd deploy && docker compose -f docker-compose.yml -f docker-compose.dev.yml up -d --build
dev-down:
	cd deploy && docker compose -f docker-compose.yml -f docker-compose.dev.yml down -v

EXE :=
ifeq ($(OS),Windows_NT)
EXE := .exe
endif

.PHONY: load
load:
	cd src/cli && go build -o ../../bin/xnc$(EXE) .
	cd src/mockagent && go build -o ../../bin/mockagent$(EXE) .
	bin/mockagent$(EXE) --server http://127.0.0.1:8080 --token $$(bin/xnc$(EXE) token create default --max-uses 1000 --json | sed -n 's/.*"token":"\([^"]*\)".*/\1/p') --count $(N)
