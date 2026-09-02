.PHONY: test vet lint vuln build integration bench bench-postgres loadtest up down migrate

test:
	go test -race -shuffle=on -count=1 ./...

vet:
	go vet ./...

lint:
	golangci-lint run ./...

vuln:
	go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...

build:
	go build -trimpath -o bin/usage-billing ./cmd/usage-billing

bench:
	go test -run='^$$' -bench=BenchmarkHTTPHandler -benchmem -count=10 -cpu=1 ./internal/httpapi

bench-postgres:
	@test -n "$$TEST_DATABASE_URL" || (echo 'TEST_DATABASE_URL must point to a disposable test database' >&2; exit 1)
	go test -tags=integration -run='^$$' -bench=BenchmarkStore -benchmem -count=10 -cpu=1 -timeout=5m ./internal/postgres

loadtest:
	go run ./cmd/loadtest -allow-demo-writes -requests 500 -concurrency 8

integration:
	@test -n "$$TEST_DATABASE_URL" || (echo 'TEST_DATABASE_URL must point to an isolated, migrated test database' >&2; exit 1)
	go test -race -tags=integration -shuffle=on -count=1 -timeout=3m ./...

up:
	docker compose up -d --build

down:
	docker compose down

migrate:
	docker compose run --rm migrate up
