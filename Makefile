MODULES := proto server agent cli mockagent shellsmoke

# 版本注入（版本单一来源）：构建时经 -ldflags 注入，缺省回落 0.0.0-dev。
# 例: make build-prod VERSION=0.4.6   （bundle 版本须与注入值一致）
VERSION ?= 0.0.0-dev
AGENT_LDFLAGS := -X xnc/agent/machineinfo.Version=$(VERSION)
SERVER_LDFLAGS := -X xnc/server/internal/version.Version=$(VERSION)

.PHONY: build build-prod test fmt sqlc
build:
	@for m in $(MODULES); do (cd $$m && go build ./...); done
build-prod: build-agent build-server
build-agent:
	cd agent && go build -ldflags "$(AGENT_LDFLAGS)" -o ../bin/xnc-agent$(EXE) ./cmd/xnc-agent
build-server:
	cd server && go build -ldflags "$(SERVER_LDFLAGS)" -o ../bin/xnc-server$(EXE) ./cmd/xnc-server
test:
	@for m in $(MODULES); do (cd $$m && go test ./...); done
fmt:
	@for m in $(MODULES); do (cd $$m && gofmt -l -w .); done
sqlc:
	cd server && sqlc generate

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
	cd cli && go build -o ../bin/xnc$(EXE) .
	cd mockagent && go build -o ../bin/mockagent$(EXE) .
	bin/mockagent$(EXE) --server http://127.0.0.1:8080 --token $$(bin/xnc$(EXE) token create default --max-uses 1000 --json | sed -n 's/.*"token":"\([^"]*\)".*/\1/p') --count $(N)
