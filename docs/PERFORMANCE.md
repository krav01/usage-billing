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

Use Go 1.26.6, record CPU/OS and PostgreSQL version, and stop other CPU-intensive
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
