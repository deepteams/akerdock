package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// TypeEncryptionRotate re-encrypts every row still using an older master
// key version towards the active one (ADR-003, §23.2).
const TypeEncryptionRotate = "encryption.rotate"

// rotateBatch bounds each pass: the rewrite happens in batches, never as
// one blocking update (§19.2).
const rotateBatch = 200

// EncryptionRotate rewrites the *_enc columns of the data dictionary §12
// inventory. Idempotent: rows already on the active version are skipped, so
// a crashed rotation simply resumes.
//
// The set of columns it rewrites is READ FROM THE SCHEMA, never listed here.
// It used to be a list of six tables while the schema held twenty-three
// encrypted columns, and the key-version histogram was written from the same
// list — so it announced a converged rotation over the very columns it was
// blind to, which is the signal the runbook turns into "you may now delete the
// old key version". Deriving both from one schema query is what makes that
// class of gap impossible rather than merely fixed.
type EncryptionRotate struct {
	Store   *store.Queries
	Keyring *envelope.Keyring
	Logger  *slog.Logger
}

// Execute rotates every encrypted column towards the active key version.
func (h *EncryptionRotate) Execute(ctx context.Context, _ store.Job, rec *queue.StepRecorder) (any, error) {
	active := int32(h.Keyring.ActiveVersion())

	raw, err := h.Store.ListEncryptedColumns(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the encryption inventory: %w", err)
	}
	columns, err := envelope.DecodeInventory(raw)
	if err != nil {
		return nil, err
	}
	// An empty inventory means the query stopped seeing the schema, not that
	// there is nothing to protect: reporting "rotation complete" on it would be
	// the exact lie this design exists to prevent.
	if len(columns) == 0 {
		return nil, errors.New("the encryption inventory came back empty: refusing to report a rotation over nothing")
	}

	total := 0
	for _, column := range columns {
		rec.Start(ctx, column.String())
		n, err := h.rotateColumn(ctx, column, active)
		if err != nil {
			rec.Fail(ctx, err.Error())
			return nil, err
		}
		total += n
		rec.Succeed(ctx, fmt.Sprintf("%d rows re-encrypted", n))
	}

	h.Logger.Info("encryption rotation complete",
		"active_version", active, "columns", len(columns), "rows", total)
	return map[string]any{
		"active_key_version": active,
		"columns_covered":    len(columns),
		"rows_reencrypted":   total,
	}, nil
}

// rotateColumn rewrites one column, batch after batch, until no row carries
// another key version.
func (h *EncryptionRotate) rotateColumn(ctx context.Context, column envelope.Column, active int32) (int, error) {
	rotated := 0
	for {
		raw, err := h.Store.EncryptionRotationCandidates(ctx, store.EncryptionRotationCandidatesParams{
			TableName: column.Table, ColumnName: column.Column,
			ActiveVersion: active, RowLimit: rotateBatch,
		})
		if err != nil {
			return rotated, fmt.Errorf("%s: %w", column, err)
		}
		rows, err := envelope.DecodeCandidates(raw)
		if err != nil {
			return rotated, err
		}
		if len(rows) == 0 {
			return rotated, nil
		}

		written := 0
		for _, row := range rows {
			enc, err := h.rewrite(column, row.RowAAD, row.Ciphertext)
			if err != nil {
				return rotated, fmt.Errorf("%s id=%d: %w", column, row.RowID, err)
			}
			n, err := h.Store.EncryptionRotationApply(ctx, store.EncryptionRotationApplyParams{
				TableName: column.Table, ColumnName: column.Column,
				RowID: row.RowID, Value: enc,
			})
			if err != nil {
				return rotated, fmt.Errorf("%s id=%d: %w", column, row.RowID, err)
			}
			// A row deleted between the read and the write is not an error —
			// it no longer needs a key. It simply made no progress.
			if n == 0 {
				continue
			}
			written++
			rotated++
		}
		// The batch returned rows and none could be written: the next pass
		// would return the same ones forever. Stop loudly instead.
		if written == 0 {
			return rotated, fmt.Errorf("%s: %d candidate rows could not be rewritten", column, len(rows))
		}
	}
}

// rewrite decrypts with whichever version the row carries and re-encrypts
// with the active one, preserving the AAD binding (table, column, row identity).
// Decryption comes first, so a wrong identity fails here — before anything is
// written — rather than producing a row nobody can read afterwards.
func (h *EncryptionRotate) rewrite(column envelope.Column, rowAAD string, ciphertext []byte) ([]byte, error) {
	plaintext, err := h.Keyring.Decrypt(column.Table, column.Column, rowAAD, ciphertext)
	if err != nil {
		return nil, err
	}
	return h.Keyring.Encrypt(column.Table, column.Column, rowAAD, plaintext)
}
