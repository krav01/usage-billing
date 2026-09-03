//go:build integration

package postgres_test

import (
	"os"
	"testing"

	"github.com/krav01/usage-billing/internal/postgres"
)

func TestRequestIDMigrationPreservesLegacyAndRefusesDataLoss(t *testing.T) {
	t.Parallel()
	store, pool := fixture(t)
	legacy := event("legacy", "synthetic", 2, 1000)
	if _, _, err := store.Accept(t.Context(), legacy); err != nil {
		t.Fatal(err)
	}
	// Exercise a downgrade/upgrade while only pre-correlation rows exist.
	for _, path := range []string{
		"../../migrations/000003_request_id.down.sql",
		"../../migrations/000003_request_id.up.sql",
	} {
		migration, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), string(migration)); err != nil {
			t.Fatal(err)
		}
	}
	old, err := store.Get(t.Context(), legacy.EventID)
	if err != nil || old.RequestID != "" || old.AmountMicros != 2000 {
		t.Fatalf("migration rewrote legacy usage: %+v %v", old, err)
	}
	result, err := postgres.New(pool).ProcessBatchWithResults(t.Context(), 1)
	if err != nil || result.Processed != 1 || len(result.Events) != 1 || result.Events[0].RequestID != "" {
		t.Fatalf("legacy processing invented an ID or failed: %+v %v", result, err)
	}
	input := event("correlated", "synthetic", 1, 1000)
	input.RequestID = "0123456789abcdef0123456789abcdef"
	if _, _, err := store.Accept(t.Context(), input); err != nil {
		t.Fatal(err)
	}
	down, err := os.ReadFile("../../migrations/000003_request_id.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(t.Context(), string(down)); err == nil {
		t.Fatal("downgrade erased correlation history")
	}
	if _, err := conn.Exec(t.Context(), `ROLLBACK`); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(t.Context(), input.EventID)
	if err != nil || got.RequestID != input.RequestID {
		t.Fatalf("failed downgrade changed metadata: %+v %v", got, err)
	}
}

func TestAcceptRejectsInvalidPersistedRequestID(t *testing.T) {
	t.Parallel()
	store, _ := fixture(t)
	input := event("invalid", "synthetic", 1, 1000)
	input.RequestID = "forged\nlog-entry"
	if _, _, err := store.Accept(t.Context(), input); err == nil {
		t.Fatal("invalid request ID persisted")
	}
	result, err := store.ProcessBatchWithResults(t.Context(), 1)
	if err != nil || result.Processed != 0 || len(result.Events) != 0 {
		t.Fatalf("invalid event was queued: %+v %v", result, err)
	}
}
