package gitforge

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// GitLab talks to the GitLab REST API v4 (protocols §4.4). repo is the
// project id (previews.repo_reference), already URL-safe by construction.
type GitLab struct {
	// BaseURL is the API root, e.g. https://gitlab.com/api/v4 (git_sources.api_url).
	BaseURL string
	// Token is a project/group/personal access token, scope api (§4.1).
	Token      string
	HTTPClient *http.Client
}

func (g *GitLab) headers() map[string]string {
	// PRIVATE-TOKEN is the header GitLab documents for access tokens.
	return map[string]string{"PRIVATE-TOKEN": g.Token}
}

// SetCommitStatus implements POST /projects/:id/statuses/:sha (§4.4). The
// status shows in the MR and is usable in merge rules.
func (g *GitLab) SetCommitStatus(ctx context.Context, repo, sha string, state StatusState, targetURL string) error {
	glState := map[StatusState]string{
		StatusQueued:  "pending",
		StatusRunning: "running",
		StatusSuccess: "success",
		StatusFailure: "failed",
	}[state]
	if glState == "" {
		return fmt.Errorf("unmapped status state %q", state)
	}
	payload := map[string]string{"state": glState, "name": StatusContext}
	if targetURL != "" {
		payload["target_url"] = targetURL
	}
	u := fmt.Sprintf("%s/projects/%s/statuses/%s", g.BaseURL, url.PathEscape(repo), url.PathEscape(sha))
	_, err := doJSON(ctx, g.HTTPClient, http.MethodPost, u, g.headers(), payload, nil)
	return err
}

// UpsertComment finds THE preview note by its marker and updates it in
// place, creating it on first transition (§4.4: GET then PUT, else POST).
func (g *GitLab) UpsertComment(ctx context.Context, repo string, number int, marker, body string) error {
	full := markerHTML(marker) + "\n" + body
	notes, err := g.listNotes(ctx, repo, number)
	if err != nil {
		return err
	}
	for _, n := range notes {
		if containsMarker(n.Body, marker) {
			u := fmt.Sprintf("%s/projects/%s/merge_requests/%d/notes/%d", g.BaseURL, url.PathEscape(repo), number, n.ID)
			_, err := doJSON(ctx, g.HTTPClient, http.MethodPut, u, g.headers(), map[string]string{"body": full}, nil)
			return err
		}
	}
	u := fmt.Sprintf("%s/projects/%s/merge_requests/%d/notes", g.BaseURL, url.PathEscape(repo), number)
	_, err = doJSON(ctx, g.HTTPClient, http.MethodPost, u, g.headers(), map[string]string{"body": full}, nil)
	return err
}

type gitlabNote struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
}

func (g *GitLab) listNotes(ctx context.Context, repo string, number int) ([]gitlabNote, error) {
	// One page of 100 is enough: the marked note is AkerDock's own and long
	// MR threads bury old notes, not the bot's single updated one.
	u := fmt.Sprintf("%s/projects/%s/merge_requests/%d/notes?per_page=100", g.BaseURL, url.PathEscape(repo), number)
	var notes []gitlabNote
	if _, err := doJSON(ctx, g.HTTPClient, http.MethodGet, u, g.headers(), nil, &notes); err != nil {
		return nil, err
	}
	return notes, nil
}

// AuthorCanWrite implements the §4.3 rights check: GET
// /projects/:id/members/all/:user_id, access_level >= 30 (Developer).
func (g *GitLab) AuthorCanWrite(ctx context.Context, repo, _ string, userID int64) (bool, error) {
	u := fmt.Sprintf("%s/projects/%s/members/all/%d", g.BaseURL, url.PathEscape(repo), userID)
	var member struct {
		AccessLevel int `json:"access_level"`
	}
	status, err := doJSON(ctx, g.HTTPClient, http.MethodGet, u, g.headers(), nil, &member)
	if status == http.StatusNotFound {
		// Not a member at all: no rights, and not an error.
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return member.AccessLevel >= 30, nil
}
