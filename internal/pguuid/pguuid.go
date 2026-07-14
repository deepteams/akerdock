// Package pguuid bridges pgtype.UUID with the string form used by the API
// and generates application-side UUIDs (needed when the envelope AAD must
// bind a ciphertext to its row uuid before the insert).
package pguuid

import (
	"crypto/rand"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
)

// New returns a random (v4) UUID.
func New() (pgtype.UUID, error) {
	var u pgtype.UUID
	if _, err := rand.Read(u.Bytes[:]); err != nil {
		return u, fmt.Errorf("pguuid: %w", err)
	}
	u.Bytes[6] = (u.Bytes[6] & 0x0f) | 0x40
	u.Bytes[8] = (u.Bytes[8] & 0x3f) | 0x80
	u.Valid = true
	return u, nil
}

// String renders the canonical lowercase form; "" when invalid/NULL.
func String(u pgtype.UUID) string {
	if !u.Valid {
		return ""
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", u.Bytes[0:4], u.Bytes[4:6], u.Bytes[6:8], u.Bytes[8:10], u.Bytes[10:16])
}

// MustParse converts a canonical UUID string; an invalid input yields the
// NULL uuid, which matches nothing.
func MustParse(s string) pgtype.UUID {
	var u pgtype.UUID
	_ = u.Scan(s)
	return u
}
