package jobs

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/deepteams/akerdock/internal/store"
)

// previewRouteTemplate is one configured preview route (ADR-035): a host
// pattern and an optional target port.
type previewRouteTemplate struct {
	Host string `json:"host"`
	Port *int   `json:"port,omitempty"`
}

// previewTemplates returns the application's preview route table, falling back
// to the legacy single preview_url_template as one implicit row so existing
// applications keep their behaviour without a data migration (ADR-035).
func previewTemplates(app store.GetApplicationByIDRow) []previewRouteTemplate {
	if raw := app.Application.PreviewUrlTemplates; len(raw) > 0 {
		var rows []previewRouteTemplate
		if err := json.Unmarshal(raw, &rows); err == nil {
			kept := make([]previewRouteTemplate, 0, len(rows))
			for _, r := range rows {
				if strings.TrimSpace(r.Host) != "" {
					kept = append(kept, r)
				}
			}
			if len(kept) > 0 {
				return kept
			}
		}
	}
	if t := strings.TrimSpace(app.Application.PreviewUrlTemplate); t != "" {
		return []previewRouteTemplate{{Host: t}}
	}
	return nil
}

// resolvePreviewHost substitutes the placeholders of one host pattern. service
// is normalised (underscores → dashes) for a valid DNS label.
func resolvePreviewHost(host string, prID int, domain, service, random string) string {
	return strings.ToLower(strings.NewReplacer(
		"{{pr_id}}", fmt.Sprint(prID),
		"{{domain}}", domain,
		"{{service}}", strings.ReplaceAll(service, "_", "-"),
		"{{random}}", random,
	).Replace(host))
}

// hostHasService reports whether a template expands per served service.
func hostHasService(host string) bool { return strings.Contains(host, "{{service}}") }

// serviceTemplate returns the first per-service template row ({{service}}), or
// nil. It is the catch-all applied to every served service.
func serviceTemplate(templates []previewRouteTemplate) *previewRouteTemplate {
	for i := range templates {
		if hostHasService(templates[i].Host) {
			return &templates[i]
		}
	}
	return nil
}

// explicitTemplates returns the rows WITHOUT {{service}} — each a single route
// whose target service is resolved by its port (like an application domain).
func explicitTemplates(templates []previewRouteTemplate) []previewRouteTemplate {
	out := make([]previewRouteTemplate, 0, len(templates))
	for _, t := range templates {
		if !hostHasService(t.Host) {
			out = append(out, t)
		}
	}
	return out
}
