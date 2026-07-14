package compose

import (
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

func components(names ...string) map[string]bool {
	out := map[string]bool{}
	for _, n := range names {
		out[NormalizeComponentID(n)] = true
	}
	return out
}

func TestNormalizeComponentID(t *testing.T) {
	cases := map[string]string{
		"open-webui": "OPEN_WEBUI",
		"db":         "DB",
		"app.v2":     "APP_V2",
	}
	for in, want := range cases {
		if got := NormalizeComponentID(in); got != want {
			t.Fatalf("NormalizeComponentID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseMagicName(t *testing.T) {
	comps := components("open-webui", "db", "app2")

	cases := []struct {
		name string
		want MagicRef
	}{
		{"SERVICE_PASSWORD_DB", MagicRef{Type: MagicPassword, ID: "DB", Length: 32, Credential: true}},
		{"SERVICE_PASSWORD_64_DB", MagicRef{Type: MagicPassword, ID: "DB", Length: 64, Credential: true}},
		{"SERVICE_PASSWORDWITHSYMBOLS_DB", MagicRef{Type: MagicPasswordWithSymbols, ID: "DB", Length: 32, Credential: true}},
		{"SERVICE_USER_OPEN_WEBUI", MagicRef{Type: MagicUser, ID: "OPEN_WEBUI", Length: 16, Credential: true}},
		{"SERVICE_BASE64_128_DB", MagicRef{Type: MagicBase64, ID: "DB", Length: 128, Credential: true}},
		{"SERVICE_REALBASE64_32_DB", MagicRef{Type: MagicRealBase64, ID: "DB", Length: 32, Credential: true}},
		{"SERVICE_HEX_64_DB", MagicRef{Type: MagicHex, ID: "DB", Length: 64, Credential: true}},
		{"SERVICE_FQDN_OPEN_WEBUI", MagicRef{Type: MagicFQDN, ID: "OPEN_WEBUI"}},
		{"SERVICE_FQDN_OPEN_WEBUI_8080", MagicRef{Type: MagicFQDN, ID: "OPEN_WEBUI", Port: 8080}},
		// A component whose ID ends in digits must win over the port reading.
		{"SERVICE_URL_APP2", MagicRef{Type: MagicURL, ID: "APP2"}},
	}
	for _, tc := range cases {
		got, finding := ParseMagicName(tc.name, comps)
		if finding != nil {
			t.Fatalf("ParseMagicName(%q): unexpected finding %v", tc.name, finding)
		}
		tc.want.Name = tc.name
		if got != tc.want {
			t.Fatalf("ParseMagicName(%q) = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestParseMagicNameErrors(t *testing.T) {
	comps := components("db")
	cases := map[string]string{
		"SERVICE_TOKEN_DB":       CodeMagicVariableInvalidType, // unknown type
		"SERVICE_PASSWORD_48_DB": CodeMagicVariableInvalidType, // invalid length
		"SERVICE_FQDN_UNKNOWN":   CodeMagicVariableUnknownComp,
		"SERVICE_URL_NOPE_8080":  CodeMagicVariableUnknownComp,
	}
	for name, code := range cases {
		_, finding := ParseMagicName(name, comps)
		if finding == nil || finding.Code != code {
			t.Fatalf("ParseMagicName(%q) = %v, want code %s", name, finding, code)
		}
	}
}

func TestScanMagicReferences(t *testing.T) {
	content := `
services:
  db:
    image: postgres:16
    environment:
      POSTGRES_USER: ${SERVICE_USER_DB}
      POSTGRES_PASSWORD: ${SERVICE_PASSWORD_DB}
  app:
    image: acme/app
    environment:
      DATABASE_PASSWORD: ${SERVICE_PASSWORD_DB}
      PUBLIC_URL: ${SERVICE_URL_APP}
`
	refs, findings := ScanMagicReferences(content, []string{"db", "app"})
	if len(findings) != 0 {
		t.Fatalf("unexpected findings: %v", findings)
	}
	// SERVICE_PASSWORD_DB referenced twice = ONE variable (§4.1).
	if len(refs) != 3 {
		t.Fatalf("expected 3 distinct references, got %d: %+v", len(refs), refs)
	}
	if refs[0].Name != "SERVICE_PASSWORD_DB" || refs[1].Name != "SERVICE_URL_APP" || refs[2].Name != "SERVICE_USER_DB" {
		t.Fatalf("wrong order or content: %+v", refs)
	}
}

func TestGenerateMagicValue(t *testing.T) {
	mustRef := func(name string) MagicRef {
		ref, finding := ParseMagicName(name, components("db"))
		if finding != nil {
			t.Fatalf("%s: %v", name, finding)
		}
		return ref
	}

	user, err := GenerateMagicValue(mustRef("SERVICE_USER_DB"))
	if err != nil || !regexp.MustCompile(`^[a-z][a-z0-9]{15}$`).MatchString(user) {
		t.Fatalf("user %q does not match the spec alphabet", user)
	}

	password, _ := GenerateMagicValue(mustRef("SERVICE_PASSWORD_DB"))
	if !regexp.MustCompile(`^[A-Za-z0-9]{32}$`).MatchString(password) {
		t.Fatalf("password %q does not match the spec alphabet", password)
	}

	long, _ := GenerateMagicValue(mustRef("SERVICE_PASSWORD_64_DB"))
	if len(long) != 64 {
		t.Fatalf("long password has length %d", len(long))
	}

	hexValue, _ := GenerateMagicValue(mustRef("SERVICE_HEX_64_DB"))
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(hexValue) {
		t.Fatalf("hex %q does not match", hexValue)
	}

	real64, _ := GenerateMagicValue(mustRef("SERVICE_REALBASE64_32_DB"))
	raw, err := base64.StdEncoding.DecodeString(real64)
	if err != nil || len(raw) != 32 {
		t.Fatalf("realbase64 %q must decode to 32 bytes (%d, %v)", real64, len(raw), err)
	}

	// Two draws must differ: the generator is a CSPRNG, not a constant.
	again, _ := GenerateMagicValue(mustRef("SERVICE_PASSWORD_DB"))
	if password == again {
		t.Fatalf("two generations produced the same value")
	}

	if _, err := GenerateMagicValue(mustRef("SERVICE_FQDN_DB")); err == nil ||
		!strings.Contains(err.Error(), "not generated") {
		t.Fatalf("FQDN must not be generated here: %v", err)
	}
}
