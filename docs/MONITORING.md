# Local monitoring demo

The optional `compose.monitoring.yaml` adds Prometheus and Grafana to the existing
demo. It does not publish anything to a cloud account or configure notification
recipients. Both new ports bind only to loopback.

## Start

Use a local Docker Compose v2 installation.
Keep the same `POSTGRES_PASSWORD` and `BILLING_API_TOKEN` used to start the billing
demo. On a fresh demo only, generate all three values:

```bash
export POSTGRES_PASSWORD="$(openssl rand -hex 24)"
export BILLING_API_TOKEN="$(openssl rand -hex 32)"
export GRAFANA_ADMIN_PASSWORD="$(openssl rand -hex 24)"
make monitoring-up
make monitoring-test
```

For an existing demo, generate only the new Grafana password. Do not regenerate
the database password for an existing PostgreSQL volume: changing the environment
does not change the database's stored password.

Open Grafana at <http://127.0.0.1:3000>, sign in as `admin` with your generated
`GRAFANA_ADMIN_PASSWORD`, and open **Usage Billing - Operations**. Prometheus is
available at <http://127.0.0.1:9090>. It has no UI authentication in this localhost-only
demo, so do not expose its port, reverse-proxy it publicly, or use this setup on a
shared/untrusted machine. Grafana disables anonymous access and public sign-up.

`make monitoring-up` writes the billing token and Grafana password to the ignored
`.local/monitoring-secrets/` directory. Its mode is `0700`; individual files are
readable by the non-root containers and mounted read-only under `/run/secrets`.
File-backed secrets preserve read-only container root filesystems; Compose rejects
environment-backed secrets with that setting. No values are printed or tracked in
Git. Keep these local files private and do not force-add them to Git. The directory
is excluded from Docker build contexts as well as Git. The application
build asserts that `.local` was not copied into its build stage. This is local
secret injection, not an encrypted external manager; Docker administrators can
still access it. The files remain after stopping so container restarts can mount
them again. This helper is for disposable demo credentials, not secret rotation.
Keep your generated credentials securely. Grafana's password setting initializes
a new Grafana database; changing that environment value does not rotate an existing
admin account's password.

## Demonstrate

```bash
make loadtest
```

The load client writes synthetic usage to the local API. Wait at least two 15-second
scrapes for rate calculations. The six panels show queue depth, oldest pending age,
worker throughput, worker batch error ratio, event API request rates by status class,
and target/queue-query/worker health. Rates describe the observed interval, not a
capacity guarantee. A short-lived queue can drain between scrapes and be missed.
HTTP latency percentiles remain in the full-service load artifacts; this dashboard
does not invent histogram metrics that the service does not export.

## Alerts and tests

Prometheus evaluates five demo alerts: missing/down target, unreadable queue,
sustained old backlog, sustained worker batch errors, and a stopped worker with a
reachable API. Thresholds and `for` durations are examples, not measured SLOs.
View state in the Prometheus **Alerts** page. There is no Alertmanager, email,
Slack, or other external notification routing.

`make monitoring-test` runs `promtool` tests covering healthy/idle behavior, target
failure and recovery, missing targets, missing queue observations, sustained backlog,
worker failure, low-throughput error ratios, and counter resets. The ratio denominator
uses a tiny floor only for zero avoidance; clamping it to one request per second
would incorrectly underreport errors during backoff.

CI additionally starts the real images, requires an authenticated scrape of the
billing API, verifies the provisioned dashboard and datasource via Grafana's API,
checks each panel's PromQL, and requires anonymous dashboard access to be denied.

## Stop and retention

```bash
make monitoring-down
```

This stops the stack and preserves database and monitoring volumes. Prometheus
retains samples for at most 24 hours or its 256 MB size limit, whichever is reached
first; WAL/head overhead means this is not a strict total disk limit. The provisioned
dashboard is source-controlled. Do not use `down --volumes` unless you intend to
permanently erase the demo database and monitoring history.

Images are pinned to Prometheus `v3.14.0` and Grafana `13.2.1`, verified against
their official releases on 2026-09-03. No extra Go module dependencies are added.

References: [Prometheus rule testing](https://prometheus.io/docs/prometheus/latest/configuration/unit_testing_rules/),
[Grafana provisioning](https://grafana.com/docs/grafana/latest/administration/provisioning/),
and [Compose secrets](https://docs.docker.com/reference/compose-file/secrets/).
