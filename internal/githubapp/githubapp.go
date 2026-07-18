// Package githubapp implements the GitHub App integration of
// git-webhook-protocols.md §2: manifest conversion, App JWTs, installation
// access tokens, repository discovery and the rich preview feedback (checks,
// deployments, upserted PR comment).
//
// Every call goes through an injectable HTTP client against a configurable
// api_url (GitHub Enterprise, §2.6) — which is also what makes the package
// unit-testable against httptest, without GitHub.
package githubapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// AppJWT signs the App-level JWT (§2.2): RS256, iss = app id, iat backdated
// 60 s for clock tolerance, exp = iat + 9 min (GitHub caps at 10).
func AppJWT(appID int64, privateKeyPEM []byte, now time.Time) (string, error) {
	block, _ := pem.Decode(privateKeyPEM)
	if block == nil {
		return "", fmt.Errorf("githubapp: private key is not PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		// Manifest conversions return PKCS#1, but accept PKCS#8 too.
		parsed, err8 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err8 != nil {
			return "", fmt.Errorf("githubapp: parse private key: %w", err)
		}
		var ok bool
		if key, ok = parsed.(*rsa.PrivateKey); !ok {
			return "", fmt.Errorf("githubapp: private key is not RSA")
		}
	}

	b64 := func(v any) (string, error) {
		raw, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		return base64.RawURLEncoding.EncodeToString(raw), nil
	}
	header, err := b64(map[string]string{"alg": "RS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := b64(map[string]any{
		"iss": strconv.FormatInt(appID, 10),
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
	})
	if err != nil {
		return "", err
	}
	signing := header + "." + claims
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// Manifest renders the App manifest of §2.1: minimal permissions, app-level
// webhook, JSON content type.
func Manifest(instanceURL, appUUID, name string) map[string]any {
	base := strings.TrimRight(instanceURL, "/")
	return map[string]any{
		"name": name,
		"url":  base,
		"hook_attributes": map[string]any{
			"url":    base + "/webhooks/github/apps/" + appUUID,
			"active": true,
		},
		"redirect_url":   base + "/webhooks/github/manifest/callback",
		"setup_url":      base + "/github-apps/" + appUUID + "/setup",
		"public":         false,
		"default_events": []string{"push", "pull_request", "installation_repositories", "issue_comment"},
		"default_permissions": map[string]string{
			"contents":      "read",
			"metadata":      "read",
			"pull_requests": "write",
			"checks":        "write",
			"deployments":   "write",
			"issues":        "read",
		},
	}
}

// Client talks to one GitHub API host.
type Client struct {
	// APIURL is https://api.github.com or the GHES /api/v3 base — never a
	// hardcoded default at call sites (§2.6).
	APIURL string
	HTTP   *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return defaultHTTP
}

// defaultHTTP trusts the system roots plus AKERDOCK_GITHUB_CA_FILE when set:
// a GitHub Enterprise Server behind a private CA (protocols §2.6) has to be
// verifiable, and macOS ignores SSL_CERT_FILE — Go's darwin roots come from
// the Security framework. Read once: the CA of an instance's GHES does not
// change while the process runs.
var defaultHTTP = buildDefaultHTTP()

func buildDefaultHTTP() *http.Client {
	path := os.Getenv("AKERDOCK_GITHUB_CA_FILE")
	if path == "" {
		return http.DefaultClient
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return http.DefaultClient
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(raw) {
		return http.DefaultClient
	}
	return &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
}

func (c *Client) do(ctx context.Context, method, path, bearer string, body any, out any) error {
	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.APIURL, "/")+path, payload)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return &APIError{Status: res.StatusCode, Body: string(raw), Path: path}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(res.Body).Decode(out)
}

// APIError is a non-2xx GitHub answer; the body is bounded and never contains
// our credentials.
type APIError struct {
	Status int
	Path   string
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github: %s answered %d: %s", e.Path, e.Status, e.Body)
}

// IsNotFound reports a 404 — used to distinguish "gone" from "broken".
func (e *APIError) IsNotFound() bool { return e.Status == http.StatusNotFound }

// AppCredentials is what a manifest conversion returns (§2.1 step 5) — never
// logged (INV-003).
type AppCredentials struct {
	AppID         int64  `json:"id"`
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	WebhookSecret string `json:"webhook_secret"`
	PEM           string `json:"pem"`
	HTMLURL       string `json:"html_url"`
}

// ConvertManifest exchanges the one-shot code for the App credentials —
// unauthenticated by design, the code IS the proof (§2.1 step 5).
func (c *Client) ConvertManifest(ctx context.Context, code string) (AppCredentials, error) {
	var out AppCredentials
	err := c.do(ctx, http.MethodPost, "/app-manifests/"+code+"/conversions", "", nil, &out)
	return out, err
}

// InstallationToken mints a one-hour installation access token (§2.2),
// restricted to the given repositories when known — least privilege if the
// token ever leaks into a build log.
type InstallationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (c *Client) InstallationToken(ctx context.Context, appJWT string, installationID int64, repositories []string) (InstallationToken, error) {
	var body any
	if len(repositories) > 0 {
		body = map[string]any{"repositories": repositories}
	}
	var out InstallationToken
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", installationID), appJWT, body, &out)
	return out, err
}

// Repo is one discovered repository (§7.3): ExternalID is the stable identity.
type Repo struct {
	ID            int64  `json:"id"`
	FullName      string `json:"full_name"`
	DefaultBranch string `json:"default_branch"`
	HTMLURL       string `json:"html_url"`
	Private       bool   `json:"private"`
}

// ListInstallationRepos pages through everything the installation can see.
func (c *Client) ListInstallationRepos(ctx context.Context, installationToken string) ([]Repo, error) {
	var all []Repo
	for page := 1; ; page++ {
		var out struct {
			TotalCount   int    `json:"total_count"`
			Repositories []Repo `json:"repositories"`
		}
		path := fmt.Sprintf("/installation/repositories?per_page=100&page=%d", page)
		if err := c.do(ctx, http.MethodGet, path, installationToken, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Repositories...)
		if len(all) >= out.TotalCount || len(out.Repositories) == 0 {
			return all, nil
		}
	}
}

// CheckRun mirrors the fields of §2.7a the engine drives.
type CheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type CheckRunInput struct {
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha,omitempty"`
	Status     string `json:"status,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
	DetailsURL string `json:"details_url,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
	Output     *struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	} `json:"output,omitempty"`
}

