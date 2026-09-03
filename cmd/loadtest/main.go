// Command loadtest measures the isolated localhost demo, never a remote service.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/krav01/usage-billing/internal/billing"
)

type config struct {
	baseURL, token        string
	requests, concurrency int
	timeout               time.Duration
	allowWrites           bool
}

type queueSample struct {
	ElapsedMS float64 `json:"elapsed_ms"`
	Pending   int64   `json:"pending"`
	Processed int64   `json:"processed"`
}

type report struct {
	GoVersion          string          `json:"go_version"`
	Model              string          `json:"model"`
	Requests           int             `json:"requests"`
	Concurrency        int             `json:"concurrency"`
	Attempted          int             `json:"attempted"`
	Accepted           int             `json:"accepted"`
	Errors             int             `json:"errors"`
	Unattempted        int             `json:"unattempted"`
	ErrorRate          float64         `json:"error_rate"`
	AcceptanceSeconds  float64         `json:"acceptance_seconds"`
	AcceptedPerSecond  float64         `json:"accepted_per_second"`
	P95MS              float64         `json:"acceptance_p95_ms"`
	P99MS              float64         `json:"acceptance_p99_ms"`
	LatenciesMS        []float64       `json:"successful_acceptance_latencies_ms"`
	QueueSamples       []queueSample   `json:"queue_samples"`
	QueueSampleErrors  int             `json:"queue_sample_errors"`
	MaxObservedPending int64           `json:"max_observed_pending"`
	DrainSeconds       float64         `json:"drain_seconds_after_acceptance"`
	Settled            bool            `json:"settled"`
	Final              billing.Summary `json:"final_summary"`
}

