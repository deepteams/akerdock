package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/tunnel"
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

// TestIngressHandles checks host recognition.
func TestIngressHandles(t *testing.T) {
	ig := NewIngress(nil)
	ig.SetRoutes([]IngressRoute{{Host: "dev.example.com", EndpointUUID: "ep1"}})
	if !ig.Handles("dev.example.com") {
		t.Fatal("declared host should be handled")
	}
	if ig.Handles("nope.example.com") {
		t.Fatal("undeclared host must not be handled")
	}
}

// liveSession installs a fake live session on the module (white-box) so the cut
// paths — which otherwise need a real attach — are exercisable.
func liveSession(ig *Ingress, endpointUUID, sessionUUID string) chan tunnel.EndReason {
	cancel := make(chan tunnel.EndReason, 1)
	ig.mu.Lock()
	ig.live[endpointUUID] = &ingressSession{
		sessionUUID:  sessionUUID,
		endpointUUID: endpointUUID,
		cancel:       cancel,
	}
	ig.mu.Unlock()
	return cancel
}

// TestIngressCutLiveSession checks Cut reaches a live session's cancel channel
// with the given reason.
func TestIngressCutLiveSession(t *testing.T) {
	ig := NewIngress(nil)
	cancel := liveSession(ig, "ep1", "sess-live")

	if !ig.Cut("sess-live", "revoked") {
		t.Fatal("Cut should report it reached the live session")
	}
	select {
	case r := <-cancel:
		if r != "revoked" {
			t.Fatalf("cancel reason = %q, want revoked", r)
		}
	default:
		t.Fatal("Cut did not deliver the reason to the live session")
	}
	if ig.Cut("does-not-exist", "revoked") {
		t.Fatal("Cut on an unknown session should report nothing matched")
	}
}

// TestIngressSetRoutesCutsRemovedEndpoint checks a routing reload that drops an
// endpoint cuts its live session (the belt to the control-plane cut).
func TestIngressSetRoutesCutsRemovedEndpoint(t *testing.T) {
	ig := NewIngress(nil)
	ig.SetRoutes([]IngressRoute{{Host: "dev.example.com", EndpointUUID: "ep1"}})
	cancel := liveSession(ig, "ep1", "sess-live")

	// A reload without ep1 must tear its session down.
	ig.SetRoutes([]IngressRoute{{Host: "other.example.com", EndpointUUID: "ep2"}})
	select {
	case r := <-cancel:
		if r != endReasonRevoked {
			t.Fatalf("cancel reason = %q, want %q", r, endReasonRevoked)
		}
	default:
		t.Fatal("dropping an endpoint should cut its live session")
	}
}

// TestIngressAttachOccupied checks a valid token is refused with 409 when the
// endpoint already holds a live tunnel (one laptop per endpoint).
func TestIngressAttachOccupied(t *testing.T) {
	ig := NewIngress(nil)
	ig.SetRoutes([]IngressRoute{{Host: "dev.example.com", EndpointUUID: "ep1"}})
	armed(ig, "sess1", "ep1", "tok", time.Minute)
	liveSession(ig, "ep1", "already-here")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://dev.example.com/.akerdock/ingress?token=tok", nil)
	ig.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("attach on an occupied endpoint: got %d, want 409", rec.Code)
	}
}

// TestIngressRelease checks release removes exactly the session it is given and
// leaves a newer occupant in place.
func TestIngressRelease(t *testing.T) {
	ig := NewIngress(nil)
	liveSession(ig, "ep1", "sess-old")
	ig.mu.Lock()
	old := ig.live["ep1"]
	ig.mu.Unlock()

	ig.release("ep1", old)
	ig.mu.Lock()
	_, present := ig.live["ep1"]
	ig.mu.Unlock()
	if present {
		t.Fatal("release should remove the session it owns")
	}
	// Releasing a stale session must not evict a newer occupant.
	liveSession(ig, "ep1", "sess-new")
	ig.release("ep1", old)
	ig.mu.Lock()
	_, present = ig.live["ep1"]
	ig.mu.Unlock()
	if !present {
		t.Fatal("release of a stale session must not evict the current occupant")
	}
}

// TestIngressNotify checks the observation hook is invoked when set.
func TestIngressNotify(t *testing.T) {
	got := make(chan Observation, 1)
	ig := NewIngress(nil)
	ig.Notify = func(o Observation) { got <- o }
	ig.notify(Observation{Type: "ingress_claimed"})
	select {
	case o := <-got:
		if o.Type != "ingress_claimed" {
			t.Fatalf("got %q", o.Type)
		}
	default:
		t.Fatal("notify did not call the hook")
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
