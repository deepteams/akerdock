package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/store"
)

// The ingestion endpoint must refuse anything but an agent-scheme bearer
// BEFORE touching the store: a user bearer, a random string, or no token at
// all never reaches a query.
func TestAgentObservationsRequiresAgentToken(t *testing.T) {
	a := &API{} // Store deliberately nil: reaching it would panic the test
	for _, auth := range []string{"", "Bearer akd_user-token", "Bearer nope", "akda_raw-without-bearer-prefix-ok"} {
		req := httptest.NewRequest(http.MethodPost, "/agent/v1/observations", strings.NewReader(`{}`))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rr := httptest.NewRecorder()
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("auth %q reached the store before validating the scheme", auth)
				}
			}()
			// The raw-token form IS scheme-prefixed, so it proceeds to the
			// store lookup — exclude it from the nil-store run.
			if strings.HasPrefix(strings.TrimPrefix(auth, "Bearer "), "akda_") {
				return
			}
			a.AgentObservations(rr, req)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("auth %q: status = %d, want 401", auth, rr.Code)
			}
		}()
	}
}

func TestSplitComponentContainer(t *testing.T) {
	uuid := "fe09322c-657f-4f1f-a112-ce715c2eac1f"
	if u, comp, ok := splitComponentContainer(uuid + "-worker"); !ok || comp != "worker" || !u.Valid {
		t.Fatalf("compose container not parsed: ok=%v comp=%q", ok, comp)
	}
	for _, name := range []string{uuid, "akerdock-waker", "not-a-uuid-worker", uuid + "-"} {
		if _, _, ok := splitComponentContainer(name); ok {
			t.Fatalf("%q parsed as a component container", name)
		}
	}
}

func TestObservedFromDockerAction(t *testing.T) {
	cases := map[string]store.ResourceObservedStatus{
		"start":                    store.ResourceObservedStatusStarting,
		"restart":                  store.ResourceObservedStatusStarting,
		"health_status: healthy":   store.ResourceObservedStatusHealthy,
		"health_status: unhealthy": store.ResourceObservedStatusUnhealthy,
		"die":                      store.ResourceObservedStatusExited,
		"oom":                      store.ResourceObservedStatusExited,
	}
	for action, want := range cases {
		if got, ok := observedFromDockerAction(action); !ok || got != want {
			t.Fatalf("action %q → %v (%v), want %v", action, got, ok, want)
		}
	}
	for _, action := range []string{"pause", "destroy", "exec_start", ""} {
		if _, ok := observedFromDockerAction(action); ok {
			t.Fatalf("action %q mapped to an observed status, want skipped", action)
		}
	}
}
