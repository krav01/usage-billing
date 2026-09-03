package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/krav01/usage-billing/internal/billing"
	"github.com/krav01/usage-billing/internal/httpapi"
)

const token = "synthetic-test-token-not-a-real-secret-123"
const validBody = `{"event_id":"event-1","customer_id":"customer-1","meter":"api_calls","units":7}`

type fakeService struct {
	err     error
	created bool
}

type waitingService struct{ fakeService }

type metricsSource struct {
	called atomic.Bool
}

func (m *metricsSource) Metrics(context.Context) string {
	m.called.Store(true)
	return "synthetic_operational_metric 7"
}

func (waitingService) Get(ctx context.Context, _ string) (billing.Event, error) {
	<-ctx.Done()
	return billing.Event{}, ctx.Err()
}

func (f fakeService) Accept(_ context.Context, input billing.Input) (billing.Event, bool, error) {
	return billing.Event{Input: input, Currency: "USD", UnitPriceMicros: 1000, AmountMicros: input.Units * 1000},
		f.created, f.err
}

func (f fakeService) Get(_ context.Context, id string) (billing.Event, error) {
	return billing.Event{Input: billing.Input{EventID: id}}, f.err
}

func (f fakeService) Summary(_ context.Context, id string) (billing.Summary, error) {
	return billing.Summary{CustomerID: id, Currency: "USD", Units: "7", AmountMicros: "7000"}, f.err
}

func (f fakeService) Retry(_ context.Context, id string, generation int64) (billing.Event, bool, error) {
	return billing.Event{Input: billing.Input{EventID: id}, RetryGeneration: generation + 1}, f.created, f.err
}

type retrySpy struct {
	fakeService
	calls       int
	hasDeadline bool
}

func (s *retrySpy) Retry(ctx context.Context, id string, generation int64) (billing.Event, bool, error) {
	s.calls++
	_, s.hasDeadline = ctx.Deadline()
	return s.fakeService.Retry(ctx, id, generation)
}

func TestRetryRoute(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		body    string
		err     error
		created bool
		status  int
		calls   int
	}{
		{name: "reactivated", body: `{"retry_generation":0}`, created: true, status: 202, calls: 1},
		{name: "replay", body: `{"retry_generation":0}`, status: 200, calls: 1},
		{name: "stale", body: `{"retry_generation":0}`, err: billing.ErrRetryConflict, status: 409, calls: 1},
		{name: "missing", body: `{"retry_generation":0}`, err: billing.ErrNotFound, status: 404, calls: 1},
		{name: "database error", body: `{"retry_generation":0}`, err: errors.New("private database details"), status: 500, calls: 1},
		{name: "missing generation", body: `{}`, status: 400},
		{name: "null", body: `{"retry_generation":null}`, status: 400},
		{name: "duplicate", body: `{"retry_generation":0,"retry_generation":0}`, status: 400},
		{name: "case variant", body: `{"Retry_Generation":0}`, status: 400},
		{name: "negative", body: `{"retry_generation":-1}`, status: 400},
		{name: "overflow", body: `{"retry_generation":9223372036854775808}`, status: 400},
		{name: "decimal", body: `{"retry_generation":0.0}`, status: 400},
		{name: "string", body: `{"retry_generation":"0"}`, status: 400},
		{name: "trailing object", body: `{"retry_generation":0}{}`, status: 400},
		{name: "extra field", body: `{"retry_generation":0,"units":7}`, status: 400},
		{name: "invalid utf8", body: "{\xff}", status: 400},
		{name: "oversized", body: strings.Repeat(" ", 16385), status: 413},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &retrySpy{fakeService: fakeService{err: tc.err, created: tc.created}}
			var logs bytes.Buffer
			h := newHandler(t, svc, &logs)
			w := request(h, http.MethodPost, "/v1/events/event-1/retry", tc.body)
			if w.Code != tc.status || svc.calls != tc.calls {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, svc.calls, w.Body)
			}
			if svc.calls > 0 && !svc.hasDeadline {
				t.Error("retry lost HTTP deadline")
			}
			if strings.Contains(w.Body.String()+logs.String(), "private database details") {
				t.Error("database error leaked")
			}
			if w.Code < 300 {
				var event billing.Event
				if err := json.Unmarshal(w.Body.Bytes(), &event); err != nil || event.EventID != "event-1" || event.RetryGeneration != 1 {
					t.Fatalf("retry arguments lost: %+v %v", event, err)
				}
			}
		})
	}
}

