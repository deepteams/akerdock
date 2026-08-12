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

func TestRobotsIsServedAsPlainText(t *testing.T) {
	rec := httptest.NewRecorder()
	robotsHandler(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/robots.txt", nil))

	res := rec.Result()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q: a crawler ignores a robots.txt served as HTML", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "User-agent: *") {
		t.Errorf("robots.txt misses %q:\n%s", "User-agent: *", body)
	}
}

func TestRobotsDoesNotBlockTheCrawl(t *testing.T) {
	// ADR-074, and the regression that produced it: `Disallow: /` forbids the
	// fetch that reads `X-Robots-Tag: noindex, nofollow`, so a URL indexed
	// before the header shipped can never be recrawled and dropped. The rule
	// is stated as an invariant rather than an exact body so the explanatory
	// comment in the file can be reworded without failing here.
	rec := httptest.NewRecorder()
	robotsHandler(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/robots.txt", nil))

	for line := range strings.Lines(rec.Body.String()) {
		directive := strings.TrimSpace(line)
		if strings.HasPrefix(directive, "#") {
			continue // the comment quotes the ban precisely to explain its absence
		}
		if !strings.HasPrefix(strings.ToLower(directive), "disallow:") {
			continue
		}
		if strings.TrimSpace(directive[len("disallow:"):]) != "" {
			t.Errorf("robots.txt forbids a crawl the noindex header needs: %q", directive)
		}
	}
}

func TestRobotsIsItselfNoindexed(t *testing.T) {
	// With the crawl allowed, the header is the whole mechanism — including on
	// the one path a crawler is guaranteed to fetch first.
	h := SecurityHeaders(false)(Robots(http.NotFoundHandler()))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/robots.txt", nil))

	if got := rec.Result().Header.Get("X-Robots-Tag"); got != "noindex, nofollow" {
		t.Errorf("X-Robots-Tag on /robots.txt = %q, want %q", got, "noindex, nofollow")
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
	if !strings.Contains(rec.Body.String(), "User-agent: *") {
		t.Errorf("/robots.txt/ did not get the robots body:\n%s", rec.Body.String())
	}
}

func TestControlPlaneIsNeverIndexable(t *testing.T) {
	// robots.txt cannot keep anything out of an index — it only governs the
	// fetch. This header is what does it, on every response (ADR-074).
	if got := serveWithHeaders(t, false).Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
		t.Fatalf("X-Robots-Tag = %q, want noindex", got)
	}
}
