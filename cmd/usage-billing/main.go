package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/krav01/usage-billing/internal/billing"
	"github.com/krav01/usage-billing/internal/httpapi"
	"github.com/krav01/usage-billing/internal/postgres"
	"github.com/krav01/usage-billing/internal/worker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("service stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg config, logger *slog.Logger) error {
	poolCfg, err := pgxpool.ParseConfig(cfg.databaseURL)
	if err != nil {
		// Driver parse errors can contain credentials. Never log or return them.
		return errors.New("invalid database connection configuration")
	}
	poolCfg.MaxConns = 8
	poolCfg.MinConns = 0
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute
	poolCfg.ConnConfig.ConnectTimeout = 5 * time.Second
	poolCfg.ConnConfig.RuntimeParams["statement_timeout"] = "5000"
	poolCfg.ConnConfig.RuntimeParams["idle_in_transaction_session_timeout"] = "10000"
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return errors.New("database pool setup failed")
	}
	defer pool.Close()
	startupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = pool.Ping(startupCtx)
	cancel()
	if err != nil {
		return errors.New("database is unavailable during startup")
	}
	store := postgres.New(pool)
	service, err := billing.New(store, cfg.rateMicros)
	if err != nil {
		return errors.New("billing service setup failed")
	}
	handler, err := httpapi.New(
		service,
		pool.Ping,
		cfg.token,
		logger,
	)
	if err != nil {
		return errors.New("http handler setup failed")
	}
	w := worker.New(
		store,
		cfg.workerInterval,
		cfg.workerBatch,
		logger,
	)
	lc := net.ListenConfig{}
	listener, err := lc.Listen(ctx, "tcp", cfg.httpAddr)
	if err != nil {
		return errors.New("http listener setup failed")
	}
	logger.Info("usage billing started")
	return serve(ctx, listener, handler, w.Run)
}

// serve owns the listener and joins both HTTP and worker goroutines before return.
func serve(
	ctx context.Context,
	listener net.Listener,
	handler http.Handler,
	runWorker func(context.Context) error,
) error {
	workerCtx, cancelWorker := context.WithCancel(ctx)
	defer cancelWorker()
	requestCtx, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()
	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
		BaseContext:       func(net.Listener) context.Context { return requestCtx },
	}
	httpDone := make(chan error, 1)
	workerDone := make(chan error, 1)
	go func() { httpDone <- server.Serve(listener) }()
	go func() { workerDone <- runWorker(workerCtx) }()
	var result error
	var isHTTPDone, isWorkerDone bool
	select {
	case <-ctx.Done():
	case err := <-httpDone:
		isHTTPDone = true
		if !errors.Is(err, http.ErrServerClosed) {
			result = errors.New("http server stopped unexpectedly")
		}
	case <-workerDone:
		isWorkerDone = true
		if ctx.Err() == nil {
			result = errors.New("worker stopped unexpectedly")
		}
	}
	cancelWorker()
	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancelShutdown()
	if err := server.Shutdown(shutdownCtx); err != nil {
		cancelRequests()
		closeErr := server.Close()
		result = errors.Join(result, errors.New("http shutdown deadline exceeded"), closeErr)
	}
	if !isHTTPDone {
		if err := <-httpDone; !errors.Is(err, http.ErrServerClosed) {
			result = errors.Join(result, errors.New("http server stopped unexpectedly"))
		}
	}
	if !isWorkerDone {
		// The worker and all database calls receive cancellation and bounded timeouts.
		if err := <-workerDone; err != nil && !errors.Is(err, context.Canceled) {
			result = errors.Join(result, errors.New("worker shutdown failed"))
		}
	}
	return result
}
