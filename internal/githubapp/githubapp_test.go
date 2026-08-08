package githubapp

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testKeyPEM(t *testing.T) ([]byte, *rsa.PublicKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	raw := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return raw, &key.PublicKey
}

func TestAppJWT(t *testing.T) {
	pemBytes, public := testKeyPEM(t)
	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

	token, err := AppJWT(4242, pemBytes, now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("not a JWT: %q", token)
	}

	// The signature must verify against the public key: RS256 over the
	// signing string, exactly.
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	if err := rsa.VerifyPKCS1v15(public, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}

	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	if claims.Iss != "4242" {
		t.Fatalf("iss = %q", claims.Iss)
	}
	if claims.Iat != now.Add(-time.Minute).Unix() {
		t.Fatalf("iat must be backdated 60s, got %d", claims.Iat)
	}
	if claims.Exp != now.Add(9*time.Minute).Unix() {
		t.Fatalf("exp must be iat+9min-ish, got %d", claims.Exp)
	}
}

func TestAppJWTKeyFormatsAndErrors(t *testing.T) {
	if _, err := AppJWT(1, []byte("not PEM"), time.Now()); err == nil {
		t.Fatal("non-PEM key was accepted")
	}
	badDER := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("bad")})
	if _, err := AppJWT(1, badDER, time.Now()); err == nil {
		t.Fatal("invalid private-key DER was accepted")
	}

	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	edDER, err := x509.MarshalPKCS8PrivateKey(edKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AppJWT(1, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: edDER}), time.Now()); err == nil ||
		!strings.Contains(err.Error(), "not RSA") {
		t.Fatalf("non-RSA PKCS#8 should be rejected, got %v", err)
	}

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AppJWT(1, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), time.Now()); err != nil {
		t.Fatalf("RSA PKCS#8 should be accepted: %v", err)
	}
}

func TestAppJWTSignFailure(t *testing.T) {
	// A 256-bit modulus parses fine but is too small to hold a PKCS#1 v1.5
	// SHA-256 signature, forcing the signing branch to fail.
	t.Setenv("GODEBUG", "rsa1024min=0")
	key, err := rsa.GenerateKey(rand.Reader, 256)
	if err != nil {
		t.Fatal(err)
	}
	tiny := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if _, err := AppJWT(1, tiny, time.Now()); err == nil {
		t.Fatal("signing with an undersized key should fail")
	}
}

func TestManifest(t *testing.T) {
	m := Manifest("https://paas.example.com/", "app-uuid", "akerdock-prod")
	hook := m["hook_attributes"].(map[string]any)
	if hook["url"] != "https://paas.example.com/webhooks/github/apps/app-uuid" {
		t.Fatalf("hook url: %v", hook["url"])
	}
	perms := m["default_permissions"].(map[string]string)
	if perms["contents"] != "read" || perms["checks"] != "write" {
		t.Fatalf("permissions drifted from §2.3: %v", perms)
	}
	if m["public"] != false {
		t.Fatalf("the app must be private")
	}
}

func TestConvertManifestAndTokens(t *testing.T) {
	pemBytes, _ := testKeyPEM(t)
	mints := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app-manifests/one-shot-code/conversions":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 77, "slug": "akerdock-x", "client_id": "cid",
				"client_secret": "csec", "webhook_secret": "wsec", "pem": string(pemBytes),
				"html_url": "https://github.com/apps/akerdock-x",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/9/access_tokens":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
				t.Errorf("missing app JWT: %q", r.Header.Get("Authorization"))
			}
			mints++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": "ghs_test", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
			if r.Header.Get("Authorization") != "Bearer ghs_test" {
				t.Errorf("repos must use the installation token")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count": 1,
				"repositories": []map[string]any{{
					"id": 5, "full_name": "acme/shop", "default_branch": "main", "private": true,
				}},
			})
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	client := &Client{APIURL: server.URL, HTTP: server.Client()}
	creds, err := client.ConvertManifest(context.Background(), "one-shot-code")
	if err != nil || creds.AppID != 77 || creds.WebhookSecret != "wsec" {
		t.Fatalf("conversion failed: %+v %v", creds, err)
	}

	ts := NewTokenSource(client, creds.AppID, []byte(creds.PEM))
	token, err := ts.Token(context.Background(), 9, nil)
	if err != nil || token != "ghs_test" {
		t.Fatalf("token: %q %v", token, err)
	}
	// A second call within the validity window must hit the cache.
	if _, err := ts.Token(context.Background(), 9, nil); err != nil {
		t.Fatal(err)
	}
	if mints != 1 {
		t.Fatalf("expected 1 mint, got %d", mints)
	}
	ts.Invalidate(9, nil)
	if _, err := ts.Token(context.Background(), 9, nil); err != nil {
		t.Fatal(err)
	}
	if mints != 2 {
		t.Fatalf("invalidating must force a second mint, got %d", mints)
	}

	repos, err := client.ListInstallationRepos(context.Background(), token)
	if err != nil || len(repos) != 1 || repos[0].FullName != "acme/shop" {
		t.Fatalf("repos: %+v %v", repos, err)
	}
}

