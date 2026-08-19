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
