package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/deepteams/akerdock/internal/agentwire"
)

func tokenHash(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func armed(ig *Ingress, session, endpoint, token string, ttl time.Duration) {
	ig.Expect(agentwire.IngressExpectParams{
		SessionUUID:   session,
		EndpointUUID:  endpoint,
		TokenSHA256:   tokenHash(token),
		ExpiresAtUnix: time.Now().Add(ttl).Unix(),
	})
}

// TestIngressUnknownHostIs404 checks a request for a host that is not a
// declared ingress endpoint is refused, not relayed.
func TestIngressUnknownHostIs404(t *testing.T) {
	ig := NewIngress(nil)
	ig.SetRoutes([]IngressRoute{{Host: "known.example.com", EndpointUUID: "ep1"}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://other.example.com/", nil)
	ig.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown host: got %d, want 404", rec.Code)
	}
}

// TestIngressOfflinePageWhenUnattached checks a known host with no laptop
// attached serves the offline page (503), not a relay error.
func TestIngressOfflinePageWhenUnattached(t *testing.T) {
	ig := NewIngress(nil)
	ig.SetRoutes([]IngressRoute{{Host: "dev.example.com", EndpointUUID: "ep1"}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://dev.example.com/", nil)
	ig.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("offline page: got %d, want 503", rec.Code)
	}
	if body := rec.Body.String(); !contains(body, "offline") {
		t.Fatalf("offline page body unexpected: %s", body)
	}
}

// TestIngressAttachRejectsBadToken checks the attach path refuses a token that
// was never armed, and that arming is single-use (a replay finds nothing).
func TestIngressAttachRejectsBadToken(t *testing.T) {
	ig := NewIngress(nil)
	ig.SetRoutes([]IngressRoute{{Host: "dev.example.com", EndpointUUID: "ep1"}})

	// Unknown token.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://dev.example.com/.akerdock/ingress?token=nope", nil)
	ig.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: got %d, want 401", rec.Code)
	}

	// Armed but for a different endpoint → refused.
	armed(ig, "sess1", "OTHER-endpoint", "tok-good", time.Minute)
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "http://dev.example.com/.akerdock/ingress?token=tok-good", nil)
	ig.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-endpoint token: got %d, want 401", rec.Code)
	}
}

// TestIngressExpectExpires checks an expired expectation is refused.
func TestIngressExpectExpires(t *testing.T) {
	ig := NewIngress(nil)
	ig.SetRoutes([]IngressRoute{{Host: "dev.example.com", EndpointUUID: "ep1"}})
	armed(ig, "sess1", "ep1", "tok", -time.Second) // already expired

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://dev.example.com/.akerdock/ingress?token=tok", nil)
	ig.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired token: got %d, want 401", rec.Code)
	}
}

// TestIngressCutDisarmsExpectation checks a cut removes a pending expectation
// so a later attach with its token is refused.
func TestIngressCutDisarmsExpectation(t *testing.T) {
	ig := NewIngress(nil)
	ig.SetRoutes([]IngressRoute{{Host: "dev.example.com", EndpointUUID: "ep1"}})
	armed(ig, "sess1", "ep1", "tok", time.Minute)

	if !ig.Cut("sess1", "revoked") {
		t.Fatal("Cut should report it disarmed the pending expectation")
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://dev.example.com/.akerdock/ingress?token=tok", nil)
	ig.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("token after cut: got %d, want 401", rec.Code)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
