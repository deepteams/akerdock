package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// RollbackApplication implements POST /applications/{uuid}/rollback
// (permission: deploy): redeploys a previously verified artifact without a
// rebuild (ADR-006). Target selection: deployment_uuid, image_digest, or —
// with an empty body — the most recent previous artifact. A missing
// artifact yields 409.
func (a *API) RollbackApplication(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, params api.RollbackApplicationParams) {
	id, ok := a.require(w, r, auth.PermApplicationsDeploy)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	var body struct {
		DeploymentUuid *string `json:"deployment_uuid"`
		ImageDigest    *string `json:"image_digest"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.DeploymentUuid != nil && body.ImageDigest != nil {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Code: ptr("invalid"), Message: "deployment_uuid and image_digest are mutually exclusive",
		}})
		return
	}

	artifact, err := a.resolveArtifact(r, row.Resource.ID, body.DeploymentUuid, body.ImageDigest)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			httpapi.WriteError(w, r, http.StatusConflict, "invalid_state",
				"no rollback artifact is available — the requested image no longer exists (ADR-006)")
			return
		}
		a.internalError(w, r, "rollback", err)
		return
	}

	active, err := a.Store.CountActiveDeploymentsForServer(r.Context(), row.ServerRowID)
	if err != nil {
		a.internalError(w, r, "rollback", err)
		return
	}
	server, err := a.Store.GetServerByID(r.Context(), row.ServerRowID)
	if err != nil {
		a.internalError(w, r, "rollback", err)
		return
	}
	if active >= int64(server.DeploymentQueueLimit) {
		w.Header().Set("Retry-After", "30")
		httpapi.WriteError(w, r, http.StatusTooManyRequests, "rate_limited", "the server deployment queue is full — retry later (§5.5)")
		return
	}

	u, err := pguuid.New()
	if err != nil {
		a.internalError(w, r, "rollback", err)
		return
	}
	snapshot, _ := json.Marshal(map[string]any{
		"config_version": row.Resource.Version,
		"rollback_to":    uuidString(artifact.Uuid),
		"image":          artifact.ImageName,
	})
	deployment, err := a.Store.CreateRollbackDeployment(r.Context(), store.CreateRollbackDeploymentParams{
		Uuid: u, ResourceID: row.Resource.ID, Trigger: store.DeploymentTriggerApi,
		ApiTokenID: apiTokenRef(id),
		ImageName:  &artifact.ImageName, ImageTag: artifact.ImageTag, ImageDigest: artifact.ImageDigest,
		ServerID: row.ServerRowID, ConfigSnapshot: snapshot,
	})
	if err != nil {
		a.internalError(w, r, "rollback", err)
		return
	}

	lockKey := "deploy:app:" + uuidString(row.Resource.Uuid)
	if _, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue:          "deploy",
		Type:           jobs.TypeDeploymentRun,
		Payload:        jobs.DeploymentRunPayload{DeploymentID: deployment.ID},
		LockKey:        &lockKey,
		TeamID:         ptr(id.TeamID),
		ResourceID:     ptr(row.Resource.ID),
		MaxAttempts:    1,
		IdempotencyKey: params.IdempotencyKey,
	}); err != nil {
		a.internalError(w, r, "rollback", err)
		return
	}

	a.recordAudit(r, id, "deployment.rollback", "deployment", deployment.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.DeploymentAccepted{
		DeploymentUuid: uuidString(deployment.Uuid),
		StatusUrl:      "/deployments/" + uuidString(deployment.Uuid),
	})
}

// resolveArtifact picks the rollback target per the OpenAPI contract.
func (a *API) resolveArtifact(r *http.Request, resourceID int64, deploymentUUID, digest *string) (store.DeploymentArtifact, error) {
	switch {
	case deploymentUUID != nil:
		var u pgtype.UUID
		if err := u.Scan(*deploymentUUID); err != nil {
			return store.DeploymentArtifact{}, pgx.ErrNoRows
		}
		return a.Store.GetArtifactForDeployment(r.Context(), store.GetArtifactForDeploymentParams{Uuid: u, ResourceID: resourceID})
	case digest != nil:
		return a.Store.GetArtifactByDigest(r.Context(), store.GetArtifactByDigestParams{ImageDigest: digest, ResourceID: resourceID})
	default:
		return a.Store.GetPreviousArtifact(r.Context(), resourceID)
	}
}
