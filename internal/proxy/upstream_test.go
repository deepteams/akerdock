package proxy

import (
	"strings"
	"testing"
)

func useUpstreamTLS(t *testing.T) {
	t.Helper()
	SetUpstreamTLS(true)
	t.Cleanup(func() { SetUpstreamTLS(false) })
}

func TestUpstreamTLSDynamicHasNoTLSRouters(t *testing.T) {
	useUpstreamTLS(t)
	out := GenerateDynamic(RouteGroup{
		AppUUID:    "app-1",
		ForceHTTPS: true,
		Routes:     []Route{{FQDN: "app.example.com", Path: "/", TargetPort: 3000}},
	}, 7)

	for _, banned := range []string{"websecure", "tls:", "certResolver", "https-redirect", "redirectScheme"} {
		if strings.Contains(out, banned) {
			t.Errorf("upstream config must not contain %q\n%s", banned, out)
		}
	}
	if !strings.Contains(out, "entryPoints: [web]") {
		t.Errorf("upstream router must listen on web\n%s", out)
	}
}

func TestUpstreamTLSNoindexAppliesOnWeb(t *testing.T) {
	useUpstreamTLS(t)
	out := GenerateDynamic(RouteGroup{
		AppUUID: "app-1",
		Noindex: true,
		Routes:  []Route{{FQDN: "app.example.com", Path: "/", TargetPort: 3000}},
	}, 7)

	if !strings.Contains(out, "middlewares: [app-1-noindex]") {
		t.Errorf("noindex must be attached to the web router\n%s", out)
	}
}

func TestUpstreamTLSStaticHasNoACME(t *testing.T) {
	useUpstreamTLS(t)
	out := GenerateStatic(80, 443, "ops@example.com", "cloudflare", nil, 3)

	for _, banned := range []string{"certificatesResolvers", "acme:", "storage: /acme/acme.json"} {
		if strings.Contains(out, banned) {
			t.Errorf("upstream static config must not contain %q\n%s", banned, out)
		}
	}
}

func TestDefaultTLSStaticKeepsACME(t *testing.T) {
	SetUpstreamTLS(false)
	out := GenerateStatic(80, 443, "ops@example.com", "", nil, 3)

	if !strings.Contains(out, "certificatesResolvers:") {
		t.Errorf("default mode must keep the ACME resolver\n%s", out)
	}
}
