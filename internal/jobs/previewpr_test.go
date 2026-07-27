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
			got := PreviewEngaged(store.Preview{Status: tc.status, LastDeployedAt: tc.last})
			if got != tc.want {
				t.Fatalf("PreviewEngaged(status=%s, deployed=%v) = %v, want %v",
					tc.status, tc.last.Valid, got, tc.want)
			}
		})
	}
}

// ManualFirstReserved is the gate shared by the PR webhook and the scheduler's
// capacity queue: a reserved preview (manual-first policy, no human deploy
// order) must never be auto-promoted — the bug was the scheduler promoting
// every 'queued' row, deploying previews the setting promised to leave alone.
func TestManualFirstReserved(t *testing.T) {
	set := pgtype.Timestamptz{Valid: true}
	appWith := func(deployOnOpen bool) store.GetApplicationByIDRow {
		var row store.GetApplicationByIDRow
		row.Application.PreviewDeployOnOpen = deployOnOpen
		return row
	}

	cases := []struct {
		name     string
		onOpen   bool
		preview  store.Preview
		reserved bool
	}{
		{"auto mode: fresh queued row is promotable", true,
			store.Preview{Status: store.PreviewStatusQueued}, false},
		{"manual mode: fresh queued row is RESERVED", false,
			store.Preview{Status: store.PreviewStatusQueued}, true},
		{"manual mode: human requested deploy — promotable", false,
			store.Preview{Status: store.PreviewStatusQueued, DeployRequestedAt: set}, false},
		{"manual mode: already engaged (active) — pushes keep updating", false,
			store.Preview{Status: store.PreviewStatusActive}, false},
		{"manual mode: deployed once then queued again — promotable", false,
			store.Preview{Status: store.PreviewStatusQueued, LastDeployedAt: set}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ManualFirstReserved(appWith(tc.onOpen), tc.preview)
			if got != tc.reserved {
				t.Fatalf("ManualFirstReserved(onOpen=%v, %+v) = %v, want %v",
					tc.onOpen, tc.preview, got, tc.reserved)
			}
		})
	}
}
