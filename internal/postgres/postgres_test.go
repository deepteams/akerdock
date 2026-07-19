package postgres

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pressly/goose/v3"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func restoreConnectSeams() func() {
	oldParse, oldNew := parsePoolConfig, newPool
	oldPing, oldClose := pingPool, closePool
	oldNow, oldAfter := now, after
	return func() {
		parsePoolConfig, newPool = oldParse, oldNew
		pingPool, closePool = oldPing, oldClose
		now, after = oldNow, oldAfter
	}
}

func TestConnectRejectsInvalidDSN(t *testing.T) {
	if _, err := Connect(context.Background(), "://bad", testLogger()); err == nil ||
		!strings.Contains(err.Error(), "unparsable DSN") {
		t.Fatalf("Connect invalid DSN = %v", err)
	}
}

func TestConnectSucceeds(t *testing.T) {
	defer restoreConnectSeams()()
	var pool pgxpool.Pool
	newPool = func(context.Context, *pgxpool.Config) (*pgxpool.Pool, error) { return &pool, nil }
	pingPool = func(*pgxpool.Pool, context.Context) error { return nil }
	got, err := Connect(context.Background(), "postgres://localhost/database", testLogger())
	if err != nil || got != &pool {
		t.Fatalf("Connect = %p, %v", got, err)
	}
}

func TestConnectRetriesAndStopsAtDeadline(t *testing.T) {
	defer restoreConnectSeams()()
	errDown := errors.New("database down")
	newPool = func(context.Context, *pgxpool.Config) (*pgxpool.Pool, error) { return nil, errDown }
	start := time.Unix(100, 0)
	times := []time.Time{start, start, start.Add(time.Second), start.Add(connectWindow + time.Second)}
	now = func() time.Time {
		value := times[0]
		times = times[1:]
		return value
	}
	var waits []time.Duration
	after = func(delay time.Duration) <-chan time.Time {
		waits = append(waits, delay)
		ready := make(chan time.Time, 1)
		ready <- start
		return ready
	}
	_, err := Connect(context.Background(), "postgres://localhost/database", testLogger())
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("Connect error = %v", err)
	}
	if len(waits) != 2 || waits[0] != 500*time.Millisecond || waits[1] != time.Second {
		t.Fatalf("retry waits = %v", waits)
	}
}

func TestConnectClosesFailedPoolAndHonoursCancellation(t *testing.T) {
	defer restoreConnectSeams()()
	var pool pgxpool.Pool
	newPool = func(context.Context, *pgxpool.Config) (*pgxpool.Pool, error) { return &pool, nil }
	pingPool = func(*pgxpool.Pool, context.Context) error { return errors.New("not ready") }
	closed := false
	closePool = func(*pgxpool.Pool) { closed = true }
	after = func(time.Duration) <-chan time.Time { return make(chan time.Time) }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Connect(ctx, "postgres://localhost/database", testLogger())
	if err == nil || !closed {
		t.Fatalf("Connect cancelled = %v, closed=%v", err, closed)
	}
}

func restoreMigrationSeams() func() {
	oldOpen, oldClose := openMigrationDB, closeMigrationDB
	oldBaseFS, oldLogger := setBaseFS, setGooseLogger
	oldDialect, oldUp, oldVersion := setDialect, upMigrations, migrationVersion
	return func() {
		openMigrationDB, closeMigrationDB = oldOpen, oldClose
		setBaseFS, setGooseLogger = oldBaseFS, oldLogger
		setDialect, upMigrations, migrationVersion = oldDialect, oldUp, oldVersion
	}
}

func TestMigrateStages(t *testing.T) {
	errBoom := errors.New("boom")
	t.Run("open", func(t *testing.T) {
		defer restoreMigrationSeams()()
		openMigrationDB = func(string, string) (*sql.DB, error) { return nil, errBoom }
		if err := Migrate(context.Background(), "dsn", testLogger()); err == nil ||
			!strings.Contains(err.Error(), "migrations: open") {
			t.Fatalf("Migrate = %v", err)
		}
	})
	t.Run("migration", func(t *testing.T) {
		defer restoreMigrationSeams()()
		openMigrationDB = func(string, string) (*sql.DB, error) { return nil, nil }
		closeMigrationDB = func(*sql.DB) error { return nil }
		upMigrations = func(context.Context, *sql.DB, string, ...goose.OptionsFunc) error { return errBoom }
		if err := Migrate(context.Background(), "dsn", testLogger()); err == nil ||
			!strings.Contains(err.Error(), "migrations: boom") {
			t.Fatalf("Migrate = %v", err)
		}
	})
	t.Run("version", func(t *testing.T) {
		defer restoreMigrationSeams()()
		openMigrationDB = func(string, string) (*sql.DB, error) { return nil, nil }
		closeMigrationDB = func(*sql.DB) error { return nil }
		upMigrations = func(context.Context, *sql.DB, string, ...goose.OptionsFunc) error { return nil }
		migrationVersion = func(context.Context, *sql.DB) (int64, error) { return 0, errBoom }
		if err := Migrate(context.Background(), "dsn", testLogger()); !errors.Is(err, errBoom) {
			t.Fatalf("Migrate = %v", err)
		}
	})
	t.Run("success closes database", func(t *testing.T) {
		defer restoreMigrationSeams()()
		openMigrationDB = func(string, string) (*sql.DB, error) { return nil, nil }
		closed := false
		closeMigrationDB = func(*sql.DB) error { closed = true; return nil }
		upMigrations = func(context.Context, *sql.DB, string, ...goose.OptionsFunc) error { return nil }
		migrationVersion = func(context.Context, *sql.DB) (int64, error) { return 42, nil }
		if err := Migrate(context.Background(), "dsn", testLogger()); err != nil || !closed {
			t.Fatalf("Migrate = %v, closed=%v", err, closed)
		}
	})
}
