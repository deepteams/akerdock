package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The typed tree resolves a target from a bare name, a directory default, or
// nothing at all (ADR-070 §1). These are the three paths, plus the refusal that
// replaced the REF.
func TestTargetName(t *testing.T) {
	t.Run("positional argument wins", func(t *testing.T) {
		setupHome(t)
		name, err := targetName(kindDB, []string{"pg"})
		if err != nil || name != "pg" {
			t.Fatalf("name=%q err=%v", name, err)
		}
	})

	t.Run("an application falls back to the directory default", func(t *testing.T) {
		setupHome(t)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, dirConfigName), []byte("application: varuna\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		name, err := targetName(kindApp, nil)
		if err != nil || name != "varuna" {
			t.Fatalf("name=%q err=%v", name, err)
		}
	})

	// A .akerdock names the application a repository deploys, never the database
	// it talks to — so the other kinds have no default to fall back on, and the
	// message must ask for a name rather than mention a file that would not help.
	t.Run("other kinds have no default", func(t *testing.T) {
		setupHome(t)
		_, err := targetName(kindDB, nil)
		if err == nil || strings.Contains(err.Error(), ".akerdock") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("no argument and no default", func(t *testing.T) {
		setupHome(t)
		if _, err := targetName(kindApp, nil); err == nil || !strings.Contains(err.Error(), "no application given") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("settings failure surfaces", func(t *testing.T) {
		setupHome(t)
		t.Setenv("HOME", "")
		if _, err := targetName(kindApp, nil); err == nil {
			t.Fatal("expected a settings error")
		}
	})
}

// The old spelling must never resolve as a literal name: it names the command
// that replaced it (ADR-070 §5).
func TestCheckNotARef(t *testing.T) {
	cases := []struct {
		kind     resourceKind
		arg      string
		want     string // "" = accepted as a name
		contains string
	}{
		{kind: kindApp, arg: "varuna", want: "varuna"},
		{kind: kindApp, arg: "app/varuna", contains: "akerdock app <verb> varuna"},
		{kind: kindApp, arg: "db/pg", contains: "akerdock db <verb> pg"},
		{kind: kindDB, arg: "database/pg", contains: "akerdock db <verb> pg"},
		// `preview/…` had no group: --pr does that job, so the refusal points at
		// the group being addressed rather than inventing one.
		{kind: kindApp, arg: "preview/42", contains: "akerdock app <verb> 42"},
		{kind: kindApp, arg: "weird/thing", contains: "invalid application name"},
		{kind: kindApp, arg: "app/", contains: "invalid application name"},
	}
	for _, tc := range cases {
		got, err := checkNotARef(tc.kind, tc.arg)
		switch {
		case tc.want != "":
			if err != nil || got != tc.want {
				t.Errorf("checkNotARef(%q) = (%q, %v), want %q", tc.arg, got, err, tc.want)
			}
		case err == nil || !strings.Contains(err.Error(), tc.contains):
			t.Errorf("checkNotARef(%q) err = %v, want it to mention %q", tc.arg, err, tc.contains)
		}
	}
}

func TestDefaultComponent(t *testing.T) {
	setupHome(t)
	if got := defaultComponent("web"); got != "web" {
		t.Fatalf("explicit flag must win, got %q", got)
	}
	t.Setenv(envComponent, "postgres")
	if got := defaultComponent(""); got != "postgres" {
		t.Fatalf("resolved default lost, got %q", got)
	}
	// A broken config yields no default rather than an error: the component is
	// optional and the command may not need one at all.
	t.Setenv("HOME", "")
	if got := defaultComponent(""); got != "" {
		t.Fatalf("got %q", got)
	}
}

// listServer fakes the /api/v1 list endpoints the resolver walks.
func listServer(t *testing.T, items map[string][]resource) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/v1")
		data, ok := items[path]
		if !ok {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":"boom","message":"list failed"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(resourcePage{Data: data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveNamed(t *testing.T) {
	srv := listServer(t, map[string][]resource{
		"/applications": {
			{Uuid: "app-1", Name: "varuna"},
			{Uuid: "app-2", Name: "twin"},
			{Uuid: "app-3", Name: "twin"},
		},
	})
	c := &Client{base: srv.URL, token: "tok", http: srv.Client()}
	ctx := context.Background()

	t.Run("by uuid", func(t *testing.T) {
		res, err := c.resolveNamed(ctx, kindApp, "app-1")
		if err != nil || res.Name != "varuna" {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})
	t.Run("by name", func(t *testing.T) {
		res, err := c.resolveNamed(ctx, kindApp, "varuna")
		if err != nil || res.Uuid != "app-1" {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})
	t.Run("no match", func(t *testing.T) {
		if _, err := c.resolveNamed(ctx, kindApp, "ghost"); err == nil || !strings.Contains(err.Error(), `no applications named "ghost"`) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("ambiguous name asks for the uuid", func(t *testing.T) {
		if _, err := c.resolveNamed(ctx, kindApp, "twin"); err == nil || !strings.Contains(err.Error(), "use the UUID") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("list error", func(t *testing.T) {
		if _, err := c.resolveNamed(ctx, kindDB, "pg"); err == nil {
			t.Fatal("expected the list error to surface")
		}
	})
}
