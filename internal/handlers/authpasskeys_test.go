package handlers

import (
	"strings"
	"testing"
)

// The passkey label is cosmetic but STORED and displayed: it must come out of
// this function bounded and non-empty, whatever the client sent.
func TestPasskeyName(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "passkey"},
		{"   ", "passkey"},
		{"YubiKey 5", "YubiKey 5"},
		{"  MacBook Touch ID  ", "MacBook Touch ID"},
		{strings.Repeat("x", 200), strings.Repeat("x", 64)},
	}
	for _, c := range cases {
		if got := passkeyName(c.in); got != c.want {
			t.Errorf("passkeyName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
