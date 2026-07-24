package jobs

import (
	"strings"
	"testing"

	"github.com/deepteams/akerdock/internal/store"
)

func ptr32(v int32) *int32 { return &v }

// An application-level domain on a compose stack must land on the stack's
// web service — the stack has no container of its own, and routing to it is
// a guaranteed 502 (compose-spec §6).
func TestResolveWebComponent(t *testing.T) {
	web := store.ServiceComponent{Name: "varuna", DefaultRoutePort: ptr32(1337)}
	db := store.ServiceComponent{Name: "postgres"}
	admin := store.ServiceComponent{Name: "admin", DefaultRoutePort: ptr32(9000)}

	// The single exposing service wins, port-less domain or not.
	c, err := resolveWebComponent([]store.ServiceComponent{db, web}, nil)
	if err != nil || c.Name != "varuna" {
		t.Fatalf("single web service: %v, %v", c.Name, err)
	}

	// With several candidates, the domain's target port discriminates.
	c, err = resolveWebComponent([]store.ServiceComponent{web, admin, db}, ptr32(9000))
	if err != nil || c.Name != "admin" {
		t.Fatalf("target-port match: %v, %v", c.Name, err)
	}

	// Several candidates and no discriminating port: a deterministic error
	// that names the fix, never a guessed container.
	if _, err = resolveWebComponent([]store.ServiceComponent{web, admin}, nil); err == nil ||
		!strings.Contains(err.Error(), "compose_routable_component_ambiguous") {
		t.Fatalf("ambiguity must error with remediation, got %v", err)
	}

	// No service exposes anything: same contract.
	if _, err = resolveWebComponent([]store.ServiceComponent{db}, nil); err == nil ||
		!strings.Contains(err.Error(), "compose_routable_port_unresolved") {
		t.Fatalf("no routable service must error, got %v", err)
	}
}
