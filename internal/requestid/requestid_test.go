package requestid_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/krav01/usage-billing/internal/requestid"
)

func TestNew(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool)
	for range 100 {
		id := requestid.New()
		if len(id) != 32 || requestid.ForLog(id) != id || seen[id] {
			t.Fatalf("invalid or repeated request ID: %q", id)
		}
		seen[id] = true
	}
}

func TestContextPreservesCancellationAndIsolation(t *testing.T) {
	t.Parallel()
	parent, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	id := requestid.New()
	ctx := requestid.WithContext(parent, id)
	if requestid.FromContext(ctx) != id || requestid.FromContext(parent) != "" {
		t.Fatal("request metadata missing or leaked into parent")
	}
	deadline, _ := parent.Deadline()
	if got, ok := ctx.Deadline(); !ok || !got.Equal(deadline) {
		t.Fatal("request deadline changed")
	}
	cancel()
	if ctx.Err() != context.Canceled || requestid.FromContext(ctx) != id {
		t.Fatal("cancellation or correlation lost")
	}
}

func TestForLog(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, input, want string
	}{
		{name: "legacy"},
		{name: "valid", input: strings.Repeat("a1", 16), want: strings.Repeat("a1", 16)},
		{name: "canonical", input: strings.Repeat("AB", 16), want: strings.Repeat("ab", 16)},
		{name: "short", input: "abc"},
		{name: "oversized", input: strings.Repeat("a", 10000)},
		{name: "non hex", input: strings.Repeat("g", 32)},
		{name: "injection", input: strings.Repeat("a", 31) + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := requestid.ForLog(tc.input); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if got := requestid.FromContext(requestid.WithContext(t.Context(), tc.input)); got != tc.want {
				t.Fatalf("context got %q, want %q", got, tc.want)
			}
		})
	}
}
