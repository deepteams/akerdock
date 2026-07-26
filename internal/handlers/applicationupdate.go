package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// UpdateApplication implements PATCH /applications/{application_uuid}
// (permission: write). Sensitive PATCH — If-Match mandatory. Config
// changes are versioned (INV-014) and apply on the next deployment, except
// domains, whose routing is regenerated immediately.
func (a *API) UpdateApplication(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, params api.UpdateApplicationParams) {
	id, ok := a.require(w, r, auth.PermWrite)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	expected, err := strconv.Atoi(strings.Trim(strings.TrimSpace(params.IfMatch), `"`))
	if err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid If-Match header")
		return
	}

	var body api.ApplicationUpdate
	patch, ok := decodePatch(w, r, &body)
	if !ok {
		return
	}

	// source_type is immutable; per-source fields must match the pack.
	var details []api.ErrorDetail
	isGit := row.BuildConfig.BuildPack != store.BuildPackImage &&
		!(row.BuildConfig.BuildPack == store.BuildPackDockerfile && row.BuildConfig.DockerfileContent != nil)
	gitTouched := body.GitRepository != nil || body.GitBranch != nil || body.BuildPack != nil ||
		body.BaseDirectory != nil || body.DockerfileLocation != nil || patch.Has("publish_directory") ||
		body.ComposeFileLocation != nil || body.RawCompose != nil || body.WatchPaths != nil ||
		body.AutoDeploy != nil
	var newBuildPack *store.BuildPack
	if gitTouched && !isGit {
		details = append(details, api.ErrorDetail{Field: ptr("git_repository"), Code: ptr("invalid"), Message: "git settings only apply to git applications (source_type is immutable)"})
	} else if gitTouched {
		// Cross-field validation runs on the EFFECTIVE values — the body merged
		// over the stored row — exactly like the create-side validation, so a
		// branch change is validated against the stored URL and vice versa.
		if body.GitRepository != nil || body.GitBranch != nil {
			gitURL := deref(row.Application.GitRepositoryUrl)
			if body.GitRepository != nil {
				gitURL = *body.GitRepository
			}
			branch := deref(row.Application.GitBranch)
			if body.GitBranch != nil {
				branch = *body.GitBranch
			}
			details = append(details, validateGitSource(gitURL, branch, row.PrivateKeyUuid.Valid)...)
		}
		if body.BuildPack != nil {
			switch *body.BuildPack {
			case "dockerfile":
				newBuildPack = ptr(store.BuildPackDockerfile)
			case "static":
				newBuildPack = ptr(store.BuildPackStatic)
			case "nixpacks":
				newBuildPack = ptr(store.BuildPackNixpacks)
			case "compose":
				newBuildPack = ptr(store.BuildPackCompose)
			default:
				details = append(details, api.ErrorDetail{Field: ptr("build_pack"), Code: ptr("not_implemented"), Message: "build_pack must be dockerfile, static, nixpacks or compose (railpack lands later)"})
			}
		}
		for field, value := range map[string]*string{
			"base_directory":        body.BaseDirectory,
			"dockerfile_location":   body.DockerfileLocation,
			"publish_directory":     body.PublishDirectory,
			"compose_file_location": body.ComposeFileLocation,
		} {
			if value != nil && !safePathFormat.MatchString(*value) {
				details = append(details, api.ErrorDetail{Field: ptr(field), Code: ptr("invalid"), Message: "invalid " + field})
			}
		}
	}
	if body.DockerImage != nil || body.DockerImageTag != nil {
		if row.BuildConfig.BuildPack != store.BuildPackImage {
			details = append(details, api.ErrorDetail{Field: ptr("docker_image"), Code: ptr("invalid"), Message: "docker_image only applies to docker_image applications (source_type is immutable)"})
		}
		if body.DockerImage != nil && !jobs.ImageRef.MatchString(*body.DockerImage) {
			details = append(details, api.ErrorDetail{Field: ptr("docker_image"), Code: ptr("invalid"), Message: "invalid image reference"})
		}
		if body.DockerImageTag != nil && !jobs.TagRef.MatchString(*body.DockerImageTag) {
			details = append(details, api.ErrorDetail{Field: ptr("docker_image_tag"), Code: ptr("invalid"), Message: "invalid image tag"})
		}
	}
	if body.Dockerfile != nil {
		if row.BuildConfig.BuildPack != store.BuildPackDockerfile {
			details = append(details, api.ErrorDetail{Field: ptr("dockerfile"), Code: ptr("invalid"), Message: "dockerfile only applies to dockerfile applications (source_type is immutable)"})
		} else if strings.TrimSpace(*body.Dockerfile) == "" {
			details = append(details, api.ErrorDetail{Field: ptr("dockerfile"), Code: ptr("required"), Message: "dockerfile content cannot be empty"})
		}
	}
	name := row.Resource.Name
	if body.Name != nil {
		if *body.Name == "" || len(*body.Name) > 255 {
			details = append(details, api.ErrorDetail{Field: ptr("name"), Code: ptr("required"), Message: "name must be non-empty and at most 255 characters"})
		}
		name = *body.Name
	}
	var domainSpecs []domainSpec
	if body.Domains != nil {
		for _, raw := range *body.Domains {
			spec, err := parseDomain(raw)
			if err != nil {
				details = append(details, api.ErrorDetail{Field: ptr("domains"), Code: ptr("invalid"), Message: err.Error()})
				continue
			}
			domainSpecs = append(domainSpecs, spec)
		}
	}
	if len(details) > 0 {
		httpapi.WriteValidationError(w, r, details)
		return
	}

	description := row.Resource.Description
	if patch.Has("description") {
		description = body.Description
	}
	imageName, imageTag := row.BuildConfig.ImageName, row.BuildConfig.ImageTag
	if body.DockerImage != nil {
		imageName = body.DockerImage
	}
	if body.DockerImageTag != nil {
		imageTag = body.DockerImageTag
	}
	dockerfile := row.BuildConfig.DockerfileContent
	if body.Dockerfile != nil {
		dockerfile = body.Dockerfile
	}
	portsExposes := row.RuntimeConfig.PortsExposes
	if patch.Has("ports_exposes") {
		portsExposes = body.PortsExposes
	}
	memoryLimit := row.RuntimeConfig.MemoryLimit
	if body.Limits != nil && body.Limits.MemoryLimit != nil {
		memoryLimit = body.Limits.MemoryLimit
	}

	tx, err := a.Pool.Begin(r.Context())
	if err != nil {
		a.internalError(w, r, "update application", err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	qtx := a.Store.WithTx(tx)

	rows, err := qtx.UpdateResourceMeta(r.Context(), store.UpdateResourceMetaParams{
		ID: row.Resource.ID, Name: name, Description: description, ExpectedVersion: int32(expected),
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a resource with this name already exists in this environment")
			return
		}
		a.internalError(w, r, "update application", err)
		return
	}
	if rows == 0 {
		writeVersionConflict(w, r, row.Resource.Version)
		return
	}
	// An explicit null clears the credential ("this image is public now"); an
	// omitted field leaves it alone. Collapsing the two would keep pulling with
	// a credential the operator meant to remove.
	buildSource := store.UpdateBuildConfigSourceParams{
		ApplicationID: row.Resource.ID, ImageName: imageName, ImageTag: imageTag, DockerfileContent: dockerfile,
	}
	if patch.Has("registry_credential_uuid") {
		buildSource.SetRegistryCredential = true
		if body.RegistryCredentialUuid != nil && *body.RegistryCredentialUuid != "" {
			cred, ok := a.resolveRegistryCredential(w, r, id, *body.RegistryCredentialUuid)
			if !ok {
				return
			}
			buildSource.RegistryCredentialID = &cred.ID
		}
	}
	if err := qtx.UpdateBuildConfigSource(r.Context(), buildSource); err != nil {
		a.internalError(w, r, "update application", err)
		return
	}
	if gitTouched {
		gitParams := store.UpdateApplicationGitSettingsParams{
			ID: row.Resource.ID, GitRepositoryUrl: body.GitRepository, GitBranch: body.GitBranch,
			BaseDirectory: body.BaseDirectory, AutoDeployEnabled: body.AutoDeploy,
		}
		if body.WatchPaths != nil {
			gitParams.SetWatchPaths = true
			if joined := strings.Join(*body.WatchPaths, "\n"); joined != "" {
				gitParams.WatchPaths = &joined
			}
		}
		if err := qtx.UpdateApplicationGitSettings(r.Context(), gitParams); err != nil {
			a.internalError(w, r, "update application", err)
			return
		}
		buildParams := store.UpdateBuildConfigGitPipelineParams{
			ApplicationID: row.Resource.ID, BuildPack: newBuildPack,
			DockerfilePath: body.DockerfileLocation, ComposeFilePath: body.ComposeFileLocation,
			RawCompose: body.RawCompose,
		}
		if patch.Has("publish_directory") {
			buildParams.SetPublishDirectory = true
			if body.PublishDirectory != nil && *body.PublishDirectory != "" {
				buildParams.PublishDirectory = body.PublishDirectory
			}
		}
		if err := qtx.UpdateBuildConfigGitPipeline(r.Context(), buildParams); err != nil {
			a.internalError(w, r, "update application", err)
			return
		}
	}
	// Preview settings (§20.4): partial like everything else — an omitted
	// field is untouched, an explicit null clears the bound (TTL, cap).
	if body.PreviewsEnabled != nil || body.PreviewUrlTemplate != nil || patch.Has("preview_max_concurrent") ||
		patch.Has("preview_ttl_minutes") || body.PreviewProtection != nil ||
		body.PreviewForkApprovalEnabled != nil || body.PreviewExcludeDrafts != nil ||
		body.PreviewDeployOnOpen != nil || patch.Has("preview_url_templates") ||
		patch.Has("preview_require_label") || body.PreviewCommentCommandsEnabled != nil ||
		body.PreviewCancelObsoleteBuilds != nil || body.PreviewScaleToZero != nil ||
		body.PreviewScaleToZeroAfterMinutes != nil {
		params := store.UpdateApplicationPreviewSettingsParams{
			ID:                            row.Resource.ID,
			PreviewsEnabled:               body.PreviewsEnabled,
			PreviewUrlTemplate:            body.PreviewUrlTemplate,
			PreviewForkApprovalEnabled:    body.PreviewForkApprovalEnabled,
			PreviewExcludeDrafts:          body.PreviewExcludeDrafts,
			PreviewDeployOnOpen:           body.PreviewDeployOnOpen,
			PreviewCommentCommandsEnabled: body.PreviewCommentCommandsEnabled,
			PreviewCancelObsoleteBuilds:   body.PreviewCancelObsoleteBuilds,
			PreviewScaleToZero:            body.PreviewScaleToZero,
		}
		if body.PreviewScaleToZeroAfterMinutes != nil {
			m := int32(*body.PreviewScaleToZeroAfterMinutes)
			params.PreviewScaleToZeroAfterMinutes = &m
		}
		if patch.Has("preview_max_concurrent") {
			params.SetMaxConcurrent = true
			params.PreviewMaxConcurrent = int32PtrOf(body.PreviewMaxConcurrent)
		}
		if patch.Has("preview_ttl_minutes") {
			params.SetTtl = true
			params.PreviewTtlMinutes = int32PtrOf(body.PreviewTtlMinutes)
		}
		if patch.Has("preview_require_label") {
			params.SetRequireLabel = true
			if body.PreviewRequireLabel != nil && *body.PreviewRequireLabel != "" {
				params.PreviewRequireLabel = body.PreviewRequireLabel
			}
		}
		if body.PreviewProtection != nil {
			protection := store.PreviewProtection(*body.PreviewProtection)
			params.PreviewProtection = &protection
		}
		if patch.Has("preview_url_templates") {
			params.SetUrlTemplates = true
			// An empty table reverts to the legacy single template (stored NULL).
			if body.PreviewUrlTemplates != nil && len(*body.PreviewUrlTemplates) > 0 {
				if raw, err := json.Marshal(*body.PreviewUrlTemplates); err == nil {
					params.PreviewUrlTemplates = raw
				}
			}
		}
		if err := qtx.UpdateApplicationPreviewSettings(r.Context(), params); err != nil {
			a.internalError(w, r, "update application", err)
			return
		}
	}
	// Scale-to-zero of the application itself (ADR-037): a separate opt-in from
	// the preview scale-to-zero above.
	if body.ScaleToZero != nil || body.ScaleToZeroAfterMinutes != nil {
		stz := store.UpdateApplicationScaleToZeroParams{ID: row.Resource.ID, ScaleToZero: body.ScaleToZero}
		if body.ScaleToZeroAfterMinutes != nil {
			m := int32(*body.ScaleToZeroAfterMinutes)
			stz.ScaleToZeroAfterMinutes = &m
		}
		if err := qtx.UpdateApplicationScaleToZero(r.Context(), stz); err != nil {
			a.internalError(w, r, "update application", err)
			return
		}
	}
	// Provider API token and endpoint (amendment 31): stored on the
	// application's git source (protocols §3-§6). The token is write-only,
	// envelope-encrypted; an explicit null removes either. The source is
	// resolved ONCE for both fields — resolving twice would try to create
	// the same row twice when the application had none yet.
	if patch.Has("git_api_token") || patch.Has("git_api_url") {
		source, ok := a.ensureGitSource(w, r, qtx, id, row, "git_api_token")
		if !ok {
			return
		}
		if patch.Has("git_api_token") && !a.setGitAPIToken(w, r, qtx, source, body.GitApiToken) {
			return
		}
		if patch.Has("git_api_url") && !a.setGitAPIURL(w, r, qtx, source, body.GitApiUrl) {
			return
		}
	}

	// A PATCH that omits a hook leaves it as it was; an explicit null clears it.
	preCmd, postCmd := row.RuntimeConfig.PreDeploymentCommand, row.RuntimeConfig.PostDeploymentCommand
	if patch.Has("pre_deployment_command") {
		preCmd = body.PreDeploymentCommand
	}
	if patch.Has("post_deployment_command") {
		postCmd = body.PostDeploymentCommand
	}
	// The §10 guarantee needs a candidate, hence a health check (§7.3) — the
	// same rule as at creation, applied to the STATE THAT WILL RESULT from this
	// patch: adding the hook, or removing the health check under an existing
	// hook, would both break it.
	if postCmd != nil && *postCmd != "" && !a.willHaveHealthCheck(r, row.Resource.ID, body.HealthCheck) {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
			Field: ptr("post_deployment_command"), Code: ptr("requires_health_check"),
			Message: "a post-deployment command requires an enabled health check: without one there is no candidate " +
				"container, so a failing command could not leave the old one serving (§10, INV-005)",
		}})
		return
	}
	if err := qtx.UpdateRuntimeSettings(r.Context(), store.UpdateRuntimeSettingsParams{
		ApplicationID: row.Resource.ID, PortsExposes: portsExposes, MemoryLimit: memoryLimit,
		PreDeploymentCommand: preCmd, PostDeploymentCommand: postCmd,
	}); err != nil {
		a.internalError(w, r, "update application", err)
		return
	}
	if body.HealthCheck != nil {
		if _, err := qtx.UpsertHealthCheck(r.Context(), healthCheckParams(row.Resource.ID, *body.HealthCheck)); err != nil {
			a.internalError(w, r, "update application", err)
			return
		}
	}
	domainsChanged := patch.Has("domains")
	if domainsChanged {
		if err := qtx.DeleteDomainsForApplication(r.Context(), &row.Resource.ID); err != nil {
			a.internalError(w, r, "update application", err)
			return
		}
		for _, spec := range domainSpecs {
			du, err := pguuid.New()
			if err != nil {
				a.internalError(w, r, "update application", err)
				return
			}
			if _, err := qtx.CreateDomain(r.Context(), store.CreateDomainParams{
				Uuid: du, ApplicationID: ptr(row.Resource.ID), Fqdn: spec.FQDN, Path: spec.Path, TargetPort: spec.TargetPort,
			}); err != nil {
				if isUniqueViolation(err) {
					httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "the domain "+spec.FQDN+spec.Path+" is already routed by this instance")
					return
				}
				a.internalError(w, r, "update application", err)
				return
			}
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		a.internalError(w, r, "update application", err)
		return
	}

	updated, err := a.Store.GetApplicationByUUID(r.Context(), store.GetApplicationByUUIDParams{Uuid: row.Resource.Uuid, TeamID: id.TeamID})
	if err != nil {
		a.internalError(w, r, "reload application", err)
		return
	}

	// Domains regenerate the routing immediately (OpenAPI updateApplication)
	// — via a job, on servers with a managed proxy.
	if domainsChanged {
		server, err := a.Store.GetServerByID(r.Context(), updated.ServerRowID)
		if err == nil && server.ProxyType == store.ProxyTypeTraefik && server.Status == store.ServerStatusReady {
			lockKey := "deploy:app:" + uuidString(updated.Resource.Uuid)
			if _, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
				Queue:      "deploy",
				Type:       jobs.TypeApplyRouting,
				Payload:    jobs.ApplyRoutingPayload{ResourceID: updated.Resource.ID, Revision: int64(updated.Resource.Version)},
				LockKey:    &lockKey,
				TeamID:     ptr(id.TeamID),
				ResourceID: ptr(updated.Resource.ID),
			}); err != nil {
				a.Logger.Warn("failed to enqueue routing regeneration", "error", err)
			}
		}
	}

	a.recordAuditDiff(r, id, "application.update", "application", updated.Resource.Uuid,
		auditFieldsOf(appRow(row)), auditFieldsOf(appRow(updated)))
	w.Header().Set("ETag", etagFor(updated.Resource.Version))
	httpapi.WriteJSON(w, http.StatusOK, a.withDomains(r, applicationToAPI(appRow(updated)), updated.Resource.ID))
}

// willHaveHealthCheck reports whether the application ends this patch with an
// enabled health check — the patched value if the body carries one, the stored
// value otherwise.
func (a *API) willHaveHealthCheck(r *http.Request, resourceID int64, patched *api.HealthCheckConfig) bool {
	if patched != nil {
		return patched.Enabled != nil && *patched.Enabled
	}
	hc, err := a.Store.GetHealthCheck(r.Context(), resourceID)
	return err == nil && hc.Enabled
}

// int32PtrOf narrows the API's *int to the column's *int32.
func int32PtrOf(v *int) *int32 {
	if v == nil {
		return nil
	}
	out := int32(*v)
	return &out
}
