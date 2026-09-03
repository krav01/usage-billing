#!/usr/bin/env bash
# Local demo only: file-backed secrets work with read-only Compose containers.
set -euo pipefail
: "${BILLING_API_TOKEN:?Set the existing demo API token}"
: "${GRAFANA_ADMIN_PASSWORD:?Set the demo Grafana admin password}"
if (( ${#BILLING_API_TOKEN} < 32 || ${#GRAFANA_ADMIN_PASSWORD} < 32 )); then
  echo 'Both monitoring credentials must contain at least 32 characters' >&2
  exit 1
fi
cd "$(dirname "${BASH_SOURCE[0]}")/.."
secret_dir=.local/monitoring-secrets
umask 077
mkdir -p "$secret_dir"
chmod 700 "$secret_dir"

write_secret() {
  local temporary
  temporary=$(mktemp "$secret_dir/.secret.XXXXXX")
  printf '%s' "$2" > "$temporary"
  # Non-root containers must read the bind-mounted file. The 0700 parent
  # directory prevents other unprivileged host users from opening it.
  chmod 444 "$temporary"
  mv -f "$temporary" "$secret_dir/$1"
}

write_secret billing-token "$BILLING_API_TOKEN"
write_secret grafana-password "$GRAFANA_ADMIN_PASSWORD"
echo 'Prepared local ignored monitoring secrets (values not printed)'
