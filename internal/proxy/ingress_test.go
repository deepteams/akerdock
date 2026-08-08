package proxy

import (
	"strings"
	"testing"
)

// TestGenerateIngressWallAndNoindex checks the pre-provisioned ingress router
// (ADR-060 §1/§5): it targets the agent front, is HTTPS-forced and noindexed
// unconditionally, and when sso-walled carries the access middleware AND a
// wall-free attach router on the same host.
func TestGenerateIngressSSO(t *testing.T) {
	out := GenerateIngress(IngressGroup{
		UUID: "endpoint-uuid",
		FQDN: "dev-kedric.apps.example.com",
		Access: &AccessPolicy{
			Mode:           "sso",
			ForwardAuthURL: "https://panel/webhooks/ingress/forward-auth?endpoint=endpoint-uuid",
			CallbackURL:    "https://panel",
			CallbackPath:   "/.akerdock/ingress-callback",
		},
	}, 1)

	must := func(sub string) {
		t.Helper()
		if !strings.Contains(out, sub) {
			t.Fatalf("generated ingress file missing %q:\n%s", sub, out)
		}
	}
	// Targets the agent front, not a container.
	must("http://akerdock-agent:8080")
	// noindex and force-HTTPS are unconditional.
	must("X-Robots-Tag: \"noindex, nofollow\"")
	must("redirectScheme")
	// The wall.
	must("forwardAuth")
	must("/webhooks/ingress/forward-auth?endpoint=endpoint-uuid")
	// The reserved attach router on the same host.
	must("/.akerdock/ingress")
	// The ingress-specific callback path (never the app one).
	must("/.akerdock/ingress-callback")
	if strings.Contains(out, "/.akerdock/app-callback") {
		t.Fatalf("ingress file must not use the application callback path:\n%s", out)
	}
}

// TestGenerateIngressNoneStillGuards checks that an access:none endpoint still
// forces HTTPS and noindex, and exposes the attach router, but carries NO
// access middleware (ADR-060 §5).
func TestGenerateIngressNone(t *testing.T) {
	out := GenerateIngress(IngressGroup{
		UUID: "ep2", FQDN: "hook.example.com",
	}, 1)
	if strings.Contains(out, "forwardAuth") || strings.Contains(out, "basicAuth") {
		t.Fatalf("access:none must carry no wall middleware:\n%s", out)
	}
	if !strings.Contains(out, "X-Robots-Tag") || !strings.Contains(out, "redirectScheme") {
		t.Fatalf("noindex and force-HTTPS stay unconditional:\n%s", out)
	}
	if !strings.Contains(out, "/.akerdock/ingress") {
		t.Fatalf("attach router must exist even without a wall:\n%s", out)
	}
}
