BEGIN;

-- Refuse to silently erase correlation history on a populated database.
LOCK TABLE usage_events IN ACCESS EXCLUSIVE MODE;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM usage_events WHERE request_id <> '') THEN
        RAISE EXCEPTION 'cannot remove persisted request IDs';
    END IF;
END
$$;
ALTER TABLE usage_events DROP COLUMN request_id;

COMMIT;
