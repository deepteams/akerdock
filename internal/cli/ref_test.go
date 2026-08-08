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

func TestRefFromArgs(t *testing.T) {
	t.Run("positional argument wins", func(t *testing.T) {
		setupHome(t)
		r, err := refFromArgs([]string{"db/pg"})
		if err != nil || r.kind != "databases" || r.name != "pg" {
			t.Fatalf("r=%+v err=%v", r, err)
		}
	})

	t.Run("falls back to the default application", func(t *testing.T) {
		setupHome(t)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, dirConfigName), []byte("application: varuna\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		r, err := refFromArgs(nil)
		if err != nil || r.kind != "apps" || r.name != "varuna" {
			t.Fatalf("r=%+v err=%v", r, err)
		}
	})

	t.Run("no argument and no default", func(t *testing.T) {
		setupHome(t)
		if _, err := refFromArgs(nil); err == nil || !strings.Contains(err.Error(), "no target given") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("settings failure surfaces", func(t *testing.T) {
		setupHome(t)
		t.Setenv("HOME", "")
		if _, err := refFromArgs(nil); err == nil {
			t.Fatal("expected a settings error")
		}
	})
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

// listServer fakes the /api/v1 list endpoints resolve() walks.
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

func TestResolve(t *testing.T) {
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
		res, err := c.resolve(ctx, ref{kind: "apps", name: "app-1"})
		if err != nil || res.Name != "varuna" {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})
	t.Run("by name", func(t *testing.T) {
		res, err := c.resolve(ctx, ref{kind: "apps", name: "varuna"})
		if err != nil || res.Uuid != "app-1" {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	})
	t.Run("no match", func(t *testing.T) {
		if _, err := c.resolve(ctx, ref{kind: "apps", name: "ghost"}); err == nil || !strings.Contains(err.Error(), `no apps named "ghost"`) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("ambiguous name asks for the uuid", func(t *testing.T) {
		if _, err := c.resolve(ctx, ref{kind: "apps", name: "twin"}); err == nil || !strings.Contains(err.Error(), "use the UUID") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unsupported kind", func(t *testing.T) {
		if _, err := c.resolve(ctx, ref{kind: "previews", name: "x"}); err == nil || !strings.Contains(err.Error(), "not supported") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("list error", func(t *testing.T) {
		if _, err := c.resolve(ctx, ref{kind: "databases", name: "pg"}); err == nil {
			t.Fatal("expected the list error to surface")
		}
	})
}

func TestResolvePreview(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/applications/app-1/previews" {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"data":[
			{"uuid":"pv-1","pr_id":42,"status":"active"},
			{"uuid":"pv-2","pr_id":7,"status":"destroyed"}]}`))
	}))
	defer srv.Close()
	c := &Client{base: srv.URL, token: "tok", http: srv.Client()}
	ctx := context.Background()

	p, err := c.resolvePreview(ctx, "app-1", 42)
	if err != nil || p.Uuid != "pv-1" {
		t.Fatalf("p=%+v err=%v", p, err)
	}
	if _, err := c.resolvePreview(ctx, "app-1", 7); err == nil || !strings.Contains(err.Error(), "destroyed") {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.resolvePreview(ctx, "app-1", 99); err == nil || !strings.Contains(err.Error(), "no preview for PR #99") {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.resolvePreview(ctx, "other-app", 42); err == nil {
		t.Fatal("expected the API error to surface")
	}
}
