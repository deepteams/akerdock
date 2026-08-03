package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/httpapi"
	"github.com/deepteams/akerdock/internal/jobs"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// appRow is the common shape of the two application join queries.
type appRow struct {
	Resource        store.Resource
	Application     store.Application
	BuildConfig     store.BuildConfig
	RuntimeConfig   store.RuntimeConfig
	EnvironmentUuid pgtype.UUID
	ProjectUuid     pgtype.UUID
	DestinationUuid pgtype.UUID
	ServerUuid      pgtype.UUID
	ServerRowID     int64
	PrivateKeyUuid  pgtype.UUID
	// RegistryCredentialUuid is set when the image is pulled from a private
	// registry (amendement n°17).
	RegistryCredentialUuid pgtype.UUID
	// PushRegistryCredentialUuid is where a build server pushes what it built
	// (amendement n°19).
	PushRegistryCredentialUuid pgtype.UUID
	// GitApiTokenSet reports a provider API token on the git source — never
	// the token itself (INV-003, amendement n°31).
	GitApiTokenSet bool
	// GitApiUrl is the git source's self-hosted API endpoint.
	GitApiUrl *string
	// GithubAppUuid is set when a GitHub App provides the repository and the
	// clone authentication — the UI hides the manual git source then.
	GithubAppUuid pgtype.UUID
}

// watchPathsToAPI splits the stored newline-joined pattern list back into the
// API's array form; a NULL or empty column is an absent field, not [""].
// previewTemplatesToAPI decodes the stored preview route table (JSONB) for the
// response; nil (legacy single-template apps) reads back as absent (ADR-035).
func previewTemplatesToAPI(raw []byte) *[]api.PreviewRouteTemplate {
	if len(raw) == 0 {
		return nil
	}
	var rows []api.PreviewRouteTemplate
	if err := json.Unmarshal(raw, &rows); err != nil || len(rows) == 0 {
		return nil
	}
	return &rows
}

func watchPathsToAPI(stored *string) *[]string {
	if stored == nil || *stored == "" {
		return nil
	}
	return ptr(strings.Split(*stored, "\n"))
}

func applicationToAPI(row appRow) api.Application {
	var sourceType api.ApplicationSourceType
	switch row.BuildConfig.BuildPack {
	case store.BuildPackImage:
		sourceType = "docker_image"
	case store.BuildPackDockerfile:
		if row.BuildConfig.DockerfileContent != nil {
			sourceType = "dockerfile"
		} else {
			sourceType = "git"
		}
	default:
		sourceType = "git"
	}
	var limits *api.ResourceLimits
	if row.RuntimeConfig.MemoryLimit != nil {
		limits = &api.ResourceLimits{MemoryLimit: row.RuntimeConfig.MemoryLimit}
	}
	return api.Application{
		Uuid:                           ptr(uuidString(row.Resource.Uuid)),
		Name:                           row.Resource.Name,
		Description:                    row.Resource.Description,
		ProjectUuid:                    ptr(uuidString(row.ProjectUuid)),
		EnvironmentUuid:                ptr(uuidString(row.EnvironmentUuid)),
		ServerUuid:                     ptr(uuidString(row.ServerUuid)),
		DestinationUuid:                ptr(uuidString(row.DestinationUuid)),
		SourceType:                     ptr(sourceType),
		DockerImage:                    row.BuildConfig.ImageName,
		DockerImageTag:                 row.BuildConfig.ImageTag,
		Dockerfile:                     row.BuildConfig.DockerfileContent,
		GitRepository:                  row.Application.GitRepositoryUrl,
		GitBranch:                      row.Application.GitBranch,
		GithubAppUuid:                  optionalUUID(row.GithubAppUuid),
		BuildPack:                      buildPackToAPI(row.BuildConfig.BuildPack),
		BaseDirectory:                  ptr(row.Application.BaseDirectory),
		WatchPaths:                     watchPathsToAPI(row.Application.WatchPaths),
		AutoDeploy:                     ptr(row.Application.AutoDeployEnabled),
		DockerfileLocation:             row.BuildConfig.DockerfilePath,
		PublishDirectory:               row.BuildConfig.PublishDirectory,
		ComposeFileLocation:            row.BuildConfig.ComposeFilePath,
		RawCompose:                     ptr(row.BuildConfig.RawCompose),
		PrivateKeyUuid:                 optionalUUID(row.PrivateKeyUuid),
		RegistryCredentialUuid:         optionalUUID(row.RegistryCredentialUuid),
		UseBuildServer:                 ptr(row.BuildConfig.UseBuildServer),
		PushRegistryCredentialUuid:     optionalUUID(row.PushRegistryCredentialUuid),
		PreviewsEnabled:                ptr(row.Application.PreviewsEnabled),
		PreviewUrlTemplate:             ptr(row.Application.PreviewUrlTemplate),
		PreviewMaxConcurrent:           intPtrOf(row.Application.PreviewMaxConcurrent),
		PreviewTtlMinutes:              intPtrOf(row.Application.PreviewTtlMinutes),
		PreviewProtection:              ptr(api.ApplicationPreviewProtection(row.Application.PreviewProtection)),
		AccessProtection:               ptr(api.ApplicationAccessProtection(row.Application.AccessProtection)),
		AccessBasicAuthSet:             ptr(len(row.Application.AccessBasicAuthEnc) > 0),
		AccessPublicRoutes:             publicRoutesToAPI(row.Application.AccessPublicRoutes),
		PreviewForkApprovalEnabled:     ptr(row.Application.PreviewForkApprovalEnabled),
		PreviewExcludeDrafts:           ptr(row.Application.PreviewExcludeDrafts),
		PreviewDeployOnOpen:            ptr(row.Application.PreviewDeployOnOpen),
		PreviewUrlTemplates:            previewTemplatesToAPI(row.Application.PreviewUrlTemplates),
		PreviewRequireLabel:            row.Application.PreviewRequireLabel,
		PreviewCommentCommandsEnabled:  ptr(row.Application.PreviewCommentCommandsEnabled),
		PreviewCancelObsoleteBuilds:    ptr(row.Application.PreviewCancelObsoleteBuilds),
		PreviewScaleToZero:             ptr(row.Application.PreviewScaleToZero),
		PreviewScaleToZeroAfterMinutes: ptr(int(row.Application.PreviewScaleToZeroAfterMinutes)),
		ScaleToZero:                    ptr(row.Application.ScaleToZero),
		ScaleToZeroAfterMinutes:        ptr(int(row.Application.ScaleToZeroAfterMinutes)),
		ScaleAsleep:                    ptr(row.Application.ScaleSleptAt.Valid),
		GitApiTokenSet:                 ptr(row.GitApiTokenSet),
		GitApiUrl:                      row.GitApiUrl,
		PreDeploymentCommand:           row.RuntimeConfig.PreDeploymentCommand,
		PostDeploymentCommand:          row.RuntimeConfig.PostDeploymentCommand,
		PortsExposes:                   row.RuntimeConfig.PortsExposes,
		Noindex:                        ptr(row.RuntimeConfig.Noindex),
		Limits:                         limits,
		DesiredStatus:                  api.DesiredStatus(row.Resource.DesiredStatus),
		ObservedStatus:                 api.ObservedStatus(row.Resource.ObservedStatus),
		ObservedAt:                     timePtr(row.Resource.ObservedAt),
		Version:                        ptr(int(row.Resource.Version)),
		CreatedAt:                      timePtr(row.Resource.CreatedAt),
		UpdatedAt:                      timePtr(row.Resource.UpdatedAt),
	}
}

