#!/usr/bin/env bash
# Destructive fault injection for the disposable GitHub-hosted CI stack only.
set -euo pipefail

if [[ ${GITHUB_ACTIONS:-} != true || ${RUNNER_ENVIRONMENT:-} != github-hosted ||
      ! ${COMPOSE_PROJECT_NAME:-} =~ ^usage-billing-ci-[0-9]+-[0-9]+$ ]]; then
  echo 'Run this test only through the Verify Usage Billing workflow on a GitHub-hosted runner.' >&2
  exit 2
fi
: "${BILLING_API_TOKEN:?CI must generate a disposable API token}"
: "${POSTGRES_PASSWORD:?CI must generate disposable database credentials}"
repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
test_dir=$(mktemp -d)
locker_pid=''

dc() {
  docker compose --project-name "$COMPOSE_PROJECT_NAME" --file "$repo_root/compose.yaml" "$@"
}

cleanup() {
  if [[ -n $locker_pid ]]; then
    # Only the local lock-holder process spawned below is terminated here.
    # Workflow cleanup removes this run's disposable containers and volume.
    kill "$locker_pid" 2>/dev/null || true
    wait "$locker_pid" 2>/dev/null || true
  fi
}
trap cleanup EXIT

sql() {
  timeout 5s docker compose --project-name "$COMPOSE_PROJECT_NAME" --file "$repo_root/compose.yaml" \
    exec -T -e 'PGOPTIONS=-c statement_timeout=2000' postgres \
    psql -X -U billing_demo -d usage_billing -v ON_ERROR_STOP=1 -Atc "$1"
}

request() {
  curl --silent --show-error --noproxy '*' --proto '=http' --max-time 7 \
    -H "Authorization: Bearer $BILLING_API_TOKEN" "$@"
}

wait_for() {
  local description=$1
  local deadline=$((SECONDS + 30))
  shift
  until "$@"; do
    if (( SECONDS >= deadline )); then
      echo "Timed out: $description" >&2
      return 1
    fi
    sleep 0.2
  done
}

