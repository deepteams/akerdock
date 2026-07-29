package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// errNothingToRedeploy: a skip_build deployment redeploys what is running, so
// something must be running. Never a 500 — the state is legitimate, it is the
// action that does not apply yet.
var errNothingToRedeploy = errors.New("nothing deployed to redeploy")

// deployBody is the request body shared by the deploy endpoints. The two
// flags are opposites — one rebuilds everything, the other builds nothing —
// and asking for both is a client bug, answered as such.
type deployBody struct {
	ForceRebuild bool `json:"force_rebuild"`
	SkipBuild    bool `json:"skip_build"`
}

func decodeDeployBody(w http.ResponseWriter, r *http.Request) (deployBody, bool) {
	var body deployBody
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.ForceRebuild && body.SkipBuild {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Code: ptr("invalid"), Message: "force_rebuild and skip_build are mutually exclusive",
		}})
		return deployBody{}, false
	}
	return body, true
}

// enqueueNoBuildDeployment queues a deployment that rebuilds nothing
// (ADR-048): the pipeline reruns whole — fresh environment file, container
// created again, health check, switchover — over the artifact already
// running. This is what applies an environment variable that changed since
// the last deployment; `restart` cannot, because a container freezes its
// environment at creation time.
//
// `preview` scopes it to one PR instance, whose artifact and variables are
// strictly its own (INV-010/INV-011).
func (a *API) enqueueNoBuildDeployment(r *http.Request, id *auth.Identity, row appRow, preview *store.Preview, idempotencyKey *string) (store.Deployment, error) {
	// The commit is inherited, never re-resolved: applying a configuration
	// must not quietly deploy whatever landed on the branch since.
	var last store.Deployment
	var err error
	if preview != nil {
		last, err = a.Store.GetLastSucceededPreviewDeployment(r.Context(), &preview.ID)
	} else {
		last, err = a.Store.GetLastSucceededDeployment(r.Context(), row.Resource.ID)
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.Deployment{}, errNothingToRedeploy
		}
		return store.Deployment{}, err
	}

	// A compose stack runs one image PER SERVICE, resolved from the compose
	// file at the deployed commit — there is no single artifact to pin, and
	// the engine reuses each service's image on its own (compose-spec §8.2).
	var artifact store.DeploymentArtifact
	if row.BuildConfig.BuildPack != store.BuildPackCompose {
		if preview != nil {
			artifact, err = a.Store.GetCurrentPreviewArtifact(r.Context(), &preview.ID)
		} else {
			artifact, err = a.Store.GetCurrentArtifact(r.Context(), row.Resource.ID)
		}
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return store.Deployment{}, errNothingToRedeploy
			}
			return store.Deployment{}, err
		}
	}

	active, err := a.Store.CountActiveDeploymentsForServer(r.Context(), row.ServerRowID)
	if err != nil {
		return store.Deployment{}, err
	}
	server, err := a.Store.GetServerByID(r.Context(), row.ServerRowID)
	if err != nil {
		return store.Deployment{}, err
	}
	if active >= int64(server.DeploymentQueueLimit) {
		return store.Deployment{}, errQueueFull
	}

	u, err := pguuid.New()
	if err != nil {
		return store.Deployment{}, err
	}
	snapshot, _ := json.Marshal(map[string]any{
		"config_version": row.Resource.Version,
		"applies_over":   uuidString(last.Uuid),
		"image":          artifact.ImageName,
	})
	params := store.CreateNoBuildDeploymentParams{
		Uuid: u, ResourceID: row.Resource.ID, Trigger: store.DeploymentTriggerConfigApply,
		ApiTokenID: apiTokenRef(id),
		ServerID:   row.ServerRowID, ConfigSnapshot: snapshot, CommitSha: last.CommitSha,
	}
	if artifact.ImageName != "" {
		params.ImageName, params.ImageTag, params.ImageDigest = &artifact.ImageName, artifact.ImageTag, artifact.ImageDigest
	}
	lockKey := "deploy:app:" + uuidString(row.Resource.Uuid)
	if preview != nil {
		params.PreviewID = &preview.ID
		// A preview deploys next to production, never serialized against it
		// (§20.4) — same lock its own deployments already take.
		lockKey = "deploy:preview:" + uuidString(preview.Uuid)
	}
	deployment, err := a.Store.CreateNoBuildDeployment(r.Context(), params)
	if err != nil {
		return store.Deployment{}, err
	}

	if _, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue:          "deploy",
		Type:           jobs.TypeDeploymentRun,
		Payload:        jobs.DeploymentRunPayload{DeploymentID: deployment.ID},
		LockKey:        &lockKey,
		TeamID:         ptr(id.TeamID),
		ResourceID:     ptr(row.Resource.ID),
		MaxAttempts:    1, // a failed deployment attempt is terminal (§21.1)
		IdempotencyKey: idempotencyKey,
	}); err != nil {
		return store.Deployment{}, err
	}
	return deployment, nil
}

// writeDeployError maps the shared deployment failures onto the contract:
// a full queue is a 429 with Retry-After, nothing to redeploy a 409.
func (a *API) writeDeployError(w http.ResponseWriter, r *http.Request, action string, err error) {
	switch {
	case errors.Is(err, errQueueFull):
		w.Header().Set("Retry-After", "30")
		httpapi.WriteError(w, r, http.StatusTooManyRequests, "rate_limited",
			"the server deployment queue is full — retry later (§5.5)")
	case errors.Is(err, errNothingToRedeploy):
		httpapi.WriteError(w, r, http.StatusConflict, "invalid_state",
			"there is no successful deployment to reapply the configuration over — deploy it once first")
	default:
		a.internalError(w, r, action, err)
	}
}
