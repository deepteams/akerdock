package handlers

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// Revoking a grant — or closing a tunnel from the dashboard — has to reach the
// socket somebody is holding, not merely the row that records it. The registry
// is what carries the cut from the HTTP request to the bridge goroutine.
func TestTunnelPresenceCutsTheRegisteredBridge(t *testing.T) {
	var p TunnelPresence

	if p.Cut(1, tunnel.EndUserClose) {
		t.Error("cutting a session no bridge is running here must report that it reached nothing")
	}

	cancel := p.register(1)
	if !p.Cut(1, tunnelEndReasonRevoked) {
		t.Fatal("a registered bridge must be reachable")
	}
	select {
	case got := <-cancel:
		if got != tunnelEndReasonRevoked {
			t.Errorf("reason = %q, want revoked — the CLI prints this value to the developer", got)
		}
	case <-time.After(time.Second):
		t.Fatal("the cut never reached the bridge")
	}

	// A second cut must not block on a bridge that is already leaving: the
	// channel holds one value and nobody will read it again.
	done := make(chan bool, 1)
	go func() { done <- p.Cut(1, tunnelEndReasonRevoked) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a repeated cut blocked")
	}

	p.unregister(1)
	if p.Cut(1, tunnel.EndUserClose) {
		t.Error("a finished bridge must no longer be reachable")
	}
}

