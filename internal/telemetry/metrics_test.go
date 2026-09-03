package telemetry_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/krav01/usage-billing/internal/telemetry"
	"github.com/krav01/usage-billing/internal/worker"
)

func TestNew(t *testing.T) {
	t.Parallel()
	queue := func(context.Context) (int64, int64, float64, error) { return 0, 0, 0, nil }
	work := func() worker.Stats { return worker.Stats{} }
	if _, err := telemetry.New(nil, work); err == nil {
		t.Fatal("nil queue callback accepted")
	}
	if _, err := telemetry.New(queue, nil); err == nil {
		t.Fatal("nil worker callback accepted")
	}
}

func TestMetrics(t *testing.T) {
	t.Parallel()
	collector, err := telemetry.New(
		func(context.Context) (int64, int64, float64, error) { return 7, 3, 2.5, nil },
		func() worker.Stats {
			return worker.Stats{
				Running: true, BatchInFlight: true, BatchAttempts: 9,
				BatchErrors: 2, BatchCancellations: 1, EventsProcessed: 30,
			}
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	body := collector.Metrics(t.Context())
	for _, metric := range []string{
		"usage_billing_queue_scrape_success 1\n",
		"usage_billing_queue_pending_events 7\n",
		"usage_billing_queue_failed_events 3\n",
		"usage_billing_queue_oldest_event_age_seconds 2.5\n",
		"usage_billing_queue_scrape_errors_total 0\n",
		"usage_billing_worker_running 1\n",
		"usage_billing_worker_batch_in_flight 1\n",
		"usage_billing_worker_batch_attempts_total 9\n",
		"usage_billing_worker_batch_errors_total 2\n",
		"usage_billing_worker_batch_cancellations_total 1\n",
		"usage_billing_worker_events_processed_total 30\n",
	} {
		if !strings.Contains(body, metric) {
			t.Errorf("missing %q in:\n%s", metric, body)
		}
	}
}

func TestMetricsReportsQueueFailureWithoutFalseGauges(t *testing.T) {
	t.Parallel()
	collector, err := telemetry.New(
		func(context.Context) (int64, int64, float64, error) {
			return 0, 0, 0, errors.New("private database error")
		},
		func() worker.Stats { return worker.Stats{} },
	)
	if err != nil {
		t.Fatal(err)
	}
	body := collector.Metrics(t.Context())
	if !strings.Contains(body, "usage_billing_queue_scrape_success 0\n") ||
		!strings.Contains(body, "usage_billing_queue_scrape_errors_total 1\n") {
		t.Fatalf("queue failure not exposed:\n%s", body)
	}
	for _, unavailable := range []string{
		"usage_billing_queue_pending_events ",
		"usage_billing_queue_failed_events ",
		"usage_billing_queue_oldest_event_age_seconds ",
	} {
		if strings.Contains(body, unavailable) {
			t.Fatalf("failed query exposed a false gauge %q", unavailable)
		}
	}
}
