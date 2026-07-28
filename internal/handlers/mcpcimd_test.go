package handlers

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
)

// resolveCIMD dials out through the SSRF guard, which refuses private ranges
// — including the loopback an httptest server listens on. These tests
// therefore exercise the VALIDATION rules directly, where the security of the
// scheme actually lives, and the fetch path is covered by the guard's own
// tests (internal/safedial).
func TestCIMDRejectsMalformedClientIDs(t *testing.T) {
	a := &API{}
	for _, id := range []string{
		"http://client.example.com/mcp.json",        // not https
		"https://",                                  // no host
		"https://client.example.com/mcp.json#frag",  // fragment
		"https://user:pw@client.example.com/m.json", // credentials
	} {
		if _, err := a.resolveCIMD(context.Background(), id); err == nil {
			t.Fatalf("client_id %q was accepted", id)
		}
	}
}

func TestIsCIMD(t *testing.T) {
	if !isCIMD("https://client.example.com/mcp.json") {
		t.Fatal("an https client_id must be treated as a metadata document")
	}
	for _, id := range []string{"akdmc_abc", "http://x/y", ""} {
		if isCIMD(id) {
			t.Fatalf("%q must not be treated as a metadata document", id)
		}
	}
}

// The two rules that make a CIMD identity meaningful: the document must claim
// the URL it was served from, and its redirect uris must live on that origin.
// Without them, anyone hosting a document could impersonate another client.
func TestSameOriginRedirectRule(t *testing.T) {
	const origin = "https://client.example.com"
	if !sameOrigin("https://client.example.com/callback", origin) {
		t.Fatal("a callback on the document's origin must be accepted")
	}
	for _, uri := range []string{
		"https://evil.example.com/callback",        // another host
		"http://client.example.com/callback",       // another scheme
		"https://client.example.com:8443/callback", // another port
		"not a url",
	} {
		if sameOrigin(uri, origin) {
			t.Fatalf("redirect_uri %q must not pass the same-origin rule", uri)
		}
	}
}

func TestCIMDCacheExpiresAndBounds(t *testing.T) {
	cache := &cimdCache{entries: map[string]cimdEntry{}}
	client := mcpClient{ID: "https://c.example.com/m.json", Name: "c", Verified: true}
	cache.put(client.ID, client)
	if got, ok := cache.get(client.ID); !ok || got.Name != "c" {
		t.Fatal("a fresh entry must be served from the cache")
	}
	// An expired entry is a miss, not a stale hit.
	cache.entries[client.ID] = cimdEntry{client: client}
	if _, ok := cache.get(client.ID); ok {
		t.Fatal("an expired entry was served")
	}
	// A hostile caller must not grow the cache without bound.
	for i := 0; i < 300; i++ {
		cache.put(strings.Repeat("x", i%50)+string(rune(i)), client)
	}
	if len(cache.entries) > 300 {
		t.Fatalf("cache grew to %d entries — it must stay bounded", len(cache.entries))
	}
}

// Dynamic registration is closed unless the instance opted in (ADR-044), and
// the refusal must name the way in rather than being a bare 403.
func TestRegistrationRefusalNamesCIMD(t *testing.T) {
	rec := httptest.NewRecorder()
	writeOAuthError(rec, 403, "access_denied",
		"dynamic client registration is disabled on this instance — use a Client ID Metadata Document")
	body := rec.Body.String()
	if !strings.Contains(body, "access_denied") || !strings.Contains(body, "Metadata Document") {
		t.Fatalf("refusal body = %s", body)
	}
}

// The consent screen is the one page whose text the user must be able to
// trust: it shows a verified origin for a CIMD client, warns explicitly for a
// self-declared one, and escapes everything the client controls.
func TestConsentPageShowsIdentityAndEscapes(t *testing.T) {
	req := mcpAuthorizeParams{
		ClientID: "https://client.example.com/mcp.json", RedirectURI: "https://client.example.com/cb",
		State: "st", Challenge: "ch",
	}
	verified := mcpConsentPage(
		mcpClient{Name: "Assistant", Verified: true, Origin: "https://client.example.com"},
		req, "Platform", "csrf-value")
	if !strings.Contains(verified, "Verified") || !strings.Contains(verified, "https://client.example.com") {
		t.Fatalf("verified identity not shown:\n%s", verified)
	}
	if !strings.Contains(verified, "Platform") || !strings.Contains(verified, "Read-only") {
		t.Fatalf("scope not stated:\n%s", verified)
	}
	if !strings.Contains(verified, `name="csrf_token" value="csrf-value"`) {
		t.Fatal("the form must carry the session CSRF token")
	}

	unverified := mcpConsentPage(
		mcpClient{Name: `<img src=x onerror=alert(1)>`, Verified: false}, req, "", "csrf")
	if !strings.Contains(unverified, "self-declared") {
		t.Fatalf("a dynamically registered client must be flagged:\n%s", unverified)
	}
	if strings.Contains(unverified, "<img src=x") {
		t.Fatal("the client-controlled name was not escaped — that is stored XSS on the consent page")
	}
}
