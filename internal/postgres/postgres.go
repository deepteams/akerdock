// Package postgres opens the application pool and applies the embedded
// migrations, following the startup sequence of instance-config §6.1.
package postgres

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/deepteams/akerdock/db"
)

// connectWindow bounds the retry loop: it covers the compose
// depends_on: service_healthy window (§6.1 step 2).
const connectWindow = 30 * time.Second

// Connect opens a pgx pool, retrying with backoff for at most 30 seconds.
func Connect(ctx context.Context, dsn string, logger *slog.Logger) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("AKERDOCK_DATABASE_URL: unparsable DSN: %w", err)
	}
	deadline := time.Now().Add(connectWindow)
	backoff := 500 * time.Millisecond
	for {
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				return pool, nil
			}
			pool.Close()
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Errorf("postgres: unreachable after %s: %w", connectWindow, err)
		}
		logger.Info("postgres not ready, retrying", "backoff", backoff.String())
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

// Migrate applies the embedded goose migrations, before any network listen
// (§6.1 step 3). Each migration runs in its own transaction; a failure stops
// the process with the database left at the last complete migration.
func Migrate(ctx context.Context, dsn string, logger *slog.Logger) error {
	sqlDB, err := goose.OpenDBWithDriver("pgx", dsn)
	if err != nil {
		return fmt.Errorf("migrations: open: %w", err)
	}
	defer func() { _ = sqlDB.Close() }()

	goose.SetBaseFS(db.Migrations)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	version, err := goose.GetDBVersionContext(ctx, sqlDB)
	if err != nil {
		return err
	}
	// "migrations applied" is a normative log marker relied upon by the
	// upgrade-downgrade runbook.
	logger.Info("migrations applied", "version", version)
	return nil
}

var _ = stdlib.GetDefaultDriver // ensure the pgx database/sql driver is linked