func TestRetryBoundaryProtection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, method, auth, contentType string
		status                          int
	}{
		{name: "missing token", method: "POST", contentType: "application/json", status: 401},
		{name: "wrong token", method: "POST", auth: "Bearer invalid", contentType: "application/json", status: 401},
		{name: "wrong method", method: "GET", auth: "Bearer " + token, contentType: "application/json", status: 405},
		{name: "wrong content type", method: "POST", auth: "Bearer " + token, contentType: "text/plain", status: 415},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &retrySpy{}
			h := newHandler(t, svc, io.Discard)
			r := httptest.NewRequest(tc.method, "/v1/events/event-1/retry", strings.NewReader(`{"retry_generation":0}`))
			r.Header.Set("Authorization", tc.auth)
			r.Header.Set("Content-Type", tc.contentType)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.status || svc.calls != 0 {
				t.Fatalf("status=%d calls=%d", w.Code, svc.calls)
			}
		})
	}
}

func newHandler(t *testing.T, service httpapi.Service, logs io.Writer) http.Handler {
	t.Helper()
	h, err := httpapi.New(
		service,
		func(context.Context) error { return nil },
		token,
		slog.New(slog.NewJSONHandler(logs, nil)),
	)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func request(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestNew(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	ready := func(context.Context) error { return nil }
	for _, candidate := range []string{"", "short", strings.Repeat("a", 31), token + "\n", token + " "} {
		if _, err := httpapi.New(fakeService{}, ready, candidate, logger); err == nil {
			t.Errorf("invalid token accepted: length %d", len(candidate))
		}
	}
	if _, err := httpapi.New(nil, ready, token, logger); err == nil {
		t.Error("nil service accepted")
	}
	if _, err := httpapi.New(fakeService{}, nil, token, logger); err == nil {
		t.Error("nil readiness accepted")
	}
	if _, err := httpapi.New(fakeService{}, ready, token, nil); err == nil {
		t.Error("nil logger accepted")
	}
	var nilSource *metricsSource
	if _, err := httpapi.New(fakeService{}, ready, token, logger, nilSource, nilSource); err == nil {
		t.Error("multiple metrics sources accepted")
	}
}

func TestRoutes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		method string
		path   string
		status int
	}{
		{name: "health", method: "GET", path: "/healthz", status: 200},
		{name: "ready", method: "GET", path: "/readyz", status: 200},
		{name: "accept", method: "POST", path: "/v1/events", status: 202},
		{name: "get", method: "GET", path: "/v1/events/event-1", status: 200},
		{name: "summary", method: "GET", path: "/v1/customers/customer-1/summary", status: 200},
		{name: "metrics", method: "GET", path: "/metrics", status: 200},
		{name: "unknown", method: "GET", path: "/arbitrary", status: 404},
		{name: "no automatic redirect", method: "GET", path: "/v1//events", status: 404},
		{name: "wrong method", method: "DELETE", path: "/v1/events/event-1", status: 405},
		{name: "wrong health method", method: "POST", path: "/healthz", status: 405},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHandler(t, fakeService{created: true}, io.Discard)
			w := request(h, tc.method, tc.path, validBody)
			if w.Code != tc.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if tc.path != "/metrics" && !json.Valid(w.Body.Bytes()) {
				t.Fatalf("response is not JSON: %s", w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
				t.Fatal("missing security headers")
			}
		})
	}
}

func TestAuthentication(t *testing.T) {
	t.Parallel()
	h := newHandler(t, fakeService{}, io.Discard)
	cases := []struct {
		name   string
		values []string
		status int
	}{
		{name: "missing", values: []string{}, status: 401},
		{name: "wrong", values: []string{"Bearer wrong"}, status: 401},
		{name: "prefix", values: []string{"Bearer " + token + "extra"}, status: 401},
		{name: "short", values: []string{"Bearer " + token[:len(token)-1]}, status: 401},
		{name: "basic", values: []string{"Basic " + token}, status: 401},
		{name: "double", values: []string{"Bearer " + token, "Bearer " + token}, status: 401},
		{name: "valid", values: []string{"Bearer " + token}, status: 200},
		{name: "case insensitive scheme", values: []string{"bearer " + token}, status: 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, path := range []string{"/metrics", "/v1/events/event-1", "/v1/customers/customer-1/summary"} {
				r := httptest.NewRequest(http.MethodGet, path, nil)
				for _, value := range tc.values {
					r.Header.Add("Authorization", value)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != tc.status {
					t.Fatalf("%s: got %d, want %d", path, w.Code, tc.status)
				}
			}
		})
	}
	for _, path := range []string{"/healthz", "/readyz"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("public %s status %d", path, w.Code)
		}
	}
}

func TestStrictJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
	}{
		{name: "empty", body: ""},
		{name: "array", body: "[]"},
		{name: "null object", body: "null"},
		{name: "unknown", body: strings.Replace(validBody, `"units":7`, `"units":7,"admin":true`, 1)},
		{name: "duplicate", body: strings.Replace(validBody, `"units":7`, `"units":7,"units":8`, 1)},
		{name: "escaped duplicate", body: strings.Replace(validBody, `"units":7`, `"units":7,"\u0075nits":8`, 1)},
		{name: "case variant", body: strings.Replace(validBody, `"units"`, `"Units"`, 1)},
		{name: "two objects", body: validBody + validBody},
		{name: "trailing value", body: validBody + " true"},
		{name: "trailing malformed", body: validBody + " !"},
		{name: "missing", body: `{"event_id":"e","customer_id":"c","units":1}`},
		{name: "null member", body: strings.Replace(validBody, `"units":7`, `"units":null`, 1)},
		{name: "fractional", body: strings.Replace(validBody, `"units":7`, `"units":1.5`, 1)},
		{name: "overflow", body: strings.Replace(validBody, `"units":7`, `"units":9223372036854775808`, 1)},
		{name: "string units", body: strings.Replace(validBody, `"units":7`, `"units":"7"`, 1)},
		{name: "nested", body: strings.Replace(validBody, `"units":7`, `"units":{"units":7}`, 1)},
		{name: "invalid UTF8", body: strings.Replace(validBody, "event-1", "event-\xff", 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHandler(t, fakeService{created: true}, io.Discard)
			w := request(h, http.MethodPost, "/v1/events", tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestAcceptBodyLimitAndType(t *testing.T) {
	t.Parallel()
	h := newHandler(t, fakeService{created: true}, io.Discard)
	if w := request(h, http.MethodPost, "/v1/events", validBody+strings.Repeat(" ", 16384)); w.Code != 413 {
		t.Fatalf("oversized valid prefix status %d", w.Code)
	}
	if w := request(h, http.MethodPost, "/v1/events", validBody+strings.Repeat(" ", 16384-len(validBody))); w.Code != 202 {
		t.Fatalf("exact limit status %d", w.Code)
	}
	for _, contentType := range []string{"", "text/plain", "application/xml", "not a media type"} {
		r := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(validBody))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", contentType)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("type %q status %d", contentType, w.Code)
		}
	}
	w := request(newHandler(t, fakeService{}, io.Discard), http.MethodPost, "/v1/events", validBody)
	if w.Code != http.StatusOK {
		t.Fatalf("replay status %d", w.Code)
	}
	var event billing.Event
	if err := json.Unmarshal(w.Body.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event.EventID != "event-1" || event.AmountMicros != 7000 {
		t.Fatalf("response %+v", event)
	}
}

func TestErrorsDoNotLeak(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{name: "invalid", err: fmt.Errorf("wrapped: %w", billing.ErrInvalid), status: 400},
		{name: "conflict", err: billing.ErrConflict, status: 409},
		{name: "missing", err: billing.ErrNotFound, status: 404},
		{name: "internal", err: errors.New("postgres://sensitive-password secret SQL"), status: 500},
		{name: "queue full", err: fmt.Errorf("wrapped: %w", billing.ErrQueueFull), status: 503},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logs bytes.Buffer
			h := newHandler(t, fakeService{err: tc.err}, &logs)
			w := request(h, http.MethodPost, "/v1/events", validBody)
			if w.Code != tc.status {
				t.Fatalf("status %d", w.Code)
			}
			combined := w.Body.String() + logs.String()
			for _, secret := range []string{"sensitive-password", "postgres://", "event-1", token} {
				if strings.Contains(combined, secret) {
					t.Fatalf("leaked %q", secret)
				}
			}
		})
	}
}

func TestQueueFullResponse(t *testing.T) {
	t.Parallel()
	h := newHandler(t, fakeService{err: fmt.Errorf("wrapped: %w", billing.ErrQueueFull)}, io.Discard)
	w := request(h, http.MethodPost, "/v1/events", validBody)
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") != "1" ||
		strings.TrimSpace(w.Body.String()) != `{"error":"queue_full"}` {
		t.Fatalf("queue full response: status=%d headers=%v body=%s", w.Code, w.Header(), w.Body)
	}
}

func TestReadinessDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var logs bytes.Buffer
		h, err := httpapi.New(
			fakeService{},
			func(ctx context.Context) error {
				<-ctx.Done()
				return errors.New("private database hostname")
			},
			token,
			slog.New(slog.NewJSONHandler(&logs, nil)),
		)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		w := request(h, http.MethodGet, "/readyz", "")
		if w.Code != 503 || time.Since(start) != time.Second {
			t.Fatalf("ready status %d, elapsed %v", w.Code, time.Since(start))
		}
		if strings.Contains(w.Body.String()+logs.String(), "private database") {
			t.Fatal("readiness leaked error")
		}
	})
}

func TestFixedMetricsAndConcurrentRequests(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	h := newHandler(t, fakeService{}, &logs)
	const count = 40
	var wg sync.WaitGroup
	for i := range count {
		wg.Go(func() {
			path := fmt.Sprintf("/v1/events/private-customer-%d?secret=hidden", i)
			w := request(h, http.MethodGet, path, "")
			if w.Code != 200 {
				t.Errorf("status %d", w.Code)
			}
		})
	}
	wg.Wait()
	w := request(h, http.MethodGet, "/metrics", "")
	if !strings.Contains(w.Body.String(), `route="event",status_class="2xx"} 40`) {
		t.Fatalf("missing aggregate counter: %s", w.Body.String())
	}
	for _, forbidden := range []string{"private-customer", "hidden", token, "/v1/events"} {
		if strings.Contains(w.Body.String()+logs.String(), forbidden) {
			t.Fatalf("unbounded or secret label/log: %q", forbidden)
		}
	}
}

func TestOperationalMetricsRequireAuthentication(t *testing.T) {
	t.Parallel()
	source := &metricsSource{}
	h, err := httpapi.New(
		fakeService{},
		func(context.Context) error { return nil },
		token,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		source,
	)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized := httptest.NewRecorder()
	h.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if unauthorized.Code != http.StatusUnauthorized || source.called.Load() {
		t.Fatalf("unauthorized status=%d, source called=%v", unauthorized.Code, source.called.Load())
	}
	w := request(h, http.MethodGet, "/metrics", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "synthetic_operational_metric 7\n") {
		t.Fatalf("authorized metrics status=%d body=%q", w.Code, w.Body.String())
	}
}

func TestBusinessRequestCancellation(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newHandler(t, waitingService{}, io.Discard)
		start := time.Now()
		w := request(h, http.MethodGet, "/v1/events/e", "")
		if w.Code != 500 || time.Since(start) != 5*time.Second {
			t.Fatalf("status %d, elapsed %v", w.Code, time.Since(start))
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		r := httptest.NewRequest(http.MethodGet, "/v1/events/e", nil).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer "+token)
		w = httptest.NewRecorder()
		start = time.Now()
		h.ServeHTTP(w, r)
		if w.Code != 500 || time.Since(start) != 0 {
			t.Fatalf("cancelled request status %d, elapsed %v", w.Code, time.Since(start))
		}
	})
}

func TestRequestIDCorrelation(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	h := newHandler(t, fakeService{}, &logs)
	r := httptest.NewRequest(http.MethodGet, "/v1/events/event-1", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	got := w.Header().Get("X-Request-ID")
	if w.Code != http.StatusOK || !requestIDFormat(got) {
		t.Fatalf("status=%d request_id=%q", w.Code, got)
	}
	if !strings.Contains(logs.String(), `"request_id":"`+got+`"`) {
		t.Fatalf("request ID missing from log: %s", logs.String())
	}
}

func TestRequestIDIsServerGenerated(t *testing.T) {
	t.Parallel()
	h := newHandler(t, fakeService{}, io.Discard)
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.Header.Set("X-Request-ID", "screening-demo-42")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	got := w.Header().Get("X-Request-ID")
	if !requestIDFormat(got) || got == "screening-demo-42" {
		t.Fatalf("server-generated request_id=%q", got)
	}
}

func requestIDFormat(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := range len(id) {
		if !((id[i] >= '0' && id[i] <= '9') || (id[i] >= 'a' && id[i] <= 'f')) {
			return false
		}
	}
	return true
}
