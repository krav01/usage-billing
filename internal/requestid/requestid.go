// Package requestid carries server-generated correlation metadata across layers.
package requestid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
)

type contextKey struct{}

// New returns an opaque 128-bit ID. Entropy failure is unrecoverable.
func New() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic("request id entropy unavailable")
	}
	return hex.EncodeToString(raw[:])
}

// WithContext preserves the parent's deadline and cancellation.
// Callers must use server-generated IDs, never client-supplied headers.
func WithContext(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, contextKey{}, ForLog(id))
}

// FromContext returns an empty ID when no request metadata was attached.
func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(contextKey{}).(string)
	return id
}

// ForLog bounds and re-encodes persisted metadata before it reaches a log sink.
// Empty and invalid legacy values are omitted, never replaced with invented IDs.
func ForLog(id string) string {
	if len(id) != 32 {
		return ""
	}
	raw, err := hex.DecodeString(id)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(raw)
}
