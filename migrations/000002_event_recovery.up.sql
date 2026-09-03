BEGIN;

-- Failed work retains its admission slot; it is never silently discarded.
ALTER TABLE pending_events
    ADD COLUMN processing_failures integer NOT NULL DEFAULT 0 CHECK (processing_failures BETWEEN 0 AND 3),
    ADD COLUMN failure_code text NOT NULL DEFAULT '' CHECK (failure_code IN ('', '23502', '23503', '23505', '23514', '23P01')),
    ADD COLUMN retry_generation bigint NOT NULL DEFAULT 0 CHECK (retry_generation >= 0),
    ADD CONSTRAINT pending_failure_state CHECK ((processing_failures = 0) = (failure_code = ''));
DROP INDEX pending_events_order_idx;
CREATE INDEX pending_events_order_idx ON pending_events (enqueued_at, event_id)
    WHERE processing_failures < 3;

COMMIT;
