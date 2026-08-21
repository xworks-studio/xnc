MODULES := proto server agent cli mockagent shellsmoke

.PHONY: build test fmt sqlc
build:
	@for m in $(MODULES); do (cd $$m && go build ./...); done
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

.PHONY: e2e
e2e:
	bash scripts/e2e_phase1.sh

.PHONY: e2e2
e2e2:
	bash scripts/e2e_phase2.sh

.PHONY: e2e3
e2e3:
	bash scripts/e2e_phase3.sh

.PHONY: e2e4
e2e4:
	bash scripts/e2e_phase4.sh

.PHONY: e2e5
e2e5:
	bash scripts/e2e_phase5.sh

EXE :=
ifeq ($(OS),Windows_NT)
EXE := .exe
endif

.PHONY: load
load:
	cd cli && go build -o ../bin/xnc$(EXE) .
	cd mockagent && go build -o ../bin/mockagent$(EXE) .
	bin/mockagent$(EXE) --server http://127.0.0.1:8080 --token $$(bin/xnc$(EXE) token create default --max-uses 1000 --json | sed -n 's/.*"token":"\([^"]*\)".*/\1/p') --count $(N)
