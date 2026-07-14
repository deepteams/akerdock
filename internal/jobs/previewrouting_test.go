package jobs

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/store"
	"gopkg.in/yaml.v3"
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
	content, err := RenderPreviewRoutingFile(app, preview, 7, "", "preview:$2y$hash")
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
	if _, ok := doc.HTTP.Middlewares[previewUUID+"-auth"]; !ok {
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

// Without basic auth, only noindex is attached — a public preview is an
// explicit choice, but it is never indexable.
func TestRenderPreviewRoutingFileWithoutAuth(t *testing.T) {
	app, preview := previewFixture(t)
	app.Application.PreviewProtection = store.PreviewProtectionNone
	content, err := RenderPreviewRoutingFile(app, preview, 7, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, "basicAuth") {
		t.Fatalf("no auth expected:\n%s", content)
	}
	if !strings.Contains(content, "X-Robots-Tag: noindex") {
		t.Fatalf("noindex is not optional:\n%s", content)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		t.Fatalf("invalid YAML: %v\n%s", err, content)
	}
}
