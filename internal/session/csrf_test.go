package session

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// CSRF is the price of using cookies, and the whole reason this code exists.
//
// A session cookie is attached by the browser to EVERY request to this origin —
// including one that evil.com triggered with a hidden form. So the cookie proves
// which browser is calling; it never proves the user meant to call. The token
// below is readable by our page (same origin) and unreadable by theirs, so
// echoing it in a header is proof of intent.
//
// These tests do not need a database: they check the decision, not the lookup.

type fakeStore struct{}

func TestSafeMethodsNeedNoToken(t *testing.T) {
	m := &Manager{}
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		r := httptest.NewRequest(method, "/api/v1/applications", nil)
		r.AddCookie(&http.Cookie{Name: CookieName, Value: "whatever"})
		if err := m.VerifyCSRF(context.Background(), r); err != nil {
			t.Errorf("%s was refused: a safe method must not require a CSRF token (and a GET that "+
				"changes state is a bug this check would only hide)", method)
		}
	}
}

// A bearer-token call cannot be forged cross-site: the browser never attaches an
// Authorization header on its own. Requiring CSRF there would break every script
// for no gain.
func TestBearerCallsAreNotCSRFChecked(t *testing.T) {
	m := &Manager{}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/projects", nil)
	r.Header.Set("Authorization", "Bearer akd_something")
	if err := m.VerifyCSRF(context.Background(), r); err != nil {
		t.Error("a bearer-authenticated POST was CSRF-checked: it carries no cookie, so there is nothing to forge")
	}
}

var _ = fakeStore{}
