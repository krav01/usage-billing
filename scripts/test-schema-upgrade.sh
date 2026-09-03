#!/usr/bin/env bash
# Populated schema upgrade drill for a newly created, disposable CI database.
set -euo pipefail
if [[ ${GITHUB_ACTIONS:-} != true || ${RUNNER_ENVIRONMENT:-} != github-hosted ||
      ! ${COMPOSE_PROJECT_NAME:-} =~ ^usage-billing-ci-[0-9]+-[0-9]+$ ]]; then
  echo 'Run this drill only in the disposable GitHub-hosted verification workflow.' >&2
  exit 2
fi
: "${BILLING_API_TOKEN:?CI must generate a disposable API token}"
: "${POSTGRES_PASSWORD:?CI must generate a disposable database password}"
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
umask 077
drill_dir=$(mktemp -d)
upgrade_api=''

dc() {
  timeout 90s docker compose --project-name "$COMPOSE_PROJECT_NAME" --file "$repo_root/compose.yaml" "$@"
}

cleanup() {
  local result=$?
  if [[ -n $upgrade_api ]]; then
    if (( result != 0 )); then
      timeout 10s docker logs --tail=50 "$upgrade_api" >&2 || true
    fi
    timeout 25s docker stop --time=15 "$upgrade_api" >/dev/null || true
    timeout 10s docker rm "$upgrade_api" >/dev/null || true
  fi
  rm -f -- "$drill_dir/before.json" "$drill_dir/after.json" "$drill_dir/summary.json" \
    "$drill_dir/event.json" "$drill_dir/accepted.json" "$drill_dir/accepted.headers" \
    "$drill_dir/replayed.json" "$drill_dir/replayed.headers" "$drill_dir/worker.jsonl"
  rmdir -- "$drill_dir" || true
  return "$result"
}
trap cleanup EXIT

sql() {
  dc exec -T -e PGOPTIONS='-c statement_timeout=10000 -c lock_timeout=5000' postgres \
    psql -X -U billing_demo -d billing_upgrade_ci -v ON_ERROR_STOP=1 -Atc "$1"
}

migrate() {
  dc run --rm --no-deps --entrypoint /migrate migrate -path=/migrations \
    "-database=postgres://billing_demo:$POSTGRES_PASSWORD@postgres:5432/billing_upgrade_ci?sslmode=disable" "$@"
}

# Exclude only the newly added column; preserve every pre-existing field,
# including timestamps, frozen prices, ledger rows and recovery state.
snapshot() {
  sql "SELECT jsonb_build_object(
    'usage', (SELECT jsonb_agg((to_jsonb(u) - 'request_id') ORDER BY event_id) FROM usage_events u),
    'pending', (SELECT jsonb_agg(to_jsonb(p) ORDER BY event_id) FROM pending_events p),
    'ledger', (SELECT jsonb_agg(to_jsonb(l) ORDER BY event_id) FROM ledger_entries l));"
}

request() {
  curl --silent --show-error --noproxy '*' --proto '=http' --max-time 5 \
    -H "Authorization: Bearer $BILLING_API_TOKEN" "$@"
}

wait_for() {
  local deadline=$((SECONDS + 45))
  until "$@"; do
    if (( SECONDS >= deadline )); then
      echo 'Upgrade drill readiness/settlement timed out' >&2
      return 1
    fi
    sleep 0.5
  done
}

ready() {
  request --fail http://127.0.0.1:18081/readyz >/dev/null
}

settled() {
  request --fail http://127.0.0.1:18081/v1/customers/ci-upgrade/summary > "$drill_dir/summary.json" &&
    jq -e --argjson processed "$1" --argjson failed "$2" --arg units "$3" --arg amount "$4" \
      '.processed == $processed and .failed == $failed and .pending == 0 and
       .units == $units and .amount_micros == $amount' "$drill_dir/summary.json" >/dev/null
}

header_id() {
  awk 'tolower($1) == "x-request-id:" {gsub("\r", "", $2); print $2}' "$1"
}

# Fail if the target already exists. Never restore/migrate over the source demo.
dc exec -T postgres createdb -U billing_demo --template=template0 billing_upgrade_ci
migrate goto 2
test "$(sql 'SELECT version::text || dirty::text FROM schema_migrations')" = 2false
test "$(sql "SELECT count(*) FROM information_schema.columns
  WHERE table_schema='public' AND table_name='usage_events' AND column_name='request_id'")" = 0
sql "BEGIN;
  INSERT INTO usage_events(event_id,customer_id,meter,units,unit_price_micros,amount_micros,currency)
  VALUES ('ci-upgrade-pending','ci-upgrade','api_calls',7,1000,7000,'USD'),
         ('ci-upgrade-posted','ci-upgrade','api_calls',3,1000,3000,'USD'),
         ('ci-upgrade-failed','ci-upgrade','api_calls',5,1000,5000,'USD');
  INSERT INTO pending_events(event_id) VALUES ('ci-upgrade-pending');
  INSERT INTO pending_events(event_id,processing_failures,failure_code,retry_generation)
    VALUES ('ci-upgrade-failed',3,'23514',2);
  INSERT INTO ledger_entries(event_id) VALUES ('ci-upgrade-posted');
  COMMIT;" >/dev/null
