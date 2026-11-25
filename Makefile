.PHONY: run worker test test-integration vet build db-up frontend observability-up observability-down

# .env dibuat otomatis (kunci acak + admin) jika belum ada
run: .env db-up frontend
	go run ./cmd/server

worker: .env
	go run ./cmd/worker

.env:
	go run ./cmd/envinit

test:
	go test ./...

test-integration:
	go test ./internal/auth ./internal/httpx -run Integration -count=1 -v

vet:
	go vet ./...

build: frontend
	go build ./...

frontend:
	cd frontend && npm ci && npm run build

observability-up:
	docker compose --profile observability up -d

observability-down:
	docker compose --profile observability stop otel-collector prometheus loki tempo grafana

db-up:
	docker compose up -d --wait
