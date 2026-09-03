//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/krav01/usage-billing/internal/billing"
	"github.com/krav01/usage-billing/internal/postgres"
)

func TestProcessBatchQuarantinesOnlyBrokenEvents(t *testing.T) {
	t.Parallel()
	store, pool := fixture(t)
	for _, id := range []string{"a-healthy", "b-broken", "c-healthy"} {
		if _, _, err := store.Accept(t.Context(), event(id, "recovery", 7, 1000)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(t.Context(), `ALTER TABLE ledger_entries ADD CONSTRAINT reject_broken CHECK (event_id <> 'b-broken')`); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		want := 0
		if attempt == 1 {
			want = 2
		}
		// No in-memory attempt counter: a new store must observe durable state.
		if n, err := postgres.New(pool).ProcessBatch(t.Context(), 10); err != nil || n != want {
			t.Fatalf("attempt %d: processed=%d err=%v", attempt, n, err)
		}
		got, err := store.Get(t.Context(), "b-broken")
		if err != nil || got.ProcessingFailures != attempt || got.Failed != (attempt == 3) ||
			got.FailureCode != "23514" || got.Processed || got.AmountMicros != 7000 {
			t.Fatalf("attempt %d: event=%+v err=%v", attempt, got, err)
		}
	}
	if n, err := store.ProcessBatch(t.Context(), 10); err != nil || n != 0 {
		t.Fatalf("quarantined work was claimed: %d, %v", n, err)
	}
	got, err := store.Summary(t.Context(), "recovery")
	if err != nil || got.Failed != 1 || got.Pending != 0 || got.Processed != 2 || got.AmountMicros != "14000" {
		t.Fatalf("summary: %+v, %v", got, err)
	}
	pending, failed, age, err := store.QueueStats(t.Context())
	if err != nil || pending != 0 || failed != 1 || age != 0 {
		t.Fatalf("quarantine metrics: %d %d %v %v", pending, failed, age, err)
	}
	replayed, fresh, err := store.Accept(t.Context(), event("b-broken", "recovery", 7, 9000))
	if err != nil || fresh || !replayed.Failed || replayed.ProcessingFailures != 3 || replayed.AmountMicros != 7000 {
		t.Fatalf("acceptance replay changed quarantine or price: %+v %v %v", replayed, fresh, err)
	}
	if _, err := pool.Exec(t.Context(), `ALTER TABLE ledger_entries DROP CONSTRAINT reject_broken`); err != nil {
		t.Fatal(err)
	}
	retried, changed, err := store.Retry(t.Context(), "b-broken", 0)
	if err != nil || !changed || retried.Failed || retried.ProcessingFailures != 0 ||
		retried.RetryGeneration != 1 || retried.FailureCode != "" || retried.AmountMicros != 7000 {
		t.Fatalf("retry: %+v %v %v", retried, changed, err)
	}
	if n, err := store.ProcessBatch(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("recovery: %d %v", n, err)
	}
	for range 3 {
		e, changed, err := store.Retry(t.Context(), "b-broken", 0)
		if err != nil || changed || !e.Processed || e.AmountMicros != 7000 {
			t.Fatalf("processed retry not harmless: %+v %v %v", e, changed, err)
		}
	}
	assertSummary(t, store, "recovery", "21", "21000", 0, 3)
}

func TestRetryConcurrentGenerationAndCapacity(t *testing.T) {
	t.Parallel()
	_, pool := fixture(t)
	store, err := postgres.NewWithQueueLimit(pool, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Accept(t.Context(), event("broken", "recovery", 1, 100)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `ALTER TABLE ledger_entries ADD CHECK (event_id <> 'broken')`); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			for range 5 {
				if n, err := store.ProcessBatch(t.Context(), 1); err != nil || n != 0 {
					t.Errorf("competing workers: %d %v", n, err)
				}
			}
		})
	}
	workers.Wait()
	broken, err := store.Get(t.Context(), "broken")
	if err != nil || !broken.Failed || broken.ProcessingFailures != 3 {
		t.Fatalf("concurrent quarantine: %+v %v", broken, err)
	}
	if _, _, err := store.Accept(t.Context(), event("overflow", "recovery", 1, 100)); !errors.Is(err, billing.ErrQueueFull) {
		t.Fatalf("failed event did not retain capacity: %v", err)
	}
	if _, err := store.Get(t.Context(), "overflow"); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("rejected admission persisted: %v", err)
	}
	var reactivations atomic.Int64
	var clients sync.WaitGroup
	for range 16 {
		clients.Go(func() {
			e, changed, err := store.Retry(t.Context(), "broken", 0)
			if err != nil || e.RetryGeneration != 1 || e.Failed {
				t.Errorf("concurrent retry: %+v %v", e, err)
			}
			if changed {
				reactivations.Add(1)
			}
		})
	}
	clients.Wait()
	if reactivations.Load() != 1 {
		t.Fatalf("reactivated %d times", reactivations.Load())
	}
	for range 3 {
		if _, err := store.ProcessBatch(t.Context(), 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := store.Retry(t.Context(), "broken", 0); !errors.Is(err, billing.ErrRetryConflict) {
		t.Fatalf("stale request reactivated a later failure: %v", err)
	}
	if _, changed, err := store.Retry(t.Context(), "broken", 1); err != nil || !changed {
		t.Fatalf("current generation retry: %v %v", changed, err)
	}
	for _, generation := range []int64{-1, math.MaxInt64} {
		if _, _, err := store.Retry(t.Context(), "broken", generation); !errors.Is(err, billing.ErrInvalid) {
			t.Fatalf("unsafe generation accepted: %d %v", generation, err)
		}
	}
	if _, _, err := store.Retry(t.Context(), "unknown", 0); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("missing retry: %v", err)
	}
}

