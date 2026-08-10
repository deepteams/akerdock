package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/docs"
)

// manualFixture is a two-chapter manual: one open, one gated, with a gated
// section inside the open one. Enough to exercise every branch of the filter
// without depending on what the shipped corpus happens to say today.
func manualFixture() *docs.Manual {
	return &docs.Manual{Topics: []docs.Topic{
		{
			ID: "logs", Title: "Logs", Group: "Run and debug", Summary: "Reading logs.",
			IntroHTML: "<p>Intro</p>", IntroText: "Intro",
			Sections: []docs.Section{
				{ID: "reading", Title: "Reading", HTML: "<p>open</p>", Text: "open"},
				{ID: "draining", Title: "Draining", Permission: "applications:update", HTML: "<p>gated</p>", Text: "gated"},
			},
		},
		{
			ID: "instance", Title: "Instance settings", Group: "Instance administration",
			Summary: "Root only.", Root: true,
			Sections: []docs.Section{{ID: "encryption", Title: "Encryption", HTML: "<p>root</p>", Text: "root"}},
		},
	}}
}

func manualRequest(t *testing.T, id *auth.Identity, all bool) []api.ManualTopic {
	t.Helper()
	a := &API{Manual: manualFixture()}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/docs", nil)
	req = req.WithContext(auth.WithIdentity(req.Context(), id))
	rec := httptest.NewRecorder()

	params := api.GetManualParams{}
	if all {
		params.All = &all
	}
	a.GetManual(rec, req, params)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Topics []api.ManualTopic `json:"topics"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	return body.Topics
}

func topicIDs(topics []api.ManualTopic) []string {
	out := make([]string, 0, len(topics))
	for _, t := range topics {
		out = append(out, t.Id)
	}
	return out
}

// The point of ADR-072 §4: what the reader cannot reach never leaves the
// server. Before it, the whole manual sat in everyone's bundle.
func TestManualIsFilteredServerSide(t *testing.T) {
	reader := &auth.Identity{Permissions: []string{string(auth.PermRead)}}
	topics := manualRequest(t, reader, false)

	if got := topicIDs(topics); len(got) != 1 || got[0] != "logs" {
		t.Fatalf("topics = %v, want the open chapter only", got)
	}
	// A gated section is gone with the same silence as a gated chapter.
	if len(topics[0].Sections) != 1 || topics[0].Sections[0].Id != "reading" {
		t.Fatalf("sections = %+v, want the ungated one only", topics[0].Sections)
	}
	if topics[0].Sections[0].BeyondRole != nil {
		t.Error("a section the reader can read must not be marked beyond_role")
	}
}

// The opt-in of §25.4: everything, with what is out of reach marked rather than
// silently mixed in — "why can I not do this?" is a question the manual answers.
func TestManualAllMarksWhatIsBeyondTheRole(t *testing.T) {
	reader := &auth.Identity{Permissions: []string{string(auth.PermRead)}}
	topics := manualRequest(t, reader, true)

	if got := topicIDs(topics); len(got) != 2 {
		t.Fatalf("topics = %v, want both chapters", got)
	}
	byID := map[string]api.ManualTopic{}
	for _, tp := range topics {
		byID[tp.Id] = tp
	}
	if beyond := byID["instance"].BeyondRole; beyond == nil || !*beyond {
		t.Error("the root-only chapter must come back marked")
	}
	if byID["logs"].BeyondRole != nil {
		t.Error("a chapter the reader can read must not be marked")
	}
	sections := byID["logs"].Sections
	if len(sections) != 2 {
		t.Fatalf("sections = %d, want both", len(sections))
	}
	if beyond := sections[1].BeyondRole; beyond == nil || !*beyond {
		t.Error("the gated section must come back marked, not hidden")
	}
}

// A section is never more visible than the chapter holding it (ADR-072 §1).
func TestManualSectionsInheritTheirTopicsGate(t *testing.T) {
	reader := &auth.Identity{Permissions: []string{string(auth.PermRead)}}
	for _, topic := range manualRequest(t, reader, true) {
		if topic.BeyondRole == nil {
			continue
		}
		for _, s := range topic.Sections {
			if s.BeyondRole == nil || !*s.BeyondRole {
				t.Errorf("%s#%s is reachable inside an unreachable chapter", topic.Id, s.Id)
			}
		}
	}
}

func TestManualForRoot(t *testing.T) {
	root := &auth.Identity{Permissions: []string{string(auth.PermRoot)}}
	topics := manualRequest(t, root, false)
	if len(topics) != 2 {
		t.Fatalf("root sees %d chapters, want both", len(topics))
	}
	for _, tp := range topics {
		if tp.BeyondRole != nil {
			t.Errorf("%s: nothing is beyond root", tp.Id)
		}
	}
}

// An unauthenticated caller gets the same 401 as anywhere else: the manual is
// not a secret, but it is not public either.
func TestManualRequiresAuthentication(t *testing.T) {
	a := &API{Manual: manualFixture()}
	rec := httptest.NewRecorder()
	a.GetManual(rec, httptest.NewRequest(http.MethodGet, "/api/v1/docs", nil), api.GetManualParams{})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// An assembly with no manual answers an empty one rather than failing: a
// missing manual is not a reason to break the dashboard.
func TestManualAbsent(t *testing.T) {
	a := &API{}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/docs", nil)
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{Permissions: []string{string(auth.PermRead)}}))
	rec := httptest.NewRecorder()
	a.GetManual(rec, req, api.GetManualParams{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `"topics":[]`) {
		t.Fatalf("body = %s", body)
	}
}
