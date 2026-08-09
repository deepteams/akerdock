package jobs

// Tests for encryptionrotate.go. The rotation reads the encrypted-column
// inventory FROM THE SCHEMA and rewrites whatever it names, so these tests
// feed it an inventory and assert on what it rotates — never on a list of
// tables held here. A test pinning a fixed set of tables would have passed
// happily during the whole period the rotation covered 7 of 23 columns.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// servercovInventoryJSON renders an inventory as the database returns it.
func servercovInventoryJSON(t *testing.T, columns ...envelope.Column) []byte {
	t.Helper()
	raw, err := json.Marshal(columns)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// servercovCandidatesJSON renders one batch of rotation candidates; a []byte
// ciphertext marshals to base64, exactly as the SQL side encodes it.
func servercovCandidatesJSON(t *testing.T, rows ...envelope.RotationCandidate) []byte {
	t.Helper()
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// servercovRotation scripts the three calls a rotation makes. candidates is
// served once per column and then dries up, which is how the batch loop ends.
func servercovRotation(db *servercovDB, inventory []byte, candidates func(call int) []byte) {
	db.rowAfter["ListEncryptedColumns"] = servercovOverride(map[int]func(any){
		0: servercovBytes(inventory),
	})
	call := 0
	db.rowAfter["EncryptionRotationCandidates"] = func(dest []any) {
		servercovBytes(candidates(call))(dest[0])
		call++
	}
	db.rowAfter["EncryptionRotationApply"] = servercovOverride(map[int]func(any){
		0: servercovVal(int64(1)),
	})
}

// The rotation covers EVERY column the inventory names — including the ones no
// hand-written rotation ever mentioned (mfa factors, GitHub App secrets, the
// singleton instance settings, a resource-keyed basic-auth blob).
func TestServercovEncryptionRotateCoversWholeInventory(t *testing.T) {
	q, keyring, _, logger, db := servercovDeps(t)
	inventory := []envelope.Column{
		{Table: "mfa_factors", Column: "secret_enc"},
		{Table: "github_apps", Column: "client_secret_enc"},
		{Table: "instance_settings", Column: "otlp_config_enc"},
		{Table: "applications", Column: "access_basic_auth_enc"},
	}

	// One candidate row per column, each encrypted under its own AAD: a
	// rotation that mixed columns up would fail to decrypt.
	perColumn := map[string][]byte{}
	for _, c := range inventory {
		perColumn[c.String()] = servercovEncrypt(t, keyring, c.Table, c.Column, "secret-"+c.Column)
	}
	seen := []string{}
	db.rowAfter["ListEncryptedColumns"] = servercovOverride(map[int]func(any){
		0: servercovBytes(servercovInventoryJSON(t, inventory...)),
	})
	call := 0
	db.rowAfter["EncryptionRotationCandidates"] = func(dest []any) {
		// Two calls per column: one batch, then the empty one that ends it.
		idx, empty := call/2, call%2 == 1
		call++
		if idx >= len(inventory) || empty {
			servercovBytes([]byte(`[]`))(dest[0])
			return
		}
		c := inventory[idx]
		seen = append(seen, c.String())
		servercovBytes(servercovCandidatesJSON(t, envelope.RotationCandidate{
			RowID: int64(idx + 1), RowAAD: jobFixtureUUID, Ciphertext: perColumn[c.String()],
		}))(dest[0])
	}
	db.rowAfter["EncryptionRotationApply"] = servercovOverride(map[int]func(any){
		0: servercovVal(int64(1)),
	})

	job := store.Job{ID: 10, JobType: TypeEncryptionRotate, Payload: []byte(`{}`)}
	result, err := (&EncryptionRotate{Store: q, Keyring: keyring, Logger: logger}).
		Execute(context.Background(), job, queue.NewStepRecorder(q, job))
	if err != nil {
		t.Fatal(err)
	}
	payload := result.(map[string]any)
	if payload["rows_reencrypted"] != len(inventory) || payload["columns_covered"] != len(inventory) {
		t.Fatalf("rotation result = %#v", result)
	}
	if len(seen) != len(inventory) {
		t.Fatalf("columns visited = %v, want all of %v", seen, inventory)
	}
	for i, c := range inventory {
		if seen[i] != c.String() {
			t.Fatalf("columns visited = %v, want all of %v", seen, inventory)
		}
	}
}

// An empty inventory is a broken query, not an empty database: reporting a
// converged rotation over it is what would let an operator drop a key version
// that rows still depend on.
func TestServercovEncryptionRotateRefusesEmptyInventory(t *testing.T) {
	q, keyring, _, logger, db := servercovDeps(t)
	db.rowAfter["ListEncryptedColumns"] = servercovOverride(map[int]func(any){
		0: servercovBytes([]byte(`[]`)),
	})
	job := store.Job{ID: 11, JobType: TypeEncryptionRotate, Payload: []byte(`{}`)}
	_, err := (&EncryptionRotate{Store: q, Keyring: keyring, Logger: logger}).
		Execute(context.Background(), job, queue.NewStepRecorder(q, job))
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("error = %v, want a refusal to rotate nothing", err)
	}
}

func TestServercovEncryptionRotateReportsInventoryFailure(t *testing.T) {
	q, keyring, _, logger, db := servercovDeps(t)
	db.rowErr["ListEncryptedColumns"] = errors.New("inventory unavailable")
	job := store.Job{ID: 12, JobType: TypeEncryptionRotate, Payload: []byte(`{}`)}
	_, err := (&EncryptionRotate{Store: q, Keyring: keyring, Logger: logger}).
		Execute(context.Background(), job, queue.NewStepRecorder(q, job))
	if err == nil || !strings.Contains(err.Error(), "inventory unavailable") {
		t.Fatalf("error = %v", err)
	}
}

// A row that cannot be decrypted names its column and its id: with 23 columns
// in play, "decrypt failed" alone would not tell an operator where to look.
func TestServercovEncryptionRotateReportsDecryptFailure(t *testing.T) {
	q, keyring, _, logger, db := servercovDeps(t)
	servercovRotation(db,
		servercovInventoryJSON(t, envelope.Column{Table: "git_sources", Column: "api_token_enc"}),
		func(int) []byte {
			return servercovCandidatesJSON(t, envelope.RotationCandidate{
				RowID: 7, RowAAD: jobFixtureUUID, Ciphertext: []byte("not-a-ciphertext"),
			})
		})
	job := store.Job{ID: 13, JobType: TypeEncryptionRotate, Payload: []byte(`{}`)}
	_, err := (&EncryptionRotate{Store: q, Keyring: keyring, Logger: logger}).
		Execute(context.Background(), job, queue.NewStepRecorder(q, job))
	if err == nil || !strings.Contains(err.Error(), "git_sources.api_token_enc") ||
		!strings.Contains(err.Error(), "id=7") {
		t.Fatalf("error = %v, want the failing column and row named", err)
	}
}

func TestServercovEncryptionRotateReportsCandidateFailure(t *testing.T) {
	q, keyring, _, logger, db := servercovDeps(t)
	db.rowAfter["ListEncryptedColumns"] = servercovOverride(map[int]func(any){
		0: servercovBytes(servercovInventoryJSON(t,
			envelope.Column{Table: "agent_tokens", Column: "token_enc"})),
	})
	db.rowErr["EncryptionRotationCandidates"] = errors.New("batch unavailable")
	job := store.Job{ID: 14, JobType: TypeEncryptionRotate, Payload: []byte(`{}`)}
	_, err := (&EncryptionRotate{Store: q, Keyring: keyring, Logger: logger}).
		Execute(context.Background(), job, queue.NewStepRecorder(q, job))
	if err == nil || !strings.Contains(err.Error(), "agent_tokens.token_enc") ||
		!strings.Contains(err.Error(), "batch unavailable") {
		t.Fatalf("error = %v", err)
	}
}

func TestServercovEncryptionRotateReportsWriteFailure(t *testing.T) {
	q, keyring, _, logger, db := servercovDeps(t)
	servercovRotation(db,
		servercovInventoryJSON(t, envelope.Column{Table: "shared_variables", Column: "value_enc"}),
		func(int) []byte {
			return servercovCandidatesJSON(t, envelope.RotationCandidate{
				RowID: 3, RowAAD: jobFixtureUUID,
				Ciphertext: servercovEncrypt(t, keyring, "shared_variables", "value_enc", "ok"),
			})
		})
	db.rowErr["EncryptionRotationApply"] = errors.New("write refused")
	job := store.Job{ID: 15, JobType: TypeEncryptionRotate, Payload: []byte(`{}`)}
	_, err := (&EncryptionRotate{Store: q, Keyring: keyring, Logger: logger}).
		Execute(context.Background(), job, queue.NewStepRecorder(q, job))
	if err == nil || !strings.Contains(err.Error(), "write refused") {
		t.Fatalf("error = %v", err)
	}
}

// Every candidate written away by a concurrent delete: the same batch would
// come back forever, so the job stops and says so instead of spinning.
func TestServercovEncryptionRotateStopsWhenNoRowCanBeWritten(t *testing.T) {
	q, keyring, _, logger, db := servercovDeps(t)
	servercovRotation(db,
		servercovInventoryJSON(t, envelope.Column{Table: "registry_credentials", Column: "password_enc"}),
		func(int) []byte {
			return servercovCandidatesJSON(t, envelope.RotationCandidate{
				RowID: 4, RowAAD: jobFixtureUUID,
				Ciphertext: servercovEncrypt(t, keyring, "registry_credentials", "password_enc", "ok"),
			})
		})
	// Every write reports "no row touched".
	db.rowAfter["EncryptionRotationApply"] = servercovOverride(map[int]func(any){
		0: servercovVal(int64(0)),
	})
	job := store.Job{ID: 16, JobType: TypeEncryptionRotate, Payload: []byte(`{}`)}
	_, err := (&EncryptionRotate{Store: q, Keyring: keyring, Logger: logger}).
		Execute(context.Background(), job, queue.NewStepRecorder(q, job))
	if err == nil || !strings.Contains(err.Error(), "could not be rewritten") {
		t.Fatalf("error = %v, want the loop to stop loudly", err)
	}
}
