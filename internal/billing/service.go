package billing

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/krav01/usage-billing/internal/requestid"
)

// Service validates usage and freezes the configured price at acceptance.
type Service struct {
	repo Repository
	rate int64
}

func New(repo Repository, rate int64) (*Service, error) {
	if repo == nil {
		return nil, errors.New("billing repository is required")
	}
	if rate <= 0 {
		return nil, errors.New("billing rate must be positive")
	}
	return &Service{repo: repo, rate: rate}, nil
}

func (s *Service) Accept(ctx context.Context, input Input) (Event, bool, error) {
	if err := ValidateID(input.EventID); err != nil {
		return Event{}, false, fmt.Errorf("event identifier: %w", err)
	}
	if err := ValidateID(input.CustomerID); err != nil {
		return Event{}, false, fmt.Errorf("customer identifier: %w", err)
	}
	if input.Meter != "api_calls" || input.Units <= 0 {
		return Event{}, false, ErrInvalid
	}
	if input.Units > math.MaxInt64/s.rate {
		// A price increase must not invalidate an already accepted retry. The
		// normal path remains a single atomic repository acceptance operation.
		existing, err := s.repo.Get(ctx, input.EventID)
		if errors.Is(err, ErrNotFound) {
			return Event{}, false, ErrInvalid
		}
		if err != nil {
			return Event{}, false, fmt.Errorf("find original usage: %w", err)
		}
		if existing.Input != input {
			return Event{}, false, ErrConflict
		}
		return existing, false, nil
	}
	id := requestid.FromContext(ctx)
	if id == "" {
		id = requestid.New()
	}
	event, created, err := s.repo.Accept(ctx, Event{
		RequestID: id,
		Input: input, UnitPriceMicros: s.rate,
		AmountMicros: input.Units * s.rate, Currency: "USD",
	})
	if err != nil {
		return Event{}, false, fmt.Errorf("accept usage: %w", err)
	}
	return event, created, nil
}

func (s *Service) Get(ctx context.Context, id string) (Event, error) {
	if err := ValidateID(id); err != nil {
		return Event{}, err
	}
	event, err := s.repo.Get(ctx, id)
	if err != nil {
		return Event{}, fmt.Errorf("get usage: %w", err)
	}
	return event, nil
}

func (s *Service) Summary(ctx context.Context, customer string) (Summary, error) {
	if err := ValidateID(customer); err != nil {
		return Summary{}, err
	}
	summary, err := s.repo.Summary(ctx, customer)
	if err != nil {
		return Summary{}, fmt.Errorf("get customer summary: %w", err)
	}
	return summary, nil
}

// Retry reactivates one failed generation without changing its frozen input or price.
// The boolean reports a new reactivation; replays of an applied request are no-ops.
func (s *Service) Retry(ctx context.Context, id string, generation int64) (Event, bool, error) {
	if err := ValidateID(id); err != nil {
		return Event{}, false, err
	}
	if generation < 0 || generation == math.MaxInt64 {
		return Event{}, false, ErrInvalid
	}
	event, retried, err := s.repo.Retry(ctx, id, generation)
	if err != nil {
		return Event{}, false, fmt.Errorf("retry usage: %w", err)
	}
	return event, retried, nil
}

// ValidateID accepts one to 64 ASCII letters, digits, underscores, or hyphens.
func ValidateID(id string) error {
	if len(id) == 0 || len(id) > 64 {
		return ErrInvalid
	}
	for i := range len(id) {
		b := id[i]
		letter := b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
		digit := b >= '0' && b <= '9'
		separator := b == '_' || b == '-'
		if !letter && !digit && !separator {
			return ErrInvalid
		}
	}
	return nil
}
