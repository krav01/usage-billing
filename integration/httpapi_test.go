//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/krav01/usage-billing/internal/billing"
	"github.com/krav01/usage-billing/internal/httpapi"
	"github.com/krav01/usage-billing/internal/postgres"
	"github.com/krav01/usage-billing/internal/worker"
)

// Synthetic credential used exclusively by the in-process test HTTP servers.
const testToken = "synthetic-integration-token-never-for-real-use"

func TestHTTPFailedEventRecovery(t *testing.T) {
	t.Parallel()
	pool := isolatedDatabase(t)
	server := newServer(t, pool, 1000)
	body := []byte(`{"event_id":"event-one","customer_id":"customer-one","meter":"api_calls","units":3}`)
	request(t, server, http.MethodPost, "/v1/events", body, http.StatusAccepted)
	request(t, server, http.MethodPost, "/v1/events/event-one/retry", []byte(`{"retry_generation":0}`), http.StatusConflict)
	if _, err := pool.Exec(t.Context(), `ALTER TABLE ledger_entries ADD CONSTRAINT fail_post CHECK (event_id <> 'event-one')`); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if n, err := postgres.New(pool).ProcessBatch(t.Context(), 1); err != nil || n != 0 {
			t.Fatalf("isolate event: %d %v", n, err)
		}
	}
	var event billing.Event
	decode(t, request(t, server, http.MethodGet, "/v1/events/event-one", nil, http.StatusOK), &event)
	if !event.Failed || event.ProcessingFailures != 3 || event.FailureCode != "23514" {
		t.Fatalf("failure not visible: %+v", event)
	}
	if got := summary(t, server); got.Failed != 1 || got.Pending != 0 || got.AmountMicros != "0" {
		t.Fatalf("failed event billed: %+v", got)
	}
	// Restart the HTTP service with a new rate before manually retrying.
	server.Close()
	server = newServer(t, pool, 9000)
	if _, err := pool.Exec(t.Context(), `ALTER TABLE ledger_entries DROP CONSTRAINT fail_post`); err != nil {
		t.Fatal(err)
	}
	decode(t, request(t, server, http.MethodPost, "/v1/events/event-one/retry", []byte(`{"retry_generation":0}`), http.StatusAccepted), &event)
	if event.Failed || event.RetryGeneration != 1 || event.UnitPriceMicros != 1000 || event.AmountMicros != 3000 {
		t.Fatalf("retry changed frozen pricing: %+v", event)
	}
	request(t, server, http.MethodPost, "/v1/events/event-one/retry", []byte(`{"retry_generation":0}`), http.StatusOK)
	if n, err := postgres.New(pool).ProcessBatch(t.Context(), 1); err != nil || n != 1 {
		t.Fatalf("recover: %d %v", n, err)
	}
	request(t, server, http.MethodPost, "/v1/events/event-one/retry", []byte(`{"retry_generation":0}`), http.StatusOK)
	if got := summary(t, server); got.Processed != 1 || got.Failed != 0 || got.Pending != 0 || got.AmountMicros != "3000" {
		t.Fatalf("retry duplicated charge: %+v", got)
	}
}