func TestProcessBatchDeadlineDoesNotConsumeFailures(t *testing.T) {
	t.Parallel()
	store, pool := fixture(t)
	if _, _, err := store.Accept(t.Context(), event("blocked", "recovery", 1, 100)); err != nil {
		t.Fatal(err)
	}
	err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `LOCK TABLE ledger_entries IN SHARE MODE`); err != nil {
			return err
		}
		for range 3 {
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			n, err := store.ProcessBatch(ctx, 1)
			cancel()
			if n != 0 || !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("blocked batch: %d %v", n, err)
			}
			// pgx closes cancelled connections asynchronously. Wait for the actual
			// server-side row-lock release; a new SKIP LOCKED call may legally
			// return an empty batch until that rollback has finished.
			barrierCtx, barrierCancel := context.WithTimeout(t.Context(), 5*time.Second)
			var unlockedID string
			err = pool.QueryRow(barrierCtx, `SELECT event_id FROM pending_events
				WHERE event_id = 'blocked' FOR UPDATE`).Scan(&unlockedID)
			barrierCancel()
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e, err := store.Get(t.Context(), "blocked")
	if err != nil || e.Failed || e.ProcessingFailures != 0 || e.FailureCode != "" || e.Processed {
		t.Fatalf("deadline corrupted event state: %+v %v", e, err)
	}
	if n, err := store.ProcessBatch(t.Context(), 1); err != nil || n != 1 {
		t.Fatalf("recovery after deadline: %d %v", n, err)
	}
}

func TestRetryCancellationAndConcurrentWorker(t *testing.T) {
	t.Parallel()
	store, pool := fixture(t)
	if _, _, err := store.Accept(t.Context(), event("failed", "recovery", 2, 100)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE pending_events SET processing_failures = 3, failure_code = '23514'`); err != nil {
		t.Fatal(err)
	}
	err := pgx.BeginFunc(t.Context(), pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `SELECT event_id FROM pending_events WHERE event_id = 'failed' FOR UPDATE`); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		if _, changed, err := store.Retry(ctx, "failed", 0); changed || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("cancelled retry changed state: %v %v", changed, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e, err := store.Get(t.Context(), "failed")
	if err != nil || !e.Failed || e.RetryGeneration != 0 {
		t.Fatalf("cancelled retry survived: %+v %v", e, err)
	}
	var concurrent sync.WaitGroup
	var reactivated, processed atomic.Int64
	start := make(chan struct{})
	for range 12 {
		concurrent.Go(func() {
			<-start
			if _, changed, err := store.Retry(t.Context(), "failed", 0); err != nil {
				t.Errorf("concurrent retry: %v", err)
			} else if changed {
				reactivated.Add(1)
			}
		})
	}
	for range 3 {
		concurrent.Go(func() {
			<-start
			for range 5 {
				n, err := store.ProcessBatch(t.Context(), 1)
				if err != nil {
					t.Errorf("concurrent worker: %v", err)
				}
				processed.Add(int64(n))
			}
		})
	}
	close(start)
	concurrent.Wait()
	// Workers may legally finish polling before the first retry wins its lock.
	n, err := store.ProcessBatch(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	processed.Add(int64(n))
	if reactivated.Load() != 1 || processed.Load() != 1 {
		t.Fatalf("reactivations=%d processed=%d", reactivated.Load(), processed.Load())
	}
	assertSummary(t, store, "recovery", "2", "200", 0, 1)
}

func TestProcessBatchBookkeepingErrorRollsBackHealthyWork(t *testing.T) {
	t.Parallel()
	store, pool := fixture(t)
	for _, id := range []string{"a-healthy", "b-broken"} {
		input := event(id, "recovery", 1, 100)
		input.RequestID = "0123456789abcdef0123456789abcdef"
		if _, _, err := store.Accept(t.Context(), input); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(t.Context(), `ALTER TABLE ledger_entries ADD CHECK (event_id <> 'b-broken');
		ALTER TABLE pending_events ADD CHECK (processing_failures = 0)`); err != nil {
		t.Fatal(err)
	}
	result, err := store.ProcessBatchWithResults(t.Context(), 10)
	if err == nil || result.Processed != 0 || len(result.Events) != 2 {
		t.Fatalf("bookkeeping failure falsely committed work: %+v %v", result, err)
	}
	for _, item := range result.Events {
		if item.RequestID != "0123456789abcdef0123456789abcdef" || item.Outcome != "" || item.ProcessingFailures != 0 {
			t.Fatalf("unconfirmed work lost correlation or claimed a durable outcome: %+v", item)
		}
	}
	assertSummary(t, store, "recovery", "0", "0", 2, 0)
	e, err := store.Get(t.Context(), "b-broken")
	if err != nil || e.ProcessingFailures != 0 {
		t.Fatalf("uncommitted failure survived: %+v %v", e, err)
	}
}

func TestRecoveryMigrationRefusesToEraseActiveState(t *testing.T) {
	t.Parallel()
	store, pool := fixture(t)
	if _, _, err := store.Accept(t.Context(), event("broken", "recovery", 1, 100)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE pending_events SET processing_failures = 3, failure_code = '23514'`); err != nil {
		t.Fatal(err)
	}
	down, err := os.ReadFile("../../migrations/000002_event_recovery.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(t.Context(), string(down)); err == nil {
		t.Error("down migration erased recovery state")
	}
	if _, err := conn.Exec(t.Context(), `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	e, err := store.Get(t.Context(), "broken")
	if err != nil || !e.Failed {
		t.Fatalf("migration guard lost quarantine: %+v %v", e, err)
	}
}
