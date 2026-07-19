package auth

import (
	"errors"
	"strings"
	"testing"
)

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

func TestPermissionHierarchy(t *testing.T) {
	cases := []struct {
		granted  []string
		required Permission
		want     bool
	}{
		{[]string{"read"}, PermRead, true},
		{[]string{"read"}, PermWrite, false},
		{[]string{"read"}, PermReadSensitive, false},
		{[]string{"write"}, PermRead, true},
		{[]string{"write"}, PermWrite, true},
		{[]string{"write"}, PermRoot, false},
		{[]string{"read:sensitive"}, PermRead, true},
		{[]string{"read:sensitive"}, PermWrite, false},
		{[]string{"deploy"}, PermRead, true},
		{[]string{"deploy"}, PermDeploy, true},
		{[]string{"deploy"}, PermWrite, false},
		{[]string{"root"}, PermRead, true},
		{[]string{"root"}, PermWrite, true},
		{[]string{"root"}, PermRoot, true},
		{nil, PermRead, false},
	}
	for _, c := range cases {
		if got := Has(c.granted, c.required); got != c.want {
			t.Errorf("Has(%v, %s) = %v, want %v", c.granted, c.required, got, c.want)
		}
	}
}

func TestNewTokenShape(t *testing.T) {
	token, prefix, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "akd_") || len(token) != 4+48 {
		t.Fatalf("unexpected token shape: %q", token)
	}
	if prefix != token[:PrefixLen] || len(prefix) != 10 {
		t.Fatalf("unexpected prefix: %q", prefix)
	}
	if hash != HashToken(token) || len(hash) != 64 {
		t.Fatalf("unexpected hash: %q", hash)
	}
	token2, _, _, _ := NewToken()
	if token == token2 {
		t.Fatal("two tokens must differ")
	}
}

func TestSplitBearer(t *testing.T) {
	token, _, _, _ := NewToken()
	cases := map[string]string{
		"Bearer " + token:    token,
		"bearer " + token:    token, // scheme is case-insensitive
		"":                   "",
		"Bearer":             "",
		"Basic dXNlcjpwYXNz": "",
		"Bearer not-a-token": "",
		"Bearer akd_short":   "",
		"Token " + token:     "",
		"Bearer  " + token:   token, // tolerate extra space
	}
	for header, want := range cases {
		if got := SplitBearer(header); got != want {
			t.Errorf("SplitBearer(%q) = %q, want %q", header, got, want)
		}
	}
}

func TestIdentityTeamIsolation(t *testing.T) {
	member := &Identity{TeamID: 1, Permissions: []string{"write"}}
	root := &Identity{TeamID: 1, Permissions: []string{"root"}}
	if member.CanAccessTeam(2) {
		t.Fatal("non-root token must not access another team")
	}
	if !member.CanAccessTeam(1) || !root.CanAccessTeam(2) {
		t.Fatal("own team and root access must be allowed")
	}
}

func TestHashEqual(t *testing.T) {
	if !HashEqual("same", "same") || HashEqual("same", "different") {
		t.Fatal("HashEqual returned an incorrect result")
	}
}

func TestNewTokenEntropyFailure(t *testing.T) {
	old := randomReader
	randomReader = errorReader{}
	t.Cleanup(func() { randomReader = old })
	if _, _, _, err := NewToken(); err == nil || !strings.Contains(err.Error(), "token generation") {
		t.Fatalf("NewToken should report entropy failure, got %v", err)
	}
}