func TestScopeKeyDoesNotCollideAndDoesNotMutateInput(t *testing.T) {
	repositories := []string{"z/repo", "a/repo"}
	first := scopeKey(2_000_000, repositories)
	second := scopeKey(3_000_000, repositories)
	if first == second {
		t.Fatal("large installation IDs collided in the token cache key")
	}
	if repositories[0] != "z/repo" {
		t.Fatalf("scopeKey mutated caller repositories: %v", repositories)
	}
	if !strings.Contains(first, "a/repo\x00z/repo") {
		t.Fatalf("repository scope is not deterministic: %q", first)
	}
}

func TestUpsertPRComment(t *testing.T) {
	var patched, posted int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/shop/issues/12/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "body": "unrelated"},
				{"id": 2, "body": "<!-- akerdock:preview-42 -->\nold body"},
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/acme/shop/issues/comments/2":
			patched++
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/shop/issues/12/comments":
			posted++
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("{}"))
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	client := &Client{APIURL: server.URL, HTTP: server.Client()}
	// The marked comment exists: updated in place, never duplicated (§20.4.6).
	if err := client.UpsertPRComment(context.Background(), "tok", "acme/shop", 12, "preview-42", "new body"); err != nil {
		t.Fatal(err)
	}
	if patched != 1 || posted != 0 {
		t.Fatalf("expected an in-place update, got patch=%d post=%d", patched, posted)
	}
}

func TestPreviewFeedbackEndpoints(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Errorf("authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/shop/check-runs":
			_ = json.NewEncoder(w).Encode(CheckRun{ID: 10, Name: "preview", Status: "queued"})
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/acme/shop/check-runs/10":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/shop/deployments":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 20})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/shop/deployments/20/statuses":
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := &Client{APIURL: server.URL, HTTP: server.Client()}

	check, err := client.CreateCheckRun(context.Background(), "token", "acme/shop", CheckRunInput{Name: "preview", HeadSHA: "abc"})
	if err != nil || check.ID != 10 {
		t.Fatalf("check run = %+v, %v", check, err)
	}
	if err := client.UpdateCheckRun(context.Background(), "token", "acme/shop", 10, CheckRunInput{Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	deploymentID, err := client.CreateDeployment(context.Background(), "token", "acme/shop", "abc", "preview/pr-1")
	if err != nil || deploymentID != 20 {
		t.Fatalf("deployment = %d, %v", deploymentID, err)
	}
	if err := client.CreateDeploymentStatus(context.Background(), "token", "acme/shop", 20, "success", "https://preview.example"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 4 {
		t.Fatalf("calls = %v", calls)
	}
}

func TestCollaboratorPermissions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/acme/shop/collaborators/"), "/permission")
		switch username {
		case "writer":
			_ = json.NewEncoder(w).Encode(map[string]string{"permission": "write"})
		case "admin":
			_ = json.NewEncoder(w).Encode(map[string]string{"permission": "admin"})
		case "reader":
			_ = json.NewEncoder(w).Encode(map[string]string{"permission": "read"})
		case "missing":
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	client := &Client{APIURL: server.URL, HTTP: server.Client()}

	for _, tc := range []struct {
		username string
		want     bool
		wantErr  bool
	}{
		{"writer", true, false},
		{"admin", true, false},
		{"reader", false, false},
		{"missing", false, false},
		{"broken", false, true},
	} {
		got, err := client.CollaboratorCanWrite(context.Background(), "token", "acme/shop", tc.username)
		if got != tc.want || (err != nil) != tc.wantErr {
			t.Errorf("%s = %v, %v", tc.username, got, err)
		}
	}
}

func TestPaginationAndCommentCreation(t *testing.T) {
	var commentPosts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/installation/repositories":
			page := r.URL.Query().Get("page")
			if page == "1" {
				repos := make([]Repo, 100)
				for i := range repos {
					repos[i] = Repo{ID: int64(i + 1), FullName: "acme/repo"}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 101, "repositories": repos})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"total_count":  101,
					"repositories": []Repo{{ID: 101, FullName: "acme/last"}},
				})
			}
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			commentPosts++
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := &Client{APIURL: server.URL, HTTP: server.Client()}

	repositories, err := client.ListInstallationRepos(context.Background(), "token")
	if err != nil || len(repositories) != 101 || repositories[100].ID != 101 {
		t.Fatalf("repositories = %d, %v", len(repositories), err)
	}
	if err := client.UpsertPRComment(context.Background(), "token", "acme/shop", 1, "preview", "ready"); err != nil {
		t.Fatal(err)
	}
	if commentPosts != 1 {
		t.Fatalf("comment posts = %d", commentPosts)
	}
}

