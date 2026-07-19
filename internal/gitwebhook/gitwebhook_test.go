package gitwebhook

import (
	"net/http"
	"testing"
)

var secret = []byte("s3cr3t")

func TestProviderMetadataHeaders(t *testing.T) {
	headers := http.Header{
		"X-Github-Delivery":   {"gh-delivery"},
		"X-Gitlab-Event-Uuid": {"gl-delivery"},
		"X-Gitea-Delivery":    {"gt-delivery"},
		"X-Github-Event":      {"push"},
		"X-Gitlab-Event":      {"Push Hook"},
		"X-Gitea-Event":       {"push-native"},
	}
	for _, tc := range []struct {
		provider Provider
		delivery string
		event    string
	}{
		{GitHub, "gh-delivery", "push"},
		{GitLab, "gl-delivery", "Push Hook"},
		{Gitea, "gt-delivery", "push-native"},
	} {
		if !Supported(string(tc.provider)) {
			t.Errorf("%s should be supported", tc.provider)
		}
		if got := DeliveryID(tc.provider, headers); got != tc.delivery {
			t.Errorf("%s delivery = %q, want %q", tc.provider, got, tc.delivery)
		}
		if got := EventType(tc.provider, headers); got != tc.event {
			t.Errorf("%s event = %q, want %q", tc.provider, got, tc.event)
		}
	}
	if Supported("bitbucket") || DeliveryID("unknown", headers) != "" || EventType("unknown", headers) != "" {
		t.Fatal("unknown providers must remain unsupported and expose no metadata")
	}

	headers.Del("X-Gitea-Event")
	if got := EventType(Gitea, headers); got != "push" {
		t.Fatalf("Gitea should fall back to the GitHub-compatible event header, got %q", got)
	}
}

func TestVerifyGitHub(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	h := http.Header{}
	h.Set("X-Hub-Signature-256", "sha256="+Sign(body, secret))
	if err := VerifySignature(GitHub, h, body, secret); err != nil {
		t.Fatalf("a valid signature was refused: %v", err)
	}

	// A body altered after signing must fail — this is the whole point.
	if err := VerifySignature(GitHub, h, []byte(`{"ref":"refs/heads/evil"}`), secret); err == nil {
		t.Error("a tampered body was accepted")
	}
	// Wrong secret.
	if err := VerifySignature(GitHub, h, body, []byte("other")); err == nil {
		t.Error("a signature made with another secret was accepted")
	}
	// No signature at all: never trusted, whatever the payload says.
	if err := VerifySignature(GitHub, http.Header{}, body, secret); err == nil {
		t.Error("an unsigned delivery was accepted")
	}
	// The legacy SHA-1 header must not be honoured.
	legacy := http.Header{}
	legacy.Set("X-Hub-Signature", "sha1=deadbeef")
	if err := VerifySignature(GitHub, legacy, body, secret); err == nil {
		t.Error("the legacy SHA-1 header was accepted")
	}
}

func TestVerifyGitea(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/main"}`)
	h := http.Header{}
	h.Set("X-Gitea-Signature", Sign(body, secret)) // no sha256= prefix
	if err := VerifySignature(Gitea, h, body, secret); err != nil {
		t.Fatalf("a valid Gitea signature was refused: %v", err)
	}
	// Fallback on the GitHub-compatible header.
	compat := http.Header{}
	compat.Set("X-Hub-Signature-256", "sha256="+Sign(body, secret))
	if err := VerifySignature(Gitea, compat, body, secret); err != nil {
		t.Errorf("the GitHub-compatible header was refused: %v", err)
	}
}

// GitLab sends the secret in clear: it must still be compared in constant time
// and refused when absent.
func TestVerifyGitLab(t *testing.T) {
	body := []byte(`{}`)
	h := http.Header{}
	h.Set("X-Gitlab-Token", "s3cr3t")
	if err := VerifySignature(GitLab, h, body, secret); err != nil {
		t.Fatalf("a valid GitLab token was refused: %v", err)
	}
	h.Set("X-Gitlab-Token", "wrong")
	if err := VerifySignature(GitLab, h, body, secret); err == nil {
		t.Error("a wrong GitLab token was accepted")
	}
	if err := VerifySignature(GitLab, http.Header{}, body, secret); err == nil {
		t.Error("a missing GitLab token was accepted")
	}
}

func TestSignatureMalformedInputs(t *testing.T) {
	body := []byte(`{}`)
	for _, signature := range []string{"deadbeef", "sha256=not-hex"} {
		h := http.Header{"X-Hub-Signature-256": {signature}}
		if err := VerifySignature(GitHub, h, body, secret); err == nil {
			t.Errorf("malformed signature %q was accepted", signature)
		}
	}
	if err := VerifySignature("unknown", http.Header{}, body, secret); err == nil {
		t.Fatal("an unknown provider must never authenticate")
	}
}

func TestParsePushGitHub(t *testing.T) {
	body := []byte(`{
	  "ref": "refs/heads/main",
	  "after": "abc123",
	  "head_commit": {"message": "fix: thing [skip ci]"},
	  "commits": [{"added": ["web/a.ts"], "modified": ["api/b.go"], "removed": []}]
	}`)
	push, err := ParsePush(GitHub, body)
	if err != nil {
		t.Fatal(err)
	}
	if push.Branch() != "main" || push.Commit != "abc123" {
		t.Errorf("branch=%q commit=%q", push.Branch(), push.Commit)
	}
	if !push.SkipRequested() {
		t.Error("[skip ci] was not honoured")
	}
	if len(push.Files) != 2 {
		t.Errorf("files = %v", push.Files)
	}
}

func TestParsePushGitLab(t *testing.T) {
	body := []byte(`{
	  "ref": "refs/heads/prod",
	  "checkout_sha": "def456",
	  "commits": [{"message": "first", "added": ["x"]}, {"message": "head commit", "modified": ["y"]}]
	}`)
	push, err := ParsePush(GitLab, body)
	if err != nil {
		t.Fatal(err)
	}
	if push.Branch() != "prod" || push.Commit != "def456" {
		t.Errorf("branch=%q commit=%q", push.Branch(), push.Commit)
	}
	// GitLab has no head_commit: the last one is the head.
	if push.Message != "head commit" {
		t.Errorf("message = %q", push.Message)
	}
}

func TestParsePushValidationAndDirectGitLabSHA(t *testing.T) {
	push, err := ParsePush(GitLab, []byte(`{"ref":"refs/heads/main","after":"direct","checkout_sha":"fallback","commits":[]}`))
	if err != nil || push.Commit != "direct" {
		t.Fatalf("GitLab after SHA should win: %+v, %v", push, err)
	}
	if push.SkipRequested() {
		t.Fatal("a normal message should not request skipping")
	}

	for _, tc := range []struct {
		provider Provider
		body     []byte
	}{
		{GitHub, []byte(`{`)},
		{GitLab, []byte(`{`)},
		{"unknown", []byte(`{}`)},
	} {
		if _, err := ParsePush(tc.provider, tc.body); err == nil {
			t.Errorf("%s malformed/unsupported push should fail", tc.provider)
		}
	}
}

// A branch deletion carries an all-zero commit: there is nothing to build, and
// deploying it would redeploy whatever HEAD happened to be.
func TestDeletedBranch(t *testing.T) {
	body := []byte(`{"ref":"refs/heads/gone","after":"0000000000000000000000000000000000000000","commits":[]}`)
	push, err := ParsePush(GitHub, body)
	if err != nil {
		t.Fatal(err)
	}
	if !push.Deleted() {
		t.Error("a branch deletion must not be deployable")
	}
}