func TestTunnelPresenceClosesAndDrainsEveryBridgeOnShutdown(t *testing.T) {
	var p TunnelPresence
	first := p.register(1)
	second := p.register(2)

	if got := p.CloseAll(tunnel.EndDisconnect); got != 2 {
		t.Fatalf("closed bridge count = %d, want 2", got)
	}
	for i, cancel := range []<-chan tunnel.EndReason{first, second} {
		select {
		case got := <-cancel:
			if got != tunnel.EndDisconnect {
				t.Fatalf("bridge %d reason = %q, want disconnect", i+1, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("bridge %d was not closed", i+1)
		}
	}

	// A WebSocket accepted concurrently with shutdown must not escape the
	// original CloseAll snapshot.
	racing := p.register(3)
	select {
	case got := <-racing:
		if got != tunnel.EndDisconnect {
			t.Fatalf("racing bridge reason = %q, want disconnect", got)
		}
	case <-time.After(time.Second):
		t.Fatal("a bridge registered during shutdown remained open")
	}

	p.unregister(1)
	p.unregister(2)
	p.unregister(3)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !p.Wait(ctx) {
		t.Fatal("drain did not observe that all bridges were finalized")
	}
}

// The list and the 409 that caps a team's tunnels must never disagree about
// what "open" means, so this mirrors CountOpenPortForwardSessions exactly.
func TestPortForwardSessionActiveMatchesTheTeamCapDefinition(t *testing.T) {
	valid := func(d time.Duration) pgtype.Timestamptz {
		return pgtype.Timestamptz{Time: time.Now().Add(d), Valid: true}
	}
	cases := map[string]struct {
		row  store.ListPortForwardSessionsPageRow
		want bool
	}{
		"attached and running": {
			store.ListPortForwardSessionsPageRow{
				ClaimedAt: valid(-time.Minute), StartedAt: valid(-time.Minute),
				LastHeartbeatAt: valid(-10 * time.Second), TokenExpiresAt: valid(-time.Minute),
			}, true,
		},
		"legacy attached bridge without heartbeat": {
			store.ListPortForwardSessionsPageRow{
				ClaimedAt: valid(-time.Minute), StartedAt: valid(-time.Minute),
				TokenExpiresAt: valid(-time.Minute),
			}, true,
		},
		"stale heartbeat after process crash": {
			store.ListPortForwardSessionsPageRow{
				ClaimedAt: valid(-2 * time.Minute), StartedAt: valid(-2 * time.Minute),
				LastHeartbeatAt: valid(-2 * time.Minute), TokenExpiresAt: valid(-time.Minute),
			}, false,
		},
		"past hard duration": {
			store.ListPortForwardSessionsPageRow{
				ClaimedAt: valid(-5 * time.Hour), StartedAt: valid(-5 * time.Hour),
				LastHeartbeatAt: valid(-10 * time.Second), TokenExpiresAt: valid(-time.Minute),
			}, false,
		},
		"authorization expired": {
			store.ListPortForwardSessionsPageRow{
				ClaimedAt: valid(-time.Minute), StartedAt: valid(-time.Minute),
				LastHeartbeatAt: valid(-10 * time.Second), AuthorizedUntil: valid(-time.Second),
			}, false,
		},
		"minted, token still redeemable": {
			store.ListPortForwardSessionsPageRow{TokenExpiresAt: valid(time.Minute)}, true,
		},
		"minted but never redeemed": {
			store.ListPortForwardSessionsPageRow{TokenExpiresAt: valid(-time.Minute)}, false,
		},
		"ended": {
			store.ListPortForwardSessionsPageRow{ClaimedAt: valid(-time.Hour), EndedAt: valid(-time.Minute)}, false,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := portForwardSessionActive(tc.row); got != tc.want {
				t.Errorf("active = %v, want %v", got, tc.want)
			}
		})
	}
}

// The target kind is derived from the columns rather than stored, so a session
// outlives the thing it pointed at: the audit trail must not develop holes
// exactly where something was deleted.
func TestPortForwardSessionRenderingDerivesItsTarget(t *testing.T) {
	a, _ := flowAPI(t)
	r := httptest.NewRequest("GET", "/port-forward-sessions", nil)
	id := int64(7)

	var endpointUUID pgtype.UUID
	_ = endpointUUID.Scan(fixtureUUID)

	endpoint := a.portForwardSessionToAPI(r, store.ListPortForwardSessionsPageRow{
		TargetName:         "prod-replica",
		TargetPort:         5432,
		ExternalEndpointID: &id,
		EndpointUuid:       endpointUUID,
		TokenExpiresAt:     pgtype.Timestamptz{Time: time.Now().Add(time.Minute), Valid: true},
		AuthorizedUntil:    pgtype.Timestamptz{Time: time.Now().Add(2 * time.Hour), Valid: true},
	})
	if endpoint.TargetKind != api.PortForwardSessionInfoTargetKindExternalEndpoint {
		t.Errorf("target kind = %q, want external_endpoint", endpoint.TargetKind)
	}
	if endpoint.ExternalEndpointUuid == nil || *endpoint.ExternalEndpointUuid != fixtureUUID {
		t.Error("an endpoint session must name its endpoint, so the UI can link back to it")
	}
	if endpoint.AuthorizedUntil == nil {
		t.Error("the deadline must be visible: an operator reads it to know when this dies on its own")
	}
	if !endpoint.Active {
		t.Error("a freshly minted session with a redeemable token is open")
	}

	preview := a.portForwardSessionToAPI(r, store.ListPortForwardSessionsPageRow{
		TargetName: "shop · PR #12", PreviewID: &id, ResourceID: &id,
	})
	if preview.TargetKind != api.PortForwardSessionInfoTargetKindPreview {
		t.Errorf("target kind = %q, want preview — a preview instance is not its application", preview.TargetKind)
	}

	orphan := a.portForwardSessionToAPI(r, store.ListPortForwardSessionsPageRow{TargetName: "gone"})
	if orphan.TargetKind != api.PortForwardSessionInfoTargetKindUnknown {
		t.Errorf("target kind = %q, want unknown for a session whose target was deleted", orphan.TargetKind)
	}
	if orphan.TargetName != "gone" {
		t.Error("the label frozen at mint time is what keeps a deleted target readable")
	}
}

// Closing somebody else's tunnel is an administrative act. An API token has no
// user at all, so it can never claim ownership — it must go through the admin
// permission like any other third party.
func TestAnApiTokenOwnsNoTunnelSession(t *testing.T) {
	a, _ := flowAPI(t)
	r := httptest.NewRequest("DELETE", "/port-forward-sessions/x", nil)
	user := int64(3)

	token := &auth.Identity{TeamID: 1}
	if a.ownsPortForwardSession(r, token, store.PortForwardSession{UserID: &user}) {
		t.Error("a token holds no session identity: it cannot own a tunnel")
	}
	// A session opened by a token has no user either, so nobody owns it.
	browser := &auth.Identity{TeamID: 1, Session: true}
	if a.ownsPortForwardSession(r, browser, store.PortForwardSession{}) {
		t.Error("a session with no user cannot be owned by anyone")
	}
}
