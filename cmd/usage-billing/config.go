package main

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

type config struct {
	databaseURL    string
	token          string
	httpAddr       string
	rateMicros     int64
	workerInterval time.Duration
	workerBatch    int
}

func loadConfig(getenv func(string) string) (config, error) {
	cfg := config{
		databaseURL:    getenv("DATABASE_URL"),
		token:          getenv("BILLING_API_TOKEN"),
		httpAddr:       "127.0.0.1:8080",
		rateMicros:     1000,
		workerInterval: 100 * time.Millisecond,
		workerBatch:    100,
	}
	if strings.TrimSpace(cfg.databaseURL) == "" {
		return config{}, errors.New("database_url is required")
	}
	if len(cfg.token) < 32 || len(cfg.token) > 4096 {
		return config{}, errors.New("billing_api_token must contain 32 to 4096 bytes")
	}
	for i := range len(cfg.token) {
		if cfg.token[i] <= ' ' || cfg.token[i] >= 127 {
			return config{}, errors.New("billing_api_token must contain printable ascii without whitespace")
		}
	}
	if addr := getenv("HTTP_ADDR"); addr != "" {
		cfg.httpAddr = addr
	}
	if _, _, err := net.SplitHostPort(cfg.httpAddr); err != nil {
		return config{}, errors.New("http_addr must contain a host and port")
	}
	if value := getenv("BILLING_RATE_MICROS"); value != "" {
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n <= 0 {
			return config{}, errors.New("billing_rate_micros must be a positive int64")
		}
		cfg.rateMicros = n
	}
	if value := getenv("WORKER_INTERVAL"); value != "" {
		duration, err := time.ParseDuration(value)
		if err != nil || duration < 10*time.Millisecond || duration > time.Minute {
			return config{}, errors.New("worker_interval must be between 10ms and 1m")
		}
		cfg.workerInterval = duration
	}
	if value := getenv("WORKER_BATCH"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 1000 {
			return config{}, fmt.Errorf("worker_batch must be between %d and %d", 1, 1000)
		}
		cfg.workerBatch = n
	}
	return cfg, nil
}
