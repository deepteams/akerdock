package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// lsServer serves paginated lists: applications spread over two pages to prove
// listAll follows the cursor, databases and services in one page each.
func lsServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/applications":
			if r.URL.Query().Get("cursor") == "" {
				next := "page2"
				_ = json.NewEncoder(w).Encode(resourcePage{
					Data:       []resource{{Uuid: "app-1", Name: "varuna", SourceType: "compose"}},
					NextCursor: &next,
				})
				return
			}
			empty := ""
			_ = json.NewEncoder(w).Encode(resourcePage{
				Data:       []resource{{Uuid: "app-2", Name: "helios", SourceType: "dockerfile"}},
				NextCursor: &empty,
			})
		case "/api/v1/databases":
			_ = json.NewEncoder(w).Encode(resourcePage{
				Data: []resource{{Uuid: "db-1", Name: "pg", Engine: "postgres", DesiredStatus: "running"}},
			})
		case "/api/v1/services":
			_ = json.NewEncoder(w).Encode(resourcePage{Data: nil})
		default:
			w.WriteHeader(500)
			_, _ = fmt.Fprint(w, `{"code":"boom","message":"unexpected path`+r.URL.Path+`"}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLsDefaultKinds(t *testing.T) {
	srv := lsServer(t)
	setupContext(t, srv.URL)
	out, _ := captureOutput(t, func() {
		if err := runCmd(lsCmd()); err != nil {
			t.Errorf("ls: %v", err)
		}
	})
	// Both application pages, the database, and the header.
	for _, want := range []string{"varuna", "helios", "pg", "postgres", "KIND"} {
		if !strings.Contains(out, want) {
			t.Errorf("output misses %q: %q", want, out)
		}
	}
}

func TestLsSingleKindJSON(t *testing.T) {
	srv := lsServer(t)
	setupContext(t, srv.URL)
	flags.output = "json"
	out, _ := captureOutput(t, func() {
		if err := runCmd(lsCmd(), "databases"); err != nil {
			t.Errorf("ls databases: %v", err)
		}
	})
	if !strings.Contains(out, `"kind": "databases"`) || strings.Contains(out, "varuna") {
		t.Fatalf("json output = %q", out)
	}
}

func TestLsUnknownKind(t *testing.T) {
	srv := lsServer(t)
	setupContext(t, srv.URL)
	// `previews` is declared but has no transversal list endpoint.
	if err := runCmd(lsCmd(), "previews"); err == nil || !strings.Contains(err.Error(), `cannot list "previews"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestLsListError(t *testing.T) {
	srv := lsServer(t)
	setupContext(t, srv.URL)
	if err := runCmd(lsCmd(), "servers"); err == nil {
		t.Fatal("expected the API error to surface")
	}
}

func TestLsWithoutClient(t *testing.T) {
	setupHome(t)
	if err := runCmd(lsCmd()); err == nil || !strings.Contains(err.Error(), "no context selected") {
		t.Fatalf("err = %v", err)
	}
}
