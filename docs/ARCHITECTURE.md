# Why these boundaries

I kept a single Go process with compact packages and manual dependency wiring.
The HTTP layer handles transport and authentication; billing validates input and
prices new events; PostgreSQL owns durable transactions; the worker polls a durable queue.
Kafka and a DI framework would add operating cost without improving this example's contract.

## Acceptance and replay

An event's ID is global in this trusted-producer demo. The first accepted input
freezes customer, meter, units, and unit price. The database transaction writes
both the event and its pending work. An identical replay returns the stored event;
a changed payload returns a conflict. Neither path overwrites the original price.
After an ambiguous network failure, the producer retries the same ID and payload.

New admissions serialize a capacity check with a transaction-level advisory lock
scoped to this queue. A full queue rejects new input atomically with `503`; it does
not drop existing work or prevent identical replays. See [Operations](OPERATIONS.md)
for configuration, cross-instance assumptions, and throughput tradeoffs.

Pricing uses positive int64 values and checks multiplication before accepting a
new event. An existing event can still be replayed when a newer configured rate
would overflow: its original input and price remain authoritative.

## Processing

Workers lock a bounded batch of pending rows with `FOR UPDATE SKIP LOCKED`.
The transaction records the ledger effect and removes the corresponding work item.
A failure before commit leaves pending work retryable. A unique event key in the
ledger is the last defense against duplicate database charges. No external payment
call occurs inside or outside this transaction.

An integrity failure in the bulk ledger insert rolls back to a savepoint. The same
locked batch then tries each event separately; healthy events proceed and confirmed
per-event failures increment durable counters. Three failures quarantine an event.
Any infrastructure or bookkeeping failure rolls back the entire transaction, including
healthy inserts and counters. Counts are reported only after confirmed commit.
See [event recovery](EVENT_RECOVERY.md) for the exact SQLSTATE allowlist and retry protocol.

Multiple worker processes may share this queue, but it is not a general job platform.
The demo bounds unfinished work and applies capped backoff to infrastructure failures;
it has no jitter, deferred retry scheduling, or external side-effect deduplication.

## Reading totals

One database statement reads a consistent snapshot of the customer's events and
ledger state. Only processed events contribute to totals; pending, failed, and processed
counts make eventual consistency explicit. PostgreSQL numeric aggregates are
serialized as decimal strings to avoid overflowing the sum of valid int64 events.

## Trust and shutdown

The bearer token authenticates one trusted internal producer and grants access to
all demo customers. It is not a customer login or authorization boundary between tenants.
Credentials stay in environment variables and are not intentionally included in
logs or error responses. Unknown URLs and client methods are not used as metric labels.
HTTP and worker cancellation are coordinated before the connection pool closes.

Migrations are administrative, versioned, one-shot operations using an external
migration tool. They are scoped to a new disposable demo database. The app does not
modify schemas at startup. A down migration destroys data; it is not a production rollback plan.

## What this does not prove

The ledger is application-immutable, not tamper-proof against the database owner.
The demo does not calculate tax, generate invoices, reconcile payments, enforce
credit limits, or provide exactly-once delivery over the network. A new API version,
tenant isolation, schema/index review against real workloads, and operational
controls would be required before any real-money use.
