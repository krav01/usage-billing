BEGIN;

-- Educational database only. Prices are integer micro-USD and frozen at acceptance.
CREATE TABLE usage_events (
    event_id text PRIMARY KEY CHECK (event_id ~ '^[A-Za-z0-9_-]{1,64}$'),
    customer_id text NOT NULL CHECK (customer_id ~ '^[A-Za-z0-9_-]{1,64}$'),
    meter text NOT NULL CHECK (meter = 'api_calls'),
    units bigint NOT NULL CHECK (units > 0),
    unit_price_micros bigint NOT NULL CHECK (unit_price_micros > 0),
    amount_micros bigint NOT NULL CHECK (amount_micros > 0),
    currency text NOT NULL CHECK (currency = 'USD'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    CHECK (amount_micros::numeric = units::numeric * unit_price_micros::numeric)
);
CREATE INDEX usage_events_customer_idx ON usage_events (customer_id, event_id);

CREATE TABLE pending_events (
    event_id text PRIMARY KEY REFERENCES usage_events (event_id),
    enqueued_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX pending_events_order_idx ON pending_events (enqueued_at, event_id);

-- The immutable event holds the charge; this unique record marks it as posted.
-- Application SQL never updates or deletes events or ledger entries. The demo
-- database owner can still alter them: this is not a tamper-proof audit ledger.
CREATE TABLE ledger_entries (
    event_id text PRIMARY KEY REFERENCES usage_events (event_id),
    processed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

COMMIT;
