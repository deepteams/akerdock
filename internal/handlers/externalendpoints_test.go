package handlers

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// The exact-pair rule is what keeps ADR-045 from being a port scanner with an
// audit log attached: one endpoint is one destination, and a network is
// deliberately not addressable as a unit.
func TestValidateEndpointBodyRejectsAnythingButOneDestination(t *testing.T) {
	valid := api.ExternalEndpointCreate{Name: "prod-replica", Host: "10.0.0.7", Port: 5432}

	cases := map[string]struct {
		body api.ExternalEndpointCreate
		want bool
	}{
		"a single host and port":   {valid, true},
		"a hostname":               {withHost(valid, "db.internal.example"), true},
		"a scheme":                 {withHost(valid, "postgres://db"), false},
		"a path":                   {withHost(valid, "db/primary"), false},
		"a host:port pair":         {withHost(valid, "db:5432"), false},
		"a comma-separated list":   {withHost(valid, "db1,db2"), false},
		"a space":                  {withHost(valid, "db one"), false},
		"an empty host":            {withHost(valid, ""), false},
		"port zero":                {withPort(valid, 0), false},
		"port above the 16 bits":   {withPort(valid, 70000), false},
		"an empty name":            {withName(valid, ""), false},
		"a grant window too long":  {withMaxGrant(valid, 481), false},
		"the longest grant window": {withMaxGrant(valid, 480), true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest("POST", "/external-endpoints", nil)
			if got := validateEndpointBody(rec, r, tc.body); got != tc.want {
				t.Errorf("validateEndpointBody = %v, want %v (body %+v)", got, tc.want, tc.body)
			}
		})
	}
}

func withHost(b api.ExternalEndpointCreate, h string) api.ExternalEndpointCreate {
	b.Host = h
	return b
}

func withPort(b api.ExternalEndpointCreate, p int) api.ExternalEndpointCreate {
	b.Port = p
	return b
}

func withName(b api.ExternalEndpointCreate, n string) api.ExternalEndpointCreate {
	b.Name = n
	return b
}

func withMaxGrant(b api.ExternalEndpointCreate, m int) api.ExternalEndpointCreate {
	b.MaxGrantMinutes = &m
	return b
}

// Declaring an external endpoint usually means reaching a real database, so
// the protective regime is the default and downgrading is a conscious act
// (ADR-045 §6, ADR-011's protection-by-default posture).
func TestCriticalityDefaultsToSensitive(t *testing.T) {
	if got := criticalityOrDefault(nil); got != store.ExternalEndpointCriticalitySensitive {
		t.Errorf("absent criticality = %q, want sensitive", got)
	}
	standard := api.ExternalEndpointCreateCriticalityStandard
	if got := criticalityOrDefault(&standard); got != store.ExternalEndpointCriticalityStandard {
		t.Errorf("explicit standard = %q, want standard", got)
	}
	sensitive := api.ExternalEndpointCreateCriticalitySensitive
	if got := criticalityOrDefault(&sensitive); got != store.ExternalEndpointCriticalitySensitive {
		t.Errorf("explicit sensitive = %q, want sensitive", got)
	}
}

// The server picks the factor, never the client — a menu would let an attacker
// choose the weakest — and a passkey outranks a TOTP whenever one is enrolled.
func TestFreshFactorReadsTheRightMarker(t *testing.T) {
	now := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	stale := pgtype.Timestamptz{Time: time.Now().Add(-2 * grantStepUpWindow), Valid: true}

	passkeyOnly := &store.GetSessionByTokenHashRow{MfaVerifiedAt: now}
	totpOnly := &store.GetSessionByTokenHashRow{TotpVerifiedAt: now}

	if !freshFactor(passkeyOnly, "passkey") {
		t.Error("a recent passkey ceremony must satisfy the passkey requirement")
	}
	if !freshFactor(totpOnly, "totp") {
		t.Error("a recent TOTP ceremony must satisfy the TOTP requirement")
	}

	// The heart of it: the two markers are separate columns precisely so a
	// TOTP can never satisfy the passkey-only ritual the root terminal
	// requires. Collapsing them would hand every TOTP-only user a root shell.
	if freshFactor(totpOnly, "passkey") {
		t.Error("a TOTP must NOT satisfy a passkey requirement — that is the root terminal's guard")
	}
	if freshFactor(passkeyOnly, "totp") {
		t.Error("a passkey stamp must not be read out of the TOTP column")
	}

	if freshFactor(&store.GetSessionByTokenHashRow{MfaVerifiedAt: stale}, "passkey") {
		t.Error("a ceremony older than the window must not pass")
	}
	if freshFactor(&store.GetSessionByTokenHashRow{}, "passkey") {
		t.Error("a session that never re-authenticated must not pass")
	}
	if freshFactor(passkeyOnly, "") {
		t.Error("an unknown factor must never be considered fresh")
	}
}

