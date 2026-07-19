package handlers

import "testing"

// A malformed FQDN poisons invitation links, OAuth callbacks and the WebAuthn
// relying party at once — the normalization must catch every URL-shaped paste
// and keep only bare hostnames.
func TestNormalizeFqdn(t *testing.T) {
	str := func(s string) *string { return &s }

	cases := []struct {
		name    string
		in      *string
		want    *string
		refused bool
	}{
		{name: "nil clears", in: nil, want: nil},
		{name: "empty clears", in: str(""), want: nil},
		{name: "spaces clear", in: str("   "), want: nil},
		{name: "bare hostname kept", in: str("deploy.example.com"), want: str("deploy.example.com")},
		{name: "uppercase lowered", in: str("Manager.AD.Kedric.FR"), want: str("manager.ad.kedric.fr")},
		{name: "scheme refused", in: str("https://deploy.example.com"), refused: true},
		{name: "path refused", in: str("deploy.example.com/app"), refused: true},
		{name: "port refused", in: str("deploy.example.com:8080"), refused: true},
		{name: "single label refused", in: str("localhost"), refused: true},
		{name: "leading dash refused", in: str("-bad.example.com"), refused: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, detail := normalizeFqdn(tc.in)
			if tc.refused {
				if detail == nil {
					t.Fatalf("%q must be refused", *tc.in)
				}
				return
			}
			if detail != nil {
				t.Fatalf("unexpected refusal: %s", detail.Message)
			}
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("want cleared, got %q", *got)
			case tc.want != nil && (got == nil || *got != *tc.want):
				t.Fatalf("want %q, got %v", *tc.want, got)
			}
		})
	}
}

func TestNormalizeAcmeEmail(t *testing.T) {
	str := func(s string) *string { return &s }
	if _, detail := normalizeAcmeEmail(str("ops@example.com")); detail != nil {
		t.Fatal("a valid address must pass")
	}
	if got, detail := normalizeAcmeEmail(str("  ")); detail != nil || got != nil {
		t.Fatal("blank clears the contact")
	}
	if _, detail := normalizeAcmeEmail(str("not-an-email")); detail == nil {
		t.Fatal("a non-address must be refused")
	}
}
