package store_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepteams/akerdock/internal/store"
)

// The audit trail's append-only guarantee (§23.4, migration 00067) is a property
// of the DATABASE — a trigger, not Go discipline — so it is tested against a real
// PostgreSQL. Without AKERDOCK_TEST_DATABASE_URL the test skips (CI sets it).
func TestAuditEventsAreAppendOnly(t *testing.T) {
	url := os.Getenv("AKERDOCK_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AKERDOCK_TEST_DATABASE_URL is not set — skipping the audit immutability integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("cannot connect: %v", err)
	}
	t.Cleanup(pool.Close)

	// TRUNCATE bypasses row-level triggers, so it is the one way to reset the
	// append-only table between runs.
	if _, err := pool.Exec(ctx, "TRUNCATE audit_events"); err != nil {
		t.Fatalf("cannot reset audit_events (migrations applied?): %v", err)
	}
	q := store.New(pool)

	if err := q.InsertAuditEvent(ctx, store.InsertAuditEventParams{
		ActorKind: store.ActorKindUser, Action: "auth.login", Result: store.AuditResultSuccess,
	}); err != nil {
		t.Fatalf("insert audit event: %v", err)
	}

	// UPDATE is always refused.
	if _, err := pool.Exec(ctx, "UPDATE audit_events SET action = 'tampered'"); err == nil {
		t.Error("UPDATE on audit_events was allowed — the append-only trigger is missing")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("UPDATE rejected for the wrong reason: %v", err)
	}

	// A plain DELETE is refused (no purge GUC set).
	if _, err := pool.Exec(ctx, "DELETE FROM audit_events"); err == nil {
		t.Error("DELETE on audit_events was allowed outside the sanctioned purge")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("DELETE rejected for the wrong reason: %v", err)
	}

	// purge(0) keeps everything (retention disabled).
	if n, err := q.PurgeAuditEvents(ctx, 0); err != nil || n != 0 {
		t.Fatalf("purge(0) = %d, %v; want 0, nil", n, err)
	}

	// A row older than the retention window is purged; recent rows are not. The
	// backdated row is inserted raw (occurred_at has no query param).
	if _, err := pool.Exec(ctx,
		"INSERT INTO audit_events (actor_kind, action, result, occurred_at) VALUES ('system', 'old.event', 'success', now() - interval '400 days')"); err != nil {
		t.Fatalf("insert backdated event: %v", err)
	}
	n, err := q.PurgeAuditEvents(ctx, 365)
	if err != nil || n != 1 {
		t.Fatalf("purge(365) = %d, %v; want 1, nil", n, err)
	}
	// The recent login row survives.
	if c, err := q.CountAuditEvents(ctx); err != nil || c != 1 {
		t.Fatalf("remaining audit rows = %d, %v; want 1, nil", c, err)
	}
}