func TestHTTPBillingLifecycle(t *testing.T) {
	t.Parallel()
	pool := isolatedDatabase(t)
	server := newServer(t, pool, 1000)
	input := billing.Input{
		EventID: "event-one", CustomerID: "customer-one", Meter: "api_calls", Units: 3,
	}
	data, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	accepted := request(t, server, http.MethodPost, "/v1/events", data, http.StatusAccepted)
	var event billing.Event
	decode(t, accepted, &event)
	if event.AmountMicros != 3000 || event.Processed || event.CreatedAt.IsZero() {
		t.Fatalf("invalid accepted event: %+v", event)
	}
	pending := summary(t, server)
	if pending.Pending != 1 || pending.Processed != 0 || pending.AmountMicros != "0" {
		t.Fatalf("unprocessed usage became a charge: %+v", pending)
	}

	conflict := []byte(`{"event_id":"event-one","customer_id":"customer-one","meter":"api_calls","units":4}`)
	request(t, server, http.MethodPost, "/v1/events", conflict, http.StatusConflict)
	request(t, server, http.MethodGet, "/v1/events/missing", nil, http.StatusNotFound)

	// Reconstruct the application with a different current price; persisted usage
	// must replay its original price instead of being accepted or repriced again.
	server.Close()
	server = newServer(t, pool, 9000)
	replayed := request(t, server, http.MethodPost, "/v1/events", data, http.StatusOK)
	decode(t, replayed, &event)
	if event.UnitPriceMicros != 1000 || event.AmountMicros != 3000 {
		t.Fatalf("replay changed frozen pricing: %+v", event)
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(t.Context())
	results := make(chan error, 2)
	for range 2 {
		w := worker.New(postgres.New(pool), 10*time.Millisecond, 1, logger)
		go func() { results <- w.Run(ctx) }()
	}
	t.Cleanup(func() {
		cancel()
		for range 2 {
			select {
			case err := <-results:
				if err != nil {
					t.Errorf("worker shutdown: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Error("worker failed to shut down")
			}
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		got := summary(t, server)
		if got.Processed == 1 && got.Pending == 0 {
			if got.Units != "3" || got.AmountMicros != "3000" || got.Currency != "USD" {
				t.Fatalf("wrong settled totals: %+v", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("workers did not settle usage: %+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	decode(t, request(t, server, http.MethodGet, "/v1/events/event-one", nil, http.StatusOK), &event)
	if !event.Processed {
		t.Fatal("event read did not reflect committed ledger entry")
	}
	request(t, server, http.MethodPost, "/v1/events", data, http.StatusOK)
	if got := summary(t, server); got.Processed != 1 || got.AmountMicros != "3000" {
		t.Fatalf("processed replay changed total: %+v", got)
	}
}

func TestHTTPConcurrentReplay(t *testing.T) {
	t.Parallel()
	pool := isolatedDatabase(t)
	server := newServer(t, pool, 1000)
	body := []byte(`{"event_id":"event-one","customer_id":"customer-one","meter":"api_calls","units":3}`)
	const requests = 16
	type result struct {
		status int
		err    error
	}
	results := make(chan result, requests)
	var clients sync.WaitGroup
	for range requests {
		clients.Go(func() {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/events", bytes.NewReader(body))
			if err != nil {
				results <- result{err: err}
				return
			}
			req.Header.Set("Authorization", "Bearer "+testToken)
			req.Header.Set("Content-Type", "application/json")
			response, err := server.Client().Do(req)
			if err != nil {
				results <- result{err: err}
				return
			}
			_, readErr := io.Copy(io.Discard, response.Body)
			closeErr := response.Body.Close()
			if readErr != nil {
				results <- result{err: readErr}
				return
			}
			results <- result{status: response.StatusCode, err: closeErr}
		})
	}
	clients.Wait()
	close(results)
	var created int
	for got := range results {
		if got.err != nil {
			t.Errorf("concurrent HTTP request: %v", got.err)
			continue
		}
		switch got.status {
		case http.StatusAccepted:
			created++
		case http.StatusOK:
		default:
			t.Errorf("concurrent replay status = %d", got.status)
		}
	}
	if created != 1 {
		t.Fatalf("created %d events for one idempotency key, want 1", created)
	}
	count, err := postgres.New(pool).ProcessBatch(t.Context(), requests)
	if err != nil || count != 1 {
		t.Fatalf("settled events = %d, error = %v; want 1", count, err)
	}
	if got := summary(t, server); got.Processed != 1 || got.Pending != 0 || got.AmountMicros != "3000" {
		t.Fatalf("concurrent replay changed totals: %+v", got)
	}
}

func TestHTTPHealthAndAuthorization(t *testing.T) {
	t.Parallel()
	pool := isolatedDatabase(t)
	server := newServer(t, pool, 1000)
	for _, path := range []string{"/v1/events/event-one", "/v1/customers/customer-one/summary", "/metrics"} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s = %d, want 401", path, response.StatusCode)
		}
	}
	request(t, server, http.MethodGet, "/healthz", nil, http.StatusOK)
	request(t, server, http.MethodGet, "/readyz", nil, http.StatusOK)
	pool.Close()
	request(t, server, http.MethodGet, "/healthz", nil, http.StatusOK)
	request(t, server, http.MethodGet, "/readyz", nil, http.StatusServiceUnavailable)
}

func newServer(t *testing.T, pool *pgxpool.Pool, rate int64) *httptest.Server {
	t.Helper()
	service, err := billing.New(postgres.New(pool), rate)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	handler, err := httpapi.New(service, pool.Ping, testToken, logger)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	server.Client().Timeout = 5 * time.Second
	t.Cleanup(server.Close)
	return server
}

func request(t *testing.T, server *httptest.Server, method, path string, data []byte, want int) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, server.URL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
	}()
	body, err := io.ReadAll(io.LimitReader(response.Body, 32*1024))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, response.StatusCode, want, body)
	}
	return body
}

func decode(t *testing.T, data []byte, destination any) {
	t.Helper()
	if err := json.Unmarshal(data, destination); err != nil {
		t.Fatal(err)
	}
}

func summary(t *testing.T, server *httptest.Server) billing.Summary {
	t.Helper()
	var got billing.Summary
	decode(t, request(t, server, http.MethodGet, "/v1/customers/customer-one/summary", nil, http.StatusOK), &got)
	return got
}

// Every test owns one random schema; no shared records are truncated or deleted.
// Applying the checked-in migrations here is fixture setup, not an
// application migration engine. CI separately exercises the official CLI.
func isolatedDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Fatal("TEST_DATABASE_URL is required and must reference a disposable PostgreSQL database")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal("cannot connect to disposable PostgreSQL database")
	}
	schema := pgx.Identifier{"http_test_" + rand.Text()}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = admin.Close(context.Background())
		t.Fatal("cannot create isolated test schema")
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cleanupCancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error("cannot remove owned test schema")
		}
		if err := admin.Close(cleanupCtx); err != nil {
			t.Error("cannot close test administrator connection")
		}
	})
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("cannot configure test pool")
	}
	config.MaxConns = 8
	config.ConnConfig.RuntimeParams["search_path"] = schema
	config.ConnConfig.RuntimeParams["statement_timeout"] = "5000"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("cannot create test pool")
	}
	t.Cleanup(pool.Close)
	for _, path := range []string{
		"../migrations/000001_init.up.sql",
		"../migrations/000002_event_recovery.up.sql",
		"../migrations/000003_request_id.up.sql",
	} {
		migration, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(migration)); err != nil {
			t.Fatalf("cannot initialize isolated schema: %v", err)
		}
	}
	return pool
}
