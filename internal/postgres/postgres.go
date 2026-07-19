// Package postgres opens the application pool and applies the embedded
// migrations, following the startup sequence of instance-config §6.1.
package postgres

import (
	"context"
	"database/sql"
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

var (
	parsePoolConfig = pgxpool.ParseConfig
	newPool         = pgxpool.NewWithConfig
	pingPool        = (*pgxpool.Pool).Ping
	closePool       = (*pgxpool.Pool).Close
	now             = time.Now
	after           = time.After

	openMigrationDB  = goose.OpenDBWithDriver
	closeMigrationDB = (*sql.DB).Close
	setBaseFS        = goose.SetBaseFS
	setGooseLogger   = goose.SetLogger
	setDialect       = goose.SetDialect
	upMigrations     = goose.UpContext
	migrationVersion = goose.GetDBVersionContext
)

// Connect opens a pgx pool, retrying with backoff for at most 30 seconds.
func Connect(ctx context.Context, dsn string, logger *slog.Logger) (*pgxpool.Pool, error) {
	cfg, err := parsePoolConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("AKERDOCK_DATABASE_URL: unparsable DSN: %w", err)
	}
	deadline := now().Add(connectWindow)
	backoff := 500 * time.Millisecond
	for {
		pool, err := newPool(ctx, cfg)
		if err == nil {
			if err = pingPool(pool, ctx); err == nil {
				return pool, nil
			}
			closePool(pool)
		}
		if now().After(deadline) || ctx.Err() != nil {
			return nil, fmt.Errorf("postgres: unreachable after %s: %w", connectWindow, err)
		}
		logger.Info("postgres not ready, retrying", "backoff", backoff.String())
		select {
		case <-after(backoff):
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
	sqlDB, err := openMigrationDB("pgx", dsn)
	if err != nil {
		return fmt.Errorf("migrations: open: %w", err)
	}
	defer func() { _ = closeMigrationDB(sqlDB) }()

	setBaseFS(db.Migrations)
	setGooseLogger(goose.NopLogger())
	if err := setDialect("postgres"); err != nil {
		return err
	}
	if err := upMigrations(ctx, sqlDB, "migrations"); err != nil {
		return fmt.Errorf("migrations: %w", err)
	}
	version, err := migrationVersion(ctx, sqlDB)
	if err != nil {
		return err
	}
	// "migrations applied" is a normative log marker relied upon by the
	// upgrade-downgrade runbook.
	logger.Info("migrations applied", "version", version)
	return nil
}

var _ = stdlib.GetDefaultDriver // ensure the pgx database/sql driver is linked
