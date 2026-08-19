MODULES := proto server agent cli mockagent

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
