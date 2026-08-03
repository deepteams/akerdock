package jobs

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"gopkg.in/yaml.v3"

	"github.com/deepteams/akerdock/internal/accessroute"
	"github.com/deepteams/akerdock/internal/compose"
	"github.com/deepteams/akerdock/internal/store"
)

func previewFixture(t *testing.T) (store.GetApplicationByIDRow, store.Preview) {
	t.Helper()
	var appUUID, previewUUID pgtype.UUID
	if err := appUUID.Scan("11111111-2222-3333-4444-555555555555"); err != nil {
		t.Fatal(err)
	}
	if err := previewUUID.Scan("99999999-8888-7777-6666-555555555555"); err != nil {
		t.Fatal(err)
	}
	ports := "3000"
	fqdn := "12.shop.example.com"
	app := store.GetApplicationByIDRow{}
	app.Resource.Uuid = appUUID
	app.Resource.Name = "shop"
	app.RuntimeConfig.PortsExposes = &ports
	app.Application.PreviewProtection = store.PreviewProtectionBasicAuth
	preview := store.Preview{Uuid: previewUUID, PrID: 12, Fqdn: &fqdn}
	return app, preview
}

// The routing file of a preview must be VALID YAML with the middlewares in
// their own section: appended at the end, they would be parsed as services
// and Traefik would reject the whole file — the preview would never route.
func TestRenderPreviewRoutingFileIsValidYAML(t *testing.T) {
	app, preview := previewFixture(t)
	content, err := RenderPreviewRoutingFile(app, preview, 7, "", "preview:$2y$hash", "")
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		HTTP struct {
			Routers     map[string]map[string]any `yaml:"routers"`
			Middlewares map[string]map[string]any `yaml:"middlewares"`
			Services    map[string]map[string]any `yaml:"services"`
		} `yaml:"http"`
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("invalid YAML: %v\n%s", err, content)
	}

	previewUUID := "99999999-8888-7777-6666-555555555555"
	if _, ok := doc.HTTP.Middlewares[previewUUID+"-noindex"]; !ok {
		t.Fatalf("the noindex middleware is not in the middlewares section:\n%s", content)
	}
	if _, ok := doc.HTTP.Middlewares[previewUUID+"-access"]; !ok {
		t.Fatalf("the basic auth middleware is missing:\n%s", content)
	}
	// The middleware definitions must NOT have leaked into the services map.
	for name := range doc.HTTP.Services {
		if strings.Contains(name, "noindex") || strings.Contains(name, "-auth") {
			t.Fatalf("a middleware leaked into the services section: %s", name)
		}
	}
	// Every https router carries both middlewares.
	secure := 0
	for _, router := range doc.HTTP.Routers {
		entry, _ := router["entryPoints"].([]any)
		if len(entry) == 1 && entry[0] == "websecure" {
			secure++
			mws, _ := router["middlewares"].([]any)
			if len(mws) != 2 {
				t.Fatalf("the https router must carry noindex + auth, got %v", mws)
			}
		}
	}
	if secure == 0 {
		t.Fatalf("no https router generated:\n%s", content)
	}
}

func TestRenderPreviewRoutingFileHonorsPublicRoutes(t *testing.T) {
	app, preview := previewFixture(t)
	app.Application.AccessPublicRoutes = []byte(
		`[{"path":"/mcp","match":"exact","methods":["GET","POST"]}]`,
	)
	content, err := RenderPreviewRoutingFile(app, preview, 7, "", "preview:$2y$hash", "")
	if err != nil {
		t.Fatal(err)
	}

	var doc struct {
		HTTP struct {
			Routers map[string]struct {
				Rule        string   `yaml:"rule"`
				Middlewares []string `yaml:"middlewares"`
			} `yaml:"routers"`
		} `yaml:"http"`
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("invalid YAML: %v\n%s", err, content)
	}

	previewUUID := "99999999-8888-7777-6666-555555555555"
	protected := doc.HTTP.Routers[previewUUID+"-r0"]
	public := doc.HTTP.Routers[previewUUID+"-r0-public-0"]
	if !containsAll(protected.Middlewares, previewUUID+"-access", previewUUID+"-noindex") {
		t.Fatalf("protected route must retain access + noindex, got %v", protected.Middlewares)
	}
	if len(public.Middlewares) != 1 || public.Middlewares[0] != previewUUID+"-noindex" {
		t.Fatalf("public route must omit only access, got %v", public.Middlewares)
	}
	for _, want := range []string{"Path(`/mcp`)", "Method(`GET`) || Method(`POST`)"} {
		if !strings.Contains(public.Rule, want) {
			t.Fatalf("public route rule missing %q: %s", want, public.Rule)
		}
	}
}

