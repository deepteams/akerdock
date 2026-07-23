package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCursorRoundTrip(t *testing.T) {
	c := encodeCursor(42)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	id, ok := afterID(w, r, &c)
	if !ok || id != 42 {
		t.Fatalf("round-trip failed: id=%d ok=%v", id, ok)
	}
}

func TestCursorRejectsGarbage(t *testing.T) {
	for _, c := range []string{"not-base64!", "djE6YWJj" /* v1:abc */, "djI6NDI" /* v2:42 */, "NDI" /* 42, no prefix */} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if _, ok := afterID(w, r, &c); ok {
			t.Errorf("cursor %q must be rejected", c)
		}
		if w.Code != 400 {
			t.Errorf("cursor %q must produce 400, got %d", c, w.Code)
		}
	}
}

func TestPageLimitBounds(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if limit, ok := pageLimit(w, r, nil); !ok || limit != 25 {
		t.Fatalf("default limit = %d, want 25", limit)
	}
	for _, bad := range []int{0, -1, 101} {
		w := httptest.NewRecorder()
		if _, ok := pageLimit(w, r, &bad); ok {
			t.Errorf("limit %d must be rejected", bad)
		}
	}
	good := 100
	if limit, ok := pageLimit(httptest.NewRecorder(), r, &good); !ok || limit != 100 {
		t.Fatalf("limit 100 must be accepted, got %d", limit)
	}
}

func TestNextCursor(t *testing.T) {
	type row struct{ id int64 }
	rows := []row{{10}, {9}, {8}}
	kept, cursor := nextCursor(rows, 2, func(r row) int64 { return r.id })
	if len(kept) != 2 || cursor == nil {
		t.Fatalf("expected truncated page with cursor, got %d rows cursor=%v", len(kept), cursor)
	}
	kept, cursor = nextCursor(rows, 3, func(r row) int64 { return r.id })
	if len(kept) != 3 || cursor != nil {
		t.Fatal("last page must have nil cursor")
	}
}

// html_url stores the HOST base, but rows converted before the fix carry the
// app page URL GitHub returns — the builder must not double the path on them.
func TestGithubInstallURL(t *testing.T) {
	cases := map[string]string{
		"https://github.com":                     "https://github.com/apps/my-app/installations/new",
		"https://github.com/":                    "https://github.com/apps/my-app/installations/new",
		"https://ghes.corp.example":              "https://ghes.corp.example/apps/my-app/installations/new",
		"https://github.com/apps/my-app":         "https://github.com/apps/my-app/installations/new",
		"https://ghes.corp.example/apps/my-app/": "https://ghes.corp.example/apps/my-app/installations/new",
	}
	for htmlURL, want := range cases {
		if got := githubInstallURL(htmlURL, "my-app"); got != want {
			t.Errorf("githubInstallURL(%q) = %q, want %q", htmlURL, got, want)
		}
	}
}
