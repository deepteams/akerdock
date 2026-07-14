package pguuid

import (
	"regexp"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestNew(t *testing.T) {
	// A handful of draws: version and variant bits must hold for every one,
	// not just a lucky first.
	for range 32 {
		u, err := New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if !u.Valid {
			t.Fatal("Valid = false, want true")
		}
		if u.Bytes[6]&0xf0 != 0x40 {
			t.Fatalf("version nibble = %#x, want 0x40 (v4)", u.Bytes[6]&0xf0)
		}
		if u.Bytes[8]&0xc0 != 0x80 {
			t.Fatalf("variant bits = %#x, want 0x80 (RFC 4122)", u.Bytes[8]&0xc0)
		}
	}
}

func TestString(t *testing.T) {
	t.Run("invalid yields empty string", func(t *testing.T) {
		if got := String(pgtype.UUID{}); got != "" {
			t.Errorf("String(NULL uuid) = %q, want \"\"", got)
		}
	})

	t.Run("canonical lowercase 8-4-4-4-12", func(t *testing.T) {
		u := pgtype.UUID{
			Bytes: [16]byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x23, 0x45, 0x67, 0x89, 0xAB, 0xCD, 0xEF, 0xFE, 0xDC, 0xBA, 0x98},
			Valid: true,
		}
		want := "deadbeef-0123-4567-89ab-cdeffedcba98"
		if got := String(u); got != want {
			t.Errorf("String = %q, want %q", got, want)
		}
	})

	t.Run("generated UUIDs match the canonical shape", func(t *testing.T) {
		canonical := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
		u, err := New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if got := String(u); !canonical.MatchString(got) {
			t.Errorf("String = %q, want canonical lowercase 8-4-4-4-12", got)
		}
	})
}

func TestMustParse(t *testing.T) {
	t.Run("round-trip", func(t *testing.T) {
		tests := []string{
			"deadbeef-0123-4567-89ab-cdeffedcba98",
			"00000000-0000-0000-0000-000000000000",
			"ffffffff-ffff-ffff-ffff-ffffffffffff",
		}
		for _, s := range tests {
			u := MustParse(s)
			if !u.Valid {
				t.Errorf("MustParse(%q).Valid = false, want true", s)
			}
			if got := String(u); got != s {
				t.Errorf("String(MustParse(%q)) = %q, want the input back", s, got)
			}
		}
	})

	t.Run("invalid input yields the NULL uuid", func(t *testing.T) {
		tests := []string{
			"",
			"not-a-uuid",
			"deadbeef-0123-4567-89ab",              // truncated
			"zzadbeef-0123-4567-89ab-cdeffedcba98", // non-hex
			"deadbeef-0123-4567-89ab-cdeffedcba98-extra", // too long
		}
		for _, s := range tests {
			if u := MustParse(s); u.Valid {
				t.Errorf("MustParse(%q).Valid = true, want false", s)
			}
		}
	})
}
