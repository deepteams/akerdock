package oidc

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeIdP is a minimal OpenID provider: discovery, JWKS, and a signing key.
type fakeIdP struct {
	key    *rsa.PrivateKey
	server *httptest.Server
	issuer string
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIdP{key: key}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 idp.issuer,
			"authorization_endpoint": idp.issuer + "/authorize",
			"token_endpoint":         idp.issuer + "/token",
			"jwks_uri":               idp.issuer + "/jwks",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		pub := key.Public().(*rsa.PublicKey)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []map[string]string{{
				"kty": "RSA", "kid": "test-key", "alg": "RS256",
				"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
			}},
		})
	})
	idp.server = httptest.NewServer(mux)
	idp.issuer = idp.server.URL
	t.Cleanup(idp.server.Close)
	return idp
}

// sign issues an RS256 ID token with the given claims.
func (idp *fakeIdP) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test-key"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(header + "." + body))
	sig, err := rsa.SignPKCS1v15(rand.Reader, idp.key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return header + "." + body + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (idp *fakeIdP) claims() map[string]any {
	return map[string]any{
		"iss": idp.issuer, "sub": "user-42", "aud": "client-1",
		"exp": time.Now().Add(time.Hour).Unix(), "nonce": "nonce-1",
		"email": "Jean.Luc@Example.COM", "email_verified": true, "name": "Jean-Luc",
	}
}

func discover(t *testing.T, idp *fakeIdP) *Endpoints {
	t.Helper()
	ep, err := New().Discover(context.Background(), idp.issuer)
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

func TestDiscovery(t *testing.T) {
	idp := newFakeIdP(t)
	ep := discover(t, idp)
	if ep.AuthorizeURL != idp.issuer+"/authorize" || ep.JWKSURL != idp.issuer+"/jwks" {
		t.Fatalf("discovery returned %+v", ep)
	}
}

// A discovery document naming another issuer is either broken or hostile —
// both mean no (discovery spec §4.3).
func TestDiscoveryRefusesIssuerMismatch(t *testing.T) {
	lying := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 "https://somebody-else.example",
			"authorization_endpoint": "https://x/a", "token_endpoint": "https://x/t", "jwks_uri": "https://x/j",
		})
	}))
	defer lying.Close()
	if _, err := New().Discover(context.Background(), lying.URL); err == nil {
		t.Fatal("a discovery document naming another issuer was accepted")
	}
}

