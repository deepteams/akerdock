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
			w.WriteHeader(http.StatusInternalServerError)
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
		if err := runCmd(listCmd()); err != nil {
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
		if err := runCmd(listCmd(), "databases"); err != nil {
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
	if err := runCmd(listCmd(), "previews"); err == nil || !strings.Contains(err.Error(), `cannot list "previews"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestLsListError(t *testing.T) {
	srv := lsServer(t)
	setupContext(t, srv.URL)
	if err := runCmd(listCmd(), "servers"); err == nil {
		t.Fatal("expected the API error to surface")
	}
}

func TestLsWithoutClient(t *testing.T) {
	setupHome(t)
	if err := runCmd(listCmd()); err == nil || !strings.Contains(err.Error(), "no context selected") {
		t.Fatalf("err = %v", err)
	}
}

// Each group lists its own kind (ADR-070 §1), so `akerdock db list` answers
// without making the reader translate the type into an argument of another
// command. The transversal KIND column disappears: the group already said it.
func TestListGroupCmd(t *testing.T) {
	srv := lsServer(t)

	t.Run("renders the kind's own listing", func(t *testing.T) {
		setupContext(t, srv.URL)
		out, _ := captureOutput(t, func() {
			if err := runCmd(listGroupCmd(kindDB)); err != nil {
				t.Errorf("db list: %v", err)
			}
		})
		if !strings.Contains(out, "pg") || !strings.Contains(out, "postgres") {
			t.Fatalf("output = %q", out)
		}
		if strings.Contains(out, "KIND") {
			t.Fatalf("the group already names the kind: %q", out)
		}
	})

	t.Run("follows the cursor", func(t *testing.T) {
		setupContext(t, srv.URL)
		out, _ := captureOutput(t, func() {
			if err := runCmd(listGroupCmd(kindApp)); err != nil {
				t.Errorf("app list: %v", err)
			}
		})
		// varuna is on the first page, helios behind the cursor.
		if !strings.Contains(out, "varuna") || !strings.Contains(out, "helios") {
			t.Fatalf("output = %q", out)
		}
	})

	t.Run("-o json passes the API objects through", func(t *testing.T) {
		setupContext(t, srv.URL)
		flags.output = "json"
		t.Cleanup(func() { flags.output = "table" })
		out, _ := captureOutput(t, func() {
			if err := runCmd(listGroupCmd(kindDB)); err != nil {
				t.Errorf("db list -o json: %v", err)
			}
		})
		var items []resource
		if err := json.Unmarshal([]byte(out), &items); err != nil {
			t.Fatalf("stdout is not JSON: %v (%q)", err, out)
		}
		if len(items) != 1 || items[0].Name != "pg" {
			t.Fatalf("items = %+v", items)
		}
	})

	// An empty collection is an answer, not a failure: the header alone tells
	// the reader they have no stack, which is what they asked.
	t.Run("an empty kind renders its header", func(t *testing.T) {
		setupContext(t, srv.URL)
		out, _ := captureOutput(t, func() {
			if err := runCmd(listGroupCmd(kindSvc)); err != nil {
				t.Errorf("svc list: %v", err)
			}
		})
		if !strings.Contains(out, "NAME") {
			t.Fatalf("output = %q", out)
		}
	})
}
