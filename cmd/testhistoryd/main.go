// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// testhistoryd serves gotst's HTTP test-history protocol from PostgreSQL 17.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	listenAddr  = flag.String("listen", envOr("TESTHISTORYD_LISTEN", "127.0.0.1:8080"), "HTTP listen address")
	postgresDSN = flag.String("postgres-dsn", os.Getenv("TESTHISTORYD_POSTGRES_DSN"), "PostgreSQL connection URL (or TESTHISTORYD_POSTGRES_DSN)")
	scopeID     = flag.String("scope-id", os.Getenv("TESTHISTORYD_SCOPE_ID"), "UUID isolating one repository/tenant (or TESTHISTORYD_SCOPE_ID)")
	bearerToken = flag.String("token", os.Getenv("TESTHISTORYD_TOKEN"), "bearer token; empty disables authentication (or TESTHISTORYD_TOKEN)")
	maxConns    = flag.Int("max-conns", 16, "maximum PostgreSQL connections")
)

func main() {
	flag.Parse()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if *postgresDSN == "" {
		logger.Error("missing -postgres-dsn or TESTHISTORYD_POSTGRES_DSN")
		os.Exit(2)
	}
	if *scopeID == "" {
		logger.Error("missing -scope-id or TESTHISTORYD_SCOPE_ID")
		os.Exit(2)
	}
	if *maxConns < 1 {
		logger.Error("-max-conns must be positive")
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	config, err := pgxpool.ParseConfig(*postgresDSN)
	if err != nil {
		logger.Error("parsing PostgreSQL DSN", "error", err)
		os.Exit(1)
	}
	config.MaxConns = int32(*maxConns)
	config.ConnConfig.RuntimeParams["application_name"] = "testhistoryd"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		logger.Error("opening PostgreSQL", "error", err)
		os.Exit(1)
	}
	defer pool.Close()
	store, err := newPostgresStore(ctx, pool, *scopeID)
	if err != nil {
		logger.Error("initializing PostgreSQL history store", "error", err)
		os.Exit(1)
	}

	httpServer := &http.Server{
		Addr:              *listenAddr,
		Handler:           (&apiServer{store: store, token: *bearerToken, logger: logger}).handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("serving test history", "address", httpServer.Addr, "scope_id", *scopeID)
		errCh <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server failed", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutting down HTTP server", "error", err)
			os.Exit(1)
		}
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
