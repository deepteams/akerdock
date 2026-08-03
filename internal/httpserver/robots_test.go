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

func TestControlPlaneIsNeverIndexable(t *testing.T) {
	// robots.txt only asks crawlers not to FETCH; a dashboard URL linked from
	// anywhere else still gets indexed without this header.
	if got := serveWithHeaders(t, false).Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
		t.Fatalf("X-Robots-Tag = %q, want noindex", got)
	}
}
