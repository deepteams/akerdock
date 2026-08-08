package handlers

import (
	"testing"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/store"
)

func TestValidIngressFQDN(t *testing.T) {
	good := []string{
		"dev-kedric.apps.example.com",
		"hook.example.com",
		"a.b.c.example.org",
	}
	for _, f := range good {
		if !validIngressFQDN(f) {
			t.Errorf("expected %q to be valid", f)
		}
	}
	bad := []string{
		"",                      // empty
		"nodot",                 // not a FQDN
		"*.example.com",         // wildcard
		"https://x.example.com", // scheme
		"x.example.com:8080",    // port
		"x.example.com/path",    // path
		"has space.example.com", // space
	}
	for _, f := range bad {
		if validIngressFQDN(f) {
			t.Errorf("expected %q to be rejected", f)
		}
	}
}

func TestIngressAccessOrDefaultIsSSO(t *testing.T) {
	// The default — a fresh endpoint is walled by SSO (ADR-060 §5).
	if got := ingressAccessOrDefault(nil); got != store.IngressAccessSso {
		t.Fatalf("nil access should default to sso, got %q", got)
	}
	none := api.IngressEndpointCreateAccessNone
	if got := ingressAccessOrDefault(&none); got != store.IngressAccessNone {
		t.Fatalf("explicit none should resolve to none, got %q", got)
	}
	basic := api.IngressEndpointCreateAccessBasicAuth
	if got := ingressAccessOrDefault(&basic); got != store.IngressAccessBasicAuth {
		t.Fatalf("explicit basic_auth should resolve, got %q", got)
	}
}

func TestIngressTokenPrefix(t *testing.T) {
	tok, err := newIngressToken()
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) < 5 || tok[:5] != "akdi_" {
		t.Fatalf("ingress token must carry the akdi_ prefix, got %q", tok)
	}
	// Hash is deterministic and not the token itself.
	if h := hashIngressToken(tok); h == tok || len(h) != 64 {
		t.Fatalf("unexpected hash %q", h)
	}
}
