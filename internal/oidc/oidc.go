// Package oidc implements the relying-party side of the dashboard's
// federated login (PRD §10.2): OpenID Connect for Google, Azure and a
// generic IdP, plain OAuth2-plus-userinfo for GitHub, GitLab and Bitbucket.
//
// It is written in-house for the same reason internal/totp is: the parts we
// need — one discovery GET, one authorize URL, one form POST and an RS256
// signature check — are small enough to read in one sitting and to pin to
// the specs' own test vectors, where a general-purpose library would be a
// far larger surface to audit.
//
// The §23.3 requirements are enforced structurally, not by caller
// discipline:
//
//   - PKCE (S256) on every flow, OIDC or not;
//   - the ID token is accepted ONLY with alg RS256 and a key from the
//     issuer's JWKS — "none" and HMAC confusions die in one place;
//   - issuer, audience, expiry and nonce are checked before any claim is
//     believed;
//   - emails come out normalized (lower-case, trimmed) with their
//     verification status, so the account-collision decision upstream works
//     on canonical values.
package oidc

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

var randomReader io.Reader = rand.Reader

// Timeout bounds every call to an identity provider: a slow IdP must delay
// one login, not pile up handler goroutines.
const Timeout = 10 * time.Second

// Endpoints is what a flow needs to know about a provider, discovered
// (OIDC) or fixed (OAuth2 profiles).
type Endpoints struct {
	Issuer       string
	AuthorizeURL string
	TokenURL     string
	JWKSURL      string
	UserinfoURL  string // OAuth2 profiles only
}

// Identity is the verified outcome of a callback: who the provider says
// this is. Email is normalized; Subject is the provider's stable id — the
// ONLY join key for identities (§23.3: email is a claim, not a key).
type Identity struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

// Client talks to identity providers.
type Client struct {
	HTTP *http.Client
}

// New returns a client with the package timeout.
func New() *Client {
	return &Client{HTTP: &http.Client{Timeout: Timeout}}
}

// --- issuer validation -----------------------------------------------------

// ValidateIssuer refuses issuer URLs a misled root user could be tricked
// into: only https (plain http tolerated for localhost alone, the dev
// fallback the passkeys already use), no query, no fragment.
func ValidateIssuer(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("issuer_url is not a valid URL")
	}
	host := u.Hostname()
	switch {
	case u.Scheme == "https":
		// fine
	case u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1"):
		// dev instance without TLS — same tolerance as the passkey RP
	default:
		return fmt.Errorf("issuer_url must be https")
	}
	if host == "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("issuer_url must be a bare https URL, without query or fragment")
	}
	return nil
}

// --- discovery ---------------------------------------------------------------

// discoveryDoc is the subset of the OpenID Provider Metadata we consume.
type discoveryDoc struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// Discover fetches issuer/.well-known/openid-configuration and returns the
// endpoints. The document's own issuer must equal the configured one
// (discovery spec §4.3): an IdP that says it is someone else is either
// broken or hostile, and both mean no.
func (c *Client) Discover(ctx context.Context, issuer string) (*Endpoints, error) {
	wellKnown := strings.TrimSuffix(issuer, "/") + "/.well-known/openid-configuration"
	var doc discoveryDoc
	if err := c.getJSON(ctx, wellKnown, "", &doc); err != nil {
		return nil, fmt.Errorf("oidc discovery %s: %w", wellKnown, err)
	}
	if doc.Issuer != issuer && doc.Issuer != strings.TrimSuffix(issuer, "/") {
		return nil, fmt.Errorf("oidc discovery: the document names issuer %q, expected %q", doc.Issuer, issuer)
	}
	if doc.AuthorizationEndpoint == "" || doc.TokenEndpoint == "" || doc.JWKSURI == "" {
		return nil, errors.New("oidc discovery: document lacks authorization_endpoint, token_endpoint or jwks_uri")
	}
	return &Endpoints{
		Issuer:       doc.Issuer,
		AuthorizeURL: doc.AuthorizationEndpoint,
		TokenURL:     doc.TokenEndpoint,
		JWKSURL:      doc.JWKSURI,
	}, nil
}

// --- PKCE (RFC 7636) ---------------------------------------------------------

// NewVerifier mints a PKCE code_verifier: 32 random bytes, base64url.
func NewVerifier() (string, error) {
	raw := make([]byte, 32)
	if _, err := io.ReadFull(randomReader, raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// ChallengeS256 derives the code_challenge (RFC 7636 §4.2).
func ChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// --- authorize ---------------------------------------------------------------

// AuthorizeURL builds the front-channel redirect. The nonce parameter is
// sent even to plain-OAuth2 providers, which ignore it — harmless there,
// mandatory where an ID token comes back.
func AuthorizeURL(ep *Endpoints, clientID, redirectURI, state, nonce, verifier string, scopes []string) string {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"scope":                 {strings.Join(scopes, " ")},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {ChallengeS256(verifier)},
		"code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(ep.AuthorizeURL, "?") {
		sep = "&"
	}
	return ep.AuthorizeURL + sep + q.Encode()
}

// --- token exchange ----------------------------------------------------------

// TokenResponse is the back-channel answer to the code exchange.
type TokenResponse struct {
	AccessToken string `json:"access_token"`
	IDToken     string `json:"id_token"`
	TokenType   string `json:"token_type"`
}

// Exchange redeems the authorization code, authenticating as the
// confidential client (client_secret_basic — the registration default of
// every IdP we target) and proving code possession with the PKCE verifier.
func (c *Client) Exchange(ctx context.Context, ep *Endpoints, clientID, clientSecret, redirectURI, code, verifier string) (*TokenResponse, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
		// Some providers (GitHub among them) want the client id in the body
		// even with basic auth; the duplicate is harmless elsewhere.
		"client_id": {clientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json") // GitHub answers form-encoded without it
	req.SetBasicAuth(url.QueryEscape(clientID), url.QueryEscape(clientSecret))

	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		// The provider's error body names OUR misconfiguration (bad secret,
		// bad redirect) — worth surfacing to the operator's logs, bounded.
		return nil, fmt.Errorf("token exchange: %s answered %d: %.200s", ep.TokenURL, res.StatusCode, body)
	}
	var tok TokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("token exchange: undecodable response: %w", err)
	}
	if tok.AccessToken == "" {
		return nil, errors.New("token exchange: no access_token in response")
	}
	return &tok, nil
}

