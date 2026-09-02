package main

import (
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		key       string
		value     string
		wantError bool
	}{
		{name: "defaults"},
		{name: "missing database", key: "DATABASE_URL", wantError: true},
		{name: "short token", key: "BILLING_API_TOKEN", value: "secret", wantError: true},
		{name: "token newline", key: "BILLING_API_TOKEN", value: strings.Repeat("x", 32) + "\n", wantError: true},
		{name: "non ascii token", key: "BILLING_API_TOKEN", value: strings.Repeat("x", 32) + "Ж", wantError: true},
		{name: "bad rate", key: "BILLING_RATE_MICROS", value: "secret", wantError: true},
		{name: "zero rate", key: "BILLING_RATE_MICROS", value: "0", wantError: true},
		{name: "overflow rate", key: "BILLING_RATE_MICROS", value: "9223372036854775808", wantError: true},
		{name: "maximum rate", key: "BILLING_RATE_MICROS", value: "9223372036854775807"},
		{name: "unbounded batch", key: "WORKER_BATCH", value: "1001", wantError: true},
		{name: "zero interval", key: "WORKER_INTERVAL", value: "0", wantError: true},
		{name: "bad address", key: "HTTP_ADDR", value: "secret", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{
				"DATABASE_URL":      "postgres://synthetic:secret@localhost/usage_billing_test",
				"BILLING_API_TOKEN": strings.Repeat("x", 32),
			}
			if tt.key != "" {
				env[tt.key] = tt.value
			}
			cfg, err := loadConfig(func(key string) string { return env[key] })
			if (err != nil) != tt.wantError {
				t.Fatalf("unexpected error state: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("configuration error leaks input")
			}
			if !tt.wantError && cfg.workerBatch < 1 {
				t.Fatal("missing defaults")
			}
		})
	}
}
