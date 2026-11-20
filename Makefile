.PHONY: run test test-integration vet build db-up

# .env dibuat otomatis (kunci acak + admin) jika belum ada
run: .env db-up
	go run ./cmd/server

.env:
	go run ./cmd/envinit

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
