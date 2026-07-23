package handlers

import (
	"testing"

	"github.com/deepteams/akerdock/internal/auth"
)

// A dashboard session carries the SESSIONS row id in TokenID: writing it to
// deployments.api_token_id violates the foreign key to api_tokens (the exact
// 23503 seen in production) — a session caller must record NULL.
func TestApiTokenRef(t *testing.T) {
	if got := apiTokenRef(&auth.Identity{TokenID: 42}); got == nil || *got != 42 {
		t.Fatalf("bearer caller must reference its token, got %v", got)
	}
	if got := apiTokenRef(&auth.Identity{TokenID: 7, Session: true}); got != nil {
		t.Fatalf("session caller must record NULL, got %d", *got)
	}
}