func (a *API) resolveApplication(w http.ResponseWriter, r *http.Request, id *auth.Identity, appUUID string) (store.GetApplicationByUUIDRow, bool) {
	u, ok := a.scanUUID(w, r, appUUID, "application")
	if !ok {
		return store.GetApplicationByUUIDRow{}, false
	}
	row, err := a.Store.GetApplicationByUUID(r.Context(), store.GetApplicationByUUIDParams{Uuid: u, TeamID: id.TeamID})
	return resolveRow(a, w, r, "application", row, err)
}

// ListApplications implements GET /applications (permission: read).
func (a *API) ListApplications(w http.ResponseWriter, r *http.Request, params api.ListApplicationsParams) {
	id, ok := a.require(w, r, auth.PermApplicationsRead)
	if !ok {
		return
	}
	limit, ok := pageLimit(w, r, params.Limit)
	if !ok {
		return
	}
	after, ok := afterID(w, r, params.Cursor)
	if !ok {
		return
	}
	rows, err := a.Store.ListApplicationsPage(r.Context(), store.ListApplicationsPageParams{
		TeamID: id.TeamID, AfterID: after, PageLimit: limit + 1,
	})
	if err != nil {
		a.internalError(w, r, "list applications", err)
		return
	}
	rows, cursor := nextCursor(rows, limit, func(row store.ListApplicationsPageRow) int64 { return row.Resource.ID })

	data := make([]api.Application, 0, len(rows))
	for _, row := range rows {
		data = append(data, applicationToAPI(appRow(row)))
	}
	httpapi.WriteJSON(w, http.StatusOK, struct {
		Data       []api.Application `json:"data"`
		NextCursor *string           `json:"next_cursor"`
	}{data, cursor})
}

