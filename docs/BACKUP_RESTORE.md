# Backup and restore verification

The **Docker API smoke** job runs `scripts/test-backup-restore.sh` against its own
GitHub-hosted Compose project and synthetic data. The script refuses to run in the
normal local demo or with an arbitrary project name. It accepts no backup path or
database URL supplied by a user.

## What the drill proves

1. Stop this run's source API gracefully; add one committed, priced pending fixture.
   Earlier HTTP/load/crash tests already supplied processed usage and ledger rows.
2. Capture canonical full rows from every public table, including migration state.
3. Create a PostgreSQL custom-format dump and a **new** `billing_restore_ci` database.
   An existing destination is an error. No `--clean`, source overwrite, or drop is used.
4. Restore schema/data in a single transaction with fail-on-error, then compare all
   table rows, prices, amounts, identifiers, and timestamps before starting a worker.
5. Start the same application image against only the restored database, on a separate
   loopback port. Require pending work to become one ledger entry and exactly eleven
   units / 11000 micro-USD. Three identical replays must preserve those totals.
6. Compare the still-stopped source database again, proving the restored service did
   not change it. Remove only the temporary restored API and restart the source API.

The dump is private temporary runner data, not a public Actions artifact. It is
deleted by the script's exit cleanup. The restored database is removed with the
workflow's disposable volume; the script never deletes an existing user database.

## Limits

This is a logical restore drill, not a scheduled backup service. The source is
quiesced for deterministic comparisons; no concurrent-snapshot workload is claimed.
The destination is a separate database in the same disposable PostgreSQL instance,
not another machine, region, PostgreSQL version, or a surviving copy after disk loss.
Roles, cluster configuration, secrets, encryption/key recovery, external storage,
retention, WAL/PITR, and measured RPO/RTO are not covered. Do not adapt these commands
to production without a reviewed backup/restore plan and explicit authorization.
Never restore an untrusted dump: restore can execute SQL from its source.

References: [pg_dump](https://www.postgresql.org/docs/17/app-pgdump.html) and
[pg_restore](https://www.postgresql.org/docs/17/app-pgrestore.html).
