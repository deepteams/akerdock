package session

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/deepteams/akerdock/internal/store"
)

// The relying party is the trust anchor of WebAuthn: a signature only counts
// for the RP ID it was minted for. Deriving it wrong does not fail loudly — it
// mints credentials that either never verify, or worse, verify for an origin
// the operator never intended to trust.

func TestRelyingPartyIsPinnedToTheFQDN(t *testing.T) {
	rpID, origins := RelyingParty("paas.example.com", 8080)
	if rpID != "paas.example.com" {
		t.Errorf("rpID = %q, want the FQDN itself", rpID)
	}
	// https only: the FQDN is how the operator said the instance is reached,
	// and a plain-http origin for it would accept credentials over a channel
	// anyone on the path can rewrite.
	if len(origins) != 1 || origins[0] != "https://paas.example.com" {
		t.Errorf("origins = %v, want exactly the https origin of the FQDN", origins)
	}
}

func TestRelyingPartyStripsThePortFromTheRPID(t *testing.T) {
	// An RP ID is a registrable domain; host:port is simply invalid and every
	// browser would refuse the ceremony. The ORIGIN, however, must keep the
	// port — origins compare exactly.
	rpID, origins := RelyingParty("paas.example.com:8443", 8080)
	if rpID != "paas.example.com" {
		t.Errorf("rpID = %q: the port must be stripped, an RP ID with a port is invalid", rpID)
	}
	if len(origins) != 1 || origins[0] != "https://paas.example.com:8443" {
		t.Errorf("origins = %v: the origin must keep the port, origins compare exactly", origins)
	}
}

func TestRelyingPartyFallsBackToLocalhostOnly(t *testing.T) {
	rpID, origins := RelyingParty("", 9000)
	if rpID != "localhost" {
		t.Errorf("rpID = %q, want localhost — the one host browsers treat as a secure context over plain HTTP", rpID)
	}
	if len(origins) != 1 || origins[0] != "http://localhost:9000" {
		t.Errorf("origins = %v, want only http://localhost:9000 — a wildcard fallback would trust any origin", origins)
	}
}

func TestNewPasskeysRefusesAnEmptyRelyingParty(t *testing.T) {
	// An empty RP ID would have to be filled from the request at ceremony
	// time — which is exactly the Host-header-derived trust this design
	// refuses. Better no passkeys than passkeys for whoever asks.
	if _, err := NewPasskeys(nil, nil, "", "AkerDock", nil); err == nil {
		t.Fatal("NewPasskeys accepted an empty RP ID: credentials would be minted for an unpinned relying party")
	}
}

// --- ceremony persistence, against a real PostgreSQL ----------------------
//
// Single-use and expiry are properties of the SQL (DELETE ... RETURNING with
// the expiry in the WHERE clause): mocking the store would test the mock.
// Same contract as the queue tests: AKERDOCK_TEST_DATABASE_URL or skip.

func testStore(t *testing.T) *store.Queries {
	t.Helper()
	url := os.Getenv("AKERDOCK_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("AKERDOCK_TEST_DATABASE_URL is not set — skipping the passkey ceremony integration tests")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("cannot connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return store.New(pool)
}

func ceremonyData(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(&webauthn.SessionData{Challenge: "test-challenge"})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCeremonyIsSingleUse(t *testing.T) {
	q := testStore(t)
	ctx := context.Background()

	token, _, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := q.CreatePasskeyCeremony(ctx, store.CreatePasskeyCeremonyParams{
		TokenHash: hashToken(token), Purpose: "login", Data: ceremonyData(t),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := q.ConsumePasskeyCeremony(ctx, store.ConsumePasskeyCeremonyParams{
		TokenHash: hashToken(token), Purpose: "login",
	}); err != nil {
		t.Fatalf("first consume failed: %v", err)
	}
	if _, err := q.ConsumePasskeyCeremony(ctx, store.ConsumePasskeyCeremonyParams{
		TokenHash: hashToken(token), Purpose: "login",
	}); err == nil {
		t.Fatal("a ceremony was consumed twice: a captured challenge response could be replayed")
	}
}

func TestCeremonyPurposeIsPartOfTheLookup(t *testing.T) {
	q := testStore(t)
	ctx := context.Background()

	token, _, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := q.CreatePasskeyCeremony(ctx, store.CreatePasskeyCeremonyParams{
		TokenHash: hashToken(token), Purpose: "registration", Data: ceremonyData(t),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := q.ConsumePasskeyCeremony(ctx, store.ConsumePasskeyCeremonyParams{
		TokenHash: hashToken(token), Purpose: "login",
	}); err == nil {
		t.Fatal("a registration ceremony was redeemed as a login one: the two flows have different verifiers, crossing them must be impossible")
	}
}

func TestExpiredCeremonyIsRefused(t *testing.T) {
	q := testStore(t)
	ctx := context.Background()

	token, _, err := newToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := q.CreatePasskeyCeremony(ctx, store.CreatePasskeyCeremonyParams{
		TokenHash: hashToken(token), Purpose: "login", Data: ceremonyData(t),
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(-time.Second), Valid: true},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := q.ConsumePasskeyCeremony(ctx, store.ConsumePasskeyCeremonyParams{
		TokenHash: hashToken(token), Purpose: "login",
	}); err == nil {
		t.Fatal("an expired ceremony was accepted: the begin→finish window must be bounded")
	}
}