// CreateApplication implements POST /applications (permission: write).
// P0 of this build: docker_image source only — dockerfile and git land
// with the build pipeline.
func (a *API) CreateApplication(w http.ResponseWriter, r *http.Request, params api.CreateApplicationParams) {
	id, ok := a.require(w, r, auth.PermApplicationsCreate)
	if !ok {
		return
	}
	var body api.ApplicationCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid JSON body")
		return
	}
	discriminator, err := body.Discriminator()
	if err != nil {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("source_type"), Code: ptr("required"), Message: "source_type is required (docker_image, dockerfile or git)"}})
		return
	}

	// Common base + per-source specifics (P0: docker_image and inline
	// dockerfile; git lands with the clone pipeline). The generated allOf
	// structs are flattened, so the shared base fields are copied out.
	var create api.ApplicationCreateBase
	var details []api.ErrorDetail
	buildPack := store.BuildPackImage
	var imageName, imageTag, dockerfileContent, gitURL, gitBranch, dockerfilePath, publishDirectory, composeFilePath, watchPaths *string
	rawCompose := false
	baseDirectory := "/"
	// Deploy key of a private repository (§5.1): resolved after validation,
	// once the team is known.
	deployKeyUUID := ""
	// GitHub App source (protocols §2): resolved once the team is known.
	githubAppUUID, githubRepoFullName := "", ""
	// Set when the image lives in a private registry: resolved after validation,
	// once the team is known (a credential of another team must be a 404, not a
	// 403 — INV-002).
	var registryCredentialUUID *string
	switch discriminator {
	case "docker_image":
		img, err := body.AsApplicationCreateDockerImage()
		if err != nil {
			httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid body")
			return
		}
		create = api.ApplicationCreateBase{
			Name: img.Name, Description: img.Description,
			ProjectUuid: img.ProjectUuid, EnvironmentUuid: img.EnvironmentUuid,
			ServerUuid: img.ServerUuid, Domains: img.Domains,
			PortsExposes: img.PortsExposes, Limits: img.Limits, InstantDeploy: img.InstantDeploy,
			Tags: img.Tags, HealthCheck: img.HealthCheck,
			// The generated allOf structs are flattened: a field not copied here
			// is silently dropped at creation.
			PreDeploymentCommand:  img.PreDeploymentCommand,
			PostDeploymentCommand: img.PostDeploymentCommand,
			// Flattened allOf again: forgetting one of these here does not fail to
			// compile, it silently drops what the caller asked for.
			UseBuildServer:             img.UseBuildServer,
			PushRegistryCredentialUuid: img.PushRegistryCredentialUuid,
		}
		if !jobs.ImageRef.MatchString(img.DockerImage) {
			details = append(details, api.ErrorDetail{Field: ptr("docker_image"), Code: ptr("invalid"), Message: "invalid image reference"})
		}
		tag := "latest"
		if img.DockerImageTag != nil {
			tag = *img.DockerImageTag
		}
		if !jobs.TagRef.MatchString(tag) {
			details = append(details, api.ErrorDetail{Field: ptr("docker_image_tag"), Code: ptr("invalid"), Message: "invalid image tag"})
		}
		imageName, imageTag = ptr(img.DockerImage), ptr(tag)
		registryCredentialUUID = img.RegistryCredentialUuid
	case "dockerfile":
		df, err := body.AsApplicationCreateDockerfile()
		if err != nil {
			httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid body")
			return
		}
		create = api.ApplicationCreateBase{
			Name: df.Name, Description: df.Description,
			ProjectUuid: df.ProjectUuid, EnvironmentUuid: df.EnvironmentUuid,
			ServerUuid: df.ServerUuid, Domains: df.Domains,
			PortsExposes: df.PortsExposes, Limits: df.Limits, InstantDeploy: df.InstantDeploy,
			Tags: df.Tags, HealthCheck: df.HealthCheck,
			// The generated allOf structs are flattened: a field not copied here
			// is silently dropped at creation.
			PreDeploymentCommand:  df.PreDeploymentCommand,
			PostDeploymentCommand: df.PostDeploymentCommand,
			// Flattened allOf again: forgetting one of these here does not fail to
			// compile, it silently drops what the caller asked for.
			UseBuildServer:             df.UseBuildServer,
			PushRegistryCredentialUuid: df.PushRegistryCredentialUuid,
		}
		buildPack = store.BuildPackDockerfile
		if strings.TrimSpace(df.Dockerfile) == "" {
			details = append(details, api.ErrorDetail{Field: ptr("dockerfile"), Code: ptr("required"), Message: "dockerfile content is required"})
		}
		dockerfileContent = ptr(df.Dockerfile)
	case "git":
		g, err := body.AsApplicationCreateGit()
		if err != nil {
			httpapi.WriteError(w, r, http.StatusBadRequest, httpapi.CodeBadRequest, "invalid body")
			return
		}
		create = api.ApplicationCreateBase{
			Name: g.Name, Description: g.Description,
			ProjectUuid: g.ProjectUuid, EnvironmentUuid: g.EnvironmentUuid,
			ServerUuid: g.ServerUuid, Domains: g.Domains,
			PortsExposes: g.PortsExposes, Limits: g.Limits, InstantDeploy: g.InstantDeploy,
			Tags: g.Tags, HealthCheck: g.HealthCheck,
			// The generated allOf structs are flattened: a field not copied here
			// is silently dropped at creation.
			PreDeploymentCommand:  g.PreDeploymentCommand,
			PostDeploymentCommand: g.PostDeploymentCommand,
			// Flattened allOf again: forgetting one of these here does not fail to
			// compile, it silently drops what the caller asked for.
			UseBuildServer:             g.UseBuildServer,
			PushRegistryCredentialUuid: g.PushRegistryCredentialUuid,
		}
		switch g.BuildPack {
		case "dockerfile":
			buildPack = store.BuildPackDockerfile
		case "static":
			buildPack = store.BuildPackStatic
		case "nixpacks":
			buildPack = store.BuildPackNixpacks
		case "compose":
			// The repository's compose file becomes a multi-service stack
			// (compose-spec.md) — validated at deploy time, once cloned.
			buildPack = store.BuildPackCompose
			if g.UseBuildServer != nil && *g.UseBuildServer {
				details = append(details, api.ErrorDetail{Field: ptr("use_build_server"), Code: ptr("not_implemented"), Message: "compose stacks build on the deployment server for now"})
			}
			composeFilePath = ptr("/docker-compose.yml")
			if g.ComposeFileLocation != nil {
				if !safePathFormat.MatchString(*g.ComposeFileLocation) {
					details = append(details, api.ErrorDetail{Field: ptr("compose_file_location"), Code: ptr("invalid"), Message: "invalid compose_file_location"})
				}
				composeFilePath = g.ComposeFileLocation
			}
			if g.RawCompose != nil {
				rawCompose = *g.RawCompose
			}
		default:
			details = append(details, api.ErrorDetail{Field: ptr("build_pack"), Code: ptr("not_implemented"), Message: "build_pack must be dockerfile, static, nixpacks or compose (railpack lands later)"})
		}
		// The published directory is COPYied by a generated Dockerfile that a
		// remote shell then builds (INV-012).
		if g.PublishDirectory != nil && *g.PublishDirectory != "" {
			if !safePathFormat.MatchString(*g.PublishDirectory) {
				details = append(details, api.ErrorDetail{Field: ptr("publish_directory"), Code: ptr("invalid"), Message: "invalid publish_directory"})
			}
			publishDirectory = g.PublishDirectory
		}
		if g.PrivateKeyUuid != nil && *g.PrivateKeyUuid != "" {
			deployKeyUUID = *g.PrivateKeyUuid
		}
		if g.GithubAppUuid != nil && *g.GithubAppUuid != "" {
			// GitHub App source (git-webhook-protocols §2): the repository
			// comes from the discovery cache, the clone from an installation
			// token — a deploy key on top would be two identities for one
			// clone, refused as ambiguous.
			if deployKeyUUID != "" {
				details = append(details, api.ErrorDetail{Field: ptr("private_key_uuid"), Code: ptr("invalid"), Message: "a GitHub App source does not use a deploy key"})
			}
			if g.RepositoryFullName == nil || *g.RepositoryFullName == "" {
				details = append(details, api.ErrorDetail{Field: ptr("repository_full_name"), Code: ptr("required"), Message: "repository_full_name (owner/name) is required with github_app_uuid"})
			}
			githubAppUUID = *g.GithubAppUuid
			if g.RepositoryFullName != nil {
				githubRepoFullName = *g.RepositoryFullName
			}
			gitBranch = ptr(g.GitBranch)
			if g.GitBranch != "" && len(details) == 0 {
				// URL and branch are resolved below, once the team is known.
				gitURL = ptr("")
			} else if g.GitBranch == "" {
				details = append(details, api.ErrorDetail{Field: ptr("git_branch"), Code: ptr("required"), Message: "git_branch is required"})
			}
		} else {
			details = append(details, validateGitSource(g.GitRepository, g.GitBranch, deployKeyUUID != "")...)
			gitURL, gitBranch = ptr(g.GitRepository), ptr(g.GitBranch)
		}
		if g.BaseDirectory != nil {
			if !safePathFormat.MatchString(*g.BaseDirectory) {
				details = append(details, api.ErrorDetail{Field: ptr("base_directory"), Code: ptr("invalid"), Message: "invalid base_directory"})
			}
			baseDirectory = *g.BaseDirectory
		}
		dockerfilePath = ptr("/Dockerfile")
		if g.DockerfileLocation != nil {
			if !safePathFormat.MatchString(*g.DockerfileLocation) {
				details = append(details, api.ErrorDetail{Field: ptr("dockerfile_location"), Code: ptr("invalid"), Message: "invalid dockerfile_location"})
			}
			dockerfilePath = g.DockerfileLocation
		}
		if g.WatchPaths != nil {
			if joined := strings.Join(*g.WatchPaths, "\n"); joined != "" {
				watchPaths = &joined
			}
		}
	default:
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("source_type"), Code: ptr("required"), Message: "source_type must be docker_image, dockerfile or git"}})
		return
	}

	// §10 promises that a failing post-deployment command does NOT switch traffic
	// and leaves the old container serving (INV-005). That promise only holds
	// when a CANDIDATE exists — i.e. when the deployment is rolling, which
	// requires a health check (§7.3). Without one, the old container is already
	// gone by the time the hook runs, and the guarantee would be a lie. A lie in
	// a safety guarantee is worse than no guarantee, so the combination is
	// refused rather than silently degraded.
	if create.PostDeploymentCommand != nil && *create.PostDeploymentCommand != "" &&
		(create.HealthCheck == nil || create.HealthCheck.Enabled == nil || !*create.HealthCheck.Enabled) {
		details = append(details, api.ErrorDetail{
			Field: ptr("post_deployment_command"), Code: ptr("requires_health_check"),
			Message: "a post-deployment command requires an enabled health check: without one there is no candidate " +
				"container, so a failing command could not leave the old one serving (§10, INV-005)",
		})
	}

	if create.Name == "" || len(create.Name) > 255 {
		details = append(details, api.ErrorDetail{Field: ptr("name"), Code: ptr("required"), Message: "name must be non-empty and at most 255 characters"})
	}
	var domainSpecs []domainSpec
	if create.Domains != nil {
		for _, raw := range *create.Domains {
			spec, err := parseDomain(raw)
			if err != nil {
				details = append(details, api.ErrorDetail{Field: ptr("domains"), Code: ptr("invalid"), Message: err.Error()})
				continue
			}
			domainSpecs = append(domainSpecs, spec)
		}
	}
	var tagNames []string
	if create.Tags != nil {
		for _, t := range *create.Tags {
			if t = strings.TrimSpace(t); t != "" {
				tagNames = append(tagNames, t)
			}
		}
	}
	if len(details) > 0 {
		httpapi.WriteValidationError(w, r, details)
		return
	}

	// Resolve the organizational chain within the team (INV-002).
	project, ok := a.resolveProject(w, r, id, create.ProjectUuid)
	if !ok {
		return
	}
	env, ok := a.resolveEnvironment(w, r, project, create.EnvironmentUuid)
	if !ok {
		return
	}
	server, ok := a.resolveServer(w, r, id, create.ServerUuid)
	if !ok {
		return
	}
	if server.Status != store.ServerStatusReady || server.IsBuildServer {
		httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("server_uuid"), Code: ptr("invalid_state"), Message: "the target server must be ready (validated) and not a build server"}})
		return
	}
	dest, err := a.defaultDestination(r, server.ID)
	if err != nil {
		a.internalError(w, r, "resolve destination", err)
		return
	}

	// GitHub App source: resolve the app, its git source and the discovered
	// repository — exact identity, never a URL comparison (INV-009).
	var gitSourceID *int64
	var repositoryID *int64
	if githubAppUUID != "" {
		gh, ok := a.resolveGithubApp(w, r, id, githubAppUUID)
		if !ok {
			return
		}
		if gh.AppID == nil || gh.InstallationID == nil {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("github_app_uuid"), Code: ptr("invalid_state"), Message: "this GitHub App is not installed yet"}})
			return
		}
		source, err := a.Store.GetGitSourceForGithubApp(r.Context(), &gh.ID)
		if err != nil {
			a.internalError(w, r, "github app source", err)
			return
		}
		repo, err := a.Store.GetRepositoryByFullName(r.Context(), store.GetRepositoryByFullNameParams{GithubAppID: &gh.ID, FullName: githubRepoFullName})
		if err != nil {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{Field: ptr("repository_full_name"), Code: ptr("invalid"), Message: "unknown repository — refresh the discovery (GET /github-apps/{uuid}/repositories?refresh=true)"}})
			return
		}
		gitSourceID = &source.ID
		repositoryID = &repo.ID
		cloneURL := strings.TrimRight(gh.HtmlUrl, "/") + "/" + repo.FullName + ".git"
		if repo.HtmlUrl != nil && *repo.HtmlUrl != "" {
			cloneURL = *repo.HtmlUrl + ".git"
		}
		gitURL = &cloneURL
	}

	// The deploy key must belong to the team (INV-002): an unknown or foreign
	// key is indistinguishable from a non-existent one.
	if deployKeyUUID != "" {
		key, ok := a.resolvePrivateKey(w, r, id, deployKeyUUID)
		if !ok {
			return
		}
		source, err := a.deployKeySource(r, id, key, *gitURL)
		if err != nil {
			a.internalError(w, r, "create application", err)
			return
		}
		gitSourceID = &source.ID
	}

	var registryCredentialID *int64
	if registryCredentialUUID != nil && *registryCredentialUUID != "" {
		cred, ok := a.resolveRegistryCredential(w, r, id, *registryCredentialUUID)
		if !ok {
			return
		}
		registryCredentialID = &cred.ID
	}

	// A build server without a push registry builds an image the deployment
	// server cannot pull: the build would succeed, on the wrong machine, and the
	// deployment would fail on "image not found". Refused here rather than
	// discovered there (§3.4).
	useBuildServer := create.UseBuildServer != nil && *create.UseBuildServer
	var pushCredentialID *int64
	if create.PushRegistryCredentialUuid != nil && *create.PushRegistryCredentialUuid != "" {
		cred, ok := a.resolveRegistryCredential(w, r, id, *create.PushRegistryCredentialUuid)
		if !ok {
			return
		}
		pushCredentialID = &cred.ID
	}
	if useBuildServer {
		if pushCredentialID == nil {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
				Field: ptr("push_registry_credential_uuid"), Code: ptr("required"),
				Message: "a build server needs a push registry: the image it builds must be pushed somewhere the deployment server can pull it from (§3.4)",
			}})
			return
		}
		if buildPack == store.BuildPackImage {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
				Field: ptr("use_build_server"), Code: ptr("invalid"),
				Message: "a docker_image application builds nothing, so there is nothing to build elsewhere",
			}})
			return
		}
		builders, err := a.Store.ListReadyBuildServers(r.Context(), id.TeamID)
		if err != nil {
			a.internalError(w, r, "create application", err)
			return
		}
		if len(builders) == 0 {
			httpapi.WriteValidationError(w, r, []api.ErrorDetail{{
				Field: ptr("use_build_server"), Code: ptr("invalid"),
				Message: "this team has no ready build server — register one (is_build_server) before asking to build on it",
			}})
			return
		}
	}

	u, err := pguuid.New()
	if err != nil {
		a.internalError(w, r, "create application", err)
		return
	}
	tx, err := a.Pool.Begin(r.Context())
	if err != nil {
		a.internalError(w, r, "create application", err)
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	qtx := a.Store.WithTx(tx)

	resource, err := qtx.CreateResource(r.Context(), store.CreateResourceParams{
		Uuid: u, TeamID: id.TeamID, EnvironmentID: env.ID, DestinationID: dest.ID,
		ResourceType: store.ResourceTypeApplication, Name: create.Name, Description: create.Description,
	})
	if err != nil {
		if isUniqueViolation(err) {
			httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "a resource with this name already exists in this environment")
			return
		}
		a.internalError(w, r, "create application", err)
		return
	}
	if err := qtx.CreateApplicationRow(r.Context(), store.CreateApplicationRowParams{
		ID: resource.ID, GitRepositoryUrl: gitURL, GitBranch: gitBranch, BaseDirectory: baseDirectory,
		GitSourceID: gitSourceID, RepositoryID: repositoryID, WatchPaths: watchPaths,
	}); err != nil {
		a.internalError(w, r, "create application", err)
		return
	}
	if err := qtx.CreateBuildConfig(r.Context(), store.CreateBuildConfigParams{
		ApplicationID: resource.ID, BuildPack: buildPack,
		ImageName: imageName, ImageTag: imageTag, DockerfileContent: dockerfileContent,
		DockerfilePath: dockerfilePath, PublishDirectory: publishDirectory,
		ComposeFilePath:          composeFilePath,
		RawCompose:               rawCompose,
		RegistryCredentialID:     registryCredentialID,
		UseBuildServer:           useBuildServer,
		PushEnabled:              pushCredentialID != nil,
		PushRegistryCredentialID: pushCredentialID,
	}); err != nil {
		a.internalError(w, r, "create application", err)
		return
	}
	var memoryLimit *string
	if create.Limits != nil {
		memoryLimit = create.Limits.MemoryLimit
	}
	if err := qtx.CreateRuntimeConfig(r.Context(), store.CreateRuntimeConfigParams{
		PreDeploymentCommand:  create.PreDeploymentCommand,
		PostDeploymentCommand: create.PostDeploymentCommand,
		ApplicationID:         resource.ID, PortsExposes: create.PortsExposes, MemoryLimit: memoryLimit,
		Noindex: create.Noindex != nil && *create.Noindex,
	}); err != nil {
		a.internalError(w, r, "create application", err)
		return
	}
	if create.HealthCheck != nil {
		if _, err := qtx.UpsertHealthCheck(r.Context(), healthCheckParams(resource.ID, *create.HealthCheck)); err != nil {
			a.internalError(w, r, "create application", err)
			return
		}
	}
	for _, name := range tagNames {
		tag, err := qtx.UpsertTag(r.Context(), store.UpsertTagParams{TeamID: id.TeamID, Name: name})
		if err != nil {
			a.internalError(w, r, "create application", err)
			return
		}
		if err := qtx.TagResource(r.Context(), store.TagResourceParams{ResourceID: resource.ID, TagID: tag.ID}); err != nil {
			a.internalError(w, r, "create application", err)
			return
		}
	}
	for _, spec := range domainSpecs {
		du, err := pguuid.New()
		if err != nil {
			a.internalError(w, r, "create application", err)
			return
		}
		if _, err := qtx.CreateDomain(r.Context(), store.CreateDomainParams{
			Uuid: du, ApplicationID: ptr(resource.ID), Fqdn: spec.FQDN, Path: spec.Path, TargetPort: spec.TargetPort,
		}); err != nil {
			if isUniqueViolation(err) {
				// (fqdn, path) uniqueness is instance-global (INV-002).
				httpapi.WriteError(w, r, http.StatusConflict, httpapi.CodeConflict, "the domain "+spec.FQDN+spec.Path+" is already routed by this instance")
				return
			}
			a.internalError(w, r, "create application", err)
			return
		}
	}
	if err := tx.Commit(r.Context()); err != nil {
		a.internalError(w, r, "create application", err)
		return
	}

	row, err := a.Store.GetApplicationByUUID(r.Context(), store.GetApplicationByUUIDParams{Uuid: resource.Uuid, TeamID: id.TeamID})
	if err != nil {
		a.internalError(w, r, "reload application", err)
		return
	}
	if create.InstantDeploy != nil && *create.InstantDeploy {
		if _, err := a.enqueueDeployment(r, id, appRow(row), false, nil); err != nil {
			a.Logger.Warn("instant deploy failed to enqueue", "error", err)
		}
	}
	w.Header().Set("ETag", etagFor(row.Resource.Version))
	httpapi.WriteJSON(w, http.StatusCreated, a.withDomains(r, applicationToAPI(appRow(row)), row.Resource.ID))
}

