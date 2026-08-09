package store_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The encrypted-column inventory (ADR-003, migration 00093) is a property of
// the SCHEMA — which columns exist, and what row identity each one binds into
// its AAD — so it is tested against a real PostgreSQL. Without
// AKERDOCK_TEST_DATABASE_URL the test skips (CI sets it).
//
// What makes this worth a database: the rotation and the `GET /system/encryption`
// histogram both derive from these functions. If the inventory misses a column,
// the histogram reports a converged rotation over columns it never looked at,
// and the runbook turns that into deleting a key version those columns still
// need. A unit test with a fake database cannot see that — only the schema can.

type invColumn struct {
	Table  string `json:"tbl"`
	Column string `json:"col"`
}

func encryptionTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("AKERDOCK_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AKERDOCK_TEST_DATABASE_URL is not set — skipping the encryption inventory integration test")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("cannot connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func encryptionInventory(t *testing.T, pool *pgxpool.Pool) []invColumn {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(), "SELECT encryption_inventory_json()").Scan(&raw); err != nil {
		t.Fatalf("encryption_inventory_json: %v", err)
	}
	var columns []invColumn
	if err := json.Unmarshal(raw, &columns); err != nil {
		t.Fatalf("decoding the inventory: %v", err)
	}
	return columns
}

// The inventory is exactly the set of bytea `*_enc` columns of the schema —
// compared against information_schema rather than against a list written here,
// because a list written here is the very thing that drifted.
func TestEncryptionInventoryMatchesTheSchema(t *testing.T) {
	pool := encryptionTestPool(t)
	ctx := context.Background()

	got := map[string]bool{}
	for _, c := range encryptionInventory(t, pool) {
		got[c.Table+"."+c.Column] = true
	}

	rows, err := pool.Query(ctx, `
		SELECT c.table_name || '.' || c.column_name
		FROM information_schema.columns c
		JOIN information_schema.tables tb
		  ON tb.table_schema = c.table_schema AND tb.table_name = c.table_name
		 AND tb.table_type = 'BASE TABLE'
		WHERE c.table_schema = current_schema()
		  AND c.data_type = 'bytea' AND c.column_name LIKE '%\_enc'`)
	if err != nil {
		t.Fatalf("listing encrypted columns: %v", err)
	}
	defer rows.Close()
	want := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		want[name] = true
	}
	if len(want) == 0 {
		t.Fatal("the schema declares no encrypted column at all — the query is wrong, not the schema")
	}
	for name := range want {
		if !got[name] {
			t.Errorf("%s is encrypted but absent from the inventory: it would never be rotated", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("%s is in the inventory but not an encrypted column of the schema", name)
		}
	}
}

// Every column of the inventory must actually be queryable: this executes the
// dynamic SQL — key-version extraction AND the row-identity expression — once
// per column against the real tables. A table whose identity expression does
// not apply to it fails here rather than at rotation time, on production data.
func TestEncryptionRotationCandidatesRunOnEveryColumn(t *testing.T) {
	pool := encryptionTestPool(t)
	ctx := context.Background()
	columns := encryptionInventory(t, pool)
	if len(columns) == 0 {
		t.Fatal("empty inventory")
	}
	for _, c := range columns {
		var raw []byte
		err := pool.QueryRow(ctx,
			"SELECT encryption_rotation_candidates($1, $2, $3, $4)",
			c.Table, c.Column, 1, 10).Scan(&raw)
		if err != nil {
			t.Errorf("%s.%s: %v", c.Table, c.Column, err)
			continue
		}
		var batch []struct {
			RowID      int64  `json:"row_id"`
			RowAAD     string `json:"row_aad"`
			Ciphertext []byte `json:"ciphertext"`
		}
		if err := json.Unmarshal(raw, &batch); err != nil {
			t.Errorf("%s.%s: decoding candidates: %v", c.Table, c.Column, err)
		}
	}
}

// The histogram covers the whole inventory and nothing else: it is the signal
// the runbook reads before deleting a key version.
func TestEncryptionHistogramStaysWithinTheInventory(t *testing.T) {
	pool := encryptionTestPool(t)
	ctx := context.Background()

	inventory := map[string]bool{}
	for _, c := range encryptionInventory(t, pool) {
		inventory[c.Table+"."+c.Column] = true
	}

	var raw []byte
	if err := pool.QueryRow(ctx, "SELECT encryption_key_histogram()").Scan(&raw); err != nil {
		t.Fatalf("encryption_key_histogram: %v", err)
	}
	var entries []struct {
		Table      string `json:"tbl"`
		Column     string `json:"col"`
		KeyVersion int    `json:"key_version"`
		RowCount   int64  `json:"row_count"`
	}
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("decoding the histogram: %v", err)
	}
	for _, e := range entries {
		if !inventory[e.Table+"."+e.Column] {
			t.Errorf("the histogram reports %s.%s, which is not an encrypted column", e.Table, e.Column)
		}
	}
}

