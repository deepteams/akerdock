package envelope

import (
	"encoding/json"
	"fmt"
)

// The schema-derived encryption inventory (ADR-003, data dictionary §12).
//
// The database computes what is encrypted — every `*_enc` column, the row
// identity each one binds into its AAD — and hands it over as JSON (migration
// 00093). Nothing on this side keeps a list: a hand-kept one is what let the
// rotation and the key-version histogram cover 7 of 23 columns while reporting
// a converged rotation, which invites an operator to delete a key version that
// 16 columns still depend on.

// Column names one envelope-encrypted column of the schema.
type Column struct {
	Table  string `json:"tbl"`
	Column string `json:"col"`
}

// String renders the column as it is shown to operators and in errors.
func (c Column) String() string { return c.Table + "." + c.Column }

// HistogramEntry counts the rows of one column sitting on one key version.
type HistogramEntry struct {
	Table      string `json:"tbl"`
	Column     string `json:"col"`
	KeyVersion int    `json:"key_version"`
	RowCount   int64  `json:"row_count"`
}

// RotationCandidate is one row still encrypted under an older key version.
// Ciphertext arrives base64-encoded and decodes into []byte on its own.
type RotationCandidate struct {
	RowID      int64  `json:"row_id"`
	RowAAD     string `json:"row_aad"`
	Ciphertext []byte `json:"ciphertext"`
}

// DecodeInventory reads the column inventory returned by the database.
func DecodeInventory(raw []byte) ([]Column, error) {
	return decodeJSON[Column]("encryption inventory", raw)
}

// DecodeHistogram reads the key-version histogram returned by the database.
func DecodeHistogram(raw []byte) ([]HistogramEntry, error) {
	return decodeJSON[HistogramEntry]("encryption histogram", raw)
}

// DecodeCandidates reads one batch of rotation candidates.
func DecodeCandidates(raw []byte) ([]RotationCandidate, error) {
	return decodeJSON[RotationCandidate]("rotation candidates", raw)
}

func decodeJSON[T any](what string, raw []byte) ([]T, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out []T
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decoding the %s: %w", what, err)
	}
	return out, nil
}