// GetApplication implements GET /applications/{application_uuid}.
func (a *API) GetApplication(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid) {
	id, ok := a.require(w, r, auth.PermApplicationsRead)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	w.Header().Set("ETag", etagFor(row.Resource.Version))
	httpapi.WriteJSON(w, http.StatusOK, a.withDomains(r, applicationToAPI(appRow(row)), row.Resource.ID))
}

// DeleteApplication implements DELETE /applications/{application_uuid}
// (permission: write): asynchronous deletion — routing, then workloads,
// then the logical object (§20.6). Volumes are kept unless requested.
func (a *API) DeleteApplication(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, params api.DeleteApplicationParams) {
	id, ok := a.require(w, r, auth.PermApplicationsDelete)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	deleteVolumes := params.DeleteVolumes != nil && *params.DeleteVolumes

	if err := a.Store.SetResourceDesiredStatus(r.Context(), store.SetResourceDesiredStatusParams{
		ID: row.Resource.ID, DesiredStatus: store.ResourceDesiredStatusDeleting,
	}); err != nil {
		a.internalError(w, r, "delete application", err)
		return
	}
	lockKey := "resource:delete:" + uuidString(row.Resource.Uuid)
	job, err := queue.Enqueue(r.Context(), a.Store, queue.EnqueueOptions{
		Queue:      "default",
		Type:       jobs.TypeApplicationDelete,
		Payload:    jobs.ApplicationDeletePayload{ResourceID: row.Resource.ID, DeleteVolumes: deleteVolumes},
		LockKey:    &lockKey,
		TeamID:     ptr(id.TeamID),
		ResourceID: ptr(row.Resource.ID),
	})
	if err != nil {
		a.internalError(w, r, "enqueue application deletion", err)
		return
	}
	a.recordAudit(r, id, "application.delete", "application", row.Resource.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.JobAccepted{
		JobUuid:   uuidString(job.Uuid),
		StatusUrl: "/jobs/" + uuidString(job.Uuid),
	})
}

