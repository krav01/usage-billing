// Package worker posts accepted usage in bounded, retryable batches.
package worker

import (
	"context"
	"errors"
	"log/slog"
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
			count, err := w.processor.ProcessBatch(attemptCtx, w.batch)
			cancel()
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
