BEGIN;

-- Refuse to reactivate quarantined work or erase active retry preconditions.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pending_events WHERE processing_failures > 0 OR retry_generation > 0) THEN
        RAISE EXCEPTION 'drain recovery state before reverting event recovery';
    END IF;
END $$;
DROP INDEX pending_events_order_idx;
ALTER TABLE pending_events
    DROP CONSTRAINT pending_failure_state,
    DROP COLUMN processing_failures,
    DROP COLUMN failure_code,
    DROP COLUMN retry_generation;
CREATE INDEX pending_events_order_idx ON pending_events (enqueued_at, event_id);

COMMIT;
