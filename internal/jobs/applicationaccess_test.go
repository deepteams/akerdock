package jobs

import (
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/store"
)

// A minimal routing file shaped like the generator's output: one https
// router, a middlewares section and a services section.
const accessRoutingFixture = `http:
  routers:
    app-web:
      rule: Host(` + "`app.example.com`" + `)
      entryPoints: [websecure]
      service: app-svc
  middlewares:
    https-redirect:
      redirectScheme:
        scheme: https
  services:
    app-svc:
      loadBalancer:
        servers:
          - url: http://app:8080
`

func TestInjectApplicationMiddlewaresNoneLeavesContentUntouched(t *testing.T) {
	// A public application (the default) must come out byte-identical: the
	// wall is opt-in, and a no-op that rewrites the file would churn every
	// deploy's routing revision.
	for _, protection := range []store.PreviewProtection{
		store.PreviewProtectionNone, store.PreviewProtectionSignedLink,
	} {
		got := injectApplicationMiddlewares(accessRoutingFixture, "app-1", protection, "u:hash", "https://panel/fa")
		if got != accessRoutingFixture {
			t.Fatalf("protection %q rewrote the routing file:\n%s", protection, got)
		}
	}
	// Selected but unusable (no credential / no url): still untouched — a
	// half-applied wall would serve publicly while claiming to protect.
	if got := injectApplicationMiddlewares(accessRoutingFixture, "app-1", store.PreviewProtectionBasicAuth, "", ""); got != accessRoutingFixture {
		t.Fatal("basic_auth without credentials must not inject anything")
	}
	if got := injectApplicationMiddlewares(accessRoutingFixture, "app-1", store.PreviewProtectionSso, "", ""); got != accessRoutingFixture {
		t.Fatal("sso without a forward-auth url must not inject anything")
	}
}

func TestInjectApplicationMiddlewaresBasicAuth(t *testing.T) {
	got := injectApplicationMiddlewares(accessRoutingFixture, "app-1",
		store.PreviewProtectionBasicAuth, "akerdock:$2y$10$hash", "")

	if !strings.Contains(got, "middlewares: [app-1-access]") {
		t.Fatalf("the https router is not behind the wall:\n%s", got)
	}
	if !strings.Contains(got, "basicAuth:") || !strings.Contains(got, `"akerdock:$2y$10$hash"`) {
		t.Fatalf("basicAuth definition missing:\n%s", got)
	}
	// The definition MUST precede the services section, or Traefik parses it
	// as a service and rejects the whole file.
	if strings.Index(got, "app-1-access:") > strings.Index(got, "  services:") {
		t.Fatalf("middleware defined after the services section:\n%s", got)
	}
}

func TestInjectApplicationMiddlewaresSSOCarriesIdentityInAddress(t *testing.T) {
	got := injectApplicationMiddlewares(accessRoutingFixture, "app-1",
		store.PreviewProtectionSso, "", "https://panel.example.com/webhooks/applications/forward-auth")

	if !strings.Contains(got, "forwardAuth:") {
		t.Fatalf("forwardAuth definition missing:\n%s", got)
	}
	// The identity travels in the ADDRESS: intermediate proxies rewrite
	// X-Forwarded-Host, a query parameter survives every hop (ADR-030).
	if !strings.Contains(got, "?application=app-1") {
		t.Fatalf("application identity missing from the forward-auth address:\n%s", got)
	}
	if !strings.Contains(got, "middlewares: [app-1-access]") {
		t.Fatalf("the https router is not behind the wall:\n%s", got)
	}
}
