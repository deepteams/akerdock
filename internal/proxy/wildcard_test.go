package proxy

import (
	"strings"
	"testing"
)

func TestUnderWildcard(t *testing.T) {
	tests := []struct {
		name   string
		fqdn   string
		domain string
		want   bool
	}{
		{
			name:   "single label under the wildcard",
			fqdn:   "app.example.com",
			domain: "example.com",
			want:   true,
		},
		{
			// Documented trap: a wildcard certificate covers exactly one
			// level, so *.example.com does NOT cover a.b.example.com.
			name:   "two labels are not covered",
			fqdn:   "a.b.example.com",
			domain: "example.com",
			want:   false,
		},
		{
			name:   "empty domain matches nothing",
			fqdn:   "app.example.com",
			domain: "",
			want:   false,
		},
		{
			name:   "apex itself is not under the wildcard",
			fqdn:   "example.com",
			domain: "example.com",
			want:   false,
		},
		{
			// Suffix match without the dot separator must not count:
			// notexample.com is a different domain entirely.
			name:   "suffix without dot boundary",
			fqdn:   "notexample.com",
			domain: "example.com",
			want:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := underWildcard(tt.fqdn, tt.domain); got != tt.want {
				t.Errorf("underWildcard(%q, %q) = %v, want %v", tt.fqdn, tt.domain, got, tt.want)
			}
		})
	}
}

// A wildcard_domain WITHOUT a DNS provider is a naming template, not a
// wildcard certificate (§7.2): every host under it must fall back to its own
// per-router HTTP-01 certificate, and no tls.domains block may ask the CA for
// a wildcard it cannot issue that way.
func TestWildcardWithoutDNSProviderFallsBackToHTTP01(t *testing.T) {
	out := GenerateDynamic(RouteGroup{
		AppUUID:        "9f3c2a1e",
		WildcardDomain: "ad.kedric.fr",
		DNSProvider:    "",
		Routes: []Route{
			{FQDN: "app.ad.kedric.fr", Path: "/", TargetPort: 3000},
		},
	}, 1)

	if !strings.Contains(out, "certResolver: http01") {
		t.Fatalf("expected the per-host http01 resolver, got:\n%s", out)
	}
	for _, forbidden := range []string{"domains:", "sans:", "dns01"} {
		if strings.Contains(out, forbidden) {
			t.Fatalf("a wildcard certificate must not be requested without DNS-01 (%q found):\n%s", forbidden, out)
		}
	}
}
