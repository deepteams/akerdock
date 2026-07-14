package proxy

import "testing"

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
