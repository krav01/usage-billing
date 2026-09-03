# Failed event recovery

I keep failed events in the durable queue rather than discarding their frozen
usage or silently freeing admission capacity. This is an educational database-only
protocol: it does not send money or implement a general message broker.

## Automatic processing

- New work starts with `processing_failures=0`, `retry_generation=0`, and no error code.
- The normal worker path still posts a bulk batch. A ledger integrity failure triggers
  per-event attempts inside savepoints, retaining the original claimed row locks.
- Only PostgreSQL ledger-insert SQLSTATE `23502`, `23503`, `23505`, `23514`, and `23P01`
  consume a failure attempt. Duplicate event primary keys remain harmless through
  `ON CONFLICT`. The initial failed bulk probe is not an event attempt.
- A confirmed per-event failure increments `processing_failures` once and records
  only its allowlisted SQLSTATE, never a driver message. At three failures, `failed=true`;
  the worker's partial index/claim excludes the row until manual recovery.
- Network failures, timeouts, cancellation, deadlocks, serialization failures,
  schema/permission errors and bookkeeping failures abort the whole batch. Healthy
  writes and failure counters roll back together. Uncertain commits are reconciled
  by persisted state on the next attempt, never by inventing a successful count.

The input API already validates ordinary invalid usage. Quarantine is protection
against ledger integrity failures, not a claim that every constraint failure is
caused by bad customer input. A broken constraint can quarantine many events;
operators must investigate the cause rather than repeatedly pressing retry.

## Manual retry

1. Inspect `GET /v1/events/{event_id}` and the failed-event metric/alert.
2. Investigate and fix the underlying cause in the isolated demo.
3. Send the event's current `retry_generation` explicitly:

There is no bulk-retry or public event-list endpoint. To discover failed IDs, an
operator can run this bounded, read-only query in the isolated demo database:

```sql
SELECT event_id, processing_failures, failure_code, retry_generation
FROM pending_events WHERE processing_failures = 3
ORDER BY enqueued_at, event_id LIMIT 100;
```

```bash
# Example only: substitute an existing failed event and the generation just read.
curl --fail-with-body http://127.0.0.1:8080/v1/events/demo_001/retry \
  -H "Authorization: Bearer $BILLING_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"retry_generation":0}'
```

The endpoint uses the existing trusted-producer token, which grants access to all
demo customers, including recovery. It is not a separate administrator role.
JSON is bounded to 16 KiB; missing, duplicate, unknown, null, fractional, or invalid
generation fields are rejected. Generations must be integers from zero through
9223372036854775806, decoded with int64 precision.

| Situation | Result |
| --- | --- |
| Failed event and matching generation | `202`, clear current failure state, increment generation, enqueue at the tail |
| Same request while its reactivated generation is still pending | `200`, no mutation |
| Event is already processed | `200`, no mutation |
| Nonfailed event with no matching earlier reactivation, or stale/future generation | `409 retry_conflict` |
| Unknown event | `404` |

Concurrent identical retry requests cause one reactivation. If the event fails again,
an old request cannot restart that later failed generation. If the HTTP response is
lost, resend the **same** generation; do not automatically fetch a newer generation
and keep retrying. Read the current event on conflict and investigate.

Reactivation never changes event ID, input, creation time, price, amount, or ledger.
It retains the existing admission slot, even at capacity. Failed and pending counts
are separate in customer summaries, and neither contributes to charged totals.
`BILLING_MAX_PENDING_EVENTS` caps their **sum**; many failed events can therefore
stop new admissions intentionally until recovered.

## Observability and limits

The API exposes `failed`, `processing_failures`, `failure_code`, and `retry_generation`.
The failure count describes confirmed failed single-event ledger inserts in the
current cycle, not successful attempts or infrastructure retries. Successful queue
removal also removes this operational metadata (responses then show zero/empty
values). This is not an immutable retry history or an operator audit trail.

Prometheus exports `usage_billing_queue_failed_events`; the local Grafana queue panel
shows it separately and `BillingEventsQuarantined` alerts after one minute. No event
IDs or error messages become metric labels. Outages omit all queue gauges and mark
the scrape unsuccessful rather than suggesting zero failures. No external alert
notification routing is configured.

Normal processing adds one savepoint round trip per nonempty batch. Exceptional
processing is bounded by the batch limit and worker deadline but has per-event SQL
cost. Existing CI load samples remain measurements, not a capacity guarantee.

## Migration and verification

Migration `000002_event_recovery` applies only to this isolated educational database.
Stop old workers, apply migrations, then start the new binary. Do not mix old workers
with the new schema: old claim SQL ignores quarantine. The down migration refuses
to erase any active failure or retry-generation state; drain it with the new code
first. Never bypass that guard on a real database.

Integration tests cover healthy progress beside broken events, three-attempt state
across reconstructed stores, competing workers, concurrent retry, stale requests,
capacity retention, frozen pricing, exact totals, blocked-ledger cancellation,
bookkeeping rollback, and the down-migration guard. HTTP tests cover authorization,
strict input and the real PostgreSQL recovery lifecycle. CI also runs migration
round-trips, Docker smoke/load/crash/backup tests and Prometheus rule tests.

References: PostgreSQL [savepoints](https://www.postgresql.org/docs/17/sql-savepoint.html)
and [SQLSTATE codes](https://www.postgresql.org/docs/17/errcodes-appendix.html).
