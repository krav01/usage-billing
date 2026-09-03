// Package worker posts accepted usage in bounded, retryable batches.
package worker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

type Processor interface {
	ProcessBatch(context.Context, int) (int, error)
}

type Worker struct {
	processor Processor
	interval  time.Duration
	batch     int
	logger    *slog.Logger
	mu        sync.Mutex
	stats     Stats
}

// Stats is a point-in-time, process-local worker snapshot.
type Stats struct {
	Running            bool
	BatchInFlight      bool
	BatchAttempts      uint64
	BatchErrors        uint64
	BatchCancellations uint64
	EventsProcessed    uint64
}

func New(processor Processor, interval time.Duration, batch int, logger *slog.Logger) *Worker {
	return &Worker{processor: processor, interval: interval, batch: batch, logger: logger}
}

// Run is synchronous: its caller owns the goroutine and waits for shutdown.
// Each attempt is followed by a delay, including failures and full batches.
func (w *Worker) Run(ctx context.Context) error {
	if w.processor == nil || w.logger == nil || w.interval <= 0 || w.batch < 1 || w.batch > 1000 {
		return errors.New("invalid worker configuration")
	}
	if !w.start() {
		return errors.New("worker is already running")
	}
	defer w.stop()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			if ctx.Err() != nil {
				return nil
			}
			attemptCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			w.beginAttempt()
			count, err := w.processor.ProcessBatch(attemptCtx, w.batch)
			attemptErr := attemptCtx.Err()
			cancel()
			w.finishAttempt(count, err, attemptErr)
			if ctx.Err() != nil {
				return nil
			}
			if err != nil {
				// Driver errors may contain credentials or SQL parameters. Never log
				// their text; retry uses the same transaction-safe pending queue.
				w.logger.Warn("usage batch failed; retry scheduled")
			} else if count > 0 {
				w.logger.Info("usage batch completed", "events", count)
			}
			timer.Reset(w.interval)
		}
	}
}

// Snapshot returns a race-safe copy for metrics collection.
func (w *Worker) Snapshot() Stats {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stats
}

func (w *Worker) start() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stats.Running {
		return false
	}
	w.stats.Running = true
	return true
}

func (w *Worker) stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stats.Running = false
	w.stats.BatchInFlight = false
}

func (w *Worker) beginAttempt() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stats.BatchInFlight = true
}

func (w *Worker) finishAttempt(count int, err, contextErr error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stats.BatchInFlight = false
	w.stats.BatchAttempts++
	if err != nil {
		if contextErr != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			w.stats.BatchCancellations++
		} else {
			w.stats.BatchErrors++
		}
		return
	}
	if count > 0 {
		w.stats.EventsProcessed += uint64(count)
	}
}
