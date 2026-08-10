package handlers

import (
	"net/http"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/docs"
	"github.com/deepteams/akerdock/internal/httpapi"
)

// GetManual implements GET /docs (ADR-072 §4): the in-app manual, filtered to
// what this caller may actually read.
//
// The filter runs here rather than in the browser. Before ADR-072 the manual
// was compiled into the bundle, so a reviewer downloaded the instance
// administration chapters and the dashboard merely declined to render them —
// §25.4's promise was true of the DOM and false of the wire.
func (a *API) GetManual(w http.ResponseWriter, r *http.Request, params api.GetManualParams) {
	id, ok := a.require(w, r, auth.PermRead)
	if !ok {
		return
	}
	if a.Manual == nil {
		// The manual is parsed at boot and the boot fails without it, so this
		// is only reachable from an API-only assembly that never loaded one.
		httpapi.WriteJSON(w, http.StatusOK, api.GetManual200JSONResponse{Topics: []api.ManualTopic{}})
		return
	}
	all := params.All != nil && *params.All
	httpapi.WriteJSON(w, http.StatusOK, api.GetManual200JSONResponse{Topics: manualFor(a.Manual, id, all)})
}

// manualFor applies the gates. Without `all`, what the caller cannot reach is
// absent; with it, the same content comes back marked `beyond_role` so the
// dashboard can show the whole manual and say what is out of reach — §25.4's
// opt-in, which exists because "why can I not do this?" is a question the
// manual should answer rather than dodge.
func manualFor(m *docs.Manual, id *auth.Identity, all bool) []api.ManualTopic {
	out := make([]api.ManualTopic, 0, len(m.Topics))
	for _, t := range m.Topics {
		beyond := !reachable(id, t.Permission, t.Root)
		if beyond && !all {
			continue
		}
		topic := api.ManualTopic{
			Id: t.ID, Title: t.Title, Group: t.Group, Summary: t.Summary,
			Icon: optional(t.Icon), Permission: optional(t.Permission),
			IntroHtml: optional(t.IntroHTML), IntroText: optional(t.IntroText),
			Root: optionalBool(t.Root), BeyondRole: optionalBool(beyond),
		}
		if len(t.Links) > 0 {
			links := make([]api.ManualLink, 0, len(t.Links))
			for _, l := range t.Links {
				links = append(links, api.ManualLink{Label: l.Label, Route: optional(l.Route), Href: optional(l.Href)})
			}
			topic.Links = &links
		}
		for _, s := range t.Sections {
			// A section's gate is on top of its topic's (ADR-072 §1), so a
			// topic already out of reach makes every section out of reach —
			// no section is ever more visible than the chapter holding it.
			sectionBeyond := beyond || !reachable(id, s.Permission, s.Root)
			if sectionBeyond && !all {
				continue
			}
			topic.Sections = append(topic.Sections, api.ManualSection{
				Id: s.ID, Title: s.Title, Html: s.HTML, Text: s.Text,
				Permission: optional(s.Permission), Root: optionalBool(s.Root),
				BeyondRole: optionalBool(sectionBeyond),
			})
		}
		out = append(out, topic)
	}
	return out
}

// reachable answers the one question the gates ask: does this identity hold
// what the content is gated on?
func reachable(id *auth.Identity, permission string, root bool) bool {
	if root {
		return id.IsRoot()
	}
	if permission == "" {
		return true
	}
	return auth.Has(id.Permissions, auth.Permission(permission))
}

// optional renders an empty string as an absent field: the schema marks these
// optional, and a reader of the JSON should not have to tell "" from unset.
func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// optionalBool does the same for the two flags that are only ever true.
func optionalBool(b bool) *bool {
	if !b {
		return nil
	}
	return &b
}
