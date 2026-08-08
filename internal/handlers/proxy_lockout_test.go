package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/proxy"
	"github.com/deepteams/akerdock/internal/store"
)

// The reserved scope is the authority on which proxy serves the dashboard: it
// is written by the control-plane route generator and by nothing else, so no
// second source can drift from it (ADR-062).
func TestRevisionsRouteTheDashboard(t *testing.T) {
	appOnly := []store.ProxyConfigRevision{{Scope: "0f9c2a11-uuid"}, {Scope: "another-uuid"}}
	if revisionsRouteTheDashboard(appOnly) {
		t.Fatal("an application server's proxy must keep its one-click stop")
	}
	if revisionsRouteTheDashboard(nil) {
		t.Fatal("a proxy that routes nothing cannot be routing the dashboard")
	}
	withDashboard := append(appOnly, store.ProxyConfigRevision{Scope: proxy.ControlPlaneScope})
	if !revisionsRouteTheDashboard(withDashboard) {
		t.Fatal("the reserved control-plane scope must be recognized")
	}
}

func TestProxyLockoutAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		want     bool
		wantOK   bool
		wantCode int
	}{
		// The body is optional on purpose: every proxy but one keeps its
		// one-click stop, so sending nothing is the normal case.
		{name: "no body", body: "", want: false, wantOK: true},
		{name: "empty object", body: "{}", want: false, wantOK: true},
		{name: "explicit false", body: `{"acknowledge_lockout":false}`, want: false, wantOK: true},
		{name: "acknowledged", body: `{"acknowledge_lockout":true}`, want: true, wantOK: true},
		{name: "malformed", body: "{oops", want: false, wantOK: false, wantCode: http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/servers/x/proxy/stop", strings.NewReader(tc.body))
			got, ok := proxyLockoutAcknowledged(recorder, request)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("acknowledged = %v, ok = %v; want %v, %v", got, ok, tc.want, tc.wantOK)
			}
			if tc.wantCode != 0 && recorder.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", recorder.Code, tc.wantCode)
			}
		})
	}
}
