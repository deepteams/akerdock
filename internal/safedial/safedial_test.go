package safedial

import (
	"errors"
	"net"
	"testing"
)

func TestBlocked(t *testing.T) {
	cases := map[string]bool{
		// Blocked: loopback, link-local (metadata), private, CGNAT, unspecified.
		"127.0.0.1":       true,
		"::1":             true,
		"169.254.169.254": true, // cloud metadata
		"169.254.0.1":     true,
		"10.0.0.5":        true,
		"172.16.9.9":      true,
		"192.168.1.1":     true,
		"100.64.0.1":      true, // CGNAT
		"0.0.0.0":         true,
		"fc00::1":         true, // IPv6 ULA
		"fe80::1":         true, // IPv6 link-local
		"224.0.0.1":       true, // multicast
		// Allowed: public unicast.
		"8.8.8.8":              false,
		"1.1.1.1":              false,
		"2606:4700:4700::1111": false,
	}
	for ipStr, want := range cases {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			t.Fatalf("bad test IP %q", ipStr)
		}
		if got := Blocked(ip); got != want {
			t.Errorf("Blocked(%s) = %v, want %v", ipStr, got, want)
		}
	}
	// A nil IP is blocked (fail closed).
	if !Blocked(nil) {
		t.Error("Blocked(nil) = false, want true")
	}
}

func TestControl(t *testing.T) {
	if err := control("tcp", "169.254.169.254:80", nil); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("control(metadata) = %v, want ErrBlockedAddress", err)
	}
	if err := control("tcp", "10.1.2.3:5432", nil); !errors.Is(err, ErrBlockedAddress) {
		t.Errorf("control(private) = %v, want ErrBlockedAddress", err)
	}
	if err := control("tcp", "8.8.8.8:443", nil); err != nil {
		t.Errorf("control(public) = %v, want nil", err)
	}
}