func TestHTTPFailureModesAndAPIError(t *testing.T) {
	client := &Client{APIURL: "https://api.invalid", HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network unavailable")
	})}}
	if err := client.do(context.Background(), http.MethodGet, "/x", "", nil, nil); err == nil {
		t.Fatal("transport error was not returned")
	}
	if err := client.do(context.Background(), http.MethodPost, "/x", "", make(chan int), nil); err == nil {
		t.Fatal("unencodable body was not rejected")
	}
	invalid := &Client{APIURL: "://invalid", HTTP: http.DefaultClient}
	if err := invalid.do(context.Background(), "bad\nmethod", "/x", "", nil, nil); err == nil {
		t.Fatal("invalid request was not rejected")
	}

	response := func(status int, body string) *Client {
		return &Client{
			APIURL: "https://api.invalid",
			HTTP: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: status,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(body)),
				}, nil
			})},
		}
	}
	err := response(http.StatusNotFound, `{"message":"missing"}`).do(context.Background(), http.MethodGet, "/repos/x", "", nil, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.IsNotFound() || !strings.Contains(apiErr.Error(), "answered 404") {
		t.Fatalf("API error = %v", err)
	}
	var out map[string]any
	if err := response(http.StatusOK, "not-json").do(context.Background(), http.MethodGet, "/x", "", nil, &out); err == nil {
		t.Fatal("malformed success response was accepted")
	}
	if err := response(http.StatusNoContent, "").do(context.Background(), http.MethodDelete, "/x", "", nil, nil); err != nil {
		t.Fatalf("bodyless success failed: %v", err)
	}
}

func TestClientWithoutInjectedHTTPUsesDefault(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	defer server.Close()
	// No HTTP injected: the client must fall back to the process default.
	client := &Client{APIURL: server.URL}
	if client.http() != defaultHTTP {
		t.Fatal("nil HTTP should fall back to defaultHTTP")
	}
	if err := client.do(context.Background(), http.MethodGet, "/x", "", nil, nil); err != nil {
		t.Fatal(err)
	}
}

func TestInstallationTokenScopedToRepositories(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/7/access_tokens" {
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token": "ghs_scoped", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	}))
	defer server.Close()
	client := &Client{APIURL: server.URL, HTTP: server.Client()}

	token, err := client.InstallationToken(context.Background(), "jwt", 7, []string{"shop"})
	if err != nil || token.Token != "ghs_scoped" {
		t.Fatalf("token = %+v, %v", token, err)
	}
	// Least privilege: the repository restriction must reach GitHub (§2.2).
	repos, ok := body["repositories"].([]any)
	if !ok || len(repos) != 1 || repos[0] != "shop" {
		t.Fatalf("repositories not sent: %v", body)
	}
}

func TestPullRequestEndpoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/shop/pulls":
			if got := r.URL.Query().Get("state"); got != "open" {
				t.Errorf("state = %q", got)
			}
			_, _ = w.Write([]byte(`[
				{"number":1,"title":"first","state":"open",
				 "head":{"ref":"feature","sha":"abc","repo":{"full_name":"acme/shop"}},
				 "base":{"repo":{"full_name":"acme/shop"}}},
				{"number":2,"title":"forked","state":"open","draft":true,
				 "head":{"ref":"patch","sha":"def","repo":{"full_name":"fork/shop"}},
				 "base":{"repo":{"full_name":"acme/shop"}}}
			]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/shop/pulls/2":
			_, _ = w.Write([]byte(`{"number":2,"title":"forked","state":"open","draft":true,
				"head":{"ref":"patch","sha":"def","repo":{"full_name":"fork/shop"}},
				"base":{"repo":{"full_name":"acme/shop"}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := &Client{APIURL: server.URL, HTTP: server.Client()}

	prs, err := client.ListOpenPullRequests(context.Background(), "token", "acme/shop")
	if err != nil || len(prs) != 2 || prs[0].Number != 1 || prs[1].Head.SHA != "def" {
		t.Fatalf("prs = %+v, %v", prs, err)
	}
	pr, err := client.GetPullRequest(context.Background(), "token", "acme/shop", 2)
	if err != nil || pr.Number != 2 || !pr.Draft || pr.Head.Ref != "patch" {
		t.Fatalf("pr = %+v, %v", pr, err)
	}

	if _, err := client.ListOpenPullRequests(context.Background(), "token", "acme/missing"); err == nil {
		t.Fatal("listing a missing repo should fail")
	}
}

func TestPullRequestIsFork(t *testing.T) {
	build := func(head, base string) PullRequest {
		var pr PullRequest
		pr.Head.Repo.FullName = head
		pr.Base.Repo.FullName = base
		return pr
	}
	for _, tc := range []struct {
		name string
		pr   PullRequest
		want bool
	}{
		{"same repo", build("acme/shop", "acme/shop"), false},
		{"fork", build("fork/shop", "acme/shop"), true},
		{"deleted head repo", build("", "acme/shop"), false},
	} {
		if got := tc.pr.IsFork(); got != tc.want {
			t.Errorf("%s: IsFork() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestServerErrorsPropagate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	client := &Client{APIURL: server.URL, HTTP: server.Client()}

	if _, err := client.ListInstallationRepos(context.Background(), "token"); err == nil {
		t.Fatal("repository listing should surface the 502")
	}
	// The comment lookup fails before any create is attempted (§20.4.6).
	if err := client.UpsertPRComment(context.Background(), "token", "acme/shop", 1, "preview", "body"); err == nil {
		t.Fatal("comment lookup failure should surface")
	}
}

func TestTokenSourceErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"bad credentials"}`))
	}))
	defer server.Close()
	client := &Client{APIURL: server.URL, HTTP: server.Client()}

	// A broken private key fails before any network call.
	broken := NewTokenSource(client, 1, []byte("not PEM"))
	if _, err := broken.Token(context.Background(), 9, nil); err == nil {
		t.Fatal("invalid PEM should fail the mint")
	}

	// A rejected mint propagates the API error and caches nothing.
	pemBytes, _ := testKeyPEM(t)
	rejected := NewTokenSource(client, 1, pemBytes)
	if _, err := rejected.Token(context.Background(), 9, nil); err == nil {
		t.Fatal("a 401 mint should surface")
	}
	var apiErr *APIError
	if _, err := rejected.Token(context.Background(), 9, nil); !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("a failed mint must not be cached, got %v", err)
	}
}

func TestBuildDefaultHTTP(t *testing.T) {
	t.Setenv("AKERDOCK_GITHUB_CA_FILE", "")
	if got := buildDefaultHTTP(); got != http.DefaultClient {
		t.Fatal("empty CA setting should use the default client")
	}
	t.Setenv("AKERDOCK_GITHUB_CA_FILE", filepath.Join(t.TempDir(), "missing.pem"))
	if got := buildDefaultHTTP(); got != http.DefaultClient {
		t.Fatal("missing CA file should use the default client")
	}
	invalid := filepath.Join(t.TempDir(), "invalid.pem")
	if err := os.WriteFile(invalid, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AKERDOCK_GITHUB_CA_FILE", invalid)
	if got := buildDefaultHTTP(); got != http.DefaultClient {
		t.Fatal("invalid CA file should use the default client")
	}

	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer tlsServer.Close()
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsServer.Certificate().Raw})
	valid := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(valid, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AKERDOCK_GITHUB_CA_FILE", valid)
	if got := buildDefaultHTTP(); got == http.DefaultClient || got.Transport == nil {
		t.Fatal("valid private CA was not installed")
	}
}
