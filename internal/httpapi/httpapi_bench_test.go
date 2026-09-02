package httpapi_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/krav01/usage-billing/internal/httpapi"
)

// These measurements include request/recorder construction and JSON logging to
// io.Discard, but deliberately exclude TCP, PostgreSQL, and worker execution.
func BenchmarkHTTPHandler(b *testing.B) {
	for _, tc := range []struct {
		name    string
		created bool
		auth    bool
		status  int
	}{
		{name: "accepted", created: true, auth: true, status: http.StatusAccepted},
		{name: "replayed", auth: true, status: http.StatusOK},
		{name: "unauthorized", status: http.StatusUnauthorized},
	} {
		b.Run(tc.name, func(b *testing.B) {
			h, err := httpapi.New(
				fakeService{created: tc.created},
				func(context.Context) error { return nil },
				token,
				slog.New(slog.NewJSONHandler(io.Discard, nil)),
			)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			for b.Loop() {
				r := httptest.NewRequest(http.MethodPost, "/v1/events", strings.NewReader(validBody))
				r.Header.Set("Content-Type", "application/json")
				if tc.auth {
					r.Header.Set("Authorization", "Bearer "+token)
				}
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != tc.status {
					b.Fatalf("status = %d, want %d", w.Code, tc.status)
				}
			}
		})
	}
}
