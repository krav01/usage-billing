# Pre-publication verification

## v0.3.0 feature verification — 2026-09-03

Feature commit `161016a4ebdc22cdfaedee0c71bccec64f621098` passed
[Verify Usage Billing #85](https://github.com/krav01/usage-billing/actions/runs/33803606382)
and [CodeQL](https://github.com/krav01/usage-billing/actions/runs/33803606384).
Its [upgrade PR](https://github.com/krav01/usage-billing/pull/13) also passed
[Dependency Review](https://github.com/krav01/usage-billing/actions/runs/33803213120).

- Go race tests, vet, lint and reachable-vulnerability scanning.
- Real PostgreSQL integration, concurrent acceptance, rollback, quarantine,
  generation-checked retry, legacy rows and durable request correlation.
- Migration round-trip, authenticated Docker API, load settlement, database
  crash recovery, full-row backup/restore and monitoring checks.
- A newly created CI database populated at schema v2 with posted, pending and
  quarantined events. Upgrade to v3 preserved every pre-existing field;
  repeating `up` made no further change.
- Starting the new API at 9000 micro-USD preserved old charges at 1000.
  Recovery retained its generation, and new request correlation survived API
  restart/replay. Final totals were four ledger entries, seventeen units and
  33000 micro-USD, with no pending work.

These are results for the feature commit on isolated synthetic data. They do not
claim that an existing user database was migrated, that old/new binaries ran
concurrently, or that large-database lock times were measured. The GitHub Release
records the separate final release commit and its verification runs.

## v0.2.0 feature verification — 2026-09-03

Feature commit `deb21a1b9ff38987fbedf9956a90239605dd80e2` passed
[Verify Usage Billing](https://github.com/krav01/usage-billing/actions/runs/33771698211),
[CodeQL](https://github.com/krav01/usage-billing/actions/runs/33771698200), and
[Dependency Review](https://github.com/krav01/usage-billing/actions/runs/33771698186).
The same source tree was merged at `c1e4c85904af5758989c92e94eeefaf34349edc5`.

- Go 1.27.1 race tests, vet, golangci-lint v2.13.2, and vulnerability scanning.
- Real PostgreSQL integration, migration round-trip, concurrent queue admission,
  replay at capacity, and exact transaction outcomes.
- Three complete 500-event, eight-client load runs with exact settlement checks.
- Database crash during worker processing, rollback, and recovery using the same
  API process: two unique ledger entries, ten units, 10000 micro-USD, zero pending.
- Successful promtool checks/tests, authenticated scraping, six provisioned Grafana
  panels, healthy datasource, valid panel queries, and denied anonymous access.
- Secret files present on the runner but excluded from Git and the application
  build context, with a build-stage assertion.

CI exposed and fixed Compose secret/read-only incompatibility, promtool's writable
temporary-directory requirement, and a container-discovery race after SIGKILL.
The crash test now waits for confirmed exit and restarts the exact database ID.
These are dated results for the listed commit, not claims about future commits,
production performance, financial correctness, or permanent-disk-loss recovery.

## Initial verification — 2026-09-02

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
