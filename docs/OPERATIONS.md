# Operations

`GET /metrics` uses the same bearer token as the business API and returns
Prometheus text. Metric names and labels are bounded: event and customer IDs
never appear in metric labels or values.

## Queue metrics

Each authenticated scrape runs one context-bound PostgreSQL aggregate over the
durable `pending_events` queue. A 15-second scrape interval is a reasonable demo
default; measure the query against production-scale data before adapting this
design to a real service.

| Metric | Type | Meaning |
| --- | --- | --- |
| `usage_billing_queue_pending_events` | gauge | Global durable queue depth |
| `usage_billing_queue_oldest_event_age_seconds` | gauge | Age of the oldest pending event, or zero when empty |
| `usage_billing_queue_scrape_success` | gauge | `1` when the current queue query succeeded |
| `usage_billing_queue_scrape_errors_total` | counter | Queue queries that failed in this process |

If the query fails, the endpoint still exposes worker and HTTP metrics plus
`queue_scrape_success 0`; it omits queue depth and age rather than reporting
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
not discard failing events; this is not poison-message isolation. The deterministic
delay has no jitter, so many synchronized replicas require an additional policy.
Producer clients should respect `Retry-After`, add their own bounded backoff/jitter,
and reuse the same event ID. A retry hint does not guarantee capacity is available.
