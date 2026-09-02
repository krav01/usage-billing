package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/krav01/usage-billing/internal/billing"
)

func testConfig(base string) config {
	return config{baseURL: base, token: strings.Repeat("x", 32), requests: 20, concurrency: 4, timeout: time.Second, allowWrites: true}
}

func TestValidate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, url string
		valid     bool
	}{
		{"loopback", "http://127.0.0.1:8080", true},
		{"ipv6", "http://[::1]:8080", true},
		{"remote", "http://example.com", false},
		{"private", "http://10.0.0.1", false},
		{"hostname", "http://localhost:8080", false},
		{"credentials", "http://secret@127.0.0.1", false},
		{"path", "http://127.0.0.1/v1", false},
		{"query", "http://127.0.0.1?token=secret", false},
		{"fragment", "http://127.0.0.1#secret", false},
		{"scheme", "https://127.0.0.1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := validate(testConfig(tc.url)) == nil; got != tc.valid {
				t.Fatalf("valid=%v want %v", got, tc.valid)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		change func(*config)
	}{
		{"write consent", func(c *config) { c.allowWrites = false }},
		{"request bound", func(c *config) { c.requests = 10001 }},
		{"client bound", func(c *config) { c.concurrency = 33 }},
		{"timeout bound", func(c *config) { c.timeout = 6 * time.Minute }},
		{"token missing", func(c *config) { c.token = "" }},
		{"token whitespace", func(c *config) { c.token = strings.Repeat(" ", 32) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig("http://127.0.0.1")
			tc.change(&cfg)
			if validate(cfg) == nil {
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
}

func TestNewClientRejectsRedirect(t *testing.T) {
	t.Parallel()
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Store(true) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client, transport := newClient(1)
	defer transport.CloseIdleConnections()
	if _, err := summary(t.Context(), client, testConfig(source.URL), "synthetic"); err == nil {
		t.Fatal("redirect accepted")
	}
	if leaked.Load() {
		t.Fatal("redirect destination contacted")
	}
}

func TestRunAccounting(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	seen := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.Repeat("x", 32) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			var input billing.Input
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			mu.Lock()
			duplicate := seen[input.EventID]
			seen[input.EventID] = true
			mu.Unlock()
			if duplicate {
				t.Error("duplicate synthetic event")
			}
			if len(input.EventID) > 64 || !strings.HasPrefix(input.EventID, "load_") {
				t.Error("invalid synthetic identifier")
			}
			w.WriteHeader(http.StatusAccepted)
			if err := json.NewEncoder(w).Encode(billing.Event{Input: input, UnitPriceMicros: 1000, AmountMicros: 1000, Currency: "USD"}); err != nil {
				t.Error(err)
			}
			return
		}
		mu.Lock()
		count := len(seen)
		mu.Unlock()
		customer := strings.Split(r.URL.Path, "/")[3]
		if err := json.NewEncoder(w).Encode(billing.Summary{CustomerID: customer, Currency: "USD", Processed: int64(count), Units: strconv.Itoa(count), AmountMicros: strconv.Itoa(count * 1000)}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	got, err := run(t.Context(), testConfig(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	if got.Attempted != 20 || got.Accepted != 20 || got.Errors != 0 || got.Unattempted != 0 || !got.Settled || got.Final.Processed != 20 {
		t.Fatalf("incorrect accounting: %+v", got)
	}
	if len(got.LatenciesMS) != 20 || got.P99MS < got.P95MS || len(got.QueueSamples) == 0 {
		t.Fatal("missing measurements")
	}
}

func TestRunCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := run(ctx, testConfig("http://127.0.0.1:1"))
	if err == nil || got.Accepted != 0 || got.Settled || got.Unattempted != 20 {
		t.Fatal("cancelled run reported success")
	}
}

func TestAcceptRejectsFalseSuccess(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"replay", 200, `{}`}, {"unauthorized", 401, `{}`}, {"invalid JSON", 202, `not json`},
		{"wrong fields", 202, `{}`}, {"oversized", 202, strings.Repeat(" ", 16*1024+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				if _, err := w.Write([]byte(tc.body)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			client, transport := newClient(1)
			defer transport.CloseIdleConnections()
			if err := accept(t.Context(), client, testConfig(server.URL), billing.Input{}); err == nil {
				t.Fatal("false success accepted")
			}
		})
	}
}

func TestPercentile(t *testing.T) {
	t.Parallel()
	values := make([]float64, 100)
	for i := range values {
		values[i] = float64(100 - i)
	}
	before := slices.Clone(values)
	if percentile(values, .95) != 95 || percentile(values, .99) != 99 || percentile(nil, .95) != 0 || !slices.Equal(values, before) {
		t.Fatal("incorrect nearest-rank percentile or mutated input")
	}
}
