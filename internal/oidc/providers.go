package oidc

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Kind separates the two families of §10.2: OpenID Connect (a signed ID
// token comes back and is verified) and plain OAuth2 (no ID token — the
// identity is read from the provider's own API over TLS, authenticated by
// the access token we just exchanged the code for).
type Kind int

// Provider kinds (§10.2): OIDC discovery vs bare OAuth2.
const (
	KindOIDC Kind = iota
	KindOAuth2
)

// Profile is the static shape of one provider. Endpoints are pinned for the
// hosted providers: a configurable endpoint on a fixed brand would let a
// mistyped config send our client secret elsewhere. Only 'oidc' and 'azure'
// take an issuer from configuration — that is their point.
type Profile struct {
	Kind Kind
	// DefaultName labels the sign-in button when the operator set none.
	DefaultName string
	// Issuer is fixed for google, empty for the issuer-from-config providers.
	Issuer string
	// NeedsIssuer marks the providers whose configuration must carry
	// issuer_url (oidc, azure).
	NeedsIssuer bool
	// Fixed endpoints for the KindOAuth2 providers.
	Endpoints Endpoints
	Scopes    []string
}

// Profiles maps the oauth_provider enum to its behavior. Keys mirror the
// database enum values.
var Profiles = map[string]Profile{
	"google": {
		Kind: KindOIDC, DefaultName: "Google",
		Issuer: "https://accounts.google.com",
		Scopes: []string{"openid", "email", "profile"},
	},
	"azure": {
		Kind: KindOIDC, DefaultName: "Microsoft",
		NeedsIssuer: true, // https://login.microsoftonline.com/{tenant}/v2.0
		Scopes:      []string{"openid", "email", "profile"},
	},
	"oidc": {
		Kind: KindOIDC, DefaultName: "SSO",
		NeedsIssuer: true,
		Scopes:      []string{"openid", "email", "profile"},
	},
	"github": {
		Kind: KindOAuth2, DefaultName: "GitHub",
		Endpoints: Endpoints{
			AuthorizeURL: "https://github.com/login/oauth/authorize",
			TokenURL:     "https://github.com/login/oauth/access_token",
			UserinfoURL:  "https://api.github.com/user",
		},
		Scopes: []string{"read:user", "user:email"},
	},
	"gitlab": {
		Kind: KindOAuth2, DefaultName: "GitLab",
		Endpoints: Endpoints{
			AuthorizeURL: "https://gitlab.com/oauth/authorize",
			TokenURL:     "https://gitlab.com/oauth/token",
			UserinfoURL:  "https://gitlab.com/api/v4/user",
		},
		Scopes: []string{"read_user"},
	},
	"bitbucket": {
		Kind: KindOAuth2, DefaultName: "Bitbucket",
		Endpoints: Endpoints{
			AuthorizeURL: "https://bitbucket.org/site/oauth2/authorize",
			TokenURL:     "https://bitbucket.org/site/oauth2/access_token",
			UserinfoURL:  "https://api.bitbucket.org/2.0/user",
		},
		Scopes: []string{"account", "email"},
	},
}

// FetchOAuth2Identity reads the identity from a plain-OAuth2 provider's API.
// The subject is the provider's immutable account id — NEVER the login name,
// which can be renamed and re-registered by someone else (§23.3: the subject
// is the key, everything else is a claim).
func (c *Client) FetchOAuth2Identity(ctx context.Context, provider string, ep *Endpoints, accessToken string) (*Identity, error) {
	switch provider {
	case "github":
		return c.githubIdentity(ctx, ep, accessToken)
	case "gitlab":
		return c.gitlabIdentity(ctx, ep, accessToken)
	case "bitbucket":
		return c.bitbucketIdentity(ctx, ep, accessToken)
	default:
		return nil, fmt.Errorf("provider %q has no userinfo profile", provider)
	}
}

func (c *Client) githubIdentity(ctx context.Context, ep *Endpoints, token string) (*Identity, error) {
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
		Name  string `json:"name"`
	}
	if err := c.getJSON(ctx, ep.UserinfoURL, token, &user); err != nil {
		return nil, err
	}
	if user.ID == 0 {
		return nil, errors.New("github: userinfo lacks an account id")
	}
	// The /user email field is the PUBLIC profile email — often empty, never
	// necessarily verified. The dedicated endpoint says which address GitHub
	// actually verified.
	var emails []struct {
		Email    string `json:"email"`
		Primary  bool   `json:"primary"`
		Verified bool   `json:"verified"`
	}
	if err := c.getJSON(ctx, ep.UserinfoURL+"/emails", token, &emails); err != nil {
		return nil, err
	}
	id := &Identity{
		Subject: strconv.FormatInt(user.ID, 10),
		Name:    firstNonEmpty(user.Name, user.Login),
	}
	for _, e := range emails {
		if e.Primary && e.Verified {
			id.Email, id.EmailVerified = NormalizeEmail(e.Email), true
			break
		}
	}
	return id, nil
}

func (c *Client) gitlabIdentity(ctx context.Context, ep *Endpoints, token string) (*Identity, error) {
	var user struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		Name     string `json:"name"`
		// /api/v4/user on the token's own account returns the primary email,
		// which GitLab only sets once confirmed.
		Email string `json:"email"`
	}
	if err := c.getJSON(ctx, ep.UserinfoURL, token, &user); err != nil {
		return nil, err
	}
	if user.ID == 0 {
		return nil, errors.New("gitlab: userinfo lacks an account id")
	}
	return &Identity{
		Subject:       strconv.FormatInt(user.ID, 10),
		Email:         NormalizeEmail(user.Email),
		EmailVerified: user.Email != "",
		Name:          firstNonEmpty(user.Name, user.Username),
	}, nil
}

func (c *Client) bitbucketIdentity(ctx context.Context, ep *Endpoints, token string) (*Identity, error) {
	var user struct {
		UUID        string `json:"uuid"`
		DisplayName string `json:"display_name"`
		Username    string `json:"username"`
	}
	if err := c.getJSON(ctx, ep.UserinfoURL, token, &user); err != nil {
		return nil, err
	}
	if user.UUID == "" {
		return nil, errors.New("bitbucket: userinfo lacks an account uuid")
	}
	var emails struct {
		Values []struct {
			Email     string `json:"email"`
			Primary   bool   `json:"is_primary"`
			Confirmed bool   `json:"is_confirmed"`
		} `json:"values"`
	}
	if err := c.getJSON(ctx, ep.UserinfoURL+"/emails", token, &emails); err != nil {
		return nil, err
	}
	id := &Identity{
		// Bitbucket brackets its uuids ({...}); strip for a stable bare form.
		Subject: strings.Trim(user.UUID, "{}"),
		Name:    firstNonEmpty(user.DisplayName, user.Username),
	}
	for _, e := range emails.Values {
		if e.Primary && e.Confirmed {
			id.Email, id.EmailVerified = NormalizeEmail(e.Email), true
			break
		}
	}
	return id, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