func TestVerifyIDToken(t *testing.T) {
	idp := newFakeIdP(t)
	ep := discover(t, idp)
	token := idp.sign(t, idp.claims())

	id, err := New().VerifyIDToken(context.Background(), ep, "client-1", "nonce-1", token, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "user-42" {
		t.Errorf("subject = %q", id.Subject)
	}
	// §23.3: the email comes out NORMALIZED.
	if id.Email != "jean.luc@example.com" {
		t.Errorf("email = %q, want normalized lower-case", id.Email)
	}
	if !id.EmailVerified || id.Name != "Jean-Luc" {
		t.Errorf("identity = %+v", id)
	}
}

// Every line of the §23.3 checklist, refused independently.
func TestVerifyIDTokenRefusals(t *testing.T) {
	idp := newFakeIdP(t)
	ep := discover(t, idp)
	c := New()
	now := time.Now()

	cases := map[string]func(m map[string]any){
		"wrong issuer":   func(m map[string]any) { m["iss"] = "https://evil.example" },
		"wrong audience": func(m map[string]any) { m["aud"] = "someone-else" },
		"expired":        func(m map[string]any) { m["exp"] = now.Add(-time.Hour).Unix() },
		"wrong nonce":    func(m map[string]any) { m["nonce"] = "replayed-nonce" },
		"missing sub":    func(m map[string]any) { delete(m, "sub") },
	}
	for name, mutate := range cases {
		claims := idp.claims()
		mutate(claims)
		token := idp.sign(t, claims)
		if _, err := c.VerifyIDToken(context.Background(), ep, "client-1", "nonce-1", token, now); err == nil {
			t.Errorf("%s: token was accepted", name)
		}
	}
}

// The audience may be an array (Azure does this): ours in the list passes,
// absent fails.
func TestVerifyIDTokenAudienceArray(t *testing.T) {
	idp := newFakeIdP(t)
	ep := discover(t, idp)
	c := New()

	claims := idp.claims()
	claims["aud"] = []string{"other", "client-1"}
	if _, err := c.VerifyIDToken(context.Background(), ep, "client-1", "nonce-1", idp.sign(t, claims), time.Now()); err != nil {
		t.Errorf("aud array containing the client was refused: %v", err)
	}
	claims["aud"] = []string{"other", "another"}
	if _, err := c.VerifyIDToken(context.Background(), ep, "client-1", "nonce-1", idp.sign(t, claims), time.Now()); err == nil {
		t.Error("aud array NOT containing the client was accepted")
	}
}

// The two classic signature attacks: alg=none (no signature at all) and the
// HMAC confusion (the public key used as an HMAC secret). Both die on the
// RS256-only rule before any crypto runs.
func TestVerifyIDTokenRefusesForgedAlgorithms(t *testing.T) {
	idp := newFakeIdP(t)
	ep := discover(t, idp)
	c := New()
	payload, _ := json.Marshal(idp.claims())
	body := base64.RawURLEncoding.EncodeToString(payload)

	// alg=none
	noneHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	if _, err := c.VerifyIDToken(context.Background(), ep, "client-1", "nonce-1", noneHeader+"."+body+".", time.Now()); err == nil {
		t.Fatal("an alg=none token was accepted")
	}

	// alg=HS256 signed with the PUBLIC key bytes
	hsHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","kid":"test-key"}`))
	pub := idp.key.Public().(*rsa.PublicKey)
	mac := hmac.New(sha256.New, pub.N.Bytes())
	mac.Write([]byte(hsHeader + "." + body))
	forged := hsHeader + "." + body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if _, err := c.VerifyIDToken(context.Background(), ep, "client-1", "nonce-1", forged, time.Now()); err == nil {
		t.Fatal("an HS256 token signed with the public key was accepted")
	}
}

// A tampered payload must fail the signature check even when every claim in
// it looks right.
func TestVerifyIDTokenRefusesTampering(t *testing.T) {
	idp := newFakeIdP(t)
	ep := discover(t, idp)
	token := idp.sign(t, idp.claims())
	parts := strings.Split(token, ".")

	claims := idp.claims()
	claims["sub"] = "user-1337" // privilege escalation attempt
	forgedPayload, _ := json.Marshal(claims)
	parts[1] = base64.RawURLEncoding.EncodeToString(forgedPayload)

	if _, err := New().VerifyIDToken(context.Background(), ep, "client-1", "nonce-1", strings.Join(parts, "."), time.Now()); err == nil {
		t.Fatal("a re-signed-by-nobody payload was accepted")
	}
}

// RFC 7636 appendix B: the reference verifier/challenge pair.
func TestPKCEChallengeVector(t *testing.T) {
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	want := "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := ChallengeS256(verifier); got != want {
		t.Fatalf("ChallengeS256 = %s, want %s", got, want)
	}
}

func TestNewVerifier(t *testing.T) {
	first, err := NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	if first == second || len(first) != 43 || strings.Contains(first, "=") {
		t.Fatalf("invalid verifier pair: %q %q", first, second)
	}

	old := randomReader
	randomReader = errorReader{}
	t.Cleanup(func() { randomReader = old })
	if _, err := NewVerifier(); err == nil {
		t.Fatal("entropy failure was not returned")
	}
}

func TestAuthorizeURL(t *testing.T) {
	ep := &Endpoints{AuthorizeURL: "https://idp.example/authorize"}
	u := AuthorizeURL(ep, "client-1", "https://app.example/cb", "state-1", "nonce-1", "verifier-1", []string{"openid", "email"})
	for _, want := range []string{
		"response_type=code", "client_id=client-1", "state=state-1", "nonce=nonce-1",
		"code_challenge_method=S256", "scope=openid+email",
		"redirect_uri=" + "https%3A%2F%2Fapp.example%2Fcb",
		"code_challenge=" + ChallengeS256("verifier-1"),
	} {
		if !strings.Contains(u, want) {
			t.Errorf("authorize URL %q lacks %q", u, want)
		}
	}
	withQuery := AuthorizeURL(&Endpoints{AuthorizeURL: "https://idp.example/authorize?prompt=login"},
		"client", "https://cb", "state", "nonce", "verifier", nil)
	if !strings.Contains(withQuery, "?prompt=login&") || !strings.Contains(withQuery, "&response_type=code") {
		t.Fatalf("existing authorize query was not preserved: %q", withQuery)
	}
}

