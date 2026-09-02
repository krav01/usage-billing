//go:build integration

package postgres_test

import (
	"strconv"
	"testing"
)

// Each benchmark uses its own disposable schema; setup is excluded by b.Loop.
func BenchmarkStoreReplay(b *testing.B) {
	store, _ := fixture(b)
	input := event("bench-replay", "synthetic-customer", 7, 1000)
	if _, _, err := store.Accept(b.Context(), input); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		got, created, err := store.Accept(b.Context(), input)
		if err != nil || created || got.AmountMicros != 7000 {
			b.Fatalf("invalid replay: created=%v, amount=%d, err=%v", created, got.AmountMicros, err)
		}
	}
}

func BenchmarkStoreSummary(b *testing.B) {
	store, _ := fixture(b)
	for i := range 1000 {
		if _, _, err := store.Accept(b.Context(), event("bench-"+strconv.Itoa(i), "synthetic-customer", 1, 1000)); err != nil {
			b.Fatal(err)
		}
	}
	for range 10 {
		if n, err := store.ProcessBatch(b.Context(), 100); err != nil || n != 100 {
			b.Fatalf("settle fixture: n=%d, err=%v", n, err)
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		got, err := store.Summary(b.Context(), "synthetic-customer")
		if err != nil || got.Processed != 1000 || got.Pending != 0 || got.AmountMicros != "1000000" {
			b.Fatalf("invalid summary: %+v, err=%v", got, err)
		}
	}
}
