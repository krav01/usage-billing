# Upgrading the isolated demo

These steps apply only to the repository's educational Compose database. Keep
the existing database password and API token. Never supply a production database.
The application does not migrate on startup.

## Schema versions

| Version | Change | Upgrade constraint |
| --- | --- | --- |
| 2 | Quarantine and generation-checked retry | Stop workers from schema v1: their claim query ignores quarantine |
| 3 | Durable acceptance request ID | Apply before starting the new binary; legacy IDs remain empty |

Migration `000003` preserves existing event inputs, prices, creation times,
ledger entries and pending recovery state. It does not reconstruct historical
request IDs. New acceptance uses the current price; replay keeps the stored price.

## Update an existing local demo

Retain a recoverable backup of the disposable demo if its history matters. Review
the [backup/restore drill](BACKUP_RESTORE.md) for its scope; its CI-only script is
not a command for an existing database. From the checked-out new source version,
with the original credentials already set:

```bash
(
set -e
docker compose stop api
docker compose build api migrate
docker compose up -d --wait postgres
docker compose run --rm --no-deps migrate up
docker compose run --rm --no-deps migrate version
# Continue only when migration completed successfully and reports version 3.
docker compose up -d --no-deps api
curl --fail-with-body --retry 20 --retry-delay 1 --retry-all-errors \
  --retry-max-time 60 --max-time 3 http://127.0.0.1:8080/readyz
)
```

Run the [demo acceptance/replay and settlement steps](DEMO.md#2-accept-usage-and-replay-it)
with a new namespace. Confirm the response body's original `request_id` survives
replay and `docker compose restart api`, while the HTTP header changes. For old
events the field remains absent. The [operations guide](OPERATIONS.md#durable-request-correlation)
shows how to locate the corresponding worker log.

If migration fails, leave the API stopped and inspect the failure. Do not force a
schema version, run a down migration, or delete the volume to hide the failure.
Optional monitoring keeps its existing volumes and secret files.

## Rollback boundary

To roll back only the request-correlation binary, keep schema v3 and use a
schema-v2-compatible binary. Its inserts use the new column's empty default.
Do not roll back to code predating quarantine while failed work exists.
The v3 down migration refuses to erase nonempty request IDs; the v2 down migration
also protects active failure/retry state. Neither is part of routine rollback.

## Automated upgrade evidence

The `Verify populated schema upgrade and durable request correlation` step in
the Docker smoke job uses the existing built images and a **new** `billing_upgrade_ci`
database. It refuses to run outside the disposable GitHub-hosted workflow, and
`createdb` must succeed before migrations run.

The drill starts at schema v2 with posted, pending and quarantined synthetic
events, compares all old fields before/after v3, and repeats `up`. It then starts
the new API at a different price and verifies old-price replay, preserved recovery
generation, manual retry, and exact settlement. New acceptance, API restart and
replay must retain one durable request ID and one correlated worker outcome.

This exercises a populated schema upgrade and the new binary. It does not run
an old binary concurrently, measure migration lock time on large databases,
upgrade PostgreSQL, or update a separately hosted environment.
