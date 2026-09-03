//go:build integration

package postgres_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/krav01/usage-billing/internal/billing"
	"github.com/krav01/usage-billing/internal/postgres"
)

// Every test gets its own synthetic schema in TEST_DATABASE_URL. The supplied
// database must be disposable; no shared tables or existing schema are removed.
func fixture(t testing.TB) (*postgres.Store, *pgxpool.Pool) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_DATABASE_URL is required and must point to a disposable PostgreSQL database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	admin, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal("parse test database configuration")
	}
	schema := "billing_test_" + rand.Text()
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		admin.Close()
		t.Fatalf("create isolated test schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		// Only a locally generated, SQL-quoted schema identifier is interpolated.
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+identifier+" CASCADE"); err != nil {
			t.Errorf("remove isolated test schema: %v", err)
		}
		admin.Close()
	})
	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal("parse isolated test pool configuration")
	}
	config.MaxConns = 8
	config.ConnConfig.RuntimeParams["search_path"] = identifier
	config.ConnConfig.RuntimeParams["statement_timeout"] = "5000"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("create isolated test pool")
	}
	t.Cleanup(pool.Close)
	migration, err := os.ReadFile("../../migrations/000001_init.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, string(migration)); err != nil {
		t.Fatalf("apply test fixture: %v", err)
	}
	return postgres.New(pool), pool
}

func event(id, customer string, units, price int64) billing.Event {
	return billing.Event{
		Input:           billing.Input{EventID: id, CustomerID: customer, Meter: "api_calls", Units: units},
		UnitPriceMicros: price, AmountMicros: units * price, Currency: "USD",
	}
}

func TestNewWithQueueLimit(t *testing.T) {
	t.Parallel()
	_, pool := fixture(t)
	for _, limit := range []int64{-1, 0, 1000001} {
		if _, err := postgres.NewWithQueueLimit(pool, limit); err == nil {
			t.Errorf("invalid queue limit accepted: %d", limit)
		}
	}
	if _, err := postgres.NewWithQueueLimit(nil, 1); err == nil {
		t.Fatal("nil pool accepted")
	}
}

func TestAcceptConcurrentReplayFrozenPriceAndConflict(t *testing.T) {
	t.Parallel()
	store, _ := fixture(t)
	input := event("same-event", "synthetic-customer", 3, 1000)
	var created atomic.Int64
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			got, fresh, err := store.Accept(t.Context(), input)
			if err != nil {
				t.Errorf("accept: %v", err)
				return
			}
			if fresh {
				created.Add(1)
			}
			if got.Input != input.Input || got.AmountMicros != 3000 || got.CreatedAt.IsZero() {
				t.Errorf("unexpected persisted event: %+v", got)
			}
		})
	}
	wg.Wait()
	if created.Load() != 1 {
		t.Fatalf("created %d events, want 1", created.Load())
	}
	repriced := input
	repriced.UnitPriceMicros, repriced.AmountMicros = 2000, 6000
	got, fresh, err := store.Accept(t.Context(), repriced)
	if err != nil || fresh || got.UnitPriceMicros != 1000 || got.AmountMicros != 3000 {
		t.Fatalf("replay changed frozen price: %+v, created=%v, err=%v", got, fresh, err)
	}
	conflict := event(input.EventID, "other-customer", 3, 1000)
	if _, _, err := store.Accept(t.Context(), conflict); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("conflict returned %v", err)
	}
	summary, err := store.Summary(t.Context(), input.CustomerID)
	if err != nil || summary.Pending != 1 || summary.Processed != 0 || summary.AmountMicros != "0" {
		t.Fatalf("pending summary: %+v, %v", summary, err)
	}
}

