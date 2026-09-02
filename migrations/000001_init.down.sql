-- Destructive rollback for the isolated educational database only.
BEGIN;
DROP TABLE ledger_entries;
DROP TABLE pending_events;
DROP TABLE usage_events;
COMMIT;
