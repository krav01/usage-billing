# Failure and recovery verification

The `Docker API smoke` CI job runs `scripts/test-postgres-outage.sh` after the
existing API smoke and full-service load checks. It uses a dedicated Compose
project named for the workflow run, with synthetic data and disposable credentials.
The script refuses to run outside the GitHub-hosted CI environment. It never
connects to a user-supplied database or targets the normal local demo project.

## Observed failure scenario

1. Acquire a test-only `SHARE` lock on `ledger_entries`, allowing reads but
   blocking the worker's insert.
2. Submit an event and require HTTP `202`; observe the worker waiting inside
   its ledger insertion through `pg_stat_activity` before injecting the fault.
3. Kill only this run's PostgreSQL container with `SIGKILL`, preserving its volume.
4. Require liveness `200`, readiness `503`, a rejected write, and unavailable
   queue metrics (not misleading zero-valued queue gauges).
5. Restart only PostgreSQL. Verify automatic recovery with the exact same API
   container, process start time, and restart count.
6. Retry the previously rejected event and replay both event IDs. Require exactly
   two ledger entries, ten units, `10000` micro-USD, and no pending events for the
   synthetic test customer.

The lock coordinates the fault with an unfinished worker transaction rather than
hoping that an arbitrary delay happens to interrupt processing. All HTTP/SQL
probes and polling loops are bounded. The workflow's unconditional cleanup removes
only its own disposable containers and volume, including on failure.

## Scope and limits

This checks database-process failure and transaction rollback/recovery. It does not
simulate permanent disk loss, multi-node failover, or a connection failure precisely
after PostgreSQL commits but before the client receives the response. It is not
a claim of external exactly-once delivery or a production availability SLO.

To repeat the test, run **Verify Usage Billing** via GitHub Actions
`workflow_dispatch`; do not point fault-injection commands at a real database.

References: [PostgreSQL locking](https://www.postgresql.org/docs/17/explicit-locking.html)
and [Docker Compose kill](https://docs.docker.com/reference/cli/docker/compose/kill/).