func TestQueueCapacityConcurrentAdmissionsAndReplay(t *testing.T) {
	t.Parallel()
	_, pool := fixture(t)
	const capacity = 3
	stores := make([]*postgres.Store, 2)
	for i := range stores {
		var err error
		stores[i], err = postgres.NewWithQueueLimit(pool, capacity)
		if err != nil {
			t.Fatal(err)
		}
	}
	var accepted, rejected atomic.Int64
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Go(func() {
			input := event(fmt.Sprintf("bounded-%d", i), "bounded", 1, 1000)
			_, created, err := stores[i%len(stores)].Accept(t.Context(), input)
			switch {
			case err == nil && created:
				accepted.Add(1)
			case errors.Is(err, billing.ErrQueueFull):
				rejected.Add(1)
				if _, err := stores[0].Get(t.Context(), input.EventID); !errors.Is(err, billing.ErrNotFound) {
					t.Errorf("rejected input survived rollback: %v", err)
				}
			default:
				t.Errorf("unexpected admission result: created=%v err=%v", created, err)
			}
		})
	}
	wg.Wait()
	if accepted.Load() != capacity || rejected.Load() != 17 {
		t.Fatalf("accepted=%d rejected=%d", accepted.Load(), rejected.Load())
	}
	assertSummary(t, stores[0], "bounded", "0", "0", capacity, 0)
	lowered, err := postgres.NewWithQueueLimit(pool, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := lowered.Accept(t.Context(), event("lowered-limit", "bounded", 1, 1000)); !errors.Is(err, billing.ErrQueueFull) {
		t.Fatalf("lowered limit must reject new work: %v", err)
	}
	assertSummary(t, lowered, "bounded", "0", "0", capacity, 0)
	var id string
	if err := pool.QueryRow(t.Context(), `SELECT event_id FROM pending_events ORDER BY event_id LIMIT 1`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	input := event(id, "bounded", 1, 2000)
	got, created, err := stores[1].Accept(t.Context(), input)
	if err != nil || created || got.AmountMicros != 1000 {
		t.Fatalf("full queue rejected replay or changed price: %+v created=%v err=%v", got, created, err)
	}
	input.Units++
	input.AmountMicros = input.Units * input.UnitPriceMicros
	if _, _, err := stores[0].Accept(t.Context(), input); !errors.Is(err, billing.ErrConflict) {
		t.Fatalf("full queue masked conflict: %v", err)
	}
	if n, err := stores[0].ProcessBatch(t.Context(), 1); err != nil || n != 1 {
		t.Fatalf("free one slot: n=%d err=%v", n, err)
	}
	if _, created, err := stores[1].Accept(t.Context(), event("after-drain", "bounded", 1, 1000)); err != nil || !created {
		t.Fatalf("admission did not recover: created=%v err=%v", created, err)
	}
	assertSummary(t, stores[0], "bounded", "1", "1000", capacity, 1)
}

func TestAcceptRollsBackWhenQueueInsertionFails(t *testing.T) {
	t.Parallel()
	store, pool := fixture(t)
	if _, err := pool.Exec(t.Context(), `ALTER TABLE pending_events ADD CHECK (event_id <> 'reject-event')`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Accept(t.Context(), event("reject-event", "synthetic", 1, 1)); err == nil {
		t.Fatal("expected queue insert failure")
	}
	if _, err := store.Get(t.Context(), "reject-event"); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("usage insert survived queue failure: %v", err)
	}
}

func TestProcessBatchRollbackSkipLockedAndRestart(t *testing.T) {
	t.Parallel()
	store, pool := fixture(t)
	for _, id := range []string{"first", "second"} {
		if _, _, err := store.Accept(t.Context(), event(id, "synthetic", 1, 100)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(t.Context(), `ALTER TABLE ledger_entries ADD CONSTRAINT fail_post CHECK (event_id <> 'second')`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProcessBatch(t.Context(), 10); err == nil {
		t.Fatal("expected ledger insert failure")
	}
	assertSummary(t, store, "synthetic", "0", "0", 2, 0)
	if _, err := pool.Exec(t.Context(), `ALTER TABLE ledger_entries DROP CONSTRAINT fail_post`); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Errorf("rollback test lock: %v", err)
		}
	})
	if _, err := tx.Exec(t.Context(), `SELECT event_id FROM pending_events WHERE event_id = 'first' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	if n, err := store.ProcessBatch(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("skip locked: n=%d err=%v", n, err)
	}
	assertSummary(t, store, "synthetic", "1", "100", 1, 1)
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A newly constructed worker/store finds durable work without in-memory state.
	restarted := postgres.New(pool)
	if n, err := restarted.ProcessBatch(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("restart: n=%d err=%v", n, err)
	}
	if n, err := restarted.ProcessBatch(t.Context(), 10); err != nil || n != 0 {
		t.Fatalf("repeat batch: n=%d err=%v", n, err)
	}
	assertSummary(t, restarted, "synthetic", "2", "200", 0, 2)
	got, err := restarted.Get(t.Context(), "first")
	if err != nil || !got.Processed {
		t.Fatalf("processed event: %+v, %v", got, err)
	}
}

func TestQueueStats(t *testing.T) {
	t.Parallel()
	store, _ := fixture(t)
	pending, age, err := store.QueueStats(t.Context())
	if err != nil || pending != 0 || age != 0 {
		t.Fatalf("empty queue metrics: pending=%d age=%v err=%v", pending, age, err)
	}
	for _, id := range []string{"metrics-first", "metrics-second"} {
		if _, _, err := store.Accept(t.Context(), event(id, "metrics-customer", 1, 1)); err != nil {
			t.Fatal(err)
		}
	}
	pending, age, err = store.QueueStats(t.Context())
	if err != nil || pending != 2 || age < 0 {
		t.Fatalf("pending queue metrics: pending=%d age=%v err=%v", pending, age, err)
	}
	if n, err := store.ProcessBatch(t.Context(), 1); err != nil || n != 1 {
		t.Fatalf("process one: n=%d err=%v", n, err)
	}
	pending, age, err = store.QueueStats(t.Context())
	if err != nil || pending != 1 || age < 0 {
		t.Fatalf("partial queue metrics: pending=%d age=%v err=%v", pending, age, err)
	}
	if n, err := store.ProcessBatch(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("process remainder: n=%d err=%v", n, err)
	}
	pending, age, err = store.QueueStats(t.Context())
	if err != nil || pending != 0 || age != 0 {
		t.Fatalf("drained queue metrics: pending=%d age=%v err=%v", pending, age, err)
	}
}

func TestConcurrentWorkersAndConsistentSummary(t *testing.T) {
	t.Parallel()
	store, _ := fixture(t)
	for i := range 50 {
		if _, _, err := store.Accept(t.Context(), event(fmt.Sprintf("event-%d", i), "synthetic", 1, 1)); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				n, err := store.ProcessBatch(t.Context(), 3)
				if err != nil {
					t.Errorf("process: %v", err)
					return
				}
				if n == 0 {
					return
				}
			}
		})
	}
	for range 30 {
		summary, err := store.Summary(t.Context(), "synthetic")
		if err != nil {
			t.Fatal(err)
		}
		if summary.Pending+summary.Processed != 50 || summary.AmountMicros != fmt.Sprint(summary.Processed) {
			t.Fatalf("inconsistent snapshot: %+v", summary)
		}
	}
	wg.Wait()
	assertSummary(t, store, "synthetic", "50", "50", 0, 50)
}

func TestSummaryNumericTotalsBeyondInt64(t *testing.T) {
	t.Parallel()
	store, _ := fixture(t)
	assertSummary(t, store, "missing", "0", "0", 0, 0)
	for _, id := range []string{"large-1", "large-2"} {
		if _, _, err := store.Accept(t.Context(), event(id, "large", math.MaxInt64, 1)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ProcessBatch(t.Context(), 10); err != nil {
		t.Fatal(err)
	}
	assertSummary(t, store, "large", "18446744073709551614", "18446744073709551614", 0, 2)
}

func assertSummary(t *testing.T, store *postgres.Store, customer, units, amount string, pending, processed int64) {
	t.Helper()
	got, err := store.Summary(t.Context(), customer)
	if err != nil {
		t.Fatal(err)
	}
	want := billing.Summary{
		CustomerID: customer, Currency: "USD", Units: units,
		AmountMicros: amount, Pending: pending, Processed: processed,
	}
	if got != want {
		t.Fatalf("summary = %+v, want %+v", got, want)
	}
}
