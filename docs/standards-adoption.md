# Development standards adoption

This project adopts `krav01/dev-standards` baseline version `1.1.0` at the `high-load-critical` maturity level.

## Why high-load-critical

Although this repository is portfolio-only and not a payment processor, its correctness model includes metering, durable usage state, idempotency/replay behavior, PostgreSQL transactions, recovery, and migration safety. Those properties justify the stricter baseline.

## Existing controls retained

The project already has stronger local verification than the shared baseline in several areas:
- unit tests with race detector;
- `go vet`, `golangci-lint`, and `govulncheck`;
- real PostgreSQL integration tests;
- migration up/down/up verification;
- Docker smoke tests;
- restart/replay durability checks;
- PostgreSQL outage/recovery validation;
- backup/restore testing;
- populated schema upgrade tests;
- performance benchmark artifacts;
- CodeQL, Dependency Review, and Dependabot.

These local checks remain authoritative because they encode project-specific failure modes that a generic reusable workflow cannot safely replace.

## Shared workflow exception

This repository is public while `krav01/dev-standards` is private. GitHub Actions does not permit a public caller to use a reusable workflow from a private repository. For that reason:
- reusable central CI is not enabled;
- central standards drift workflow is not enabled;
- the adopted baseline version is recorded explicitly in `.standards.yml`.

## Upgrade process

Standards upgrades use a dedicated PR:
1. review the `dev-standards` changelog;
2. compare new rules against existing local checks;
3. update `.standards.yml`;
4. add only missing applicable controls;
5. preserve stronger project-specific checks;
6. document intentional exceptions with a revisit trigger.
