package gitforge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// record captures each request the fake forge receives.
type record struct {
	Method string
	Path   string
	Header http.Header
	Body   map[string]any
}

func capture(t *testing.T, records *[]record, respond func(r *http.Request) (int, any)) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		*records = append(*records, record{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
		status, payload := respond(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if payload != nil {
			_ = json.NewEncoder(w).Encode(payload)
		}
	}
}

func TestGitLabSetCommitStatus(t *testing.T) {
	var records []record
	srv := httptest.NewServer(capture(t, &records, func(r *http.Request) (int, any) { return 201, nil }))
	defer srv.Close()

	gl := &GitLab{BaseURL: srv.URL, Token: "tok"}
	if err := gl.SetCommitStatus(context.Background(), "42", "abc", StatusRunning, "https://pr-1.example.com"); err != nil {
		t.Fatal(err)
	}
	rec := records[0]
	if rec.Path != "/projects/42/statuses/abc" || rec.Method != "POST" {
		t.Fatalf("bad call: %s %s", rec.Method, rec.Path)
	}
	if rec.Header.Get("PRIVATE-TOKEN") != "tok" {
		t.Fatal("token must travel in PRIVATE-TOKEN, never in the URL")
	}
	if rec.Body["state"] != "running" || rec.Body["name"] != "AkerDock/preview" || rec.Body["target_url"] != "https://pr-1.example.com" {
		t.Fatalf("bad payload: %v", rec.Body)
	}
}

func TestGitLabStatusMapping(t *testing.T) {
	var records []record
	srv := httptest.NewServer(capture(t, &records, func(r *http.Request) (int, any) { return 201, nil }))
	defer srv.Close()
	gl := &GitLab{BaseURL: srv.URL, Token: "t"}

	want := map[StatusState]string{
		StatusQueued: "pending", StatusRunning: "running",
		StatusSuccess: "success", StatusFailure: "failed",
	}
	for state, glState := range want {
		records = records[:0]
		if err := gl.SetCommitStatus(context.Background(), "1", "s", state, ""); err != nil {
			t.Fatal(err)
		}
		if records[0].Body["state"] != glState {
			t.Fatalf("%s: got %v, want %s", state, records[0].Body["state"], glState)
		}
	}
}

func TestGitLabUpsertCommentCreatesThenUpdates(t *testing.T) {
	var records []record
	existing := []map[string]any{}
	srv := httptest.NewServer(capture(t, &records, func(r *http.Request) (int, any) {
		if r.Method == "GET" {
			return 200, existing
		}
		return 201, nil
	}))
	defer srv.Close()
	gl := &GitLab{BaseURL: srv.URL, Token: "t"}

	// First transition: no marked note exists, POST.
	if err := gl.UpsertComment(context.Background(), "42", 9, "preview-x-9", "hello"); err != nil {
		t.Fatal(err)
	}
	last := records[len(records)-1]
	if last.Method != "POST" || last.Path != "/projects/42/merge_requests/9/notes" {
		t.Fatalf("expected note creation, got %s %s", last.Method, last.Path)
	}
	body := last.Body["body"].(string)
	if !strings.Contains(body, "<!-- akerdock:preview-x-9 -->") || !strings.Contains(body, "hello") {
		t.Fatalf("marker missing: %q", body)
	}

	// Second transition: the marked note exists, PUT in place — never a
	// second note (§20.4.6).
	existing = []map[string]any{
		{"id": 7, "body": "unrelated"},
		{"id": 8, "body": "<!-- akerdock:preview-x-9 -->\nold"},
	}
	records = records[:0]
	if err := gl.UpsertComment(context.Background(), "42", 9, "preview-x-9", "updated"); err != nil {
		t.Fatal(err)
	}
	last = records[len(records)-1]
	if last.Method != "PUT" || last.Path != "/projects/42/merge_requests/9/notes/8" {
		t.Fatalf("expected in-place update, got %s %s", last.Method, last.Path)
	}
}

func TestGitLabAuthorCanWrite(t *testing.T) {
	cases := []struct {
		status int
		level  int
		want   bool
	}{
		{200, 40, true},  // maintainer
		{200, 30, true},  // developer
		{200, 20, false}, // reporter
		{404, 0, false},  // not a member — not an error either
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			_ = json.NewEncoder(w).Encode(map[string]any{"access_level": c.level})
		}))
		gl := &GitLab{BaseURL: srv.URL, Token: "t"}
		got, err := gl.AuthorCanWrite(context.Background(), "1", "u", 5)
		srv.Close()
		if err != nil {
			t.Fatalf("level %d: %v", c.level, err)
		}
		if got != c.want {
			t.Fatalf("level %d/status %d: got %v", c.level, c.status, got)
		}
	}
}

func TestGiteaSetCommitStatus(t *testing.T) {
	var records []record
	srv := httptest.NewServer(capture(t, &records, func(r *http.Request) (int, any) { return 201, nil }))
	defer srv.Close()

	g := &Gitea{BaseURL: srv.URL, Token: "tok"}
	if err := g.SetCommitStatus(context.Background(), "org/app", "abc", StatusFailure, "https://x"); err != nil {
		t.Fatal(err)
	}
	rec := records[0]
	if rec.Path != "/repos/org/app/statuses/abc" {
		t.Fatalf("bad path: %s", rec.Path)
	}
	if rec.Header.Get("Authorization") != "token tok" {
		t.Fatal("token must travel in the Authorization header")
	}
	if rec.Body["state"] != "failure" || rec.Body["context"] != "AkerDock/preview" {
		t.Fatalf("bad payload: %v", rec.Body)
	}
}

