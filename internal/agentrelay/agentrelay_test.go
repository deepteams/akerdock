package agentrelay

import (
	"context"
	"errors"
	"testing"

	"github.com/deepteams/akerdock/internal/dockerruntime"
)

// The full bridge is exercised end-to-end in internal/handlers/relay_test.go
// (client + api loop + scripted channel); here only the resolution failures,
// which must all surface as the mandatory-agent unavailable class.
func TestSourceResolutionFailuresAreUnavailable(t *testing.T) {
	ctx := context.Background()

	noURL := &Source{
		BaseURL: func(context.Context) (string, error) { return "", errors.New("no settings") },
		Token:   func(context.Context, int64) (string, error) { return "akda_x", nil },
	}
	if _, err := noURL.Runtime(ctx, 1); !dockerruntime.IsUnavailable(err) {
		t.Fatalf("unresolved base url = %v, want IsUnavailable", err)
	}

	noToken := &Source{
		BaseURL: func(context.Context) (string, error) { return "http://127.0.0.1:1", nil },
		Token:   func(context.Context, int64) (string, error) { return "", errors.New("no keyring") },
	}
	if _, err := noToken.Runtime(ctx, 1); !dockerruntime.IsUnavailable(err) {
		t.Fatalf("unresolved token = %v, want IsUnavailable", err)
	}

	noAPI := &Source{
		BaseURL: func(context.Context) (string, error) { return "http://127.0.0.1:1", nil }, // nothing listens
		Token:   func(context.Context, int64) (string, error) { return "akda_x", nil },
	}
	if _, err := noAPI.Runtime(ctx, 1); !dockerruntime.IsUnavailable(err) {
		t.Fatalf("failed dial = %v, want IsUnavailable", err)
	}
}
