package githubapp

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
		case r.Method == "POST" && r.URL.Path == "/app-manifests/one-shot-code/conversions":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 77, "slug": "akerdock-x", "client_id": "cid",
				"client_secret": "csec", "webhook_secret": "wsec", "pem": string(pemBytes),
				"html_url": "https://github.com/apps/akerdock-x",
			})
		case r.Method == "POST" && r.URL.Path == "/app/installations/9/access_tokens":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
				t.Errorf("missing app JWT: %q", r.Header.Get("Authorization"))
			}
			mints++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"token": "ghs_test", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})
		case r.Method == "GET" && r.URL.Path == "/installation/repositories":
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
			w.WriteHeader(500)
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

	repos, err := client.ListInstallationRepos(context.Background(), token)
	if err != nil || len(repos) != 1 || repos[0].FullName != "acme/shop" {
		t.Fatalf("repos: %+v %v", repos, err)
	}
}

func TestUpsertPRComment(t *testing.T) {
	var patched, posted int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/acme/shop/issues/12/comments":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": 1, "body": "unrelated"},
				{"id": 2, "body": "<!-- akerdock:preview-42 -->\nold body"},
			})
		case r.Method == "PATCH" && r.URL.Path == "/repos/acme/shop/issues/comments/2":
			patched++
			w.WriteHeader(200)
			_, _ = w.Write([]byte("{}"))
		case r.Method == "POST" && r.URL.Path == "/repos/acme/shop/issues/12/comments":
			posted++
			w.WriteHeader(201)
			_, _ = w.Write([]byte("{}"))
		default:
			t.Errorf("unexpected call %s %s", r.Method, r.URL.Path)
			w.WriteHeader(500)
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
