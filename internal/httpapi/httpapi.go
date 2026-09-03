// Package httpapi exposes a bounded, authenticated internal producer API.
package httpapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/krav01/usage-billing/internal/billing"
)

const maxBodyBytes = 16 * 1024

type Service interface {
	Accept(context.Context, billing.Input) (billing.Event, bool, error)
	Get(context.Context, string) (billing.Event, error)
	Summary(context.Context, string) (billing.Summary, error)
	Retry(context.Context, string, int64) (billing.Event, bool, error)
}

type MetricsSource interface {
	Metrics(context.Context) string
}

type handler struct {
	service       Service
	readiness     func(context.Context) error
	tokenHash     [sha256.Size]byte
	logger        *slog.Logger
	requests      [8][6]atomic.Uint64
	metricsSource MetricsSource
}

// New builds an internal API. Its shared bearer token grants access to every
// customer: this is deliberately not a tenant-facing authorization model.
func New(
	service Service,
	readiness func(context.Context) error,
	token string,
	logger *slog.Logger,
	metricsSources ...MetricsSource,
) (http.Handler, error) {
	if service == nil || readiness == nil || logger == nil {
		return nil, errors.New("service, readiness callback, and logger are required")
	}
	if len(token) < 32 || len(token) > 4096 {
		return nil, errors.New("API token must contain between 32 and 4096 bytes")
	}
	for i := range len(token) {
		if token[i] <= ' ' || token[i] >= 127 {
			return nil, errors.New("API token must contain printable ASCII without whitespace")
		}
	}
	if len(metricsSources) > 1 || len(metricsSources) == 1 && metricsSources[0] == nil {
		return nil, errors.New("at most one non-nil metrics source is allowed")
	}
	var metricsSource MetricsSource
	if len(metricsSources) == 1 {
		metricsSource = metricsSources[0]
	}
	return &handler{
		service: service, readiness: readiness, tokenHash: sha256.Sum256([]byte(token)),
		logger: logger, metricsSource: metricsSource,
	}, nil
}

// Route indices and metric labels are fixed; client IDs and URLs never become labels.
var routeNames = [...]string{"unmatched", "health", "ready", "accept", "event", "summary", "metrics", "retry"}

func route(path string) (int, string) {
	switch path {
	case "/healthz":
		return 1, ""
	case "/readyz":
		return 2, ""
	case "/v1/events":
		return 3, ""
	case "/metrics":
		return 6, ""
	}
	parts := strings.Split(path, "/")
	if len(parts) == 4 {
		if parts[1] == "v1" && parts[2] == "events" {
			return 4, parts[3]
		}
	}
	if len(parts) == 5 {
		if parts[1] == "v1" && parts[2] == "events" && parts[4] == "retry" {
			return 7, parts[3]
		}
		customerRoute := parts[1] == "v1" && parts[2] == "customers"
		if customerRoute && parts[4] == "summary" {
			return 5, parts[3]
		}
	}
	return 0, ""
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	index, id := route(r.URL.Path)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	status := h.serve(
		w,
		r,
		index,
		id,
	)
	h.requests[index][status/100].Add(1)
	h.logger.InfoContext(
		r.Context(),
		"http request",
		"route", routeNames[index],
		"status", status,
		"duration_ms", time.Since(start).Milliseconds(),
	)
}

func (h *handler) serve(w http.ResponseWriter, r *http.Request, index int, id string) int {
	if index != 1 && index != 2 && !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		return h.fail(w, http.StatusUnauthorized, "unauthorized")
	}
	if index == 0 {
		return h.fail(w, http.StatusNotFound, "not_found")
	}
	method := http.MethodGet
	if index == 3 || index == 7 {
		method = http.MethodPost
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		return h.fail(w, http.StatusMethodNotAllowed, "method_not_allowed")
	}
	switch index {
	case 1:
		return h.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case 2:
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := h.readiness(ctx); err != nil {
			return h.fail(w, http.StatusServiceUnavailable, "not_ready")
		}
		return h.writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	case 3:
		return h.accept(w, r)
	case 4:
		event, err := h.service.Get(r.Context(), id)
		if err != nil {
			return h.serviceError(w, err)
		}
		return h.writeJSON(w, http.StatusOK, event)
	case 5:
		summary, err := h.service.Summary(r.Context(), id)
		if err != nil {
			return h.serviceError(w, err)
		}
		return h.writeJSON(w, http.StatusOK, summary)
	case 7:
		return h.retry(w, r, id)
	default:
		return h.metrics(w, r.Context())
	}
}

func (h *handler) authorized(r *http.Request) bool {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	// Hashing makes the comparison length-independent, including wrong-length tokens.
	got := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(got[:], h.tokenHash[:]) == 1
}

func (h *handler) accept(w http.ResponseWriter, r *http.Request) int {
	data, status := h.readBody(w, r)
	if status != 0 {
		return status
	}
	input, err := decodeInput(data)
	if err != nil {
		return h.fail(w, http.StatusBadRequest, "invalid_json")
	}
	event, created, err := h.service.Accept(r.Context(), input)
	if err != nil {
		return h.serviceError(w, err)
	}
	status = http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	return h.writeJSON(w, status, event)
}

