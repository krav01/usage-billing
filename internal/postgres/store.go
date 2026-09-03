// Package postgres persists frozen usage and transactional pending work.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
			    (event_id, customer_id, meter, units, unit_price_micros, amount_micros, currency, request_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			ON CONFLICT (event_id) DO NOTHING`,
			event.EventID, event.CustomerID, event.Meter, event.Units,
			event.UnitPriceMicros, event.AmountMicros, event.Currency, event.RequestID)
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
		       u.unit_price_micros, u.amount_micros, u.currency, u.created_at, u.request_id,
		       EXISTS (SELECT 1 FROM ledger_entries l WHERE l.event_id = u.event_id),
		       COALESCE(p.processing_failures, 0), COALESCE(p.failure_code, ''), COALESCE(p.retry_generation, 0)
		FROM usage_events u LEFT JOIN pending_events p ON p.event_id = u.event_id
		WHERE u.event_id = $1`, id).Scan(
		&event.EventID, &event.CustomerID, &event.Meter, &event.Units,
		&event.UnitPriceMicros, &event.AmountMicros, &event.Currency,
		&event.CreatedAt, &event.RequestID, &event.Processed,
		&event.ProcessingFailures, &event.FailureCode, &event.RetryGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return billing.Event{}, billing.ErrNotFound
	}
	if err != nil {
		return billing.Event{}, fmt.Errorf("read usage event: %w", err)
	}
	event.Failed = event.ProcessingFailures == 3
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
		       COUNT(p.event_id) FILTER (WHERE p.processing_failures < 3), COUNT(l.event_id),
		       COUNT(p.event_id) FILTER (WHERE p.processing_failures = 3)
		FROM usage_events u
		LEFT JOIN ledger_entries l ON l.event_id = u.event_id
		LEFT JOIN pending_events p ON p.event_id = u.event_id
		WHERE u.customer_id = $1`, customer).Scan(
		&summary.Units, &summary.AmountMicros, &summary.Pending, &summary.Processed, &summary.Failed)
	if err != nil {
		return billing.Summary{}, fmt.Errorf("summarize usage: %w", err)
	}
	return summary, nil
}

// QueueStats reports pending count, failed count and oldest eligible age in
// seconds from one consistent statement. Failed work is not included in age.
func (s *Store) QueueStats(ctx context.Context) (int64, int64, float64, error) {
	var pending, failed int64
	var oldestAgeSeconds float64
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE processing_failures < 3), COUNT(*) FILTER (WHERE processing_failures = 3),
		       COALESCE(EXTRACT(EPOCH FROM statement_timestamp() - MIN(enqueued_at) FILTER (WHERE processing_failures < 3)), 0)::double precision
		FROM pending_events`).Scan(&pending, &failed, &oldestAgeSeconds)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("read pending queue metrics: %w", err)
	}
	if oldestAgeSeconds < 0 {
		oldestAgeSeconds = 0
	}
	return pending, failed, oldestAgeSeconds, nil
}

// ProcessBatch claims a bounded set of unlocked items. Ledger insertion and
// queue removal share a transaction; cancellation/crash rolls both back. Unique
// ledger IDs make retries harmless, without promising external exactly-once I/O.
// Confirmed integrity failures are isolated; three failures quarantine a row.
// The returned count excludes failed work and is nonzero only after commit.
func (s *Store) ProcessBatch(ctx context.Context, limit int) (int, error) {
	result, err := s.ProcessBatchWithResults(ctx, limit)
	return result.Processed, err
}

// ProcessBatchWithResults also reports correlation metadata for each claimed row.
// On an error, only RequestID identifies attempted work; no outcome is confirmed.
func (s *Store) ProcessBatchWithResults(ctx context.Context, limit int) (billing.BatchResult, error) {
	if limit < 1 || limit > 1000 {
		return billing.BatchResult{}, fmt.Errorf("batch limit must be between 1 and 1000")
	}
	result := billing.BatchResult{}
	processed := 0
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT p.event_id, u.request_id, p.processing_failures, p.retry_generation
			FROM pending_events p JOIN usage_events u ON u.event_id = p.event_id
			WHERE p.processing_failures < 3
			ORDER BY p.enqueued_at, p.event_id
			LIMIT $1 FOR UPDATE OF p SKIP LOCKED`, limit)
		if err != nil {
			return fmt.Errorf("claim pending events: %w", err)
		}
		defer rows.Close()
		ids := make([]string, 0, limit)
		for rows.Next() {
			var id string
			var item billing.ProcessingEvent
			if err := rows.Scan(&id, &item.RequestID, &item.ProcessingFailures, &item.RetryGeneration); err != nil {
				return fmt.Errorf("read claimed event: %w", err)
			}
			ids = append(ids, id)
			result.Events = append(result.Events, item)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("read claimed events: %w", err)
		}
		if len(ids) == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `SAVEPOINT ledger_batch`); err != nil {
			return fmt.Errorf("save ledger batch: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO ledger_entries (event_id)
			SELECT unnest($1::text[]) ON CONFLICT (event_id) DO NOTHING`, ids); err != nil {
			if integrityCode(err) == "" {
				return fmt.Errorf("post ledger entries: %w", err)
			}
			if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT ledger_batch`); err != nil {
				return fmt.Errorf("restore ledger batch: %w", err)
			}
			// Keep the claimed row locks. Only confirmed per-event integrity errors
			// consume a failure attempt; the failed bulk probe does not.
			processed, err = processIndividually(ctx, tx, ids, result.Events)
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM pending_events WHERE event_id = ANY($1::text[])`, ids)
		if err != nil {
			return fmt.Errorf("remove completed pending events: %w", err)
		}
		processed = int(tag.RowsAffected())
		for i := range result.Events {
			result.Events[i].Outcome = "processed"
		}
		return nil
	})
	if err != nil {
		// A commit error may be ambiguous. Do not report tentative outcomes,
		// failure counters, or rollback as durable facts.
		for i := range result.Events {
			result.Events[i] = billing.ProcessingEvent{RequestID: result.Events[i].RequestID}
		}
		return result, fmt.Errorf("process usage batch: %w", err)
	}
	result.Processed = processed
	return result, nil
}

