# Reproducible performance measurements

These are baselines, not production capacity claims. No performance optimization
or before/after speedup is claimed.

## What is measured

| Benchmark | Included | Excluded |
| --- | --- | --- |
| HTTPHandler/accepted, replayed, unauthorized | Request and recorder construction, authentication, JSON processing, response encoding, metrics, JSON logging to io.Discard | TCP/TLS, disk logging, real billing service, PostgreSQL, worker |
| StoreReplay | Real PostgreSQL transaction and frozen-price lookup for one existing event | HTTP, new-event writes, concurrent clients |
| StoreSummary | Real PostgreSQL aggregate over 1,000 processed events for one customer | HTTP, worker execution, large production datasets |

PostgreSQL benchmarks assert their results and use their own randomly named,
quoted schemas. Setup, seeding, and cleanup are outside the timed loop. Cleanup
removes only each benchmark's own schema. Use a disposable database only.

## Run locally

Use the Go version pinned in `go.mod`, record CPU/OS and PostgreSQL version, and stop other CPU-intensive
work. Run measurements sequentially, without the race detector:

```bash
go version
make bench | tee http-bench.txt
# Point to an isolated disposable PostgreSQL database, never production.
export TEST_DATABASE_URL='postgres://billing_demo:YOUR_HEX_PASSWORD@127.0.0.1:54329/usage_billing?sslmode=disable'
make bench-postgres | tee postgres-bench.txt
```

Both targets collect ten samples with `GOMAXPROCS=1`. `ns/op` is average operation
duration, not p95/p99 request latency. `B/op` and `allocs/op` measure client Go heap
allocations; PostgreSQL server memory/CPU is not included. Inverting handler
`ns/op` is **not** a valid end-to-end service RPS claim.

The CI verification workflow also runs these benchmarks, on separate hosted
runners for HTTP and PostgreSQL. Raw samples are downloadable as SHA-labelled
artifacts retained for 14 days. Hosted-runner timing is noisy; these jobs validate
that benchmarks run and produce evidence, not a hard latency regression gate.

The checked-in HTTP sample file is one Linux baseline. Compare future revisions
on the same machine with the same command, dataset, versions, and concurrency.
Use repeated samples and statistical comparison (for example, benchstat) before
claiming a change is faster. Do not compare raw local and hosted-runner timings
as if their hardware and load were identical.

## Full-service HTTP load test

`cmd/loadtest` sends new synthetic events over real HTTP to the running Compose
application, which writes PostgreSQL and processes its queue with the real worker.
Start only the isolated demo described in the README, with its default price of
1000 micro-USD per unit. Then run `make loadtest` or:

```bash
go run ./cmd/loadtest -allow-demo-writes -requests 500 -concurrency 8 > load.json
```

The command requires explicit consent to create demo rows, only accepts numeric
loopback HTTP origins, ignores proxy environment variables, and refuses redirects.
It generates a new customer/event namespace for every run and never deletes rows.
Do not run against a production database, including one forwarded to localhost.
The maximum is 10,000 events, 32 clients, and five minutes; defaults are 500, 8,
and two minutes. The bearer token comes only from `BILLING_API_TOKEN` and is not
included in output. Failed requests are not retried; uncertain outcomes are errors.

The JSON report contains successful HTTP acceptance p95/p99 (nearest rank), raw
successful latency samples, accepted requests/second, failed and unattempted counts,
and queue samples every 250ms plus the final drain observation. HTTP timing includes
the response body and validation but **not** asynchronous processing. Settlement
separately requires exactly the requested event count, correct units/amount, and
zero pending work. Any request/sample error, unfinished request, or failure to
settle before the deadline fails the run. A zero error rate is not sufficient.

This is a finite **closed-loop** load: clients wait for responses before sending
again, with no warmup phase. It does not measure sustainable capacity, an open-loop
arrival rate, per-event processing p95, TLS, or saturation behavior. Queue polling
adds database load and its observed maximum may miss peaks between samples.

CI runs three sequential repetitions after the Docker restart smoke check,
using the same disposable database (which grows across runs), and uploads raw
JSON plus CPU/OS/Go/PostgreSQL/Docker metadata as `full-service-load-<SHA>` for
14 days. Timing is recorded, not subjected to an arbitrary performance threshold.
Compare like-for-like repeated runs before claiming any performance change.
