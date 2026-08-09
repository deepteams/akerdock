package envelope

import (
	"strings"
	"testing"
)

// The inventory crosses the SQL boundary as JSON, so the decoding is where a
// schema-derived column list becomes Go values the rotation acts on.

func TestDecodeInventory(t *testing.T) {
	columns, err := DecodeInventory([]byte(
		`[{"tbl":"mfa_factors","col":"secret_enc"},{"tbl":"servers","col":"ca_key_enc"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(columns) != 2 {
		t.Fatalf("columns = %#v", columns)
	}
	if got := columns[0].String(); got != "mfa_factors.secret_enc" {
		t.Fatalf("column name = %q", got)
	}
	if columns[1].Table != "servers" || columns[1].Column != "ca_key_enc" {
		t.Fatalf("columns = %#v", columns)
	}
}

func TestDecodeHistogram(t *testing.T) {
	entries, err := DecodeHistogram([]byte(
		`[{"tbl":"private_keys","col":"private_key_enc","key_version":2,"row_count":7}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].KeyVersion != 2 || entries[0].RowCount != 7 {
		t.Fatalf("entries = %#v", entries)
	}
	if entries[0].Table != "private_keys" || entries[0].Column != "private_key_enc" {
		t.Fatalf("entries = %#v", entries)
	}
}

// The ciphertext travels base64-encoded and must come back as the exact bytes:
// a mangled ciphertext would fail to decrypt, and a rotation cannot repair what
// it cannot read.
func TestDecodeCandidatesRestoresCiphertext(t *testing.T) {
	rows, err := DecodeCandidates([]byte(
		`[{"row_id":42,"row_aad":"11111111-1111-4111-8111-111111111111","ciphertext":"AAAAAmNpcGhlcg=="}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %#v", rows)
	}
	if rows[0].RowID != 42 || rows[0].RowAAD != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("row = %#v", rows[0])
	}
	// 0x00 0x00 0x00 0x02 — key version 2 — followed by "cipher".
	want := append([]byte{0, 0, 0, 2}, []byte("cipher")...)
	if string(rows[0].Ciphertext) != string(want) {
		t.Fatalf("ciphertext = %q, want %q", rows[0].Ciphertext, want)
	}
}

// An absent result is not a decoding failure: an empty column set is handled by
// the caller, which refuses to call it a completed rotation.
func TestDecodeEmptyInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func() (int, error)
	}{
		{"inventory", func() (int, error) { c, err := DecodeInventory(nil); return len(c), err }},
		{"histogram", func() (int, error) { h, err := DecodeHistogram(nil); return len(h), err }},
		{"candidates", func() (int, error) { r, err := DecodeCandidates(nil); return len(r), err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := tc.call()
			if err != nil || n != 0 {
				t.Fatalf("n = %d, err = %v", n, err)
			}
		})
	}
}

// A malformed payload names what failed to decode: three shapes cross this
// boundary, and "invalid character" alone would not say which.
func TestDecodeMalformedInput(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		call       func() error
	}{
		{"inventory", "encryption inventory", func() error { _, err := DecodeInventory([]byte(`{`)); return err }},
		{"histogram", "encryption histogram", func() error { _, err := DecodeHistogram([]byte(`{`)); return err }},
		{"candidates", "rotation candidates", func() error { _, err := DecodeCandidates([]byte(`{`)); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to name the %s", err, tc.want)
			}
		})
	}
}
