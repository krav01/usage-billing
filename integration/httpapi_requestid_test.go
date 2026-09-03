//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/krav01/usage-billing/internal/billing"
	"github.com/krav01/usage-billing/internal/httpapi"
	"github.com/krav01/usage-billing/internal/postgres"
	"github.com/krav01/usage-billing/internal/worker"
)

// Stop only after the real store has committed or returned an error. The worker
// must still report that outcome during shutdown, without starting another batch.
type oneBatchProcessor struct {
	*postgres.Store
	stop context.CancelFunc
}

func (p oneBatchProcessor) ProcessBatchWithResults(ctx context.Context, limit int) (billing.BatchResult, error) {
	result, err := p.Store.ProcessBatchWithResults(ctx, limit)
	p.stop()
	return result, err
}

func processLoggedBatch(t *testing.T, pool *pgxpool.Pool, logger *slog.Logger) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	p := oneBatchProcessor{Store: postgres.New(pool), stop: cancel}
	w := worker.New(p, time.Second, 10, logger)
	if err := w.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if stats := w.Snapshot(); stats.BatchAttempts != 1 || stats.BatchErrors != 0 || stats.BatchCancellations != 0 {
		t.Fatalf("batch did not commit: %+v", stats)
	}
}

func correlationHandler(t *testing.T, pool *pgxpool.Pool, logger *slog.Logger, rate int64) http.Handler {
	t.Helper()
	service, err := billing.New(postgres.New(pool), rate)
	if err != nil {
		t.Fatal(err)
	}
	handler, err := httpapi.New(service, pool.Ping, testToken, logger)
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func correlationRequest(t *testing.T, handler http.Handler, path, body string, status int) (billing.Event, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", "client-controlled-secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body.String())
	}
	var event billing.Event
	decode(t, w.Body.Bytes(), &event)
	return event, w.Header().Get("X-Request-ID")
}

func TestHTTPRequestIDSurvivesReplayRestartAndRecovery(t *testing.T) {
	t.Parallel()
	pool := isolatedDatabase(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	handler := correlationHandler(t, pool, logger, 1000)
	body := `{"event_id":"private-broken","customer_id":"private-customer","meter":"api_calls","units":3}`
	first, originalID := correlationRequest(t, handler, "/v1/events", body, http.StatusAccepted)
	if len(originalID) != 32 || first.RequestID != originalID {
		t.Fatalf("HTTP ID was not persisted: %+v header=%q", first, originalID)
	}
	healthy, healthyID := correlationRequest(t, handler, "/v1/events",
		strings.Replace(body, "private-broken", "private-healthy", 1), http.StatusAccepted)
	if healthy.RequestID != healthyID || healthyID == originalID {
		t.Fatal("independent events share correlation")
	}
	if _, err := pool.Exec(t.Context(), `ALTER TABLE ledger_entries
		ADD CONSTRAINT reject_correlation_event CHECK (event_id <> 'private-broken')`); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		processLoggedBatch(t, pool, logger)
	}
	failed, err := postgres.New(pool).Get(t.Context(), "private-broken")
	if err != nil || !failed.Failed || failed.RequestID != originalID {
		t.Fatalf("quarantine lost correlation: %+v %v", failed, err)
	}

	// Recreate the application and pool: no in-memory request state is reused.
	restartedPool, err := pgxpool.NewWithConfig(t.Context(), pool.Config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restartedPool.Close)
	handler = correlationHandler(t, restartedPool, logger, 9000)
	replay, replayID := correlationRequest(t, handler, "/v1/events", body, http.StatusOK)
	if replay.RequestID != originalID || replayID == originalID || replay.AmountMicros != 3000 {
		t.Fatalf("replay overwrote original metadata: %+v header=%q", replay, replayID)
	}
	if _, err := restartedPool.Exec(t.Context(), `ALTER TABLE ledger_entries DROP CONSTRAINT reject_correlation_event`); err != nil {
		t.Fatal(err)
	}
	retried, retryID := correlationRequest(t, handler, "/v1/events/private-broken/retry",
		`{"retry_generation":0}`, http.StatusAccepted)
	if retried.RequestID != originalID || retryID == originalID || retried.RetryGeneration != 1 {
		t.Fatalf("manual retry lost correlation: %+v header=%q", retried, retryID)
	}
	processLoggedBatch(t, restartedPool, logger)
	posted, err := postgres.New(restartedPool).Get(t.Context(), "private-broken")
	if err != nil || !posted.Processed || posted.RequestID != originalID {
		t.Fatalf("posting lost correlation: %+v %v", posted, err)
	}
	correlationRequest(t, handler, "/v1/events", body, http.StatusOK)
	totals, err := postgres.New(restartedPool).Summary(t.Context(), "private-customer")
	if err != nil || totals.Processed != 2 || totals.Pending != 0 || totals.Failed != 0 || totals.AmountMicros != "6000" {
		t.Fatalf("correlation changed billing: %+v %v", totals, err)
	}

	for _, forbidden := range []string{"private-broken", "private-healthy", "private-customer", "client-controlled-secret", testToken} {
		if strings.Contains(logs.String(), forbidden) {
			t.Fatalf("private input reached logs: %q", forbidden)
		}
	}
	counts := make(map[string]int)
	for _, line := range bytes.Split(bytes.TrimSpace(logs.Bytes()), []byte("\n")) {
		var record struct {
			Message         string `json:"msg"`
			RequestID       string `json:"request_id"`
			Outcome         string `json:"outcome"`
			RetryGeneration int64  `json:"retry_generation"`
		}
		decode(t, line, &record)
		if record.Message == "usage event processed" {
			counts[record.RequestID+":processed"]++
			if record.RequestID == originalID && record.RetryGeneration != 1 {
				t.Fatal("worker lost retry generation")
			}
		}
		if record.Message == "usage event processing failed" {
			counts[record.RequestID+":"+record.Outcome]++
		}
	}
	if counts[originalID+":retry_scheduled"] != 2 || counts[originalID+":quarantined"] != 1 ||
		counts[originalID+":processed"] != 1 || counts[healthyID+":processed"] != 1 {
		t.Fatalf("worker correlation missing or mixed across events: %+v", counts)
	}
}

func TestHTTPRejectsClientSuppliedEventRequestID(t *testing.T) {
	t.Parallel()
	pool := isolatedDatabase(t)
	var logs bytes.Buffer
	handler := correlationHandler(t, pool, slog.New(slog.NewJSONHandler(&logs, nil)), 1000)
	req := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(
		`{"event_id":"forged","customer_id":"synthetic","meter":"api_calls","units":1,"request_id":"0123456789abcdef0123456789abcdef"}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("client supplied persistence metadata accepted: %d", w.Code)
	}
	var count int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM usage_events`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("invalid event persisted: %d %v", count, err)
	}
}