func main() {
	var cfg config
	flag.StringVar(&cfg.baseURL, "url", "http://127.0.0.1:8080", "loopback demo URL")
	flag.IntVar(&cfg.requests, "requests", 500, "new synthetic events (1..10000)")
	flag.IntVar(&cfg.concurrency, "concurrency", 8, "closed-loop clients (1..32)")
	flag.DurationVar(&cfg.timeout, "timeout", 2*time.Minute, "whole run deadline (1s..5m)")
	flag.BoolVar(&cfg.allowWrites, "allow-demo-writes", false, "confirm this is a disposable demo database")
	flag.Parse()
	cfg.token = os.Getenv("BILLING_API_TOKEN")
	if err := validate(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, err := run(ctx, cfg)
	if encodeErr := json.NewEncoder(os.Stdout).Encode(result); encodeErr != nil {
		fmt.Fprintln(os.Stderr, "cannot write load test report")
		os.Exit(1)
	}
	if err != nil {
		// Never print response bodies, authentication headers, or transport errors.
		fmt.Fprintln(os.Stderr, "load test failed; inspect report counts and settlement")
		os.Exit(1)
	}
}

func validate(cfg config) error {
	u, err := url.Parse(cfg.baseURL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || u.Opaque != "" {
		return errors.New("URL must be a plain HTTP loopback origin without credentials, path, query, or fragment")
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if err != nil || !ip.IsLoopback() || ip.Zone() != "" {
		return errors.New("only numeric loopback addresses are permitted")
	}
	if !cfg.allowWrites {
		return errors.New("-allow-demo-writes is required; use only a disposable demo database")
	}
	if cfg.requests < 1 || cfg.requests > 10000 || cfg.concurrency < 1 || cfg.concurrency > 32 || cfg.timeout < time.Second || cfg.timeout > 5*time.Minute {
		return errors.New("request count, concurrency, or timeout outside safe bounds")
	}
	if len(cfg.token) < 32 || len(cfg.token) > 4096 {
		return errors.New("BILLING_API_TOKEN must contain 32..4096 printable ASCII bytes")
	}
	for i := range len(cfg.token) {
		if cfg.token[i] <= ' ' || cfg.token[i] >= 127 {
			return errors.New("BILLING_API_TOKEN must contain printable ASCII without whitespace")
		}
	}
	return nil
}

func newClient(concurrency int) (*http.Client, *http.Transport) {
	transport := &http.Transport{
		// Do not send the token through environment-configured proxies.
		DialContext:     (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		MaxConnsPerHost: concurrency + 1, MaxIdleConnsPerHost: concurrency + 1,
		IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 5 * time.Second,
		MaxResponseHeaderBytes: 16 * 1024,
	}
	return &http.Client{
		Transport: transport, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, transport
}

func run(parent context.Context, cfg config) (report, error) {
	out := report{GoVersion: runtime.Version(), Model: "closed-loop; no warmup; HTTP acceptance latency excludes queue processing", Requests: cfg.requests, Unattempted: cfg.requests, Concurrency: cfg.concurrency}
	if err := validate(cfg); err != nil {
		return out, err
	}
	ctx, cancel := context.WithTimeout(parent, cfg.timeout)
	defer cancel()
	client, transport := newClient(cfg.concurrency)
	defer transport.CloseIdleConnections()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return out, errors.New("cannot generate run identifier")
	}
	prefix := fmt.Sprintf("load_%x", nonce)
	started := time.Now()
	if _, err := summary(ctx, client, cfg, prefix); err != nil {
		return out, err
	}
	monitorCtx, stopMonitor := context.WithCancel(ctx)
	queueDone := make(chan report, 1)
	go func() { queueDone <- monitor(monitorCtx, client, cfg, prefix, started) }()
	type sample struct {
		ms float64
		ok bool
	}
	results := make(chan sample, cfg.concurrency)
	var clients sync.WaitGroup
	acceptStarted := time.Now()
	for worker := range cfg.concurrency {
		clients.Go(func() {
			for i := worker; i < cfg.requests; i += cfg.concurrency {
				if ctx.Err() != nil {
					return
				}
				input := billing.Input{EventID: prefix + "_" + strconv.Itoa(i), CustomerID: prefix, Meter: "api_calls", Units: 1}
				begin := time.Now()
				err := accept(ctx, client, cfg, input)
				results <- sample{ms: float64(time.Since(begin)) / float64(time.Millisecond), ok: err == nil}
			}
		})
	}
	go func() { clients.Wait(); close(results) }()
	for result := range results {
		out.Attempted++
		if result.ok {
			out.Accepted++
			out.LatenciesMS = append(out.LatenciesMS, result.ms)
		} else {
			out.Errors++
		}
	}
	out.AcceptanceSeconds = time.Since(acceptStarted).Seconds()
	stopMonitor()
	observed := <-queueDone
	out.QueueSamples, out.QueueSampleErrors, out.MaxObservedPending = observed.QueueSamples, observed.QueueSampleErrors, observed.MaxObservedPending
	out.Unattempted = cfg.requests - out.Attempted
	if out.Attempted > 0 {
		out.ErrorRate = float64(out.Errors) / float64(out.Attempted)
	}
	out.AcceptedPerSecond = float64(out.Accepted) / out.AcceptanceSeconds
	out.P95MS, out.P99MS = percentile(out.LatenciesMS, .95), percentile(out.LatenciesMS, .99)
	drainStarted := time.Now()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		value, err := summary(ctx, client, cfg, prefix)
		if err != nil {
			out.QueueSampleErrors++
			break
		}
		out.observe(value, started)
		out.Final = value
		if value.Pending == 0 && value.Processed == int64(cfg.requests) && value.Units == strconv.Itoa(cfg.requests) && value.AmountMicros == strconv.Itoa(cfg.requests*1000) {
			out.Settled = true
			break
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	out.DrainSeconds = time.Since(drainStarted).Seconds()
	if out.Errors != 0 || out.Unattempted != 0 || out.QueueSampleErrors != 0 || !out.Settled {
		return out, errors.New("load or settlement failed")
	}
	return out, nil
}

func monitor(ctx context.Context, client *http.Client, cfg config, customer string, start time.Time) report {
	var out report
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return out
		case <-ticker.C:
			value, err := summary(ctx, client, cfg, customer)
			if ctx.Err() != nil {
				return out
			}
			if err != nil {
				out.QueueSampleErrors++
			} else {
				out.observe(value, start)
			}
		}
	}
}

func (out *report) observe(value billing.Summary, start time.Time) {
	out.QueueSamples = append(out.QueueSamples, queueSample{ElapsedMS: float64(time.Since(start)) / float64(time.Millisecond), Pending: value.Pending, Processed: value.Processed})
	out.MaxObservedPending = max(out.MaxObservedPending, value.Pending)
}

func accept(ctx context.Context, client *http.Client, cfg config, input billing.Input) error {
	data, err := json.Marshal(input)
	if err != nil {
		return errors.New("cannot encode synthetic event")
	}
	var event billing.Event
	if err := request(ctx, client, cfg, http.MethodPost, "/v1/events", string(data), http.StatusAccepted, &event); err != nil {
		return err
	}
	if event.Input != input || event.Currency != "USD" || event.UnitPriceMicros != 1000 || event.AmountMicros != 1000 {
		return errors.New("unexpected event response; use the default demo price")
	}
	return nil
}

func summary(ctx context.Context, client *http.Client, cfg config, customer string) (billing.Summary, error) {
	var value billing.Summary
	err := request(ctx, client, cfg, http.MethodGet, "/v1/customers/"+customer+"/summary", "", http.StatusOK, &value)
	if err == nil && (value.CustomerID != customer || value.Currency != "USD" || value.Pending < 0 || value.Processed < 0) {
		err = errors.New("invalid summary")
	}
	return value, err
}

func request(ctx context.Context, client *http.Client, cfg config, method, path, data string, want int, target any) (err error) {
	req, err := http.NewRequestWithContext(ctx, method, cfg.baseURL+path, strings.NewReader(data))
	if err != nil {
		return errors.New("cannot build request")
	}
	req.Header.Set("Authorization", "Bearer "+cfg.token)
	req.Header.Set("Content-Type", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return errors.New("HTTP request failed")
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil && err == nil {
			err = errors.New("cannot close HTTP response")
		}
	}()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16*1024+1))
	if err != nil || len(body) > 16*1024 || response.StatusCode != want {
		return errors.New("unexpected HTTP response")
	}
	if err := json.Unmarshal(body, target); err != nil {
		return errors.New("invalid JSON response")
	}
	return nil
}

// Nearest-rank percentile of successful attempts only. Preserve raw sample order.
func percentile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return sorted[int(math.Ceil(q*float64(len(sorted))))-1]
}
