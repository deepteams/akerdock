package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAPIErrorError(t *testing.T) {
	withMsg := &apiError{Code: "not_found", Message: "no such app", status: 404}
	if got := withMsg.Error(); got != "no such app (not_found)" {
		t.Fatalf("Error() = %q", got)
	}
	// A body-less failure still names the status instead of an empty string.
	bare := &apiError{status: 502}
	if got := bare.Error(); got != "HTTP 502" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestDecodeError(t *testing.T) {
	t.Run("json envelope", func(t *testing.T) {
		rec := httptest.NewRecorder()
		rec.WriteHeader(http.StatusForbidden)
		_, _ = rec.WriteString(`{"code":"forbidden","message":"nope","request_id":"r1","request_url":"https://x/grant"}`)
		err := decodeError(rec.Result())
		var apiErr *apiError
		if !asAPIError(err, &apiErr) {
			t.Fatalf("not an apiError: %v", err)
		}
		if apiErr.Code != "forbidden" || apiErr.Message != "nope" || apiErr.RequestURL != "https://x/grant" || apiErr.status != 403 {
			t.Fatalf("decoded %+v", apiErr)
		}
	})

	t.Run("plain text body becomes the message", func(t *testing.T) {
		rec := httptest.NewRecorder()
		rec.WriteHeader(http.StatusInternalServerError)
		_, _ = rec.WriteString("  boom  ")
		err := decodeError(rec.Result())
		if !strings.Contains(err.Error(), "boom") {
			t.Fatalf("plain-text body lost: %v", err)
		}
	})
}

func asAPIError(err error, target **apiError) bool {
	e, ok := err.(*apiError)
	if ok {
		*target = e
	}
	return ok
}

func TestNewClient(t *testing.T) {
	t.Run("no context selected", func(t *testing.T) {
		setupHome(t)
		if _, err := newClient(""); err == nil || !strings.Contains(err.Error(), "no context selected") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("unknown context", func(t *testing.T) {
		setupHome(t)
		if _, err := newClient("ghost"); err == nil || !strings.Contains(err.Error(), `unknown context "ghost"`) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("context without token", func(t *testing.T) {
		setupHome(t)
		cfg := &Config{CurrentContext: "test", Contexts: map[string]Context{"test": {URL: "https://m.example.com"}}}
		if err := cfg.Save(); err != nil {
			t.Fatal(err)
		}
		if _, err := newClient(""); err == nil || !strings.Contains(err.Error(), "has no token") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("ready client", func(t *testing.T) {
		setupContext(t, "https://m.example.com/")
		c, err := newClient("")
		if err != nil {
			t.Fatal(err)
		}
		if c.base != "https://m.example.com" || c.token != "akd_secret" || c.team != "team-1" {
			t.Fatalf("client = %+v", c)
		}
	})

	t.Run("config unreadable", func(t *testing.T) {
		setupHome(t)
		t.Setenv("HOME", "")
		if _, err := newClient(""); err == nil {
			t.Fatal("expected a config error")
		}
	})

	t.Run("broken dir config", func(t *testing.T) {
		setupContext(t, "https://m.example.com")
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, dirConfigName), []byte("{not yaml"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		if _, err := newClient(""); err == nil {
			t.Fatal("expected a dir config error")
		}
	})

	t.Run("broken credentials", func(t *testing.T) {
		setupContext(t, "https://m.example.com")
		home, err := configDir()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, credsFileName), []byte("{not yaml"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := newClient(""); err == nil {
			t.Fatal("expected a credentials error")
		}
	})
}

func TestClientDo(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}
	var gotAuth, gotCT, gotPath, gotQuery string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		gotBody, _ = io.ReadAll(r.Body)
		switch r.URL.Path {
		case "/api/v1/ok":
			_ = json.NewEncoder(w).Encode(payload{Name: "varuna"})
		case "/api/v1/fail":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"code":"invalid","message":"bad input"}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	c := &Client{base: srv.URL, token: "tok", http: srv.Client()}

	t.Run("request and decode", func(t *testing.T) {
		var out payload
		err := c.do(context.Background(), http.MethodPost, "/ok", url.Values{"limit": {"5"}}, payload{Name: "in"}, &out)
		if err != nil {
			t.Fatal(err)
		}
		if out.Name != "varuna" {
			t.Fatalf("out = %+v", out)
		}
		if gotAuth != "Bearer tok" || gotCT != "application/json" {
			t.Fatalf("headers auth=%q ct=%q", gotAuth, gotCT)
		}
		if gotPath != "/api/v1/ok" || gotQuery != "limit=5" {
			t.Fatalf("url path=%q query=%q", gotPath, gotQuery)
		}
		if !strings.Contains(string(gotBody), `"in"`) {
			t.Fatalf("body = %q", gotBody)
		}
	})

	t.Run("nil out ignores the body", func(t *testing.T) {
		if err := c.do(context.Background(), http.MethodGet, "/nothing", nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("api error is decoded", func(t *testing.T) {
		err := c.do(context.Background(), http.MethodGet, "/fail", nil, nil, nil)
		var apiErr *apiError
		if !asAPIError(err, &apiErr) || apiErr.Code != "invalid" || apiErr.status != 422 {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("unmarshalable body", func(t *testing.T) {
		if err := c.do(context.Background(), http.MethodPost, "/ok", nil, make(chan int), nil); err == nil {
			t.Fatal("expected a marshal error")
		}
	})

	t.Run("invalid method", func(t *testing.T) {
		if err := c.do(context.Background(), "BAD METHOD", "/ok", nil, nil, nil); err == nil {
			t.Fatal("expected a request build error")
		}
	})

	t.Run("unreachable host names the base", func(t *testing.T) {
		dead := &Client{base: "http://127.0.0.1:1", token: "tok", http: srv.Client()}
		err := dead.do(context.Background(), http.MethodGet, "/x", nil, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "cannot reach http://127.0.0.1:1") {
			t.Fatalf("err = %v", err)
		}
	})
}
