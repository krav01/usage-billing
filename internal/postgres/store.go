// Package postgres persists frozen usage and transactional pending work.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/krav01/usage-billing/internal/billing"
)

type Store struct {
	pool       *pgxpool.Pool
	maxPending int64
}

var _ billing.Repository = (*Store)(nil)

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool, maxPending: 10000}
}

// NewWithQueueLimit bounds durable pending work. All API instances sharing this
// queue must use the same limit and admission protocol.
func NewWithQueueLimit(pool *pgxpool.Pool, limit int64) (*Store, error) {
	if pool == nil || limit < 1 || limit > 1000000 {
		return nil, errors.New("pool and queue limit between 1 and 1000000 are required")
	}
	return &Store{pool: pool, maxPending: limit}, nil
}

// Accept commits both immutable input and its pending item, or neither. On a
// duplicate ID, a second READ COMMITTED statement observes the winning insert,
// including one that was uncommitted when ON CONFLICT began waiting.
func (s *Store) Accept(ctx context.Context, event billing.Event) (billing.Event, bool, error) {
	var stored billing.Event
	var created bool
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO usage_events
			    (event_id, customer_id, meter, units, unit_price_micros, amount_micros, currency)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (event_id) DO NOTHING`,
			event.EventID, event.CustomerID, event.Meter, event.Units,
			event.UnitPriceMicros, event.AmountMicros, event.Currency)
		if err != nil {
			return fmt.Errorf("insert usage event: %w", err)
		}
		created = tag.RowsAffected() == 1
		if created {
			// Serialize only new admissions, across processes, until commit. The
			// relation OID scopes this advisory key to this queue in this database.
			// Do NOT combine the lock and count into one SQL statement: the count
			// needs a fresh READ COMMITTED snapshot after the previous holder commits.
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock('pending_events'::regclass::oid::bigint)`); err != nil {
				return fmt.Errorf("lock queue admission: %w", err)
			}
			var pending int64
			if err := tx.QueryRow(ctx, `
				SELECT COUNT(*) FROM (SELECT 1 FROM pending_events LIMIT $1) AS bounded_queue`,
				s.maxPending).Scan(&pending); err != nil {
				return fmt.Errorf("check queue capacity: %w", err)
			}
			if pending >= s.maxPending {
				// Roll back the usage insert too: a rejected event is never acknowledged.
				return billing.ErrQueueFull
			}
			if _, err := tx.Exec(ctx, `INSERT INTO pending_events (event_id) VALUES ($1)`, event.EventID); err != nil {
				return fmt.Errorf("enqueue usage event: %w", err)
			}
		}
		stored, err = readEvent(ctx, tx, event.EventID)
		if err != nil {
			return err
		}
		// Prices deliberately are not compared: a replay preserves the first price.
		if stored.Input != event.Input {
			return billing.ErrConflict
		}
		return nil
	})
	if err != nil {
		return billing.Event{}, false, fmt.Errorf("accept usage: %w", err)
	}
	return stored, created, nil
}

func (s *Store) Get(ctx context.Context, id string) (billing.Event, error) {
	return readEvent(ctx, s.pool, id)
}

type rowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readEvent(ctx context.Context, query rowQuerier, id string) (billing.Event, error) {
	var event billing.Event
	err := query.QueryRow(ctx, `
		SELECT u.event_id, u.customer_id, u.meter, u.units,
		       u.unit_price_micros, u.amount_micros, u.currency, u.created_at,
		       EXISTS (SELECT 1 FROM ledger_entries l WHERE l.event_id = u.event_id)
		FROM usage_events u WHERE u.event_id = $1`, id).Scan(
		&event.EventID, &event.CustomerID, &event.Meter, &event.Units,
		&event.UnitPriceMicros, &event.AmountMicros, &event.Currency,
		&event.CreatedAt, &event.Processed)
	if errors.Is(err, pgx.ErrNoRows) {
		return billing.Event{}, billing.ErrNotFound
	}
	if err != nil {
		return billing.Event{}, fmt.Errorf("read usage event: %w", err)
	}
	return event, nil
}

// Summary uses one statement/snapshot, so a committed worker transaction cannot
// appear as both pending and processed. PostgreSQL sum(bigint) yields numeric;
// decimal strings avoid overflow across many individually bounded charges.
func (s *Store) Summary(ctx context.Context, customer string) (billing.Summary, error) {
	summary := billing.Summary{CustomerID: customer, Currency: "USD"}
	err := s.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(u.units) FILTER (WHERE l.event_id IS NOT NULL), 0)::text,
		       COALESCE(SUM(u.amount_micros) FILTER (WHERE l.event_id IS NOT NULL), 0)::text,
		       COUNT(p.event_id), COUNT(l.event_id)
		FROM usage_events u
		LEFT JOIN ledger_entries l ON l.event_id = u.event_id
		LEFT JOIN pending_events p ON p.event_id = u.event_id
		WHERE u.customer_id = $1`, customer).Scan(
		&summary.Units, &summary.AmountMicros, &summary.Pending, &summary.Processed)
	if err != nil {
		return billing.Summary{}, fmt.Errorf("summarize usage: %w", err)
	}
	return summary, nil
}

// QueueStats reports global durable backlog state with one consistent statement.
func (s *Store) QueueStats(ctx context.Context) (int64, float64, error) {
	var pending int64
	var oldestAgeSeconds float64
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*),
		       COALESCE(EXTRACT(EPOCH FROM statement_timestamp() - MIN(enqueued_at)), 0)::double precision
		FROM pending_events`).Scan(&pending, &oldestAgeSeconds)
	if err != nil {
		return 0, 0, fmt.Errorf("read pending queue metrics: %w", err)
	}
	if oldestAgeSeconds < 0 {
		oldestAgeSeconds = 0
	}
	return pending, oldestAgeSeconds, nil
}

// ProcessBatch claims a bounded set of unlocked items. Ledger insertion and
// queue removal share a transaction; cancellation/crash rolls both back. Unique
// ledger IDs make retries harmless, without promising external exactly-once I/O.
func (s *Store) ProcessBatch(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, fmt.Errorf("batch limit must be between 1 and 1000")
	}
	processed := 0
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT event_id FROM pending_events
			ORDER BY enqueued_at, event_id
			LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
		if err != nil {
			return fmt.Errorf("claim pending events: %w", err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("read claimed events: %w", err)
		}
		if len(ids) == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger_entries (event_id)
			SELECT unnest($1::text[]) ON CONFLICT (event_id) DO NOTHING`, ids); err != nil {
			return fmt.Errorf("post ledger entries: %w", err)
		}
		tag, err := tx.Exec(ctx, `DELETE FROM pending_events WHERE event_id = ANY($1::text[])`, ids)
		if err != nil {
			return fmt.Errorf("remove completed pending events: %w", err)
		}
		processed = int(tag.RowsAffected())
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("process usage batch: %w", err)
	}
	return processed, nil
}

func (s *Store) transaction(ctx context.Context, fn func(pgx.Tx) error) (err error) {
	// No mutable balance read-modify-write exists here. Unique keys, locked queue
	// rows, and transaction boundaries supply the required READ COMMITTED safety.
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		// Cleanup must still run after request cancellation, but must stay bounded.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if rollbackErr := tx.Rollback(cleanupCtx); rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			err = errors.Join(err, fmt.Errorf("rollback transaction: %w", rollbackErr))
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}