// DeployApplication implements POST /applications/{application_uuid}/deploy
// (permission: deploy): queues a full deployment, subject to the per-server
// queue limit (§5.5).
func (a *API) DeployApplication(w http.ResponseWriter, r *http.Request, applicationUuid api.ApplicationUuid, params api.DeployApplicationParams) {
	id, ok := a.require(w, r, auth.PermApplicationsDeploy)
	if !ok {
		return
	}
	row, ok := a.resolveApplication(w, r, id, applicationUuid)
	if !ok {
		return
	}
	body, ok := decodeDeployBody(w, r)
	if !ok {
		return
	}

	// skip_build reruns the pipeline over the running artifact to apply the
	// current configuration (ADR-048) — no clone, no build.
	var deployment store.Deployment
	var err error
	action := "deployment.trigger"
	if body.SkipBuild {
		deployment, err = a.enqueueNoBuildDeployment(r, id, appRow(row), nil, params.IdempotencyKey)
		action = "deployment.apply_config"
	} else {
		deployment, err = a.enqueueDeployment(r, id, appRow(row), body.ForceRebuild, params.IdempotencyKey)
	}
	if err != nil {
		a.writeDeployError(w, r, "enqueue deployment", err)
		return
	}
	a.recordAudit(r, id, action, "deployment", deployment.Uuid)
	httpapi.WriteJSON(w, http.StatusAccepted, api.DeploymentAccepted{
		DeploymentUuid: uuidString(deployment.Uuid),
		StatusUrl:      "/deployments/" + uuidString(deployment.Uuid),
	})
}

