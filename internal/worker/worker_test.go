package worker_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/krav01/usage-billing/internal/billing"
	"github.com/krav01/usage-billing/internal/worker"
)

type processorFunc func(context.Context, int) (int, error)

func (f processorFunc) ProcessBatch(ctx context.Context, limit int) (int, error) {
	return f(ctx, limit)
}

type reportingProcessor struct {
	result billing.BatchResult
	err    error
	stop   context.CancelFunc
}

func (p reportingProcessor) ProcessBatch(context.Context, int) (int, error) {
	p.stop()
	return 0, errors.New("legacy method unexpectedly called")
}

func (p reportingProcessor) ProcessBatchWithResults(context.Context, int) (billing.BatchResult, error) {
	p.stop()
	return p.result, p.err
}

func TestRunLogsConfirmedAndUnconfirmedEventOutcomes(t *testing.T) {
	t.Parallel()
	const id = "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name, outcome, requestID, message string
		err                               error
	}{
		{name: "posted", outcome: "processed", requestID: id, message: "usage event processed"},
		{name: "retry", outcome: "retry_scheduled", requestID: id, message: "usage event processing failed"},
		{name: "quarantine", outcome: "quarantined", requestID: id, message: "usage event processing failed"},
		{name: "unconfirmed", outcome: "processed", requestID: id, message: "usage event outcome unconfirmed",
			err: errors.New("postgres://secret@private-host SQL customer-input")},
		{name: "legacy", outcome: "processed"},
		{name: "invalid metadata", outcome: "processed", requestID: "secret\nforged-log"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var logs bytes.Buffer
			p := reportingProcessor{
				result: billing.BatchResult{Events: []billing.ProcessingEvent{{
					RequestID: tc.requestID, Outcome: tc.outcome, RetryGeneration: 2,
					ProcessingFailures: 3, FailureCode: "23514",
				}}},
				err: tc.err, stop: cancel,
			}
			w := worker.New(p, time.Second, 1, slog.New(slog.NewJSONHandler(&logs, nil)))
			if err := w.Run(ctx); err != nil {
				t.Fatal(err)
			}
			if tc.message == "" {
				if logs.Len() != 0 {
					t.Fatalf("unexpected metadata log: %s", logs.String())
				}
				return
			}
			var record map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &record); err != nil {
				t.Fatal(err)
			}
			if record["msg"] != tc.message || record["request_id"] != id {
				t.Fatalf("incorrect event correlation: %s", logs.String())
			}
			if tc.err != nil && (record["outcome"] != nil || record["processing_failures"] != nil) {
				t.Fatal("unconfirmed metadata reported as committed")
			}
			for _, forbidden := range []string{"secret", "private-host", "customer-input", "forged-log"} {
				if strings.Contains(logs.String(), forbidden) {
					t.Fatalf("sensitive content reached logs: %q", forbidden)
				}
			}
		})
	}
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
		time.Sleep(2*time.Second - time.Nanosecond)
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
		stats := w.Snapshot()
		if stats.Running || stats.BatchInFlight || stats.BatchAttempts != 2 ||
			stats.BatchErrors != 1 || stats.BatchCancellations != 0 || stats.EventsProcessed != 2 {
			t.Fatalf("unexpected worker metrics: %+v", stats)
		}
	})
}

func TestRunBackoffCapsResetsAndCancels(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var calls atomic.Int64
		p := processorFunc(func(context.Context, int) (int, error) {
			n := calls.Add(1)
			if n <= 6 || n == 8 {
				return 0, errors.New("synthetic batch error")
			}
			return 1, nil
		})
		w := worker.New(p, time.Second, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
		done := make(chan error, 1)
		go func() { done <- w.Run(ctx) }()
		synctest.Wait()
		if calls.Load() != 1 {
			t.Fatal("worker did not start immediately")
		}
		for i, delay := range []time.Duration{2, 4, 8, 16, 30, 30, 1} {
			time.Sleep(delay*time.Second - time.Nanosecond)
			synctest.Wait()
			if calls.Load() != int64(i+1) {
				t.Fatalf("attempt before delay %v: calls=%d", delay, calls.Load())
			}
			time.Sleep(time.Nanosecond)
			synctest.Wait()
			if calls.Load() != int64(i+2) {
				t.Fatalf("missing attempt after delay %v: calls=%d", delay, calls.Load())
			}
		}
		start := time.Now()
		cancel()
		if err := <-done; err != nil || time.Since(start) != 0 {
			t.Fatalf("backoff shutdown was not immediate: %v", err)
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
		stats := w.Snapshot()
		if stats.Running || stats.BatchInFlight || stats.BatchAttempts != 1 ||
			stats.BatchErrors != 0 || stats.BatchCancellations != 1 || stats.EventsProcessed != 0 {
			t.Fatalf("unexpected cancellation metrics: %+v", stats)
		}
	})
}

func TestRunRejectsConcurrentExecution(t *testing.T) {
	t.Parallel()
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	p := processorFunc(func(ctx context.Context, _ int) (int, error) {
		close(started)
		<-ctx.Done()
		return 0, ctx.Err()
	})
	w := worker.New(p, time.Hour, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	<-started
	if err := w.Run(t.Context()); err == nil {
		t.Fatal("concurrent execution accepted")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
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