// The row identity bound into the AAD is read off the schema, and the three
// shapes must land on the right tables: a column keyed by the wrong identity
// would fail to decrypt during rotation.
func TestEncryptionInventoryIdentityShapes(t *testing.T) {
	pool := encryptionTestPool(t)
	ctx := context.Background()

	cases := []struct {
		table, wantIn string
	}{
		// Own uuid — the common case.
		{"private_keys", "t.uuid::text"},
		{"mfa_factors", "t.uuid::text"},
		// Shares the primary key of resources: the identity is the RESOURCE's
		// uuid, which is what the handlers encrypt with.
		{"applications", "FROM resources"},
		{"services", "FROM resources"},
		// Singleton, no uuid at all.
		{"instance_settings", "t.id::text"},
	}
	for _, tc := range cases {
		var expr string
		err := pool.QueryRow(ctx,
			"SELECT identity_expr FROM encryption_inventory() WHERE tbl = $1 LIMIT 1", tc.table).Scan(&expr)
		if err != nil {
			t.Errorf("%s: %v", tc.table, err)
			continue
		}
		if !strings.Contains(expr, tc.wantIn) {
			t.Errorf("%s identity = %q, want it to use %q", tc.table, expr, tc.wantIn)
		}
	}
}

// A caller cannot point the rotation at a table that is not encrypted: the
// identifier is quoted either way, the inventory decides whether it may be
// named at all.
func TestEncryptionRotationRejectsColumnsOutsideTheInventory(t *testing.T) {
	pool := encryptionTestPool(t)
	ctx := context.Background()

	var raw []byte
	err := pool.QueryRow(ctx,
		"SELECT encryption_rotation_candidates($1, $2, $3, $4)", "users", "password_hash", 1, 10).Scan(&raw)
	if err == nil || !strings.Contains(err.Error(), "not an envelope-encrypted column") {
		t.Errorf("candidates on a non-encrypted column: %v", err)
	}
	var written int64
	err = pool.QueryRow(ctx,
		"SELECT encryption_rotation_apply($1, $2, $3, $4)", "users", "password_hash", 1, []byte("x")).Scan(&written)
	if err == nil || !strings.Contains(err.Error(), "not an envelope-encrypted column") {
		t.Errorf("apply on a non-encrypted column: %v", err)
	}
}

// End to end on the singleton, the shape no uuid-based assumption covers: a row
// stamped with a foreign key version must come back as a candidate, carrying
// the identity the AAD expects ("1", the row id).
func TestEncryptionRotationCandidatesOnTheSingleton(t *testing.T) {
	pool := encryptionTestPool(t)
	ctx := context.Background()

	// Version 99 in the first 4 bytes, then arbitrary bytes: the function reads
	// versions, it never decrypts.
	blob := append([]byte{0, 0, 0, 99}, []byte("opaque")...)
	if _, err := pool.Exec(ctx, `
		INSERT INTO instance_settings (id, otlp_config_enc) VALUES (1, $1)
		ON CONFLICT (id) DO UPDATE SET otlp_config_enc = EXCLUDED.otlp_config_enc`, blob); err != nil {
		t.Fatalf("seeding instance_settings: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "UPDATE instance_settings SET otlp_config_enc = NULL WHERE id = 1")
	})

	var raw []byte
	if err := pool.QueryRow(ctx,
		"SELECT encryption_rotation_candidates($1, $2, $3, $4)",
		"instance_settings", "otlp_config_enc", 1, 10).Scan(&raw); err != nil {
		t.Fatalf("candidates: %v", err)
	}
	var batch []struct {
		RowID      int64  `json:"row_id"`
		RowAAD     string `json:"row_aad"`
		Ciphertext []byte `json:"ciphertext"`
	}
	if err := json.Unmarshal(raw, &batch); err != nil {
		t.Fatalf("decoding candidates: %v", err)
	}
	if len(batch) != 1 {
		t.Fatalf("candidates = %d, want the row stamped with version 99", len(batch))
	}
	if batch[0].RowAAD != "1" {
		t.Errorf("row identity = %q, want the singleton id", batch[0].RowAAD)
	}
	if string(batch[0].Ciphertext) != string(blob) {
		t.Errorf("ciphertext round-trip = %q, want %q", batch[0].Ciphertext, blob)
	}

	// And it disappears from the candidates once it carries the active version.
	if _, err := pool.Exec(ctx, "SELECT encryption_rotation_apply($1, $2, $3, $4)",
		"instance_settings", "otlp_config_enc", batch[0].RowID,
		append([]byte{0, 0, 0, 1}, []byte("opaque")...)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := pool.QueryRow(ctx,
		"SELECT encryption_rotation_candidates($1, $2, $3, $4)",
		"instance_settings", "otlp_config_enc", 1, 10).Scan(&raw); err != nil {
		t.Fatalf("candidates after apply: %v", err)
	}
	if err := json.Unmarshal(raw, &batch); err != nil {
		t.Fatalf("decoding candidates: %v", err)
	}
	if len(batch) != 0 {
		t.Errorf("candidates after rewrite = %d, want none", len(batch))
	}
}