// Retry holds the same row lock as workers. Failed work still occupies a queue
// slot, so reactivation cannot exceed the admission limit or require a new slot.
func (s *Store) Retry(ctx context.Context, id string, generation int64) (billing.Event, bool, error) {
	if billing.ValidateID(id) != nil || generation < 0 || generation == math.MaxInt64 {
		return billing.Event{}, false, billing.ErrInvalid
	}
	var event billing.Event
	var retried bool
	err := s.transaction(ctx, func(tx pgx.Tx) error {
		var failures int
		var current int64
		err := tx.QueryRow(ctx, `SELECT processing_failures, retry_generation FROM pending_events
			WHERE event_id = $1 FOR UPDATE`, id).Scan(&failures, &current)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("lock retry event: %w", err)
		}
		if err == nil {
			switch {
			case failures == 3 && current == generation:
				if _, err := tx.Exec(ctx, `UPDATE pending_events SET processing_failures = 0,
					failure_code = '', retry_generation = retry_generation + 1, enqueued_at = clock_timestamp()
					WHERE event_id = $1`, id); err != nil {
					return fmt.Errorf("reactivate failed event: %w", err)
				}
				retried = true
			case failures < 3 && current == generation+1:
				// A repeated request for the already reactivated generation is a no-op.
			default:
				return billing.ErrRetryConflict
			}
		}
		event, err = readEvent(ctx, tx, id)
		return err
	})
	if err != nil {
		return billing.Event{}, false, fmt.Errorf("retry failed event: %w", err)
	}
	return event, retried, nil
}

// integrityCode intentionally excludes timeouts, cancellation, deadlocks,
// serialization, schema/permission errors and connection failures. Never retain
// driver messages: they can contain SQL values, credentials or customer data.
func integrityCode(err error) string {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		return ""
	}
	switch pgErr.Code {
	case "23502", "23503", "23505", "23514", "23P01":
		return pgErr.Code
	default:
		return ""
	}
}

func processIndividually(ctx context.Context, tx pgx.Tx, ids []string, results []billing.ProcessingEvent) (int, error) {
	processed := 0
	for i, id := range ids {
		if _, err := tx.Exec(ctx, `SAVEPOINT ledger_event`); err != nil {
			return 0, fmt.Errorf("save ledger event: %w", err)
		}
		_, postErr := tx.Exec(ctx, `INSERT INTO ledger_entries (event_id) VALUES ($1)
			ON CONFLICT (event_id) DO NOTHING`, id)
		if postErr != nil {
			code := integrityCode(postErr)
			if code == "" {
				return 0, fmt.Errorf("post isolated event: %w", postErr)
			}
			if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT ledger_event`); err != nil {
				return 0, fmt.Errorf("restore ledger event: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE pending_events SET processing_failures = processing_failures + 1,
				failure_code = $2 WHERE event_id = $1`, id, code); err != nil {
				return 0, fmt.Errorf("record event failure: %w", err)
			}
			results[i].ProcessingFailures++
			results[i].FailureCode = code
			results[i].Outcome = "retry_scheduled"
			if results[i].ProcessingFailures == 3 {
				results[i].Outcome = "quarantined"
			}
		} else {
			if _, err := tx.Exec(ctx, `DELETE FROM pending_events WHERE event_id = $1`, id); err != nil {
				return 0, fmt.Errorf("remove isolated completed event: %w", err)
			}
			processed++
			results[i].Outcome = "processed"
		}
		if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT ledger_event`); err != nil {
			return 0, fmt.Errorf("release ledger event: %w", err)
		}
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
