# Pre-publication verification

Checked on 2026-09-02 using Go 1.26.6 on Linux.

| Check | Result |
| --- | --- |
| `go test -race -shuffle=on -count=1 ./...` | Passed |
| `go vet -tags=integration ./...` | Passed |
| `golangci-lint run ./...` (v2.12.2) | No issues |
| `golangci-lint run --build-tags=integration ./...` | No issues |
| `go build ./...` | Passed |
| `go mod verify` | All modules verified |
| `govulncheck ./...` (v1.6.0) | No reachable known vulnerabilities found |
| `go test -tags=integration -run='^$' ./...` | Integration tests compile; not executed |

The initial dependency scan found GO-2026-5970 in `golang.org/x/text` v0.29.0.
The dependency was upgraded to v0.39.0 and the scan passed afterward.

PostgreSQL integration, migration up/down/up, and Docker API smoke tests have
**not yet been executed**: Docker and PostgreSQL were unavailable in the local
verification environment. The GitHub Actions workflow defines these checks on
isolated disposable databases. A successful compilation is not evidence that
these runtime checks pass. No throughput or production-readiness claim is made.
