package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// The /auth routes live outside the bearer middleware, so the audit identity
// is built by hand from the session row: it must carry the session uuid as
// actor and the session's current team — an audit row attributed to nobody
// is not an audit row (§23.4).
func TestSessionIdentity(t *testing.T) {
	u := pguuid.MustParse("0195c1f4-2f4e-7aa1-b111-222233334444")
	team := int64(42)
	id := sessionIdentity(&store.GetSessionByTokenHashRow{Uuid: u, CurrentTeamID: &team})

	if id.TokenUUID != "0195c1f4-2f4e-7aa1-b111-222233334444" {
		t.Errorf("actor uuid = %q, want the session uuid", id.TokenUUID)
	}
	if id.TeamID != 42 {
		t.Errorf("team = %d, want 42", id.TeamID)
	}
	if !id.Session {
		t.Error("identity must be marked as a session, not a bearer token")
	}

	// A session without a current team (owner between teams) still audits.
	id = sessionIdentity(&store.GetSessionByTokenHashRow{Uuid: u})
	if id.TeamID != 0 {
		t.Errorf("team = %d, want 0 when the session has none", id.TeamID)
	}
}

func TestReadMFABodyRejectsGarbage(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/auth/mfa/verify", strings.NewReader("{not json"))
	var body mfaCodeBody
	if readMFABody(w, r, &body) {
		t.Fatal("malformed JSON was accepted")
	}
	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestReadMFABodyDecodes(t *testing.T) {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/auth/mfa/verify",
		strings.NewReader(`{"challenge":"c","code":"123456","recovery_code":"r"}`))
	var body mfaCodeBody
	if !readMFABody(w, r, &body) {
		t.Fatal("valid JSON was refused")
	}
	if body.Challenge != "c" || body.Code != "123456" || body.RecoveryCode != "r" {
		t.Errorf("decoded body = %+v", body)
	}
}
