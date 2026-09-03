# Operations

`GET /metrics` uses the same bearer token as the business API and returns
Prometheus text. Metric names and labels are bounded: event and customer IDs
never appear in metric labels or values.

## Durable request correlation

Every HTTP response has a server-generated, 32-character hexadecimal
`X-Request-ID`. Incoming request ID headers are ignored. On first acceptance,
the billing service saves that ID in `usage_events.request_id` in the same
transaction as the event and pending work. Event responses expose the original
ID as the optional `request_id` field; it is not accepted in producer input.

| Operation | HTTP response header | Event response and worker logs |
| --- | --- | --- |
| First acceptance | New request ID | Same ID, stored durably |
| Idempotent replay | New request ID | Original acceptance ID |
| Event read or manual retry | New request ID | Original acceptance ID |
| Worker restart or automatic retry | No HTTP request | Original acceptance ID |
| Legacy event or older producer | Current HTTP request ID, if applicable | No historical ID invented |

Worker batches contain independent event IDs, not one shared request context.
The store returns bounded per-event metadata through `ProcessBatchWithResults`;
the original `ProcessBatch` interface remains available. Worker event logs are
emitted after the transaction returns:

- `usage event processed`: the batch commit succeeded.
- `usage event processing failed`: a committed integrity failure, with
  `outcome` equal to `retry_scheduled` or `quarantined`, plus the failure count
  and retry generation. These are not infrastructure failures.
- `usage event outcome unconfirmed`: processing returned an error after the
  row was claimed. Commit errors can be ambiguous; this does not assert rollback
  or successful posting. The next attempt reconciles through existing unique
  ledger keys. Failures before claim have only a batch-level log.

Only validated, re-encoded correlation IDs and bounded operational fields are
logged. Raw event/customer identifiers, SQL errors, credentials and request
bodies are excluded. IDs are never metric labels. Logging adds one line per
correlated event outcome; account for that volume when running load tests.
Logs are diagnostic, not a transactional audit outbox: a process crash between
commit and logging can lose a log line. PostgreSQL remains the source of truth.

Migration `000003` is additive and leaves historical IDs empty. Apply it before
deploying the new binary. Its down migration refuses to erase nonempty IDs.
To roll back the binary, keep the added column; old code ignores it and new
events from that old binary have no durable correlation ID.

With the demo stack running and `BILLING_API_TOKEN` set, find an event's original
ID and filter its logs (replace the synthetic event ID as needed):

```bash
request_id=$(curl --fail --silent --show-error --max-time 5 \
  -H "Authorization: Bearer $BILLING_API_TOKEN" \
  http://127.0.0.1:8080/v1/events/demo-event | jq -er '.request_id')
docker compose logs --no-color --no-log-prefix api \
  | jq -R --arg id "$request_id" 'fromjson? | select(.request_id == $id)'
```

## Queue metrics

Each authenticated scrape runs one context-bound PostgreSQL aggregate over the
durable `pending_events` queue. A 15-second scrape interval is a reasonable demo
default; measure the query against production-scale data before adapting this
design to a real service.

| Metric | Type | Meaning |
| --- | --- | --- |
| `usage_billing_queue_pending_events` | gauge | Global durable work eligible for automatic processing |
| `usage_billing_queue_failed_events` | gauge | Quarantined work requiring manual recovery |
| `usage_billing_queue_oldest_event_age_seconds` | gauge | Age of the oldest pending event, or zero when empty |
| `usage_billing_queue_scrape_success` | gauge | `1` when the current queue query succeeded |
| `usage_billing_queue_scrape_errors_total` | counter | Queue queries that failed in this process |

If the query fails, the endpoint still exposes worker and HTTP metrics plus
`queue_scrape_success 0`; it omits pending/failed counts and age rather than reporting
misleading zeroes.

## Worker metrics

| Metric | Type | Meaning |
| --- | --- | --- |
| `usage_billing_worker_running` | gauge | This process currently runs the worker loop |
| `usage_billing_worker_batch_in_flight` | gauge | A batch call is active |
| `usage_billing_worker_batch_attempts_total` | counter | Completed or cancelled batch calls |
| `usage_billing_worker_batch_errors_total` | counter | Non-context batch failures |
| `usage_billing_worker_batch_cancellations_total` | counter | Parent cancellations and batch deadlines |
| `usage_billing_worker_events_processed_total` | counter | Events durably removed from the queue by this process |

Worker counters are process-local and reset on restart. Prometheus handles
counter resets when `rate` is used. The queue depth and age are global database
state, not process-local estimates.

Handled integrity failures do not increment the batch-error counter: their transactions
commit recovery state successfully. Alert on `usage_billing_queue_failed_events > 0`
as well; the provisioned `BillingEventsQuarantined` rule does this. See
[event recovery](EVENT_RECOVERY.md) before manually reactivating an event.

Example PromQL:

```promql
# Processing throughput in events per second.
sum(rate(usage_billing_worker_events_processed_total[5m]))

# Non-context batch error ratio. Clamp avoids division by zero.
sum(rate(usage_billing_worker_batch_errors_total[5m]))
/
clamp_min(sum(rate(usage_billing_worker_batch_attempts_total[5m])), 1e-9)

# Candidate alerts; tune durations and thresholds from measured workload data.
usage_billing_queue_scrape_success == 0
usage_billing_queue_oldest_event_age_seconds > 60
```

The `60`-second queue-age threshold is illustrative, not a measured service-level
objective. [Local monitoring](MONITORING.md) provides an optional collector,
dashboard, and tested alert rules. Real notification routing is not configured.

## Backpressure and retry policy

`BILLING_MAX_PENDING_EVENTS` defaults to `10000` (allowed range 1–1000000).
It counts **pending plus failed** rows. Quarantine retains an admission slot;
manual retry uses that same slot, including when the queue is at capacity.
New events are rejected with HTTP `503`, `{"error":"queue_full"}`, and
`Retry-After: 1` when the durable queue reaches that limit. Rejection rolls back
both usage and queue insertion. Already accepted events are never dropped, and
identical replays still return their original frozen price even when the queue is full.

New admissions acquire a transaction-scoped PostgreSQL advisory lock identified
by the pending table's OID. A separate READ COMMITTED count after acquiring the
lock observes the previous admission's commit, preventing concurrent overshoot.
Worker queue removal can only free capacity and does not take this lock. All API
instances must use the same limit/protocol; direct SQL and older binaries bypass
this application-level rule. Reducing a limit below an existing backlog preserves
that backlog and rejects new work until it drains below the limit.

This intentionally serializes new admissions and counts at most the configured
limit. It is a correctness-first demo design, not a high-throughput distributed
quota service. Retain the full-service load measurements when evaluating its cost.

Consecutive worker failures double the delay up to `max(30s, WORKER_INTERVAL)`.
With the default interval, the delays are 200ms, 400ms, 800ms, and so on, capped
at 30 seconds. A successful batch (including an empty one) resets the delay to
`WORKER_INTERVAL`. Cancellation interrupts the delay immediately. The worker does
not discard failing events. Confirmed event integrity failures use the separate
three-attempt quarantine policy, not infrastructure backoff. The deterministic
delay has no jitter, so many synchronized replicas require an additional policy.
Producer clients should respect `Retry-After`, add their own bounded backoff/jitter,
and reuse the same event ID. A retry hint does not guarantee capacity is available.