lock_is_held() {
  [[ $(sql "SELECT EXISTS (
    SELECT 1 FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
    WHERE a.application_name = 'billing_fault_lock'
      AND l.relation = 'ledger_entries'::regclass AND l.mode = 'ShareLock' AND l.granted
  )") == t ]]
}

worker_is_blocked() {
  [[ $(sql "SELECT EXISTS (
    SELECT 1 FROM pg_stat_activity
    WHERE datname = current_database() AND state = 'active'
      AND wait_event_type = 'Lock' AND query ILIKE '%INSERT INTO ledger_entries%'
  )") == t ]]
}

ready() {
  request --fail http://127.0.0.1:8080/readyz >/dev/null
}

settled() {
  request --fail http://127.0.0.1:8080/v1/customers/ci-outage-customer/summary > "$test_dir/summary.json" &&
    jq -e '.processed == 2 and .pending == 0 and .units == "10" and .amount_micros == "10000"' \
      "$test_dir/summary.json" >/dev/null
}

wait_for 'initial readiness' ready
api_id=$(dc ps -q api)
test -n "$api_id"
postgres_id=$(dc ps -q postgres)
test -n "$postgres_id"
api_state=$(docker inspect --format '{{.State.StartedAt}}/{{.RestartCount}}' "$api_id")

# SHARE allows reads but blocks the worker's INSERT (ROW EXCLUSIVE). Observe
# the actual blocked statement before killing PostgreSQL, not a guessed sleep.
timeout 75s docker compose --project-name "$COMPOSE_PROJECT_NAME" --file "$repo_root/compose.yaml" \
  exec -T -e PGAPPNAME=billing_fault_lock -e 'PGOPTIONS=-c statement_timeout=65000' postgres \
  psql -X -U billing_demo -d usage_billing -v ON_ERROR_STOP=1 \
  -c 'BEGIN; LOCK TABLE ledger_entries IN SHARE MODE; SELECT pg_sleep(60); ROLLBACK;' \
  > "$test_dir/lock.log" 2>&1 &
locker_pid=$!
wait_for 'test lock acquisition' lock_is_held

accepted='{"event_id":"ci-outage-accepted","customer_id":"ci-outage-customer","meter":"api_calls","units":7}'
retry='{"event_id":"ci-outage-retry","customer_id":"ci-outage-customer","meter":"api_calls","units":3}'
status=$(request -o "$test_dir/accepted.json" -w '%{http_code}' \
  -H 'Content-Type: application/json' --data "$accepted" http://127.0.0.1:8080/v1/events)
test "$status" = 202
jq -e '.event_id == "ci-outage-accepted" and .amount_micros == 7000 and .processed == false' \
  "$test_dir/accepted.json" >/dev/null
wait_for 'worker blocked inside ledger insertion' worker_is_blocked
echo 'PASS: accepted event is durable; worker observed inside an unfinished transaction'

dc kill --signal SIGKILL postgres
# Wait for the engine's terminal state before attempting recovery. Compose's
# service discovery can race with SIGKILL completion and start no containers.
test "$(timeout 15s docker wait "$postgres_id")" = 137
# PostgreSQL termination deliberately breaks the lock-holder connection.
if wait "$locker_pid"; then
  echo 'Lock-holder unexpectedly completed before the injected crash' >&2
  exit 1
fi
locker_pid=''
status=$(request -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/healthz)
test "$status" = 200
status=$(request -o "$test_dir/ready.json" -w '%{http_code}' http://127.0.0.1:8080/readyz)
test "$status" = 503
status=$(request -o "$test_dir/metrics.txt" -w '%{http_code}' http://127.0.0.1:8080/metrics)
test "$status" = 200
grep -Fxq 'usage_billing_queue_scrape_success 0' "$test_dir/metrics.txt"
if grep -Eq '^usage_billing_queue_(pending_events|oldest_event_age_seconds) ' "$test_dir/metrics.txt"; then
  echo 'Unavailable queue metrics were incorrectly reported as measurements' >&2
  exit 1
fi
status=$(request -o "$test_dir/unavailable.json" -w '%{http_code}' \
  -H 'Content-Type: application/json' --data "$retry" http://127.0.0.1:8080/v1/events)
[[ $status =~ ^5[0-9][0-9]$ ]]
jq -e '.error == "internal_error"' "$test_dir/unavailable.json" >/dev/null
echo 'PASS: database outage keeps liveness available, fails readiness/writes, and marks queue metrics unavailable'

docker start "$postgres_id" >/dev/null
wait_for 'database recovery without API restart' ready
# A failed write must not have been acknowledged or persisted during the outage.
status=$(request -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/v1/events/ci-outage-retry)
test "$status" = 404
status=$(request -o "$test_dir/retry.json" -w '%{http_code}' \
  -H 'Content-Type: application/json' --data "$retry" http://127.0.0.1:8080/v1/events)
test "$status" = 202
wait_for 'exact settlement after recovery' settled

for payload in "$accepted" "$retry" "$accepted" "$retry"; do
  status=$(request -o "$test_dir/replay.json" -w '%{http_code}' \
    -H 'Content-Type: application/json' --data "$payload" http://127.0.0.1:8080/v1/events)
  test "$status" = 200
  jq -e '.processed == true' "$test_dir/replay.json" >/dev/null
done
settled
test "$(sql "SELECT COUNT(*) FROM ledger_entries l JOIN usage_events u USING (event_id)
  WHERE u.customer_id = 'ci-outage-customer'")" = 2
test "$(dc ps -q api)" = "$api_id"
test "$(docker inspect --format '{{.State.StartedAt}}/{{.RestartCount}}' "$api_id")" = "$api_state"
request --fail http://127.0.0.1:8080/metrics > "$test_dir/recovered-metrics.txt"
grep -Fxq 'usage_billing_queue_scrape_success 1' "$test_dir/recovered-metrics.txt"
grep -Fxq 'usage_billing_worker_running 1' "$test_dir/recovered-metrics.txt"
echo 'PASS: same API process recovered; two unique events, ten units, 10000 micro-USD, zero pending, no duplicate ledger entries'