func (c *Client) CreateCheckRun(ctx context.Context, token, fullName string, in CheckRunInput) (CheckRun, error) {
	var out CheckRun
	err := c.do(ctx, http.MethodPost, "/repos/"+fullName+"/check-runs", token, in, &out)
	return out, err
}

func (c *Client) UpdateCheckRun(ctx context.Context, token, fullName string, id int64, in CheckRunInput) error {
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/check-runs/%d", fullName, id), token, in, nil)
}

// CreateDeployment materializes "View deployment" on the PR (§2.7b).
func (c *Client) CreateDeployment(ctx context.Context, token, fullName, ref, environment string) (int64, error) {
	var out struct {
		ID int64 `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, "/repos/"+fullName+"/deployments", token, map[string]any{
		"ref":                   ref,
		"environment":           environment,
		"transient_environment": true,
		"production_environment": false,
		"auto_merge":            false,
		"required_contexts":     []string{},
	}, &out)
	return out.ID, err
}

func (c *Client) CreateDeploymentStatus(ctx context.Context, token, fullName string, deploymentID int64, state, environmentURL string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/deployments/%d/statuses", fullName, deploymentID), token, map[string]any{
		"state":           state,
		"environment_url": environmentURL,
	}, nil)
}

// CollaboratorCanWrite reports whether username may command a deployment on
// this repo (§2.7d): permission write or admin. A non-collaborator is a
// plain "no", not an error.
func (c *Client) CollaboratorCanWrite(ctx context.Context, token, fullName, username string) (bool, error) {
	var out struct {
		Permission string `json:"permission"`
	}
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/collaborators/%s/permission", fullName, username), token, nil, &out)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return false, nil
		}
		return false, err
	}
	return out.Permission == "write" || out.Permission == "admin", nil
}

// UpsertPRComment maintains the SINGLE preview comment of a PR (§20.4.6):
// found by its invisible marker and PATCHed in place, created once otherwise
// — never one comment per deployment.
func (c *Client) UpsertPRComment(ctx context.Context, token, fullName string, prNumber int, marker, body string) error {
	markerTag := "<!-- akerdock:" + marker + " -->"
	full := markerTag + "\n" + body

	for page := 1; page <= 10; page++ {
		var comments []struct {
			ID   int64  `json:"id"`
			Body string `json:"body"`
		}
		path := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100&page=%d", fullName, prNumber, page)
		if err := c.do(ctx, http.MethodGet, path, token, nil, &comments); err != nil {
			return err
		}
		for _, comment := range comments {
			if strings.Contains(comment.Body, markerTag) {
				return c.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/comments/%d", fullName, comment.ID), token,
					map[string]string{"body": full}, nil)
			}
		}
		if len(comments) < 100 {
			break
		}
	}
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/issues/%d/comments", fullName, prNumber), token,
		map[string]string{"body": full}, nil)
}
