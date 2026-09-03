#!/usr/bin/env bash
# Restore drill for this workflow's isolated, synthetic database only.
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
test_dir=$(mktemp -d)
restore_api=''
source_api=''
source_stopped=false

dc() {
  timeout 90s docker compose --project-name "$COMPOSE_PROJECT_NAME" --file "$repo_root/compose.yaml" "$@"
}

cleanup() {
  local result=$?
  if [[ -n $restore_api ]]; then
    if (( result != 0 )); then
      docker logs --tail=50 "$restore_api" >&2 || true
    fi
    docker stop --time=15 "$restore_api" >/dev/null || true
    docker rm "$restore_api" >/dev/null || true
  fi
  if [[ $source_stopped == true ]]; then
    docker start "$source_api" >/dev/null || true
  fi
  # Only files created by this invocation; never delete a supplied backup.
  rm -f -- "$test_dir/backup.dump" "$test_dir/source.json" "$test_dir/restored.json" \
    "$test_dir/source-after.json" "$test_dir/summary.json" "$test_dir/replay.json"
  rmdir -- "$test_dir" || true
  return "$result"
}
trap cleanup EXIT

sql() {
  dc exec -T postgres psql -X -U billing_demo -d "$1" -v ON_ERROR_STOP=1 -Atc "$2"
}

snapshot() {
  dc exec -T -e PGOPTIONS='-c statement_timeout=10000' postgres \
    psql -X -U billing_demo -d "$1" -Atf - < "$repo_root/scripts/backup-snapshot.sql"
}

request() {
  curl --silent --show-error --noproxy '*' --proto '=http' --max-time 5 \
    -H "Authorization: Bearer $BILLING_API_TOKEN" "$@"
}

wait_for() {
  local deadline=$((SECONDS + 45))
  until "$@"; do
    if (( SECONDS >= deadline )); then
      echo 'Restore drill readiness/settlement timed out' >&2
      return 1
    fi
    sleep 0.5
  done
}

restored_ready() {
  request --fail http://127.0.0.1:18080/readyz >/dev/null
}

settled() {
  request --fail http://127.0.0.1:18080/v1/customers/ci-backup-customer/summary > "$test_dir/summary.json" &&
    jq -e '.processed == 1 and .pending == 0 and .units == "11" and .amount_micros == "11000"' \
      "$test_dir/summary.json" >/dev/null
}

source_api=$(dc ps -q api)
test -n "$source_api"
dc stop api
source_stopped=true
# Use an explicit committed SQL fixture so pending work cannot race the worker.
# Existing HTTP/load/outage tests supply real accepted and processed rows too.
sql usage_billing "BEGIN;
  INSERT INTO usage_events(event_id,customer_id,meter,units,unit_price_micros,amount_micros,currency)
  VALUES ('ci-backup-pending','ci-backup-customer','api_calls',11,1000,11000,'USD');
  INSERT INTO pending_events(event_id) VALUES ('ci-backup-pending'); COMMIT;" >/dev/null
test "$(sql usage_billing "SELECT COUNT(*) FROM pending_events WHERE event_id='ci-backup-pending'")" = 1
test "$(sql usage_billing 'SELECT COUNT(*) > 0 FROM ledger_entries')" = t
snapshot usage_billing > "$test_dir/source.json"
test -s "$test_dir/source.json"

dc exec -T postgres pg_dump -U billing_demo -d usage_billing --format=custom \
  --no-privileges --lock-wait-timeout=10000 > "$test_dir/backup.dump"
test -s "$test_dir/backup.dump"
# Never --clean, --create, dropdb, or restore over the source. Existing target
# databases cause createdb to fail, rather than being silently reused.
dc exec -T postgres createdb -U billing_demo --template=template0 billing_restore_ci
dc exec -T postgres pg_restore -U billing_demo -d billing_restore_ci --no-owner \
  --no-privileges --exit-on-error --single-transaction < "$test_dir/backup.dump"
snapshot billing_restore_ci > "$test_dir/restored.json"
cmp --silent "$test_dir/source.json" "$test_dir/restored.json"
echo 'PASS: backup restored into a new database; all public table rows and migration state match'

restore_api=$(dc run -d --no-deps -p 127.0.0.1:18080:8080 \
  -e "DATABASE_URL=postgres://billing_demo:${POSTGRES_PASSWORD}@postgres:5432/billing_restore_ci?sslmode=disable" api)
[[ $restore_api =~ ^[a-f0-9]{64}$ ]]
wait_for restored_ready
wait_for settled
body='{"event_id":"ci-backup-pending","customer_id":"ci-backup-customer","meter":"api_calls","units":11}'
for attempt in 1 2 3; do
  status=$(request -o "$test_dir/replay.json" -w '%{http_code}' \
    -H 'Content-Type: application/json' --data "$body" http://127.0.0.1:18080/v1/events)
  test "$status" = 200
  jq -e '.processed == true and .unit_price_micros == 1000 and .amount_micros == 11000' \
    "$test_dir/replay.json" >/dev/null
done
settled
test "$(sql billing_restore_ci "SELECT COUNT(*) FROM ledger_entries WHERE event_id='ci-backup-pending'")" = 1
snapshot usage_billing > "$test_dir/source-after.json"
cmp --silent "$test_dir/source.json" "$test_dir/source-after.json"
echo 'PASS: restored worker processed pending work once; replay preserved 11000 micro-USD; source database was unchanged'

docker stop --time=15 "$restore_api" >/dev/null
docker rm "$restore_api" >/dev/null
restore_api=''
docker start "$source_api" >/dev/null
source_stopped=false
wait_for request --fail http://127.0.0.1:8080/readyz
# The restored database remains only in this run's disposable volume. The
# workflow's existing unconditional cleanup removes that volume at job end.