func TestExchangeSendsPKCEAndBasicAuth(t *testing.T) {
	var seen struct{ user, verifier, grant string }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		seen.user, _, _ = r.BasicAuth()
		seen.verifier = r.PostFormValue("code_verifier")
		seen.grant = r.PostFormValue("grant_type")
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "at-1", "id_token": "idt-1"})
	}))
	defer srv.Close()

	tok, err := New().Exchange(context.Background(), &Endpoints{TokenURL: srv.URL}, "client-1", "s3cret", "https://cb", "code-1", "verifier-1")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "at-1" || tok.IDToken != "idt-1" {
		t.Fatalf("token response = %+v", tok)
	}
	if seen.user != "client-1" || seen.verifier != "verifier-1" || seen.grant != "authorization_code" {
		t.Fatalf("the exchange sent %+v", seen)
	}
}

func TestExchangeFailureModes(t *testing.T) {
	response := func(status int, body string) *Client {
		return &Client{HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: status,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})}}
	}
	for _, tc := range []struct {
		name   string
		client *Client
		url    string
	}{
		{"transport", &Client{HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("network unavailable")
		})}}, "https://idp.invalid/token"},
		{"provider status", response(http.StatusUnauthorized, `{"error":"invalid_client"}`), "https://idp.invalid/token"},
		{"malformed JSON", response(http.StatusOK, `not-json`), "https://idp.invalid/token"},
		{"missing token", response(http.StatusOK, `{}`), "https://idp.invalid/token"},
		{"invalid URL", New(), "://invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.client.Exchange(context.Background(), &Endpoints{TokenURL: tc.url}, "id", "secret", "https://cb", "code", "verifier"); err == nil {
				t.Fatal("exchange should fail")
			}
		})
	}
}

func TestValidateIssuer(t *testing.T) {
	for _, ok := range []string{"https://accounts.example.com", "https://login.microsoftonline.com/tid/v2.0", "http://localhost:9999"} {
		if err := ValidateIssuer(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://idp.example.com", "ftp://x", "https://", "https://idp.example.com/?a=b", "not a url at all\x00"} {
		if err := ValidateIssuer(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// The OAuth2 profiles must key identities on the immutable account id, never
// the rename-able login, and must only trust a PRIMARY VERIFIED email.
func TestGitHubIdentity(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-1" {
			w.WriteHeader(401)
			return
		}
		fmt.Fprint(w, `{"id": 583231, "login": "octocat", "name": "The Octocat"}`)
	})
	mux.HandleFunc("/user/emails", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `[
			{"email": "Public@example.com", "primary": false, "verified": true},
			{"email": "Octocat@GitHub.com", "primary": true, "verified": true}
		]`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	id, err := New().FetchOAuth2Identity(context.Background(), "github", &Endpoints{UserinfoURL: srv.URL + "/user"}, "at-1")
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "583231" {
		t.Errorf("subject = %q, want the numeric id (logins are rename-able)", id.Subject)
	}
	if id.Email != "octocat@github.com" || !id.EmailVerified {
		t.Errorf("email = %q verified=%v, want the primary verified address, normalized", id.Email, id.EmailVerified)
	}
}

func TestGitLabAndBitbucketIdentities(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/gitlab", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"id":42,"username":"alice","name":"","email":"Alice@Example.COM"}`)
	})
	mux.HandleFunc("/bitbucket", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"uuid":"{abc-123}","display_name":"Bob","username":"bob"}`)
	})
	mux.HandleFunc("/bitbucket/emails", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"values":[
			{"email":"other@example.com","is_primary":false,"is_confirmed":true},
			{"email":"Bob@Example.COM","is_primary":true,"is_confirmed":true}
		]}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := New()

	gitlab, err := client.FetchOAuth2Identity(context.Background(), "gitlab", &Endpoints{UserinfoURL: srv.URL + "/gitlab"}, "token")
	if err != nil || gitlab.Subject != "42" || gitlab.Email != "alice@example.com" ||
		!gitlab.EmailVerified || gitlab.Name != "alice" {
		t.Fatalf("GitLab identity = %+v, %v", gitlab, err)
	}
	bitbucket, err := client.FetchOAuth2Identity(context.Background(), "bitbucket", &Endpoints{UserinfoURL: srv.URL + "/bitbucket"}, "token")
	if err != nil || bitbucket.Subject != "abc-123" || bitbucket.Email != "bob@example.com" ||
		!bitbucket.EmailVerified || bitbucket.Name != "Bob" {
		t.Fatalf("Bitbucket identity = %+v, %v", bitbucket, err)
	}
}

func TestOAuthIdentityFailures(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/empty", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{}`)
	})
	mux.HandleFunc("/broken", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	client := New()

	for _, provider := range []string{"github", "gitlab", "bitbucket"} {
		if _, err := client.FetchOAuth2Identity(context.Background(), provider, &Endpoints{UserinfoURL: srv.URL + "/empty"}, "token"); err == nil {
			t.Errorf("%s identity without a stable subject was accepted", provider)
		}
		if _, err := client.FetchOAuth2Identity(context.Background(), provider, &Endpoints{UserinfoURL: srv.URL + "/broken"}, "token"); err == nil {
			t.Errorf("%s provider HTTP failure was ignored", provider)
		}
	}
	if _, err := client.FetchOAuth2Identity(context.Background(), "unknown", &Endpoints{}, "token"); err == nil {
		t.Fatal("unknown OAuth provider was accepted")
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Fatalf("firstNonEmpty = %q", got)
	}
}

func TestDiscoveryAndJSONFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"missing endpoints", http.StatusOK, `{"issuer":"ISSUER"}`},
		{"malformed", http.StatusOK, `not-json`},
		{"HTTP error", http.StatusBadGateway, `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				body := strings.ReplaceAll(tc.body, "ISSUER", "http://"+r.Host)
				_, _ = io.WriteString(w, body)
			}))
			defer server.Close()
			if _, err := New().Discover(context.Background(), server.URL); err == nil {
				t.Fatal("invalid discovery should fail")
			}
		})
	}
}

func TestAudienceTruthyAndJWKSFailures(t *testing.T) {
	if audienceContains(nil, "client") || audienceContains(json.RawMessage(`not-json`), "client") {
		t.Fatal("malformed audience was accepted")
	}
	if truthy(json.RawMessage(`false`)) || !truthy(json.RawMessage(`"TRUE"`)) || truthy(json.RawMessage(`"no"`)) ||
		truthy(json.RawMessage(`not-json`)) {
		t.Fatal("truthy parsed a value incorrectly")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"keys":[
			{"kty":"EC","kid":"wanted","n":"AA","e":"AQAB"},
			{"kty":"RSA","kid":"other","n":"AA","e":"AQAB"},
			{"kty":"RSA","kid":"wanted","n":"%%%","e":"AQAB"},
			{"kty":"RSA","kid":"wanted","n":"AQ","e":"%%%"}
		]}`)
	}))
	defer server.Close()
	if _, err := New().jwksKey(context.Background(), server.URL, "wanted"); err == nil {
		t.Fatal("JWKS without usable matching RSA key was accepted")
	}
}

func TestVerifyIDTokenMalformedEncodings(t *testing.T) {
	idp := newFakeIdP(t)
	ep := discover(t, idp)
	client := New()
	for _, token := range []string{
		"not-a-jwt",
		"%%%.e30.sig",
		base64.RawURLEncoding.EncodeToString([]byte(`not-json`)) + ".e30.sig",
		base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"missing"}`)) + ".e30.sig",
	} {
		if _, err := client.VerifyIDToken(context.Background(), ep, "client", "nonce", token, time.Now()); err == nil {
			t.Errorf("malformed token %q was accepted", token)
		}
	}
}
