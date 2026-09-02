.PHONY: test vet lint vuln build integration up down migrate

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

integration:
	@test -n "$$TEST_DATABASE_URL" || (echo 'TEST_DATABASE_URL must point to an isolated, migrated test database' >&2; exit 1)
	go test -race -tags=integration -shuffle=on -count=1 -timeout=3m ./...

up:
	docker compose up -d --build

down:
	docker compose down

migrate:
	docker compose run --rm migrate up
