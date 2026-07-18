// Package gitforge implements the degraded preview feedback path (§20.4.6,
// protocols §4.4/§6.3): commit statuses and ONE upserted comment against the
// GitLab and Gitea APIs, authenticated by a provider API token stored on the
// git source. The GitHub App keeps its richer client (internal/githubapp);
// this package is the parity for everyone else.
//
// Everything here is best-effort by contract: callers log failures and never
// let feedback fail a deployment. The token never appears in a URL — it
// travels in a header, because URLs end up in logs and proxies (INV-003).
package gitforge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// StatusState is the normalized preview state carried to a commit status.
type StatusState string

// The four transitions a preview narrates (§2.7a vocabulary).
const (
	StatusQueued  StatusState = "queued"
	StatusRunning StatusState = "running"
	StatusSuccess StatusState = "success"
	StatusFailure StatusState = "failure"
)

// StatusContext names the status in the forge UI — stable, so branch
// protection rules can reference it (protocols §4.4/§6.3).
const StatusContext = "AkerDock/preview"

// Notifier is what the preview lifecycle needs from a forge: a commit
// status, one upserted comment, and a rights check for comment commands.
type Notifier interface {
	// SetCommitStatus publishes state for sha, pointing at targetURL.
	SetCommitStatus(ctx context.Context, repo, sha string, state StatusState, targetURL string) error
	// UpsertComment creates or updates THE preview comment on the PR/MR —
	// identified by an invisible marker, never duplicated (§2.7c).
	UpsertComment(ctx context.Context, repo string, number int, marker, body string) error
	// AuthorCanWrite reports whether the comment author may command a
	// deployment (protocols §4.3/§6.3): write access or better.
	AuthorCanWrite(ctx context.Context, repo, username string, userID int64) (bool, error)
}

// markerHTML wraps a marker in the invisible HTML comment both forges keep
// verbatim in the comment body (§2.7c).
func markerHTML(marker string) string {
	return "<!-- akerdock:" + marker + " -->"
}

// containsMarker recognizes AkerDock's own comment among the thread's.
func containsMarker(body, marker string) bool {
	return strings.Contains(body, markerHTML(marker))
}

// doJSON sends a JSON request and decodes the response into out (when non
// nil). Any non-2xx answer is an error carrying the status, never the body —
// a forge error body can echo the URL, and the URL names the repo, not a
// secret, but the discipline is uniform.
func doJSON(ctx context.Context, client *http.Client, method, url string, headers map[string]string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return 0, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("%s %s: HTTP %d", method, redactURL(url), resp.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, fmt.Errorf("unparsable response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// redactURL strips the query string — tokens never travel there by
// construction, but error strings are logged, and the discipline costs
// nothing.
func redactURL(url string) string {
	base, _, _ := strings.Cut(url, "?")
	return base
}