func (h *handler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, int) {
	contentType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		return nil, h.fail(w, http.StatusUnsupportedMediaType, "json_required")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			return nil, h.fail(w, http.StatusRequestEntityTooLarge, "body_too_large")
		}
		return nil, h.fail(w, http.StatusBadRequest, "invalid_json")
	}
	return data, 0
}

func (h *handler) retry(w http.ResponseWriter, r *http.Request, id string) int {
	data, status := h.readBody(w, r)
	if status != 0 {
		return status
	}
	generation, err := decodeRetry(data)
	if err != nil {
		return h.fail(w, http.StatusBadRequest, "invalid_json")
	}
	event, retried, err := h.service.Retry(r.Context(), id, generation)
	if err != nil {
		return h.serviceError(w, err)
	}
	status = http.StatusOK
	if retried {
		status = http.StatusAccepted
	}
	return h.writeJSON(w, status, event)
}

func decodeRetry(data []byte) (int64, error) {
	if !utf8.Valid(data) {
		return 0, billing.ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return 0, billing.ErrInvalid
	}
	if token, err := d.Token(); err != nil || token != "retry_generation" {
		return 0, billing.ErrInvalid
	}
	token, err := d.Token()
	if err != nil {
		return 0, billing.ErrInvalid
	}
	number, ok := token.(json.Number)
	if !ok {
		return 0, billing.ErrInvalid
	}
	generation, err := strconv.ParseInt(string(number), 10, 64)
	if err != nil || generation < 0 {
		return 0, billing.ErrInvalid
	}
	if token, err := d.Token(); err != nil || token != json.Delim('}') {
		return 0, billing.ErrInvalid
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return 0, billing.ErrInvalid
	}
	return generation, nil
}

// Decode keys explicitly because encoding/json otherwise accepts duplicate and
// case-insensitive object members. There is exactly one flat input object.
func decodeInput(data []byte) (billing.Input, error) {
	var input billing.Input
	if !utf8.Valid(data) {
		return input, billing.ErrInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return input, billing.ErrInvalid
	}
	seen := make(map[string]bool, 4)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return input, billing.ErrInvalid
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return input, billing.ErrInvalid
		}
		seen[key] = true
		var target any
		switch key {
		case "event_id":
			target = &input.EventID
		case "customer_id":
			target = &input.CustomerID
		case "meter":
			target = &input.Meter
		case "units":
			target = &input.Units
		default:
			return input, billing.ErrInvalid
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return input, billing.ErrInvalid
		}
		if bytes.Equal(raw, []byte("null")) {
			return input, billing.ErrInvalid
		}
		if err := json.Unmarshal(raw, target); err != nil {
			return input, billing.ErrInvalid
		}
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return input, billing.ErrInvalid
	}
	if len(seen) != 4 {
		return input, billing.ErrInvalid
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return input, billing.ErrInvalid
	}
	return input, nil
}

func (h *handler) serviceError(w http.ResponseWriter, err error) int {
	switch {
	case errors.Is(err, billing.ErrInvalid):
		return h.fail(w, http.StatusBadRequest, "invalid_input")
	case errors.Is(err, billing.ErrConflict):
		return h.fail(w, http.StatusConflict, "event_conflict")
	case errors.Is(err, billing.ErrRetryConflict):
		return h.fail(w, http.StatusConflict, "retry_conflict")
	case errors.Is(err, billing.ErrNotFound):
		return h.fail(w, http.StatusNotFound, "not_found")
	case errors.Is(err, billing.ErrQueueFull):
		w.Header().Set("Retry-After", "1")
		return h.fail(w, http.StatusServiceUnavailable, "queue_full")
	default:
		// Repository errors can contain credentials or SQL values; never log them.
		return h.fail(w, http.StatusInternalServerError, "internal_error")
	}
}

func (h *handler) fail(w http.ResponseWriter, status int, code string) int {
	return h.writeJSON(w, status, map[string]string{"error": code})
}

func (h *handler) writeJSON(w http.ResponseWriter, status int, value any) int {
	data, err := json.Marshal(value)
	if err != nil {
		status = http.StatusInternalServerError
		data = []byte(`{"error":"internal_error"}`)
		h.logger.Error("encode response failed")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(append(data, '\n')); err != nil {
		h.logger.Warn("write response failed")
	}
	return status
}

func (h *handler) metrics(w http.ResponseWriter, ctx context.Context) int {
	var body strings.Builder
	body.WriteString("# HELP usage_billing_http_requests_total Completed HTTP requests.\n")
	body.WriteString("# TYPE usage_billing_http_requests_total counter\n")
	for index, name := range routeNames {
		for class := 1; class <= 5; class++ {
			// strings.Builder.Write never returns an error.
			_, _ = fmt.Fprintf(
				&body,
				"usage_billing_http_requests_total{route=%q,status_class=%q} %d\n",
				name,
				fmt.Sprintf("%dxx", class),
				h.requests[index][class].Load(),
			)
		}
	}
	if h.metricsSource != nil {
		operational := h.metricsSource.Metrics(ctx)
		body.WriteString(operational)
		if operational != "" && !strings.HasSuffix(operational, "\n") {
			body.WriteByte('\n')
		}
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if _, err := io.WriteString(w, body.String()); err != nil {
		h.logger.Warn("write metrics failed")
	}
	return http.StatusOK
}