// apiTokenRef is the deployments.api_token_id value for this caller: the
// token row for bearer callers, NULL for dashboard sessions — TokenID there
// is a sessions row ID, and the foreign key targets api_tokens.
func apiTokenRef(id *auth.Identity) *int64 {
	if id.Session {
		return nil
	}
	return ptr(id.TokenID)
}

var errQueueFull = &queueFullError{}

type queueFullError struct{}

func (*queueFullError) Error() string { return "deployment queue full" }

func (a *API) enqueueDeployment(r *http.Request, id *auth.Identity, row appRow, forceRebuild bool, idempotencyKey *string) (store.Deployment, error) {
	return a.enqueueDeploymentWith(r, id, row, forceRebuild, idempotencyKey, store.DeploymentTriggerApi)
}

// enqueueDeploymentWith creates the deployment and its job. Webhook-triggered
// deployments coalesce: an older queued webhook deployment for the same
// application is superseded by this one (§3.4). A leased/running one is
// never coalesced — it finishes, and this one waits for the lock (§3.1).
func (a *API) enqueueDeploymentWith(r *http.Request, id *auth.Identity, row appRow, forceRebuild bool, idempotencyKey *string, trigger store.DeploymentTrigger) (store.Deployment, error) {
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
		"image":          row.BuildConfig.ImageName,
		"tag":            row.BuildConfig.ImageTag,
	})
	deployment, err := a.Store.CreateDeployment(r.Context(), store.CreateDeploymentParams{
		Uuid: u, ResourceID: row.Resource.ID, Trigger: trigger,
		ApiTokenID: apiTokenRef(id), ForceRebuild: forceRebuild,
		ImageName: row.BuildConfig.ImageName, ImageTag: row.BuildConfig.ImageTag,
		ServerID: row.ServerRowID, ConfigSnapshot: snapshot,
	})
	if err != nil {
		return store.Deployment{}, err
	}

	if trigger == store.DeploymentTriggerWebhook {
		superseded, err := a.Store.SupersedeQueuedDeployments(r.Context(), store.SupersedeQueuedDeploymentsParams{
			ResourceID: row.Resource.ID, SupersededByID: &deployment.ID,
		})
		if err != nil {
			return store.Deployment{}, err
		}
		if len(superseded) > 0 {
			if err := a.Store.CancelJobsForDeployments(r.Context(), superseded); err != nil {
				return store.Deployment{}, err
			}
			a.Logger.Info("coalesced queued webhook deployments", "count", len(superseded), "app_uuid", uuidString(row.Resource.Uuid))
		}
	}

	lockKey := "deploy:app:" + uuidString(row.Resource.Uuid)
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