func TestGiteaUpsertCommentUpdatesInPlace(t *testing.T) {
	var records []record
	srv := httptest.NewServer(capture(t, &records, func(r *http.Request) (int, any) {
		if r.Method == "GET" {
			return 200, []map[string]any{{"id": 5, "body": "<!-- akerdock:m1 -->\nold"}}
		}
		return 200, nil
	}))
	defer srv.Close()
	g := &Gitea{BaseURL: srv.URL, Token: "t"}
	if err := g.UpsertComment(context.Background(), "o/r", 3, "m1", "new"); err != nil {
		t.Fatal(err)
	}
	last := records[len(records)-1]
	if last.Method != "PATCH" || last.Path != "/repos/o/r/issues/comments/5" {
		t.Fatalf("expected PATCH in place, got %s %s", last.Method, last.Path)
	}
}

func TestGiteaUpsertCommentCreates(t *testing.T) {
	var records []record
	srv := httptest.NewServer(capture(t, &records, func(r *http.Request) (int, any) {
		if r.Method == http.MethodGet {
			return http.StatusOK, []map[string]any{}
		}
		return http.StatusCreated, nil
	}))
	defer srv.Close()

	g := &Gitea{BaseURL: srv.URL, Token: "t"}
	if err := g.UpsertComment(context.Background(), "o/r", 3, "m1", "new"); err != nil {
		t.Fatal(err)
	}
	last := records[len(records)-1]
	if last.Method != http.MethodPost || last.Path != "/repos/o/r/issues/3/comments" {
		t.Fatalf("expected comment creation, got %s %s", last.Method, last.Path)
	}
}

func TestGiteaAuthorCanWrite(t *testing.T) {
	cases := map[string]bool{"admin": true, "write": true, "read": false}
	for perm, want := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]string{"permission": perm})
		}))
		g := &Gitea{BaseURL: srv.URL, Token: "t"}
		got, err := g.AuthorCanWrite(context.Background(), "o/r", "bob", 0)
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s: got %v", perm, got)
		}
	}
}

func TestGiteaRejectsMalformedRepo(t *testing.T) {
	g := &Gitea{BaseURL: "http://unused", Token: "t"}
	for _, repo := range []string{"noslash", "", "a/b c", "x/y?z"} {
		if err := g.SetCommitStatus(context.Background(), repo, "s", StatusSuccess, ""); err == nil {
			t.Fatalf("%q: expected an error", repo)
		}
	}
}

func TestForgeInputValidation(t *testing.T) {
	gitea := &Gitea{BaseURL: "http://unused", Token: "t"}
	if err := gitea.SetCommitStatus(context.Background(), "o/r", "s", StatusState("unknown"), ""); err == nil {
		t.Fatal("Gitea should reject an unmapped status")
	}
	if err := gitea.UpsertComment(context.Background(), "invalid", 1, "m", "body"); err == nil {
		t.Fatal("Gitea should validate repositories before listing comments")
	}
	if _, err := gitea.AuthorCanWrite(context.Background(), "o/r", "bad user", 0); err == nil {
		t.Fatal("Gitea should reject unsafe usernames")
	}
	if _, err := gitea.AuthorCanWrite(context.Background(), "invalid", "bob", 0); err == nil {
		t.Fatal("Gitea should reject malformed repositories")
	}

	gitlab := &GitLab{BaseURL: "http://unused", Token: "t"}
	if err := gitlab.SetCommitStatus(context.Background(), "1", "s", StatusState("unknown"), ""); err == nil {
		t.Fatal("GitLab should reject an unmapped status")
	}
}

func TestDoJSONFailureModes(t *testing.T) {
	ctx := context.Background()
	if _, err := doJSON(ctx, nil, http.MethodPost, "http://unused", nil, make(chan int), nil); err == nil {
		t.Fatal("unencodable input should fail before making a request")
	}
	if _, err := doJSON(ctx, nil, "bad\nmethod", "http://unused", nil, nil, nil); err == nil {
		t.Fatal("an invalid HTTP method should be rejected")
	}

	networkFailure := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network unavailable")
	})}
	if _, err := doJSON(ctx, networkFailure, http.MethodGet, "http://forge.invalid", nil, nil, nil); err == nil {
		t.Fatal("transport failures must be returned")
	}

	response := func(status int, body string) *http.Client {
		return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: status,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})}
	}
	if _, err := doJSON(ctx, response(http.StatusUnauthorized, `{"token":"secret"}`), http.MethodGet,
		"https://forge.invalid/path?access_token=secret", nil, nil, nil); err == nil ||
		strings.Contains(err.Error(), "access_token") || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("non-2xx errors must be redacted and contextual: %v", err)
	}
	var out map[string]any
	if _, err := doJSON(ctx, response(http.StatusOK, "not-json"), http.MethodGet,
		"https://forge.invalid/path", nil, nil, &out); err == nil || !strings.Contains(err.Error(), "unparsable") {
		t.Fatalf("malformed response should be explicit, got %v", err)
	}
}