snapshot > "$drill_dir/before.json"
migrate up
migrate up
test "$(sql 'SELECT version::text || dirty::text FROM schema_migrations')" = 3false
snapshot > "$drill_dir/after.json"
cmp --silent "$drill_dir/before.json" "$drill_dir/after.json"
test "$(sql "SELECT count(*) FROM usage_events WHERE request_id = ''")" = 3
echo 'PASS: populated schema v2 upgraded to v3; original rows preserved; repeated up is a no-op'

# Start only after migration. A changed configured price detects accidental repricing.
upgrade_api=$(dc run -d --no-deps -p 127.0.0.1:18081:8080 \
  -e "DATABASE_URL=postgres://billing_demo:$POSTGRES_PASSWORD@postgres:5432/billing_upgrade_ci?sslmode=disable" \
  -e BILLING_RATE_MICROS=9000 api)
[[ $upgrade_api =~ ^[a-f0-9]{64}$ ]]
wait_for ready
wait_for settled 2 1 10 10000
request --fail http://127.0.0.1:18081/v1/events/ci-upgrade-failed > "$drill_dir/event.json"
jq -e '.failed == true and .processing_failures == 3 and .failure_code == "23514" and
  .retry_generation == 2 and (has("request_id") | not)' "$drill_dir/event.json" >/dev/null
legacy='{"event_id":"ci-upgrade-pending","customer_id":"ci-upgrade","meter":"api_calls","units":7}'
status=$(request -o "$drill_dir/event.json" -w '%{http_code}' -H 'Content-Type: application/json' \
  --data "$legacy" http://127.0.0.1:18081/v1/events)
test "$status" = 200
jq -e '.processed == true and .unit_price_micros == 1000 and .amount_micros == 7000 and
  (has("request_id") | not)' "$drill_dir/event.json" >/dev/null
status=$(request -o "$drill_dir/event.json" -w '%{http_code}' -H 'Content-Type: application/json' \
  --data '{"retry_generation":2}' http://127.0.0.1:18081/v1/events/ci-upgrade-failed/retry)
test "$status" = 202
jq -e '.retry_generation == 3 and .amount_micros == 5000 and
  (has("request_id") | not)' "$drill_dir/event.json" >/dev/null
wait_for settled 3 0 15 15000
echo 'PASS: legacy pending work settled once; quarantine and generation survived; retry kept frozen prices'

body='{"event_id":"ci-upgrade-new","customer_id":"ci-upgrade","meter":"api_calls","units":2}'
status=$(request -D "$drill_dir/accepted.headers" -o "$drill_dir/accepted.json" -w '%{http_code}' \
  -H 'Content-Type: application/json' -H 'X-Request-ID: client-controlled-upgrade-id' \
  --data "$body" http://127.0.0.1:18081/v1/events)
test "$status" = 202
request_id=$(jq -er '.request_id | select(test("^[0-9a-f]{32}$"))' "$drill_dir/accepted.json")
test "$(header_id "$drill_dir/accepted.headers")" = "$request_id"
jq -e '.unit_price_micros == 9000 and .amount_micros == 18000' "$drill_dir/accepted.json" >/dev/null
wait_for settled 4 0 17 33000
timeout 35s docker restart --time=15 "$upgrade_api" >/dev/null
wait_for ready
status=$(request -D "$drill_dir/replayed.headers" -o "$drill_dir/replayed.json" -w '%{http_code}' \
  -H 'Content-Type: application/json' --data "$body" http://127.0.0.1:18081/v1/events)
test "$status" = 200
jq -e --arg id "$request_id" '.request_id == $id and .processed == true and
  .unit_price_micros == 9000 and .amount_micros == 18000' "$drill_dir/replayed.json" >/dev/null
replay_id=$(header_id "$drill_dir/replayed.headers")
[[ $replay_id =~ ^[0-9a-f]{32}$ && $replay_id != "$request_id" ]]
settled 4 0 17 33000
test "$(sql 'SELECT count(*) FROM ledger_entries')" = 4
test "$(sql 'SELECT count(*) FROM pending_events')" = 0
test "$(sql "SELECT count(*) FROM usage_events WHERE request_id <> ''")" = 1
timeout 10s docker logs "$upgrade_api" > "$drill_dir/worker.jsonl" 2>&1
jq -se --arg id "$request_id" \
  '[.[] | select(.msg == "usage event processed" and .request_id == $id)] | length == 1' \
  "$drill_dir/worker.jsonl" >/dev/null
if grep -Eq 'ci-upgrade|client-controlled-upgrade-id' "$drill_dir/worker.jsonl"; then
  echo 'Synthetic producer input reached operational logs' >&2
  exit 1
fi
echo 'PASS: new request ID survived API restart/replay; one correlated worker outcome; exact totals unchanged'
# Cleanup stops only this drill's API. Workflow cleanup removes its new database
# together with the run-owned volume; no developer database is ever targeted.
