# Usage Billing

My portfolio service for metered API usage, written in Go with PostgreSQL.
It accepts usage events, freezes their unit price, and records charges asynchronously.
This is an educational project: no real customers, invoices, taxes, or payments.

**Author:** Vladimir Krauchuk ([@krav01](https://github.com/krav01)).

## What this demonstrates

- Idempotency: an event ID identifies one immutable input, not one HTTP attempt.
- Acceptance transaction: the usage event and its work item commit together.
- A concurrent worker queue built with PostgreSQL row locks and `SKIP LOCKED`.
- An immutable ledger entry per event, committed with queue removal.
- Integer pricing, overflow checks, and exact decimal-string aggregate totals.
- Strict JSON validation, a trusted-producer bearer token, bounded requests, and graceful shutdown.
- Bounded queue admission, worker retry backoff, and verified database-crash recovery.
- Full-service load measurements and optional local monitoring with tested alerts.

The database effects are idempotent; this is **not** a claim of exactly-once network delivery.
The [design notes](docs/ARCHITECTURE.md) explain transaction boundaries and deliberate limits.
See [verification evidence](docs/VERIFICATION.md) and
[reproducible benchmarks](docs/PERFORMANCE.md) for measured scope and limitations.
For a guided walkthrough, see the [demo script](docs/DEMO.md) and
[v0.2.0 release notes](docs/releases/v0.2.0.md).

## Run the isolated demo

Prerequisites: Docker with Compose, make, and OpenSSL. Run from this repository:

```bash
export POSTGRES_PASSWORD=$(openssl rand -hex 24)
export BILLING_API_TOKEN=$(openssl rand -hex 32)
make up
```

Compose uses its own database and volume. The HTTP API is bound to `127.0.0.1:8080`
and PostgreSQL to `127.0.0.1:54329`. Keep the generated password if restarting an
existing volume: PostgreSQL initialization does not change its stored password.
The stack uses a normal Docker bridge so these localhost port mappings work;
it does not block outbound network access from containers.
Never point this demo's migration command at an existing or production database.

```bash
curl --fail-with-body http://127.0.0.1:8080/healthz
curl --fail-with-body http://127.0.0.1:8080/readyz
curl --fail-with-body -i http://127.0.0.1:8080/v1/events \
  -H "Authorization: Bearer $BILLING_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"event_id":"demo_001","customer_id":"demo_customer","meter":"api_calls","units":7}'
```

The first request returns `202`. Repeat the exact request for `200` and the
original frozen price; use the same ID with different units for `409`.

```bash
curl --fail-with-body http://127.0.0.1:8080/v1/events/demo_001 \
  -H "Authorization: Bearer $BILLING_API_TOKEN"
curl --fail-with-body http://127.0.0.1:8080/v1/customers/demo_customer/summary \
  -H "Authorization: Bearer $BILLING_API_TOKEN"
make down
```

Summary amounts and units include only processed events. Pending and processed
counts are separate, so a freshly accepted event need not appear in the amount
yet. The default rate is 1000 micro-USD per call: seven calls produce `"7000"`
micro-USD after processing. Aggregate amounts/units are decimal strings so JSON
clients do not silently round large totals; individual event integers require
an int64-capable JSON decoder for full precision.

`make down` stops the demo without deleting its database volume. Deleting that
volume or applying a down migration destroys demo data; neither is part of the
normal startup path.

## API

| Method | Path | Purpose |
| --- | --- | --- |
| POST | `/v1/events` | Accept new usage or replay an earlier event |
| GET | `/v1/events/{event_id}` | Return frozen pricing and processing state |
| GET | `/v1/customers/{customer_id}/summary` | Exact processed totals and queue counts |
| GET | `/healthz` | Process liveness, public |
| GET | `/readyz` | Bounded database reachability check, public |
| GET | `/metrics` | Fixed-label HTTP, queue, and worker metrics, authenticated |

Business endpoints and metrics require a bearer token. This token represents
one trusted internal producer, **not** tenant-level authorization. IDs contain
1–64 ASCII letters, digits, underscores, or hyphens. The only meter is `api_calls`.
Units must be a positive int64 and the accepted price multiplication must fit in
int64. JSON bodies are limited to 16 KiB, with unknown or duplicate fields rejected.

See [OpenAPI](api/openapi.yaml) for request and response schemas and
[Operations](docs/OPERATIONS.md) for metric semantics and example alerts.
See [Failure and recovery verification](docs/RESILIENCE.md) for the CI database-crash scenario and its limits.
An optional [local Prometheus/Grafana demo](docs/MONITORING.md) includes a provisioned dashboard and tested alerts.

## Configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `DATABASE_URL` | required | New demo database connection, never logged |
| `BILLING_API_TOKEN` | required | 32–4096 printable ASCII bytes, no whitespace; generate randomly |
| `BILLING_RATE_MICROS` | `1000` | Positive integer micro-USD per API call |
| `BILLING_MAX_PENDING_EVENTS` | `10000` | Maximum pending queue depth, 1–1000000; shared across API instances |
| `HTTP_ADDR` | `127.0.0.1:8080` | Listener; Compose binds inside its container |
| `WORKER_INTERVAL` | `100ms` | Polling delay, 10ms–1m |
| `WORKER_BATCH` | `100` | Batch size, 1–1000 |

The pool is capped at eight connections. Connection setup, database statements,
HTTP requests, and graceful shutdown have time bounds. Changing the configured
rate affects new events only. Startup does not automatically apply SQL migrations.
Use the dedicated one-shot migration service in Compose.

## Development and verification

Go 1.27.1 is the pinned toolchain for CI and both Docker builds. pgx is the only direct runtime dependency;
HTTP routing, logging, JSON, metrics, and tests use the standard library.

```bash
make test
make vet
make lint
make vuln
```

Integration tests are separate and must target a disposable migrated database:

```bash
export TEST_DATABASE_URL="postgres://billing_demo:${POSTGRES_PASSWORD}@127.0.0.1:54329/usage_billing?sslmode=disable"
make integration
```

The integration command fails when its database configuration is absent; it does
not silently count skipped database tests as a successful integration run.
CI provisions its own PostgreSQL service and applies versioned migrations before testing.

Tests cover new/replayed/conflicting events, concurrent acceptance and processing,
frozen prices, integer boundaries, authentication, request validation, and cancellation.
Passing tests are evidence of the exercised cases, not proof of financial correctness.

## Boundaries

This repository does not operate a payment provider, spend funds, or modify
Sunday System. No performance figures are claimed without recorded measurements.
Production work would include per-tenant authorization, TLS termination, rate
limits and per-tenant quotas, separate least-privilege database roles,
backups and restore drills, retention, invoice/refund rules, and a security review.
The demo token grants access to every demo customer; never expose this setup publicly.
