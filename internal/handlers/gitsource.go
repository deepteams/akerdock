package handlers

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/store"
)

// safePathFormat bounds repository-relative paths reaching a remote shell
// (INV-012): no spaces, quotes, or traversal.
var safePathFormat = regexp.MustCompile(`^/?[A-Za-z0-9][A-Za-z0-9._/-]*$|^/$`)

// branchFormat bounds git branch names used in remote commands.
var branchFormat = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// sshRepoFormat bounds the two SSH forms git accepts, and nothing else: the
// scp-like `git@host:org/repo.git` and `ssh://git@host[:port]/org/repo.git`.
// The whole URL is interpolated into a remote command (INV-012), so the
// grammar is closed: no space, quote, backslash or shell metacharacter can
// appear, and the user part is a plain identifier — not `-oProxyCommand=...`,
// which git would otherwise hand to ssh as an option.
var sshRepoFormat = regexp.MustCompile(
	`^(?:ssh://)?[A-Za-z0-9._-]+@[A-Za-z0-9.-]+(?::[0-9]{1,5})?[:/][A-Za-z0-9][A-Za-z0-9._/-]*$`)

// isSSHRepo reports whether the URL is an SSH remote (deploy-key territory).
func isSSHRepo(rawURL string) bool {
	return strings.HasPrefix(rawURL, "ssh://") || (strings.Contains(rawURL, "@") && !strings.Contains(rawURL, "://"))
}

// validateGitSource applies the §23.3 URL policy. A public repository is
// cloned over https (git:// and http:// are tolerated for air-gapped and test
// setups); a private one over SSH with a deploy key. A credential embedded in
// the URL is always refused (INV-003) — the key is the credential.
func validateGitSource(rawURL, branch string, deployKey bool) []api.ErrorDetail {
	var details []api.ErrorDetail
	switch {
	case isSSHRepo(rawURL):
		switch {
		case !sshRepoFormat.MatchString(rawURL):
			details = append(details, api.ErrorDetail{Field: ptr("git_repository"), Code: ptr("invalid"), Message: "invalid SSH repository URL — expected git@host:org/repo.git or ssh://git@host/org/repo.git"})
		case !deployKey:
			details = append(details, api.ErrorDetail{Field: ptr("private_key_uuid"), Code: ptr("required"), Message: "an SSH repository requires a deploy key (private_key_uuid)"})
		}
	case deployKey:
		details = append(details, api.ErrorDetail{Field: ptr("git_repository"), Code: ptr("invalid"), Message: "a deploy key is only used with an SSH repository URL (git@host:org/repo.git)"})
	default:
		u, err := url.Parse(rawURL)
		switch {
		case err != nil, u.Host == "":
			details = append(details, api.ErrorDetail{Field: ptr("git_repository"), Code: ptr("invalid"), Message: "invalid repository URL"})
		case u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "git":
			details = append(details, api.ErrorDetail{Field: ptr("git_repository"), Code: ptr("invalid"), Message: "repository URL must use https (recommended), http, git, or SSH with a deploy key"})
		case u.User != nil:
			details = append(details, api.ErrorDetail{Field: ptr("git_repository"), Code: ptr("invalid"), Message: "credentials in the repository URL are forbidden (INV-003) — use a deploy key"})
		}
	}
	if !branchFormat.MatchString(branch) {
		details = append(details, api.ErrorDetail{Field: ptr("git_branch"), Code: ptr("invalid"), Message: "invalid branch name"})
	}
	return details
}

// gitProviderOf derives the provider from the host, for the git_sources row
// (data-dictionary §7.1). It is informational: nothing branches on it yet.
func gitProviderOf(rawURL string) store.GitProvider {
	host := rawURL
	if _, after, ok := strings.Cut(host, "@"); ok {
		host = after
	}
	if _, after, ok := strings.Cut(host, "://"); ok {
		host = after
	}
	host, _, _ = strings.Cut(host, "/")
	host, _, _ = strings.Cut(host, ":")
	switch {
	case strings.Contains(host, "github."):
		return store.GitProviderGithub
	case strings.Contains(host, "gitlab."):
		return store.GitProviderGitlab
	case strings.Contains(host, "bitbucket."):
		return store.GitProviderBitbucket
	case strings.Contains(host, "gitea."):
		return store.GitProviderGitea
	default:
		return store.GitProviderOther
	}
}

// imageWithTag bounds container image references reaching a remote shell
// (INV-012); identifierFormat bounds PostgreSQL user/database names.
var (
	imageWithTag     = regexp.MustCompile(`^[a-z0-9]+((\.|_{1,2}|-+|/|:[0-9]+/)[a-z0-9]+)*(:[A-Za-z0-9_][A-Za-z0-9._-]{0,127})?$`)
	identifierFormat = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,62}$`)
)

// deployKeySource returns the git_sources row binding this team to this
// deploy key, creating it on first use (data-dictionary §7.1). The API speaks
// in keys, so several applications sharing a key share one source.
func (a *API) deployKeySource(r *http.Request, id *auth.Identity, key store.PrivateKey, repoURL string) (store.GitSource, error) {
	source, err := a.Store.GetDeployKeySource(r.Context(), store.GetDeployKeySourceParams{
		TeamID: id.TeamID, PrivateKeyID: ptr(key.ID),
	})
	if err == nil {
		return source, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return store.GitSource{}, err
	}
	return a.Store.CreateGitSource(r.Context(), store.CreateGitSourceParams{
		TeamID:       id.TeamID,
		Name:         "deploy-key:" + uuidString(key.Uuid),
		Kind:         store.GitSourceKindDeployKey,
		Provider:     gitProviderOf(repoURL),
		PrivateKeyID: ptr(key.ID),
	})
}
