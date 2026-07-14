package handlers

import (
	"net/http"
	"strings"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/queue"
)

// GetEncryptionStatus implements GET /system/encryption (permission: root):
// the key-version histogram of every encrypted column (ADR-003). It never
// contains key material. A rotation has converged once only the active
// version remains referenced.
func (a *API) GetEncryptionStatus(w http.ResponseWriter, r *http.Request) {
	id, ok := a.require(w, r, auth.PermRoot)
	if !ok {
		return
	}
	rows, err := a.Store.EncryptionKeyVersionHistogram(r.Context())
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
		if row.Rows == 0 {
			continue
		}
		v := int(row.KeyVersion)
		a, ok := versions[v]
		if !ok {
			a = &agg{}
			versions[v] = a
			order = append(order, v)
		}
		a.total += int(row.Rows)
		table, column, _ := strings.Cut(row.ColumnName, ".")
		a.columns = append(a.columns, api.EncryptionColumnCount{
			Table:  table,
			Column: column,
			Rows:   int(row.Rows),
		})
	}

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
	id, ok := a.require(w, r, auth.PermRoot)
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