func TestComposePreviewRouteGroupCarriesServicePublicRoutes(t *testing.T) {
	publicRoutes := []accessroute.Route{{
		Path: "/mcp", Match: accessroute.MatchExact, Methods: []string{"GET", "POST"},
	}}
	plan := &compose.Plan{Services: []compose.ServicePlan{{
		Name: "frontend", ContainerName: "preview-frontend",
		AccessPublicRoutes: publicRoutes,
	}}}
	rg := composePreviewRouteGroup("preview", map[string]previewComposeRoute{
		"frontend": {FQDN: "preview.example.com", Port: 8080},
	}, plan)

	if len(rg.Routes) != 1 || len(rg.Routes[0].PublicRoutes) != 1 {
		t.Fatalf("compose preview lost public routes: %#v", rg.Routes)
	}
	if got := rg.Routes[0].PublicRoutes[0].Path; got != "/mcp" {
		t.Fatalf("public route path = %q, want /mcp", got)
	}

	content := renderPreviewContent(rg, "preview", 7, store.PreviewProtectionSso, "",
		"https://akerdock.example.com/webhooks/previews/forward-auth",
		[]string{"preview.example.com"})
	var doc struct {
		HTTP struct {
			Routers map[string]struct {
				Middlewares []string `yaml:"middlewares"`
			} `yaml:"routers"`
		} `yaml:"http"`
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("invalid compose preview routing YAML: %v\n%s", err, content)
	}
	if !containsAll(doc.HTTP.Routers["preview-r0"].Middlewares, "preview-access", "preview-noindex") {
		t.Fatalf("protected compose preview route lost SSO or noindex: %v",
			doc.HTTP.Routers["preview-r0"].Middlewares)
	}
	if got := doc.HTTP.Routers["preview-r0-public-0"].Middlewares; len(got) != 1 || got[0] != "preview-noindex" {
		t.Fatalf("public compose preview route must omit only SSO, got %v", got)
	}
	if !strings.Contains(content,
		`address: "https://akerdock.example.com/webhooks/previews/forward-auth?preview=preview"`) {
		t.Fatalf("protected compose preview route has no SSO forwardAuth:\n%s", content)
	}
}

func containsAll(values []string, wanted ...string) bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	for _, value := range wanted {
		if !set[value] {
			return false
		}
	}
	return true
}

// Without basic auth, only noindex is attached — a public preview is an
// explicit choice, but it is never indexable.
func TestRenderPreviewRoutingFileWithoutAuth(t *testing.T) {
	app, preview := previewFixture(t)
	app.Application.PreviewProtection = store.PreviewProtectionNone
	content, err := RenderPreviewRoutingFile(app, preview, 7, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, "basicAuth") {
		t.Fatalf("no auth expected:\n%s", content)
	}
	if !strings.Contains(content, `X-Robots-Tag: "noindex, nofollow"`) {
		t.Fatalf("noindex is not optional:\n%s", content)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("invalid YAML: %v\n%s", err, content)
	}
}

// sso protection (ADR-030): the https routers must carry a forwardAuth
// middleware to the control plane — and never a basicAuth one.
func TestRenderPreviewRoutingFileSSO(t *testing.T) {
	app, preview := previewFixture(t)
	app.Application.PreviewProtection = store.PreviewProtectionSso
	content, err := RenderPreviewRoutingFile(app, preview, 7, "", "",
		"https://manager.example.com/webhooks/previews/forward-auth")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"forwardAuth:",
		"address: \"https://manager.example.com/webhooks/previews/forward-auth?preview=",
		"-auth",
		`X-Robots-Tag: "noindex, nofollow"`,
		// The cookie bootstrap router (ADR-030): the preview's own host, the
		// callback path, proxied server-side to the control plane.
		"-authcb:",
		"PathPrefix(`/.akerdock/preview-callback`)",
		"passHostHeader: false",
		`url: "https://manager.example.com"`,
	} {
		if !strings.Contains(content, want) {
			t.Errorf("sso routing missing %q\n%s", want, content)
		}
	}
	if strings.Contains(content, "basicAuth") {
		t.Fatal("sso mode must not emit basicAuth")
	}
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(content), &parsed); err != nil {
		t.Fatalf("generated routing is not valid YAML: %v\n%s", err, content)
	}
}
