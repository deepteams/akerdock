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

// TestGenerateIngressAttachRidesItsOwnH2CService pins ADR-063: the reserved
// attach router gets a service of its own, pointed at the same agent front but
// over h2c, while the visitor router keeps its plain HTTP/1.1 service — an h2c
// hop cannot carry a relayed WebSocket upgrade (RFC 7540 §8.1.2.2).
func TestGenerateIngressAttachRidesItsOwnH2CService(t *testing.T) {
	out := GenerateIngress(IngressGroup{UUID: "ep3", FQDN: "dev.example.com"}, 7)

	must := func(sub string) {
		t.Helper()
		if !strings.Contains(out, sub) {
			t.Fatalf("generated ingress file missing %q:\n%s", sub, out)
		}
	}
	// The attach router no longer borrows the visitor service.
	must("      rule: Host(`dev.example.com`) && Path(`/.akerdock/ingress`)\n" +
		"      priority: 2000000\n" +
		"      service: ep3-ingress-attach-0\n")
	// …and that service is the h2c one.
	must("    ep3-ingress-attach-0:\n      loadBalancer:\n        servers:\n" +
		"          - url: \"h2c://akerdock-agent:8080\"\n")
	// The visitor service stays HTTP/1.1: h2c there would break every
	// relayed WebSocket upgrade.
	must("    ep3-s0:\n      loadBalancer:\n        servers:\n" +
		"          - url: \"http://akerdock-agent:8080\"\n")
	if strings.Count(out, "h2c://") != 1 {
		t.Fatalf("exactly one h2c backend is expected — the attach one:\n%s", out)
	}
}

// TestGenerateDynamicHasNoH2CService checks the scope of ADR-063 from the other
// side: an ordinary application carries no attach router and no h2c backend.
func TestGenerateDynamicHasNoH2CService(t *testing.T) {
	out := GenerateDynamic(RouteGroup{
		AppUUID: "app-uuid",
		Routes:  []Route{{FQDN: "app.example.com", Path: "/", TargetPort: 3000}},
	}, 1)
	if strings.Contains(out, "h2c://") || strings.Contains(out, "ingress-attach") {
		t.Fatalf("h2c is reserved to the ingress attach path:\n%s", out)
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
