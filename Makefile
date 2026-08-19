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
