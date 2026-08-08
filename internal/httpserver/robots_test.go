package httpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func robotsHandler(t *testing.T) http.Handler {
	t.Helper()
	return Robots(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stands in for the SPA catch-all: 200 + HTML for anything.
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>dashboard</html>"))
	}))
}

func TestRobotsDisallowsEverything(t *testing.T) {
	rec := httptest.NewRecorder()
	robotsHandler(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/robots.txt", nil))

	res := rec.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q: a crawler ignores a robots.txt served as HTML", ct)
	}
	body := rec.Body.String()
	for _, line := range []string{"User-agent: *", "Disallow: /"} {
		if !strings.Contains(body, line) {
			t.Errorf("robots.txt misses %q:\n%s", line, body)
		}
	}
}

func TestRobotsBeatsTheSPACatchAll(t *testing.T) {
	// The regression this exists for: with the SPA answering unknown paths,
	// /robots.txt came back as index.html — a 200 that reads as "no rules".
	rec := httptest.NewRecorder()
	robotsHandler(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/robots.txt", nil))
	if strings.Contains(rec.Body.String(), "<html>") {
		t.Fatalf("the SPA answered robots.txt: %s", rec.Body.String())
	}
}

func TestRobotsLeavesEveryOtherPathAlone(t *testing.T) {
	for _, path := range []string{"/", "/api/v1/health", "/robots.txt.bak", "/assets/robots.txt"} {
		rec := httptest.NewRecorder()
		robotsHandler(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if !strings.Contains(rec.Body.String(), "dashboard") {
			t.Errorf("%s was intercepted by the robots middleware", path)
		}
	}
}

func TestRobotsHeadHasNoBody(t *testing.T) {
	// A HEAD gets the same headers as GET but no body — the crawler still
	// learns the resource exists and is plain text, without the payload.
	rec := httptest.NewRecorder()
	robotsHandler(t).ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/robots.txt", nil))
	res := rec.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", res.StatusCode)
	}
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "text/plain") {
		t.Errorf("HEAD Content-Type = %q, want text/plain", res.Header.Get("Content-Type"))
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD must carry no body, got %q", rec.Body.String())
	}
}

func TestRobotsRejectsWriteMethods(t *testing.T) {
	// robots.txt is read-only: a write method is 405, and the Allow header
	// tells the client which verbs the resource accepts.
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		robotsHandler(t).ServeHTTP(rec, httptest.NewRequest(method, "/robots.txt", nil))
		res := rec.Result()
		if res.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", method, res.StatusCode)
		}
		if allow := res.Header.Get("Allow"); allow != "GET, HEAD" {
			t.Errorf("%s Allow = %q, want \"GET, HEAD\"", method, allow)
		}
	}
}

func TestRobotsMatchesTrailingSlash(t *testing.T) {
	// `/robots.txt/` normalizes to the same resource — a crawler appending a
	// slash must not slip through to the SPA catch-all.
	rec := httptest.NewRecorder()
	robotsHandler(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/robots.txt/", nil))
	if strings.Contains(rec.Body.String(), "<html>") {
		t.Fatalf("/robots.txt/ reached the SPA: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Disallow: /") {
		t.Errorf("/robots.txt/ did not get the robots body:\n%s", rec.Body.String())
	}
}

func TestControlPlaneIsNeverIndexable(t *testing.T) {
	// robots.txt only asks crawlers not to FETCH; a dashboard URL linked from
	// anywhere else still gets indexed without this header.
	if got := serveWithHeaders(t, false).Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
		t.Fatalf("X-Robots-Tag = %q, want noindex", got)
	}
}
