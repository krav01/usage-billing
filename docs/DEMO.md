# Guided portfolio demo

This service turns synthetic API usage into a durable, priced ledger entry.
It does not charge a card, create an invoice, or handle real customer data.
Use an isolated local machine and the default price of 1000 micro-USD per call.

## 1. Start a fresh demo

Prerequisites: Docker Compose v2, make, OpenSSL, curl, and jq. Go matching `go.mod`
is also needed for the optional load client. From this repository:

```bash
export POSTGRES_PASSWORD="$(openssl rand -hex 24)"
export BILLING_API_TOKEN="$(openssl rand -hex 32)"
export GRAFANA_ADMIN_PASSWORD="$(openssl rand -hex 24)"
make monitoring-up
curl --fail-with-body --retry 20 --retry-delay 1 --retry-all-errors \
  --retry-max-time 60 --max-time 3 http://127.0.0.1:8080/readyz
```

If a demo volume already exists, retain its original credentials instead of
regenerating them. Startup builds the application and runs versioned migrations
only against this Compose database. Never supply an existing/production database.
For an existing demo from an earlier version, use the explicit
[upgrade procedure](UPGRADING.md) before continuing this walkthrough.

## 2. Accept usage and replay it

Generate an independent event/customer namespace for each walkthrough:

```bash
demo_id="demo_$(openssl rand -hex 8)"
body=$(jq -nc --arg id "$demo_id" \
  '{event_id:$id,customer_id:$id,meter:"api_calls",units:7}')
curl --fail-with-body --max-time 5 -i http://127.0.0.1:8080/v1/events \
  -H "Authorization: Bearer $BILLING_API_TOKEN" \
  -H 'Content-Type: application/json' --data "$body"
# Repeat the identical event ID and payload.
curl --fail-with-body --max-time 5 -i http://127.0.0.1:8080/v1/events \
  -H "Authorization: Bearer $BILLING_API_TOKEN" \
  -H 'Content-Type: application/json' --data "$body"
```

The first request should return `202`, the replay `200`; both retain the original
unit price. Reusing the ID with different units returns `409`, not another charge.
A full queue returns `503 queue_full` for new events, but permits identical replays.
Clients should respect `Retry-After`, use bounded retries, and retain the event ID.

The event response also retains its original `request_id`; each HTTP response's
`X-Request-ID` is new. Use the original ID to find worker processing, retry and
quarantine logs across restarts; see [durable request correlation](OPERATIONS.md#durable-request-correlation).

## 3. Observe asynchronous settlement

```bash
curl --fail-with-body --max-time 5 \
  -H "Authorization: Bearer $BILLING_API_TOKEN" \
  "http://127.0.0.1:8080/v1/customers/$demo_id/summary" | jq
```

Repeat the read after a short interval if the event is still pending. After the
worker commits, expect `processed: 1`, `pending: 0`, `units: "7"`, and
`amount_micros: "7000"`. The replay must not increase those totals. The amount is
an exact decimal string in micro-USD, not floating-point dollars.

## 4. Show monitoring and evidence

Open <http://127.0.0.1:3000>, sign in as `admin` using the generated Grafana password,
and open **Usage Billing - Operations**. Then optionally run:

```bash
make loadtest
make monitoring-test
```

The load client creates 500 additional synthetic events for its own customer and
asserts exact settlement. After at least two scrapes, inspect the six dashboard
panels. Short queue peaks may be missed between scrapes. The load report's HTTP
p95/p99 excludes asynchronous worker processing and is not a capacity guarantee.

Show the **Docker API smoke** job's crash-recovery assertions and the **PostgreSQL
integration** job's concurrent queue-limit tests in GitHub Actions. The destructive
database-crash script deliberately refuses to run outside isolated hosted CI; do
not disable that guard for a presentation. See [resilience](RESILIENCE.md),
[operations](OPERATIONS.md), and [monitoring](MONITORING.md) for precise limits.

## 5. Stop without erasing demo data

```bash
make monitoring-down
```

Database/history volumes and local secret files remain. There is no public
deployment, external alert delivery, payment operation, or automatic data deletion.
