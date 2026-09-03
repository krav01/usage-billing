BEGIN;

-- Keep legacy rows and old producers compatible; never invent historical IDs.
ALTER TABLE usage_events ADD COLUMN request_id text NOT NULL DEFAULT ''
    CHECK (request_id = '' OR request_id ~ '^[0-9a-f]{32}$');

COMMIT;
