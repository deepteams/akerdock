package cli

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestParseRef(t *testing.T) {
	cases := map[string]struct {
		kind, name string
		ok         bool
	}{
		"app/varuna": {"apps", "varuna", true},
		"db/pg":      {"databases", "pg", true},
		"svc/stack":  {"services", "stack", true},
		"preview/12": {"previews", "12", true},
		"database/x": {"databases", "x", true},
		"nope/x":     {"", "", false},
		"varuna":     {"", "", false},
		"app/":       {"", "", false},
	}
	for in, want := range cases {
		r, err := parseRef(in)
		if want.ok != (err == nil) {
			t.Errorf("parseRef(%q) ok=%v, want %v (err=%v)", in, err == nil, want.ok, err)
			continue
		}
		if want.ok && (r.kind != want.kind || r.name != want.name) {
			t.Errorf("parseRef(%q) = %+v, want kind=%s name=%s", in, r, want.kind, want.name)
		}
	}
}

func TestParsePorts(t *testing.T) {
	cases := map[string]struct {
		local, remote int
		ok            bool
	}{
		"15432:5432": {15432, 5432, true},
		"6379":       {6379, 6379, true},
		"x:5432":     {0, 0, false},
		"5432:y":     {0, 0, false},
	}
	for in, want := range cases {
		l, rem, err := parsePorts(in)
		if want.ok != (err == nil) {
			t.Errorf("parsePorts(%q) ok=%v, want %v", in, err == nil, want.ok)
			continue
		}
		if want.ok && (l != want.local || rem != want.remote) {
			t.Errorf("parsePorts(%q) = %d:%d, want %d:%d", in, l, rem, want.local, want.remote)
		}
	}
}

// `port-forward` takes two optional positional arguments, so telling them apart
// by POSITION is wrong: `port-forward endpoint/replica` used to be read as a
// ports argument and complained about a missing default application. A REF
// always contains a slash; a ports argument never does.
func TestSplitForwardArgs(t *testing.T) {
	cases := []struct {
		args       []string
		ref, ports string
	}{
		{[]string{"db/pg", "15432:5432"}, "db/pg", "15432:5432"},
		{[]string{"15432:5432"}, "", "15432:5432"},
		{[]string{"5432"}, "", "5432"},
		{[]string{"endpoint/prod-replica"}, "endpoint/prod-replica", ""},
		{[]string{"app/varuna"}, "app/varuna", ""},
		{nil, "", ""},
	}
	for _, tc := range cases {
		ref, ports := splitForwardArgs(tc.args)
		if ref != tc.ref || ports != tc.ports {
			t.Errorf("splitForwardArgs(%q) = (%q, %q), want (%q, %q)", tc.args, ref, ports, tc.ref, tc.ports)
		}
	}
}

// pkcePair must produce challenge = base64url(sha256(verifier)) — the exact
// relation the server verifies (ADR-031).
func TestPKCEPair(t *testing.T) {
	verifier, challenge, err := pkcePair()
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(verifier))
	if got := base64.RawURLEncoding.EncodeToString(sum[:]); got != challenge {
		t.Fatalf("challenge %q != base64url(sha256(verifier)) %q", challenge, got)
	}
	// Distinct calls must not repeat.
	v2, _, _ := pkcePair()
	if v2 == verifier {
		t.Fatal("verifier must be random per call")
	}
}

// Config and credentials round-trip with strict separation: the token lives
// only in credentials.yaml, never in config.yaml.
func TestConfigRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cfg := &Config{Contexts: map[string]Context{}}
	cfg.Contexts["prod"] = Context{URL: "https://m.example.com", Fqdn: "m.example.com", TeamUUID: "team-1"}
	cfg.CurrentContext = "prod"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if err := setToken("prod", "akd_secret"); err != nil {
		t.Fatal(err)
	}

	got, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentContext != "prod" || got.Contexts["prod"].URL != "https://m.example.com" {
		t.Fatalf("config did not round-trip: %+v", got)
	}
	if got.resolveContextName("") != "prod" {
		t.Fatalf("resolveContextName = %q", got.resolveContextName(""))
	}
	creds, err := loadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if creds.Tokens["prod"] != "akd_secret" {
		t.Fatalf("token did not round-trip: %+v", creds.Tokens)
	}
}
