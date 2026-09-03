// Package telemetry exports bounded operational metrics for the demo service.
package telemetry

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/krav01/usage-billing/internal/worker"
)

type QueueStatsFunc func(context.Context) (pending, failed int64, oldestAgeSeconds float64, err error)

type Collector struct {
	queueStats  QueueStatsFunc
	workerStats func() worker.Stats
	queueErrors atomic.Uint64
}

func New(queueStats QueueStatsFunc, workerStats func() worker.Stats) (*Collector, error) {
	if queueStats == nil || workerStats == nil {
		return nil, errors.New("queue and worker metrics callbacks are required")
	}
	return &Collector{queueStats: queueStats, workerStats: workerStats}, nil
}

// Metrics returns Prometheus text with no business identifiers or unbounded labels.
func (c *Collector) Metrics(ctx context.Context) string {
	var body strings.Builder
	pending, failed, age, err := c.queueStats(ctx)
	body.WriteString("# HELP usage_billing_queue_scrape_success Whether the latest queue query succeeded.\n")
	body.WriteString("# TYPE usage_billing_queue_scrape_success gauge\n")
	if err != nil {
		c.queueErrors.Add(1)
		body.WriteString("usage_billing_queue_scrape_success 0\n")
	} else {
		body.WriteString("usage_billing_queue_scrape_success 1\n")
		body.WriteString("# HELP usage_billing_queue_pending_events Durable events waiting for the worker.\n")
		body.WriteString("# TYPE usage_billing_queue_pending_events gauge\n")
		writeInt(&body, "usage_billing_queue_pending_events ", pending)
		// Alert: usage_billing_queue_failed_events > 0, gated by scrape success.
		body.WriteString("# HELP usage_billing_queue_failed_events Durable events requiring manual recovery.\n")
		body.WriteString("# TYPE usage_billing_queue_failed_events gauge\n")
		writeInt(&body, "usage_billing_queue_failed_events ", failed)
		body.WriteString("# HELP usage_billing_queue_oldest_event_age_seconds Age of the oldest pending event.\n")
		body.WriteString("# TYPE usage_billing_queue_oldest_event_age_seconds gauge\n")
		writeFloat(&body, "usage_billing_queue_oldest_event_age_seconds ", age)
	}
	body.WriteString("# HELP usage_billing_queue_scrape_errors_total Failed queue metric queries.\n")
	body.WriteString("# TYPE usage_billing_queue_scrape_errors_total counter\n")
	writeUint(&body, "usage_billing_queue_scrape_errors_total ", c.queueErrors.Load())

	stats := c.workerStats()
	body.WriteString("# HELP usage_billing_worker_running Whether this process owns a running worker loop.\n")
	body.WriteString("# TYPE usage_billing_worker_running gauge\n")
	writeBool(&body, "usage_billing_worker_running ", stats.Running)
	body.WriteString("# HELP usage_billing_worker_batch_in_flight Whether a worker batch is currently running.\n")
	body.WriteString("# TYPE usage_billing_worker_batch_in_flight gauge\n")
	writeBool(&body, "usage_billing_worker_batch_in_flight ", stats.BatchInFlight)
	body.WriteString("# HELP usage_billing_worker_batch_attempts_total Worker batch calls completed or cancelled.\n")
	body.WriteString("# TYPE usage_billing_worker_batch_attempts_total counter\n")
	writeUint(&body, "usage_billing_worker_batch_attempts_total ", stats.BatchAttempts)
	body.WriteString("# HELP usage_billing_worker_batch_errors_total Worker batch calls that failed for a non-context error.\n")
	body.WriteString("# TYPE usage_billing_worker_batch_errors_total counter\n")
	writeUint(&body, "usage_billing_worker_batch_errors_total ", stats.BatchErrors)
	body.WriteString("# HELP usage_billing_worker_batch_cancellations_total Worker batch calls stopped by cancellation or deadline.\n")
	body.WriteString("# TYPE usage_billing_worker_batch_cancellations_total counter\n")
	writeUint(&body, "usage_billing_worker_batch_cancellations_total ", stats.BatchCancellations)
	body.WriteString("# HELP usage_billing_worker_events_processed_total Events durably removed from the pending queue by this process.\n")
	body.WriteString("# TYPE usage_billing_worker_events_processed_total counter\n")
	writeUint(&body, "usage_billing_worker_events_processed_total ", stats.EventsProcessed)
	return body.String()
}

func writeBool(body *strings.Builder, prefix string, value bool) {
	body.WriteString(prefix)
	if value {
		body.WriteByte('1')
	} else {
		body.WriteByte('0')
	}
	body.WriteByte('\n')
}

func writeInt(body *strings.Builder, prefix string, value int64) {
	body.WriteString(prefix)
	body.WriteString(strconv.FormatInt(value, 10))
	body.WriteByte('\n')
}

func writeUint(body *strings.Builder, prefix string, value uint64) {
	body.WriteString(prefix)
	body.WriteString(strconv.FormatUint(value, 10))
	body.WriteByte('\n')
}

func writeFloat(body *strings.Builder, prefix string, value float64) {
	body.WriteString(prefix)
	body.WriteString(strconv.FormatFloat(value, 'g', -1, 64))
	body.WriteByte('\n')
}