// defaultDestination returns the server's default destination, creating it
// lazily for servers validated before destinations existed.
func (a *API) defaultDestination(r *http.Request, serverID int64) (store.Destination, error) {
	dest, err := a.Store.GetDefaultDestination(r.Context(), serverID)
	if err == nil {
		return dest, nil
	}
	u, err := pguuid.New()
	if err != nil {
		return store.Destination{}, err
	}
	return a.Store.CreateDestination(r.Context(), store.CreateDestinationParams{
		Uuid: u, ServerID: serverID, Name: "default", Network: pguuid.String(u), IsDefault: true,
	})
}

// auditFieldsOf snapshots the fields of an application worth diffing in the
// audit trail (§23.4). Deliberately a small, explicit list rather than a
// reflection over the whole struct: a new sensitive column must not become an
// audit leak just because someone added it to the table.
func auditFieldsOf(row appRow) map[string]any {
	fields := map[string]any{
		"name":                 row.Resource.Name,
		"description":          deref(row.Resource.Description),
		"desired_status":       string(row.Resource.DesiredStatus),
		"ports_exposes":        row.RuntimeConfig.PortsExposes,
		"base_directory":       row.Application.BaseDirectory,
		"git_branch":           deref(row.Application.GitBranch),
		"git_repository":       deref(row.Application.GitRepositoryUrl),
		"build_pack":           string(row.BuildConfig.BuildPack),
		"docker_image":         deref(row.BuildConfig.ImageName),
		"docker_image_tag":     deref(row.BuildConfig.ImageTag),
		"access_protection":    string(row.Application.AccessProtection),
		"access_public_routes": string(row.Application.AccessPublicRoutes),
		"noindex":              row.RuntimeConfig.Noindex,
	}
	if row.RuntimeConfig.MemoryLimit != nil {
		fields["memory_limit"] = *row.RuntimeConfig.MemoryLimit
	}
	return fields
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// intPtrOf converts a nullable int32 column to the API's *int.
func intPtrOf(v *int32) *int {
	if v == nil {
		return nil
	}
	out := int(*v)
	return &out
}
