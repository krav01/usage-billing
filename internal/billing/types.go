// Package billing contains the usage pricing contracts and application service.
package billing

import (
	"context"
	"errors"
	"time"
)

var (
	ErrInvalid       = errors.New("invalid usage event")
	ErrConflict      = errors.New("event identifier already used for different input")
	ErrNotFound      = errors.New("event not found")
	ErrQueueFull     = errors.New("pending queue is full")
	ErrRetryConflict = errors.New("retry generation does not match failed event")
)

type Input struct {
	EventID    string `json:"event_id"`
	CustomerID string `json:"customer_id"`
	Meter      string `json:"meter"`
	Units      int64  `json:"units"`
}

type Event struct {
	Input
	UnitPriceMicros    int64     `json:"unit_price_micros"`
	AmountMicros       int64     `json:"amount_micros"`
	Currency           string    `json:"currency"`
	Processed          bool      `json:"processed"`
	CreatedAt          time.Time `json:"created_at"`
	Failed             bool      `json:"failed"`
	ProcessingFailures int       `json:"processing_failures"`
	FailureCode        string    `json:"failure_code"`
	RetryGeneration    int64     `json:"retry_generation"`
}

// Summary represents large totals as decimal strings to avoid integer overflow
// and loss of precision in JSON clients. Counts are exact int64 values.
type Summary struct {
	CustomerID   string `json:"customer_id"`
	Currency     string `json:"currency"`
	Units        string `json:"units"`
	AmountMicros string `json:"amount_micros"`
	Pending      int64  `json:"pending"`
	Processed    int64  `json:"processed"`
	Failed       int64  `json:"failed"`
}

// Repository stores priced usage atomically with its pending work item.
type Repository interface {
	Accept(context.Context, Event) (Event, bool, error)
	Get(context.Context, string) (Event, error)
	Summary(context.Context, string) (Summary, error)
	Retry(context.Context, string, int64) (Event, bool, error)
}
