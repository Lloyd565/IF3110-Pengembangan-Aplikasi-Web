.PHONY: run test test-integration vet build db-up

run:
	go run ./cmd/server

test:
	go test ./...

test-integration:
	go test ./internal/auth ./internal/httpx -run Integration -count=1 -v

vet:
	go vet ./...

build:
	go build ./...

db-up:
	docker compose up -d --wait
