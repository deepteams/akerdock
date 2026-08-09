package handlers

import (
	"net/http"
	"sort"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/queue"
)

// GetEncryptionStatus implements GET /system/encryption (permission: root):
// the key-version histogram of every encrypted column (ADR-003). It never
// contains key material. A rotation has converged once only the active
// version remains referenced.
func (a *API) GetEncryptionStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := a.requireInstanceRoot(w, r)
	if !ok {
		return
	}
	raw, err := a.Store.EncryptionKeyVersionHistogram(r.Context())
	if err != nil {
		a.internalError(w, r, "encryption status", err)
		return
	}
	// The histogram enumerates the schema's encrypted columns rather than a
	// list kept here (ADR-003, migration 00093). That is what makes "only the
	// active version remains" safe to act on: the runbook turns that signal
	// into deleting the previous key, and a column missing from the histogram
	// would be a column silently left behind on a key about to disappear.
	rows, err := envelope.DecodeHistogram(raw)
	if err != nil {
		a.internalError(w, r, "encryption status", err)
		return
	}

	type agg struct {
		total   int
		columns []api.EncryptionColumnCount
	}
	versions := map[int]*agg{}
	order := make([]int, 0, 2)
	for _, row := range rows {
		if row.RowCount == 0 {
			continue
		}
		v := row.KeyVersion
		a, ok := versions[v]
		if !ok {
			a = &agg{}
			versions[v] = a
			order = append(order, v)
		}
		a.total += int(row.RowCount)
		a.columns = append(a.columns, api.EncryptionColumnCount{
			Table:  row.Table,
			Column: row.Column,
			Rows:   int(row.RowCount),
		})
	}
	// The database returns the histogram column by column, so versions appear
	// in inventory order; operators read it as a convergence indicator, and an
	// ascending key version is what that reading expects.
	sort.Ints(order)

	data := make([]api.EncryptionKeyVersion, 0, len(order))
	for _, v := range order {
		a := versions[v]
		cols := a.columns
		data = append(data, api.EncryptionKeyVersion{
			KeyVersion: v,
			TotalRows:  a.total,
			Columns:    &cols,
		})
	}

	// A rotation job in flight is reported so the operator can follow it.
	lockKey := rotationLockKey
	var rotationJob *string
	if active, err := a.Store.CountActiveJobsByLockKey(r.Context(), &lockKey); err == nil && active > 0 {
		rotationJob = ptr("in_progress")
	}
	_ = id

	httpapi.WriteJSON(w, http.StatusOK, api.EncryptionStatus{
		ActiveKeyVersion: int(a.Keyring.ActiveVersion()),
		KeyVersions:      &data,
		RotationJobUuid:  rotationJob,
	})
}

// rotationLockKey serializes encryption rotations instance-wide.
const rotationLockKey = "system:encryption:rotate"

// RotateEncryption implements POST /system/encryption/rotate (permission:
// root): re-encrypts every row still on an older key version towards the
// active one. Long operation — 202 + job; a rotation already running yields
// 409 operation_in_progress.
func (a *API) RotateEncryption(w http.ResponseWriter, r *http.Request, params api.RotateEncryptionParams) {
	id, ok := a.requireInstanceRoot(w, r)
	if !ok {
		return
	}
	lockKey := rotationLockKey
	if active, err := a.Store.CountActiveJobsByLockKey(r.Context(), &lockKey); err != nil {
		a.internalError(w, r, "rotate encryption", err)
		return
	} else if active > 0 {
		httpapi.WriteError(w, r, http.StatusConflict, "operation_in_progress", "an encryption rotation is already running")
		return
	}

	job, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue:   "maintenance",
		Type:    jobs.TypeEncryptionRotate,
		LockKey: &lockKey,
		// Instance-wide operation, but attached to the requesting team so the
		// operator can follow it on GET /jobs (which is team-scoped).
		TeamID:         ptr(id.TeamID),
		IdempotencyKey: params.IdempotencyKey,
	})
	if err != nil {
		a.internalError(w, r, "rotate encryption", err)
		return
	}
	a.recordAudit(r, id, "encryption.rotate", "instance", job.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.JobAccepted{
		JobUuid:   uuidString(job.Uuid),
		StatusUrl: "/jobs/" + uuidString(job.Uuid),
	})
}