// A session never outlives its authorization, and ADR-032's ceiling does not
// stack on top of the grant: two bounds racing each other are two numbers to
// explain where one suffices.
func TestSessionBoundsFollowTheGrant(t *testing.T) {
	// No grant (a `standard` endpoint or a container target): the package
	// default applies, so the option stays zero.
	if got := sessionBounds(store.PortForwardSession{}); got.MaxDuration != 0 {
		t.Errorf("without a grant the bridge keeps its default, got %v", got.MaxDuration)
	}

	// A five-hour grant is NOT cut at four: the grant is the deadline.
	in5h := store.PortForwardSession{
		AuthorizedUntil: pgtype.Timestamptz{Time: time.Now().Add(5 * time.Hour), Valid: true},
	}
	got := sessionBounds(in5h).MaxDuration
	if got <= tunnel.DefaultMaxDuration {
		t.Errorf("a 5h grant must outlive ADR-032's 4h ceiling, got %v", got)
	}

	// A short grant bounds the session below the default.
	in10m := store.PortForwardSession{
		AuthorizedUntil: pgtype.Timestamptz{Time: time.Now().Add(10 * time.Minute), Valid: true},
	}
	if got := sessionBounds(in10m).MaxDuration; got > 11*time.Minute {
		t.Errorf("a 10m grant must bound the session, got %v", got)
	}

	// Lapsed between mint and attach: the budget must stay POSITIVE, because a
	// zero would read as "unset" and silently restore the 4 h ceiling.
	lapsed := store.PortForwardSession{
		AuthorizedUntil: pgtype.Timestamptz{Time: time.Now().Add(-time.Minute), Valid: true},
	}
	if got := sessionBounds(lapsed).MaxDuration; got <= 0 {
		t.Errorf("an expired grant must not hand the bridge a zero budget, got %v", got)
	}
}

// The 403 must carry the URL, not just the refusal. Telling a developer "no"
// without telling them where to go is exactly how a control stops being used
// and starts being worked around (a socat relay, a copy of the database).
func TestAccessRequestRequiredCarriesTheRequestURL(t *testing.T) {
	a, db := flowAPI(t)
	db.truthy = true // the settings row exists and carries an FQDN

	endpoint := store.ExternalEndpoint{Name: "prod-replica"}
	_ = endpoint.Uuid.Scan(fixtureUUID)

	rec := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/external-endpoints/x/port-forwards", nil)
	a.writeAccessRequestRequired(rec, r, endpoint, "no live access grant for this endpoint")

	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	var body struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		RequestURL string `json:"request_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not the flat house error shape: %v (%s)", err, rec.Body)
	}
	// The CLI switches on this exact code to decide whether to open a browser
	// rather than give up; renaming it silently breaks that path.
	if body.Code != "access_request_required" {
		t.Errorf("code = %q, want access_request_required", body.Code)
	}
	if body.Message == "" {
		t.Error("the refusal must say what is missing")
	}
	if !strings.Contains(body.RequestURL, uuidString(endpoint.Uuid)) {
		t.Errorf("request_url = %q, want it to point at this endpoint's request page", body.RequestURL)
	}
	if !strings.HasPrefix(body.RequestURL, "https://") {
		t.Errorf("request_url = %q, want an absolute https URL the CLI can open", body.RequestURL)
	}
}

// A `sensitive` endpoint is spent from the CLI, which authenticates with a
// TOKEN, while the grant is obtained in the dashboard, which authenticates with
// a SESSION. Resolving the acting human from the session alone therefore made
// the endpoint unreachable from the CLI whatever grant existed — with the mint
// still handing back the URL of the page that issues them (ADR-045 §5).
func TestActingUserIDAnswersForTokensToo(t *testing.T) {
	user := int64(42)

	cases := map[string]struct {
		identity *auth.Identity
		want     *int64
	}{
		"a dashboard session":     {&auth.Identity{Session: true, UserID: &user}, &user},
		"a CLI token":             {&auth.Identity{UserID: &user}, &user},
		"a token with no creator": {&auth.Identity{}, nil},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := actingUserID(tc.identity)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("acting user = %d, want none", *got)
			case tc.want != nil && (got == nil || *got != *tc.want):
				t.Fatalf("acting user = %v, want %d", got, *tc.want)
			}
		})
	}
}
