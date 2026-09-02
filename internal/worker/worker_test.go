package worker_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/krav01/usage-billing/internal/worker"
)

type processorFunc func(context.Context, int) (int, error)

func (f processorFunc) ProcessBatch(ctx context.Context, limit int) (int, error) {
	return f(ctx, limit)
}

func TestRunRetriesWithoutBusyLoopOrSecretLogs(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var logs bytes.Buffer
		var calls atomic.Int64
		p := processorFunc(func(ctx context.Context, limit int) (int, error) {
			call := calls.Add(1)
			if limit != 17 {
				t.Errorf("batch = %d, want 17", limit)
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Error("batch context has no deadline")
			}
			if call == 1 {
				return 0, errors.New("postgres://secret-password@private-host/billing")
			}
			return 2, nil
		})
		w := worker.New(p, time.Second, 17, slog.New(slog.NewJSONHandler(&logs, nil)))
		done := make(chan error, 1)
		go func() { done <- w.Run(ctx) }()
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("initial calls = %d, want 1", calls.Load())
		}
		time.Sleep(time.Second - time.Nanosecond)
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatalf("retried before delay: %d calls", calls.Load())
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatalf("calls after retry = %d, want 2", calls.Load())
		}
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if strings.Contains(logs.String(), "secret") || strings.Contains(logs.String(), "private-host") {
			t.Fatal("driver details leaked into logs")
		}
		if !strings.Contains(logs.String(), "retry scheduled") || !strings.Contains(logs.String(), "completed") {
			t.Fatal("missing retry or recovery log")
		}
	})
}

func TestRunCancelsInFlightBatch(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		started := make(chan struct{})
		p := processorFunc(func(ctx context.Context, _ int) (int, error) {
			close(started)
			<-ctx.Done()
			return 0, ctx.Err()
		})
		w := worker.New(p, time.Hour, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
		done := make(chan error, 1)
		go func() { done <- w.Run(ctx) }()
		<-started
		cancel()
		if err := <-done; err != nil {
			t.Fatalf("cancellation returned %v", err)
		}
	})
}

func TestRunRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := processorFunc(func(context.Context, int) (int, error) { return 0, nil })
	tests := []struct {
		name     string
		p        worker.Processor
		interval time.Duration
		batch    int
		logger   *slog.Logger
	}{
		{name: "processor", interval: time.Second, batch: 1, logger: logger},
		{name: "interval", p: p, batch: 1, logger: logger},
		{name: "batch zero", p: p, interval: time.Second, logger: logger},
		{name: "batch large", p: p, interval: time.Second, batch: 1001, logger: logger},
		{name: "logger", p: p, interval: time.Second, batch: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if err := worker.New(tt.p, tt.interval, tt.batch, tt.logger).Run(t.Context()); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}
