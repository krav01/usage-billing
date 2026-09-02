package billing_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/krav01/usage-billing/internal/billing"
)

type memoryRepository struct {
	mu     sync.Mutex
	events map[string]billing.Event
	err    error
}

func newRepository() *memoryRepository {
	return &memoryRepository{events: make(map[string]billing.Event)}
}

func (r *memoryRepository) Accept(_ context.Context, event billing.Event) (billing.Event, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return billing.Event{}, false, r.err
	}
	if prior, ok := r.events[event.EventID]; ok {
		if prior.Input != event.Input {
			return billing.Event{}, false, billing.ErrConflict
		}
		return prior, false, nil
	}
	event.CreatedAt = time.Date(2026, time.September, 2, 0, 0, 0, 0, time.UTC)
	r.events[event.EventID] = event
	return event, true, nil
}

func (r *memoryRepository) Get(_ context.Context, id string) (billing.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return billing.Event{}, r.err
	}
	event, ok := r.events[id]
	if !ok {
		return billing.Event{}, billing.ErrNotFound
	}
	return event, nil
}

func (r *memoryRepository) Summary(_ context.Context, id string) (billing.Summary, error) {
	if r.err != nil {
		return billing.Summary{}, r.err
	}
	return billing.Summary{CustomerID: id, Currency: "USD", Units: "0", AmountMicros: "0"}, nil
}

func newService(t *testing.T, repo billing.Repository, rate int64) *billing.Service {
	t.Helper()
	svc, err := billing.New(repo, rate)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func TestNew(t *testing.T) {
	t.Parallel()
	for _, rate := range []int64{0, -1, math.MinInt64} {
		if _, err := billing.New(newRepository(), rate); err == nil {
			t.Errorf("rate %d accepted", rate)
		}
	}
	if _, err := billing.New(nil, 1); err == nil {
		t.Fatal("nil repository accepted")
	}
}

func TestServiceAccept(t *testing.T) {
	t.Parallel()
	repo := newRepository()
	svc := newService(t, repo, 1000)
	input := billing.Input{EventID: "event-1", CustomerID: "customer_1", Meter: "api_calls", Units: 7}
	event, created, err := svc.Accept(t.Context(), input)
	if err != nil || !created {
		t.Fatalf("accept = %v, %v", created, err)
	}
	if event.Input != input || event.AmountMicros != 7000 || event.UnitPriceMicros != 1000 {
		t.Fatalf("unexpected priced event: %+v", event)
	}
	if event.Currency != "USD" || event.CreatedAt.IsZero() || event.Processed {
		t.Fatalf("unexpected stored event: %+v", event)
	}
	if stored, err := svc.Get(t.Context(), input.EventID); err != nil || stored != event {
		t.Fatalf("get = %+v, %v", stored, err)
	}
	for _, rate := range []int64{1, 2000, math.MaxInt64} {
		repriced := newService(t, repo, rate)
		got, created, err := repriced.Accept(t.Context(), input)
		if err != nil || created || got != event {
			t.Fatalf("rate %d changed replay: %+v, %v, %v", rate, got, created, err)
		}
		changed := input
		changed.CustomerID = "another-customer"
		if _, _, err := repriced.Accept(t.Context(), changed); !errors.Is(err, billing.ErrConflict) {
			t.Fatalf("rate %d conflict = %v", rate, err)
		}
	}
}

func TestServiceAcceptInvalid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		edit func(*billing.Input)
	}{
		{name: "missing event", edit: func(in *billing.Input) { in.EventID = "" }},
		{name: "bad customer", edit: func(in *billing.Input) { in.CustomerID = "customer\n" }},
		{name: "meter", edit: func(in *billing.Input) { in.Meter = "other" }},
		{name: "zero", edit: func(in *billing.Input) { in.Units = 0 }},
		{name: "negative", edit: func(in *billing.Input) { in.Units = -1 }},
		{name: "overflow", edit: func(in *billing.Input) { in.Units = math.MaxInt64 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			repo := newRepository()
			svc := newService(t, repo, 1000)
			input := billing.Input{EventID: "e", CustomerID: "c", Meter: "api_calls", Units: 1}
			tc.edit(&input)
			if _, _, err := svc.Accept(t.Context(), input); !errors.Is(err, billing.ErrInvalid) {
				t.Fatalf("invalid input accepted: %v", err)
			}
			if len(repo.events) != 0 {
				t.Fatal("invalid input stored")
			}
		})
	}
}

func TestServiceErrorsAndIntegerBoundary(t *testing.T) {
	t.Parallel()
	repo := newRepository()
	svc := newService(t, repo, 1)
	input := billing.Input{EventID: "max", CustomerID: "c", Meter: "api_calls", Units: math.MaxInt64}
	if event, _, err := svc.Accept(t.Context(), input); err != nil || event.AmountMicros != math.MaxInt64 {
		t.Fatalf("boundary = %+v, %v", event, err)
	}
	broken := errors.New("database unavailable")
	repo.err = broken
	if _, _, err := svc.Accept(t.Context(), input); !errors.Is(err, broken) {
		t.Fatalf("accept error = %v", err)
	}
	if _, _, err := newService(t, repo, 2).Accept(t.Context(), input); !errors.Is(err, broken) {
		t.Fatalf("overflow lookup error = %v", err)
	}
	if _, err := svc.Get(t.Context(), "max"); !errors.Is(err, broken) {
		t.Fatalf("get error = %v", err)
	}
	if _, err := svc.Summary(t.Context(), "c"); !errors.Is(err, broken) {
		t.Fatalf("summary error = %v", err)
	}
}

func TestServiceGetAndSummary(t *testing.T) {
	t.Parallel()
	svc := newService(t, newRepository(), 1)
	if _, err := svc.Get(t.Context(), "missing"); !errors.Is(err, billing.ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
	if _, err := svc.Get(t.Context(), "bad/id"); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("invalid get = %v", err)
	}
	if _, err := svc.Summary(t.Context(), ""); !errors.Is(err, billing.ErrInvalid) {
		t.Fatalf("invalid summary = %v", err)
	}
	summary, err := svc.Summary(t.Context(), "new-customer")
	if err != nil || summary.CustomerID != "new-customer" || summary.AmountMicros != "0" {
		t.Fatalf("summary = %+v, %v", summary, err)
	}
}

func TestValidateID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		id    string
		valid bool
	}{
		{name: "ascii", id: "Az09_-", valid: true},
		{name: "max", id: strings.Repeat("a", 64), valid: true},
		{name: "empty", id: ""},
		{name: "long", id: strings.Repeat("a", 65)},
		{name: "slash", id: "a/b"},
		{name: "unicode", id: "сustomer"},
		{name: "nul", id: "a\x00"},
		{name: "space", id: " a"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if err := billing.ValidateID(tc.id); (err == nil) != tc.valid {
				t.Fatalf("ValidateID(%q) = %v", tc.id, err)
			}
		})
	}
}
