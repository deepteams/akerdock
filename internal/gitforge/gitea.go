package gitforge

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Gitea talks to the Gitea/Forgejo API v1 (protocols §6.3). repo is the
// owner/repo full name (previews.repo_reference). Gitea has no Checks API:
// commit statuses are the deliberate degraded equivalent.
type Gitea struct {
	// BaseURL is the API root, e.g. https://gitea.example.com/api/v1
	// (git_sources.api_url — self-hosted by nature, §6.1).
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

func (g *Gitea) headers() map[string]string {
	return map[string]string{"Authorization": "token " + g.Token}
}

// splitRepo bounds the owner/repo shape before it reaches a URL.
func splitRepo(repo string) (string, string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.ContainsAny(repo, " ?#%") {
		return "", "", fmt.Errorf("invalid repository reference")
	}
	return owner, name, nil
}

// SetCommitStatus implements POST /repos/{owner}/{repo}/statuses/{sha} (§6.3).
func (g *Gitea) SetCommitStatus(ctx context.Context, repo, sha string, state StatusState, targetURL string) error {
	giteaState := map[StatusState]string{
		StatusQueued:  "pending",
		StatusRunning: "pending",
		StatusSuccess: "success",
		StatusFailure: "failure",
	}[state]
	if giteaState == "" {
		return fmt.Errorf("unmapped status state %q", state)
	}
	owner, name, err := splitRepo(repo)
	if err != nil {
		return err
	}
	payload := map[string]string{"state": giteaState, "context": StatusContext}
	if targetURL != "" {
		payload["target_url"] = targetURL
	}
	u := fmt.Sprintf("%s/repos/%s/%s/statuses/%s", g.BaseURL, owner, name, sha)
	_, err = doJSON(ctx, g.HTTPClient, http.MethodPost, u, g.headers(), payload, nil)
	return err
}

// UpsertComment uses the issues API — a PR is an issue in Gitea (§6.3):
// list, PATCH the marked comment in place, else POST it.
func (g *Gitea) UpsertComment(ctx context.Context, repo string, number int, marker, body string) error {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return err
	}
	full := markerHTML(marker) + "\n" + body
	var comments []struct {
		ID   int64  `json:"id"`
		Body string `json:"body"`
	}
	u := fmt.Sprintf("%s/repos/%s/%s/issues/%d/comments", g.BaseURL, owner, name, number)
	if _, err := doJSON(ctx, g.HTTPClient, http.MethodGet, u, g.headers(), nil, &comments); err != nil {
		return err
	}
	for _, c := range comments {
		if containsMarker(c.Body, marker) {
			u := fmt.Sprintf("%s/repos/%s/%s/issues/comments/%d", g.BaseURL, owner, name, c.ID)
			_, err := doJSON(ctx, g.HTTPClient, http.MethodPatch, u, g.headers(), map[string]string{"body": full}, nil)
			return err
		}
	}
	_, err = doJSON(ctx, g.HTTPClient, http.MethodPost, u, g.headers(), map[string]string{"body": full}, nil)
	return err
}

// AuthorCanWrite implements the §6.3 rights check: GET
// /repos/{owner}/{repo}/collaborators/{username}/permission.
func (g *Gitea) AuthorCanWrite(ctx context.Context, repo, username string, userID int64) (bool, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return false, err
	}
	if username == "" || strings.ContainsAny(username, " /?#%") {
		return false, fmt.Errorf("invalid username")
	}
	u := fmt.Sprintf("%s/repos/%s/%s/collaborators/%s/permission", g.BaseURL, owner, name, username)
	var perm struct {
		Permission string `json:"permission"`
	}
	status, err := doJSON(ctx, g.HTTPClient, http.MethodGet, u, g.headers(), nil, &perm)
	if status == http.StatusNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return perm.Permission == "write" || perm.Permission == "admin", nil
}
