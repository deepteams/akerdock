package jobs

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/store"
)

// previewEngaged decides whether the manual-first policy (preview_deploy_on_open
// = false) still gates the webhook: a fresh preview is never engaged, anything
// that was ever promoted is.
func TestPreviewEngaged(t *testing.T) {
	deployed := pgtype.Timestamptz{Valid: true}

	cases := []struct {
		name   string
		status store.PreviewStatus
		last   pgtype.Timestamptz
		want   bool
	}{
		{"fresh queued row is not engaged", store.PreviewStatusQueued, pgtype.Timestamptz{}, false},
		{"deploying is engaged", store.PreviewStatusDeploying, pgtype.Timestamptz{}, true},
		{"active is engaged", store.PreviewStatusActive, pgtype.Timestamptz{}, true},
		{"failed is engaged (was attempted)", store.PreviewStatusFailed, pgtype.Timestamptz{}, true},
		{"queued but already deployed once is engaged", store.PreviewStatusQueued, deployed, true},
		{"destroyed is not engaged (revived, awaits manual)", store.PreviewStatusDestroyed, pgtype.Timestamptz{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := previewEngaged(store.Preview{Status: tc.status, LastDeployedAt: tc.last})
			if got != tc.want {
				t.Fatalf("previewEngaged(status=%s, deployed=%v) = %v, want %v",
					tc.status, tc.last.Valid, got, tc.want)
			}
		})
	}
}
