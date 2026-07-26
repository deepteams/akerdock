package jobs

import (
	"testing"

	"github.com/deepteams/akerdock/internal/store"
)

func TestResolvePreviewHost(t *testing.T) {
	got := resolvePreviewHost("{{service}}-pr{{pr_id}}.{{domain}}", 8, "ad.kedric.fr", "api_gw", "")
	if got != "api-gw-pr8.ad.kedric.fr" {
		t.Fatalf("got %q", got)
	}
	// {{random}} substituted, output lowercased.
	got = resolvePreviewHost("VARUNA-pr{{pr_id}}-{{random}}.ad.kedric.fr", 3, "", "", "abc")
	if got != "varuna-pr3-abc.ad.kedric.fr" {
		t.Fatalf("got %q", got)
	}
}

func TestPreviewTemplatesFallback(t *testing.T) {
	// No table → the legacy single template becomes one implicit row.
	app := store.GetApplicationByIDRow{}
	app.Application.PreviewUrlTemplate = "varuna-pr{{pr_id}}.ad.kedric.fr"
	rows := previewTemplates(app)
	if len(rows) != 1 || rows[0].Host != "varuna-pr{{pr_id}}.ad.kedric.fr" || rows[0].Port != nil {
		t.Fatalf("fallback rows = %+v", rows)
	}

	// A JSON table wins and blank hosts are dropped.
	app.Application.PreviewUrlTemplates = []byte(
		`[{"host":"varuna-pr{{pr_id}}.ad.kedric.fr","port":1337},{"host":"  "},{"host":"api-pr{{pr_id}}.ad.kedric.fr","port":8080}]`)
	rows = previewTemplates(app)
	if len(rows) != 2 {
		t.Fatalf("expected 2 rows, got %d (%+v)", len(rows), rows)
	}
	if rows[0].Port == nil || *rows[0].Port != 1337 || rows[1].Host != "api-pr{{pr_id}}.ad.kedric.fr" {
		t.Fatalf("rows = %+v", rows)
	}

	// Empty JSON array falls back to the legacy single template.
	app.Application.PreviewUrlTemplates = []byte(`[]`)
	if rows := previewTemplates(app); len(rows) != 1 || rows[0].Host != "varuna-pr{{pr_id}}.ad.kedric.fr" {
		t.Fatalf("empty-array fallback = %+v", rows)
	}
}

func TestServiceAndExplicitTemplates(t *testing.T) {
	p := func(n int) *int { return &n }
	templates := []previewRouteTemplate{
		{Host: "{{service}}-pr{{pr_id}}.ad.kedric.fr"},
		{Host: "varuna-pr{{pr_id}}.ad.kedric.fr", Port: p(1337)},
		{Host: "api-pr{{pr_id}}.ad.kedric.fr", Port: p(8080)},
	}
	st := serviceTemplate(templates)
	if st == nil || st.Host != "{{service}}-pr{{pr_id}}.ad.kedric.fr" {
		t.Fatalf("serviceTemplate = %+v", st)
	}
	ex := explicitTemplates(templates)
	if len(ex) != 2 || ex[0].Host != "varuna-pr{{pr_id}}.ad.kedric.fr" || *ex[1].Port != 8080 {
		t.Fatalf("explicitTemplates = %+v", ex)
	}
}
