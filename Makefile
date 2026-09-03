.PHONY: test vet lint vuln build integration bench bench-postgres loadtest up down migrate monitoring-up monitoring-down monitoring-test

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

monitoring-up:
	@test "$${#GRAFANA_ADMIN_PASSWORD}" -ge 32 || (echo 'Set GRAFANA_ADMIN_PASSWORD to a random value of at least 32 characters' >&2; exit 1)
	docker compose -f compose.yaml -f compose.monitoring.yaml up -d --build

monitoring-down:
	docker compose -f compose.yaml -f compose.monitoring.yaml down

monitoring-test:
	docker compose -f compose.yaml -f compose.monitoring.yaml run --rm --no-deps --entrypoint /bin/promtool prometheus test rules /etc/prometheus/alerts.test.yml