// --- ID token verification ----------------------------------------------------

// ErrIDTokenInvalid covers every verification failure of an ID token. One
// error on purpose: the sign-in page has no business distinguishing a bad
// signature from a bad nonce, and an attacker even less.
var ErrIDTokenInvalid = errors.New("ID token verification failed")

// idTokenClaims is the subset of claims we consume.
type idTokenClaims struct {
	Issuer   string          `json:"iss"`
	Subject  string          `json:"sub"`
	Audience json.RawMessage `json:"aud"` // string or array of strings
	Expiry   int64           `json:"exp"`
	Nonce    string          `json:"nonce"`
	Email    string          `json:"email"`
	// Some IdPs (Azure AD B2C notably) serialize the boolean as a string.
	EmailVerified json.RawMessage `json:"email_verified"`
	Name          string          `json:"name"`
	PreferredName string          `json:"preferred_username"`
}

// VerifyIDToken validates the token against the issuer's JWKS and the §23.3
// checklist — signature (RS256 only), issuer, audience, expiry, nonce — and
// only then reads the identity out of it.
func (c *Client) VerifyIDToken(ctx context.Context, ep *Endpoints, clientID, nonce, raw string, now time.Time) (*Identity, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, ErrIDTokenInvalid
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, ErrIDTokenInvalid
	}
	var header struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, ErrIDTokenInvalid
	}
	// RS256 only. "none" is an attack, and any HMAC alg would turn the
	// PUBLIC key into a signing key (the classic confusion) — there is no
	// legitimate reason for the IdPs we target to sign with anything else.
	if header.Alg != "RS256" {
		return nil, ErrIDTokenInvalid
	}

	key, err := c.jwksKey(ctx, ep.JWKSURL, header.Kid)
	if err != nil {
		return nil, ErrIDTokenInvalid
	}
	signed := []byte(parts[0] + "." + parts[1])
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, ErrIDTokenInvalid
	}
	digest := sha256.Sum256(signed)
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig) != nil {
		return nil, ErrIDTokenInvalid
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, ErrIDTokenInvalid
	}
	var claims idTokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, ErrIDTokenInvalid
	}

	switch {
	case claims.Issuer != ep.Issuer:
		return nil, ErrIDTokenInvalid
	case !audienceContains(claims.Audience, clientID):
		return nil, ErrIDTokenInvalid
	case claims.Expiry <= 0 || now.After(time.Unix(claims.Expiry, 0)):
		return nil, ErrIDTokenInvalid
	case claims.Nonce != nonce || nonce == "":
		return nil, ErrIDTokenInvalid
	case claims.Subject == "":
		return nil, ErrIDTokenInvalid
	}

	name := claims.Name
	if name == "" {
		name = claims.PreferredName
	}
	return &Identity{
		Subject:       claims.Subject,
		Email:         NormalizeEmail(claims.Email),
		EmailVerified: truthy(claims.EmailVerified),
		Name:          name,
	}, nil
}

// audienceContains handles aud as a string OR an array of strings, as the
// JWT spec allows and Azure exercises.
func audienceContains(raw json.RawMessage, clientID string) bool {
	if len(raw) == 0 {
		return false
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return one == clientID
	}
	var many []string
	return json.Unmarshal(raw, &many) == nil && slices.Contains(many, clientID)
}

// truthy accepts the boolean and the string spelling of email_verified.
func truthy(raw json.RawMessage) bool {
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return b
	}
	var s string
	return json.Unmarshal(raw, &s) == nil && strings.EqualFold(s, "true")
}

// NormalizeEmail is the §23.3 canonical form: what gets compared, stored
// and joined on. citext in the database makes comparison case-insensitive
// anyway; normalizing here keeps what we DISPLAY consistent too.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// --- JWKS ---------------------------------------------------------------------

// jwks is the issuer's published key set.
type jwks struct {
	Keys []struct {
		Kty string `json:"kty"`
		Kid string `json:"kid"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

// jwksKey fetches the key set and returns the RSA key named by kid. Fetched
// per login, not cached: a dashboard login is rare enough that correctness
// on rotation beats saving one HTTPS GET.
func (c *Client) jwksKey(ctx context.Context, jwksURL, kid string) (*rsa.PublicKey, error) {
	var set jwks
	if err := c.getJSON(ctx, jwksURL, "", &set); err != nil {
		return nil, err
	}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (kid != "" && k.Kid != kid) {
			continue
		}
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		return &rsa.PublicKey{
			N: new(big.Int).SetBytes(n),
			E: int(new(big.Int).SetBytes(e).Int64()),
		}, nil
	}
	return nil, fmt.Errorf("jwks: no RSA key %q at %s", kid, jwksURL)
}

// getJSON fetches and decodes a JSON document, optionally with a bearer
// token, bounded in size and time.
func (c *Client) getJSON(ctx context.Context, rawURL, bearer string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", rawURL, res.StatusCode)
	}
	return json.Unmarshal(body, into)
}
