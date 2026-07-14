package httpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serveWithHeaders(t *testing.T, hsts bool) http.Header {
	t.Helper()
	h := SecurityHeaders(hsts)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	return rec.Result().Header
}

func TestSecurityHeadersAreSet(t *testing.T) {
	headers := serveWithHeaders(t, false)

	for name, want := range map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
	} {
		if got := headers.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	csp := headers.Get("Content-Security-Policy")
	// The clauses that carry the security value; their absence is a regression,
	// not a style choice.
	for _, clause := range []string{"default-src 'self'", "script-src 'self'", "object-src 'none'", "frame-ancestors 'none'", "base-uri 'self'"} {
		if !strings.Contains(csp, clause) {
			t.Errorf("CSP misses %q: %s", clause, csp)
		}
	}
	if strings.Contains(csp, "unsafe-eval") || strings.Contains(strings.Split(csp, "style-src")[0], "unsafe-inline") {
		t.Errorf("CSP allows inline/eval scripts — that is the whole class of attack the header exists to stop: %s", csp)
	}
}

func TestHSTSFollowsTheInstanceSecurity(t *testing.T) {
	if got := serveWithHeaders(t, false).Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS %q sent on a plain-HTTP instance: browsers would refuse http for its whole max-age, locking the operator out", got)
	}
	if got := serveWithHeaders(t, true).Get("Strict-Transport-Security"); !strings.Contains(got, "max-age=") {
		t.Errorf("HSTS missing on a https instance, got %q", got)
	}
}
