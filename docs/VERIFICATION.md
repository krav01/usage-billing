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

## GitHub Actions verification

Production-code commit `4302b9adc9d3049b693ccde62304ef6ede908981` passed
[Verify Usage Billing](https://github.com/krav01/usage-billing/actions/runs/33659761164)
and [CodeQL](https://github.com/krav01/usage-billing/actions/runs/33659761155):

- Go quality, including race detection and vulnerability scanning.
- Real PostgreSQL integration tests and migration up/down/up.
- Docker build, authenticated API calls, processing, and replay after API restart.

The initial Docker smoke run exposed non-working localhost port mappings with
an internal-only Docker bridge. The corrected bridge configuration retains
loopback-only published ports, but does not claim outbound network isolation.

These checks use isolated synthetic data, not production traffic or payments.
See the workflow runs for later changes; this is a dated verification record,
not a claim that every future commit has passed.
