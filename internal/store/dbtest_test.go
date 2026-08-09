package store_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepteams/akerdock/internal/postgres"
)

// A real PostgreSQL for the tests whose subject is the SQL itself.
//
// Some of this project's guarantees are not properties of the Go around a
// statement, they ARE the statement: the queue's SKIP LOCKED hand-off, the
// audit trail's append-only trigger, and — the reason this harness was asked
// for — ADR-065's idempotent attach claim, whose entire correctness lives in one
// UPDATE's WHERE clause per family. A protocol fake can only assert which
// arguments a handler passed and which query text it passed them with, which is
// a restatement of the statement rather than a test of it: such an assertion
// passes just as happily against a statement with the wrong clause in it. So
// these tests run against the schema the migrations actually produce, and
// assert on the row afterwards.
//
// The contract is the one internal/queue, internal/session and the two existing
// tests in this package already use: AKERDOCK_TEST_DATABASE_URL points at a
// throwaway database, and without it the tests skip rather than fail, so
// `go test ./...` stays usable on a laptop with no database. CI already sets it
// (.github/workflows/ci.yml), so no workflow change is needed to opt in.
const testDatabaseURLEnv = "AKERDOCK_TEST_DATABASE_URL"

// Migrations are applied once per test binary, through the product's own
// embedded migration path rather than a shell step, so that a bare
// `postgres:18-alpine` is the whole of what a developer has to provide:
//
//	docker run -d --rm --name akd-test -e POSTGRES_PASSWORD=test \
//	  -e POSTGRES_DB=akerdock -p 15432:5432 postgres:18-alpine
//	AKERDOCK_TEST_DATABASE_URL='postgres://postgres:test@127.0.0.1:15432/akerdock?sslmode=disable' \
//	  go test ./internal/store/
//
// goose is idempotent, so on the database CI has already migrated this costs one
// version query and nothing else.
var testDatabase = sync.OnceValues(openTestDatabase)

// openTestDatabase returns (nil, nil) when the variable is unset — "no database
// configured" is not an error, it is the laptop case, and the skip belongs at
// the test rather than here so the message names the variable each time.
func openTestDatabase() (*pgxpool.Pool, error) {
	url := os.Getenv(testDatabaseURLEnv)
	if url == "" {
		return nil, nil
	}
	// Bounded: an unreachable host must fail the suite rather than hang it,
	// and postgres.Connect's own 30 s window fits inside this one.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := postgres.Migrate(ctx, url, slog.New(slog.DiscardHandler)); err != nil {
		return nil, err
	}
	// Not the timeout context: pgxpool.New keeps it for background connection
	// establishment, and a cancelled one would poison the pool on return.
	return pgxpool.New(context.Background(), url)
}

// testDB hands back the shared pool, or skips loudly enough that a developer
// who wanted these tests to run knows exactly what to set.
func testDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := testDatabase()
	if err != nil {
		t.Fatalf("preparing the test database from %s: %v", testDatabaseURLEnv, err)
	}
	if pool == nil {
		t.Skipf("%s is not set — skipping: these assertions are about PostgreSQL's behaviour and need a throwaway database, e.g. %s=postgres://postgres:test@127.0.0.1:5432/akerdock?sslmode=disable",
			testDatabaseURLEnv, testDatabaseURLEnv)
	}
	return pool
}

// testTeam creates the disposable tenant every fixture row in this package hangs
// off, and deletes it — with everything ON DELETE CASCADE takes along — when the
// test ends.
//
// That is the isolation strategy, and it is deliberately neither of the two
// obvious ones:
//
//   - Not a transaction per test. `now()` is the transaction's start time, so
//     every TTL, expiry and "the stamp did not move" assertion in this package
//     would be judged against a clock frozen before the test began — the
//     one-millisecond-past-expiry case could not even be expressed. And the
//     concurrent-claim assertion needs two connections committing against one
//     row, which is precisely what a single transaction cannot do.
//   - Not TRUNCATE. `go test ./...` runs this package in parallel with
//     internal/queue and internal/session against the same database, and
//     truncating shared tables would delete their fixtures from under them.
//
// A team costs one INSERT, gives each test rows no other test can reach, and
// leaves both the clock and the commit boundaries real.
func testTeam(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		"INSERT INTO teams (name) VALUES ($1) RETURNING id", "test-"+randomHex(8)).Scan(&id)
	if err != nil {
		t.Fatalf("creating the test team (are the migrations applied?): %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), "DELETE FROM teams WHERE id = $1", id); err != nil {
			t.Errorf("removing the test team: %v", err)
		}
	})
	return id
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// randomKeyHash is an attach key hash as the handler stores one: 32 opaque
// bytes. Whether they are the SHA-256 of a presented key or the random bytes a
// keyless attach gets instead (ADR-065 §7) is invisible to the statement under
// test, which is the point — it compares bytes.
func randomKeyHash(t *testing.T) []byte {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("generating an attach key hash: %v", err)
	}
	return b
}

func stamp(at time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: at, Valid: true}
}
