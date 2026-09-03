#!/usr/bin/env bash
# Read-only smoke checks for the local monitoring endpoints.
set -euo pipefail
: "${GRAFANA_ADMIN_PASSWORD:?Set the disposable Grafana admin password}"
test_dir=$(mktemp -d)

request() {
  curl --silent --show-error --fail --noproxy '*' --proto '=http' --max-time 5 "$@"
}

monitoring_ready() {
  request http://127.0.0.1:9090/-/ready >/dev/null &&
    request http://127.0.0.1:3000/api/health >/dev/null &&
    request --get --data-urlencode 'query=up{job="usage-billing"}' \
      http://127.0.0.1:9090/api/v1/query > "$test_dir/target.json" &&
    jq -e '.status == "success" and (.data.result | length) == 1 and .data.result[0].value[1] == "1"' \
      "$test_dir/target.json" >/dev/null
}

deadline=$((SECONDS + 90))
until monitoring_ready; do
  if (( SECONDS >= deadline )); then
    echo 'Monitoring did not become ready with an authenticated billing scrape' >&2
    exit 1
  fi
  sleep 1
done

request -u "admin:$GRAFANA_ADMIN_PASSWORD" \
  http://127.0.0.1:3000/api/dashboards/uid/usage-billing > "$test_dir/dashboard.json"
jq -e '.dashboard.uid == "usage-billing" and (.dashboard.panels | length) == 6' \
  "$test_dir/dashboard.json" >/dev/null
request -u "admin:$GRAFANA_ADMIN_PASSWORD" \
  http://127.0.0.1:3000/api/datasources/uid/billing-prometheus/health | jq -e '.status == "OK"' >/dev/null
status=$(curl --silent --show-error --noproxy '*' --proto '=http' --max-time 5 \
  -o /dev/null -w '%{http_code}' http://127.0.0.1:3000/api/dashboards/uid/usage-billing)
test "$status" = 401

# Query every provisioned panel expression against the real Prometheus parser.
jq -r '.dashboard.panels[].targets[].expr' "$test_dir/dashboard.json" > "$test_dir/queries.txt"
while IFS= read -r query; do
  request --get --data-urlencode "query=$query" http://127.0.0.1:9090/api/v1/query |
    jq -e '.status == "success"' >/dev/null
done < "$test_dir/queries.txt"
echo 'PASS: authenticated billing scrape, six provisioned panels, healthy datasource, valid PromQL, and denied anonymous dashboard access'
