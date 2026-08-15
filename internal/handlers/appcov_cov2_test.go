package handlers

// Coverage tests for CreateApplication / ListApplications / DeleteApplication /
// DeployApplication (applications.go) and UpdateApplication
// (applicationupdate.go), on the appcov steerable protocol fake.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/store"
)

func appcovUniqueViolation() error { return &pgconn.PgError{Code: "23505"} }

// appcovCreateBody builds a CreateApplication JSON body with the mandatory
// organizational chain plus the given extra fields.
func appcovCreateBody(extra string) string {
	base := `"name":"app","project_uuid":"` + fixtureUUID + `","environment_uuid":"` + fixtureUUID +
		`","server_uuid":"` + fixtureUUID + `"`
	if extra != "" {
		base += "," + extra
	}
	return "{" + base + "}"
}

func appcovCreate(a *API, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	a.CreateApplication(rec, appcovReq(http.MethodPost, "/api/v1/applications", body), api.CreateApplicationParams{})
	return rec
}

const appcovGitHappy = `"source_type":"git","build_pack":"nixpacks",` +
	`"git_repository":"https://github.com/acme/unit.git","git_branch":"main",` +
	`"base_directory":"/app","dockerfile_location":"/Dockerfile","watch_paths":["src/**","cmd"],` +
	`"domains":["app.example.test:8080","www.example.test/blog"],"tags":[" web ",""],` +
	`"ports_exposes":"8080","pre_deployment_command":"pre","post_deployment_command":"post",` +
	`"health_check":{"enabled":true,"path":"/health"},"limits":{"memory_limit":"512m"}`

func TestAppcovCreateApplicationBadBodies(t *testing.T) {
	a, _ := appcovAPI(t)
	cases := []struct {
		name string
		body string
		want int
	}{
		{"invalid json", "{", http.StatusBadRequest},
		{"missing discriminator", "{}", http.StatusUnprocessableEntity},
		{"non-string discriminator", `{"source_type":123}`, http.StatusUnprocessableEntity},
		{"unknown discriminator", `{"source_type":"zip"}`, http.StatusUnprocessableEntity},
		{"mistyped docker_image", `{"source_type":"docker_image","name":123}`, http.StatusBadRequest},
		{"mistyped dockerfile", `{"source_type":"dockerfile","name":123}`, http.StatusBadRequest},
		{"mistyped git", `{"source_type":"git","name":123}`, http.StatusBadRequest},
		{
			"docker_image validation sweep",
			`{"source_type":"docker_image","name":"","project_uuid":"` + fixtureUUID + `","environment_uuid":"` + fixtureUUID +
				`","server_uuid":"` + fixtureUUID + `","docker_image":"Bad Image!","docker_image_tag":"!!",` +
				`"domains":["-bad-"],"post_deployment_command":"x"}`,
			http.StatusUnprocessableEntity,
		},
		{
			"dockerfile requires content",
			appcovCreateBody(`"source_type":"dockerfile","dockerfile":"   "`),
			http.StatusUnprocessableEntity,
		},
		{
			"git validation sweep",
			appcovCreateBody(`"source_type":"git","build_pack":"railpack","git_repository":"git@bad host:x",` +
				`"git_branch":"bad branch","publish_directory":"bad dir!","base_directory":"bad dir!",` +
				`"dockerfile_location":"bad file!"`),
			http.StatusUnprocessableEntity,
		},
		{
			"compose refuses a build server",
			appcovCreateBody(`"source_type":"git","build_pack":"compose","use_build_server":true,` +
				`"compose_file_location":"bad file!","git_repository":"https://github.com/a/b.git","git_branch":"main"`),
			http.StatusUnprocessableEntity,
		},
		{
			"github app conflicts",
			appcovCreateBody(`"source_type":"git","build_pack":"nixpacks","github_app_uuid":"` + fixtureUUID +
				`","private_key_uuid":"` + fixtureUUID + `","git_branch":""`),
			http.StatusUnprocessableEntity,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := appcovCreate(a, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d body = %s, want %d", rec.Code, rec.Body.String(), tc.want)
			}
		})
	}
}

func TestAppcovCreateApplicationHappyPaths(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"git nixpacks", appcovCreateBody(appcovGitHappy + `,"instant_deploy":true`)},
		{"git static publish", appcovCreateBody(`"source_type":"git","build_pack":"static",` +
			`"git_repository":"https://github.com/a/b.git","git_branch":"main","publish_directory":"dist"`)},
		{"git compose", appcovCreateBody(`"source_type":"git","build_pack":"compose",` +
			`"git_repository":"https://github.com/a/b.git","git_branch":"main",` +
			`"compose_file_location":"/dc.yml","raw_compose":true`)},
		{"git dockerfile pack", appcovCreateBody(`"source_type":"git","build_pack":"dockerfile",` +
			`"git_repository":"https://github.com/a/b.git","git_branch":"main"`)},
		{"inline dockerfile", appcovCreateBody(`"source_type":"dockerfile","dockerfile":"FROM nginx:1.27","noindex":true`)},
		{"docker image with registry credential", appcovCreateBody(`"source_type":"docker_image",` +
			`"docker_image":"nginx","registry_credential_uuid":"` + fixtureUUID + `"`)},
		{"deploy key", appcovCreateBody(`"source_type":"git","build_pack":"nixpacks",` +
			`"git_repository":"git@github.com:acme/unit.git","git_branch":"main","private_key_uuid":"` + fixtureUUID + `"`)},
		{"github app source", appcovCreateBody(`"source_type":"git","build_pack":"nixpacks",` +
			`"github_app_uuid":"` + fixtureUUID + `","repository_full_name":"acme/unit","git_branch":"main"`)},
		{"build server", appcovCreateBody(`"source_type":"git","build_pack":"nixpacks",` +
			`"git_repository":"https://github.com/a/b.git","git_branch":"main","use_build_server":true,` +
			`"push_registry_credential_uuid":"` + fixtureUUID + `"`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := appcovAPI(t)
			rec := appcovCreate(a, tc.body)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d body = %s, want 201", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("ETag") == "" {
				t.Fatal("missing ETag")
			}
		})
	}
}

func TestAppcovCreateApplicationLazyDefaultDestination(t *testing.T) {
	a, db := appcovAPI(t)
	db.noRows("GetDefaultDestination")
	rec := appcovCreate(a, appcovCreateBody(`"source_type":"dockerfile","dockerfile":"FROM nginx"`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s, want 201", rec.Code, rec.Body.String())
	}
}

func TestAppcovCreateApplicationDeployKeySourceCreatedOnFirstUse(t *testing.T) {
	a, db := appcovAPI(t)
	db.noRows("GetDeployKeySource")
	rec := appcovCreate(a, appcovCreateBody(`"source_type":"git","build_pack":"nixpacks",`+
		`"git_repository":"git@github.com:acme/unit.git","git_branch":"main","private_key_uuid":"`+fixtureUUID+`"`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s, want 201", rec.Code, rec.Body.String())
	}
}

func TestAppcovCreateApplicationServerMustBeReadyDeployTarget(t *testing.T) {
	a, db := appcovAPI(t)
	db.truthy = true // the resolved server reads back as a build server
	rec := appcovCreate(a, appcovCreateBody(`"source_type":"dockerfile","dockerfile":"FROM nginx"`))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "server_uuid") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestAppcovCreateApplicationGithubAppNotInstalled(t *testing.T) {
	a, db := appcovAPI(t)
	db.nilPointers("GetGithubAppByUUID")
	rec := appcovCreate(a, appcovCreateBody(`"source_type":"git","build_pack":"nixpacks",`+
		`"github_app_uuid":"`+fixtureUUID+`","repository_full_name":"acme/unit","git_branch":"main"`))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "not installed") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestAppcovCreateApplicationGithubRepositoryUnknown(t *testing.T) {
	a, db := appcovAPI(t)
	db.failOn("GetRepositoryByFullName", appcovDBErr())
	rec := appcovCreate(a, appcovCreateBody(`"source_type":"git","build_pack":"nixpacks",`+
		`"github_app_uuid":"`+fixtureUUID+`","repository_full_name":"acme/unit","git_branch":"main"`))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "unknown repository") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestAppcovCreateApplicationBuildServerPreconditions(t *testing.T) {
	t.Run("push registry required", func(t *testing.T) {
		a, _ := appcovAPI(t)
		rec := appcovCreate(a, appcovCreateBody(`"source_type":"git","build_pack":"nixpacks",`+
			`"git_repository":"https://github.com/a/b.git","git_branch":"main","use_build_server":true`))
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "push_registry_credential_uuid") {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("docker_image builds nothing", func(t *testing.T) {
		a, _ := appcovAPI(t)
		rec := appcovCreate(a, appcovCreateBody(`"source_type":"docker_image","docker_image":"nginx",`+
			`"use_build_server":true,"push_registry_credential_uuid":"`+fixtureUUID+`"`))
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "nothing to build") {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("no ready build server", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.noRows("ListReadyBuildServers")
		rec := appcovCreate(a, appcovCreateBody(`"source_type":"git","build_pack":"nixpacks",`+
			`"git_repository":"https://github.com/a/b.git","git_branch":"main","use_build_server":true,`+
			`"push_registry_credential_uuid":"`+fixtureUUID+`"`))
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "no ready build server") {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
	})
}

func TestAppcovCreateApplicationConflicts(t *testing.T) {
	t.Run("resource name", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.failOn("CreateResource", appcovUniqueViolation())
		rec := appcovCreate(a, appcovCreateBody(`"source_type":"dockerfile","dockerfile":"FROM nginx"`))
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d body = %s, want 409", rec.Code, rec.Body.String())
		}
	})
	t.Run("domain already routed", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.failOn("CreateDomain", appcovUniqueViolation())
		rec := appcovCreate(a, appcovCreateBody(appcovGitHappy))
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already routed") {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
	})
}

func TestAppcovCreateApplicationStoreFailures(t *testing.T) {
	steer := map[string]func(db *appcovDB){
		"GetGitSourceForGithubApp":        nil,
		"GetDeployKeySource":              nil,
		"ListReadyBuildServers":           nil,
		"CreateResource":                  nil,
		"CreateApplicationRow":            nil,
		"CreateBuildConfig":               nil,
		"CreateRuntimeConfig":             nil,
		"UpsertHealthCheck":               nil,
		"UpsertTag":                       nil,
		"TagResource":                     nil,
		"CreateDomain":                    nil,
		"GetApplicationByUUID":            nil,
		"CountActiveDeploymentsForServer": nil,
	}
	bodies := map[string]string{
		"GetGitSourceForGithubApp": appcovCreateBody(`"source_type":"git","build_pack":"nixpacks",` +
			`"github_app_uuid":"` + fixtureUUID + `","repository_full_name":"acme/unit","git_branch":"main"`),
		"GetDeployKeySource": appcovCreateBody(`"source_type":"git","build_pack":"nixpacks",` +
			`"git_repository":"git@github.com:acme/unit.git","git_branch":"main","private_key_uuid":"` + fixtureUUID + `"`),
		"ListReadyBuildServers": appcovCreateBody(`"source_type":"git","build_pack":"nixpacks",` +
			`"git_repository":"https://github.com/a/b.git","git_branch":"main","use_build_server":true,` +
			`"push_registry_credential_uuid":"` + fixtureUUID + `"`),
	}
	defaultBody := appcovCreateBody(appcovGitHappy + `,"instant_deploy":true`)
	for query := range steer {
		t.Run(query, func(t *testing.T) {
			a, db := appcovAPI(t)
			db.failOn(query, appcovDBErr())
			body, ok := bodies[query]
			if !ok {
				body = defaultBody
			}
			rec := appcovCreate(a, body)
			// An instant-deploy enqueue failure is only warned about: creation
			// already committed. Everything else on this list is a 500.
			want := http.StatusInternalServerError
			if query == "CountActiveDeploymentsForServer" {
				want = http.StatusCreated
			}
			if rec.Code != want {
				t.Fatalf("status = %d body = %s, want %d", rec.Code, rec.Body.String(), want)
			}
		})
	}

	t.Run("begin", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.beginErr = appcovDBErr()
		if rec := appcovCreate(a, defaultBody); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("commit", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.commitErr = appcovDBErr()
		if rec := appcovCreate(a, defaultBody); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("registry credential of another team", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.noRows("GetRegistryCredentialByUUID")
		rec := appcovCreate(a, appcovCreateBody(`"source_type":"docker_image","docker_image":"nginx",`+
			`"registry_credential_uuid":"`+fixtureUUID+`"`))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

// --- ListApplications / GetApplication / DeleteApplication ------------------

func TestAppcovListApplicationsParameterValidation(t *testing.T) {
	a, _ := appcovAPI(t)

	rec := httptest.NewRecorder()
	a.ListApplications(rec, appcovReq(http.MethodGet, "/x", ""), api.ListApplicationsParams{Limit: ptr(0)})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("limit: status = %d, want 400", rec.Code)
	}

	rec = httptest.NewRecorder()
	a.ListApplications(rec, appcovReq(http.MethodGet, "/x", ""), api.ListApplicationsParams{Cursor: ptr("!!!")})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("cursor: status = %d, want 400", rec.Code)
	}
}

func TestAppcovListApplicationsRejectsMalformedUUIDFilters(t *testing.T) {
	a, _ := appcovAPI(t)
	cases := []struct {
		name   string
		params api.ListApplicationsParams
	}{
		{"project_uuid", api.ListApplicationsParams{ProjectUuid: ptr("not-a-uuid")}},
		{"environment_uuid", api.ListApplicationsParams{EnvironmentUuid: ptr("not-a-uuid")}},
		{"server_uuid", api.ListApplicationsParams{ServerUuid: ptr("not-a-uuid")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			a.ListApplications(rec, appcovReq(http.MethodGet, "/x", ""), tc.params)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d body = %s, want 422", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAppcovListApplicationsAcceptsUUIDFilters(t *testing.T) {
	a, _ := appcovAPI(t)
	rec := httptest.NewRecorder()
	// A valid filter and an explicit empty one (= no filter) must both pass.
	a.ListApplications(rec, appcovReq(http.MethodGet, "/x", ""), api.ListApplicationsParams{
		ProjectUuid: ptr(fixtureUUID), EnvironmentUuid: ptr(fixtureUUID), ServerUuid: ptr(""),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
}

func TestAppcovListApplicationsStoreFailure(t *testing.T) {
	a, db := appcovAPI(t)
	db.failOn("ListApplicationsPage", appcovDBErr())
	rec := httptest.NewRecorder()
	a.ListApplications(rec, appcovReq(http.MethodGet, "/x", ""), api.ListApplicationsParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestAppcovGetApplicationMalformedUUID(t *testing.T) {
	a, _ := appcovAPI(t)
	rec := httptest.NewRecorder()
	a.GetApplication(rec, appcovReq(http.MethodGet, "/x", ""), "not-a-uuid")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestAppcovDeleteApplicationFailures(t *testing.T) {
	t.Run("desired status", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.failOn("SetResourceDesiredStatus", appcovDBErr())
		rec := httptest.NewRecorder()
		a.DeleteApplication(rec, appcovReq(http.MethodDelete, "/x", ""), fixtureUUID, api.DeleteApplicationParams{})
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("enqueue", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.failOn("EnqueueJob", appcovDBErr())
		rec := httptest.NewRecorder()
		a.DeleteApplication(rec, appcovReq(http.MethodDelete, "/x", ""), fixtureUUID, api.DeleteApplicationParams{DeleteVolumes: ptr(true)})
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
}

// --- DeployApplication ------------------------------------------------------

func TestAppcovDeployApplicationSkipBuild(t *testing.T) {
	a, _ := appcovAPI(t)
	rec := httptest.NewRecorder()
	a.DeployApplication(rec, appcovReq(http.MethodPost, "/x", `{"skip_build":true}`), fixtureUUID, api.DeployApplicationParams{})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d body = %s, want 202", rec.Code, rec.Body.String())
	}
}

func TestAppcovDeployApplicationExclusiveFlags(t *testing.T) {
	a, _ := appcovAPI(t)
	rec := httptest.NewRecorder()
	a.DeployApplication(rec, appcovReq(http.MethodPost, "/x", `{"skip_build":true,"force_rebuild":true}`), fixtureUUID, api.DeployApplicationParams{})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
}

func TestAppcovDeployApplicationQueueFull(t *testing.T) {
	a, db := appcovAPI(t)
	db.countOne = true // one active deployment against a queue limit of one
	rec := httptest.NewRecorder()
	a.DeployApplication(rec, appcovReq(http.MethodPost, "/x", `{}`), fixtureUUID, api.DeployApplicationParams{})
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("status = %d body = %s, want 429", rec.Code, rec.Body.String())
	}
}

func appcovAppRow(t *testing.T, a *API) appRow {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(fixtureUUID); err != nil {
		t.Fatal(err)
	}
	row, err := a.Store.GetApplicationByUUID(context.Background(), store.GetApplicationByUUIDParams{Uuid: u, TeamID: 1})
	if err != nil {
		t.Fatal(err)
	}
	return appRow(row)
}

func TestAppcovWebhookDeploymentsCoalesce(t *testing.T) {
	a, _ := appcovAPI(t)
	row := appcovAppRow(t, a)
	dep, err := a.enqueueDeploymentWith(appcovReq(http.MethodPost, "/x", ""), appcovIdentity(), row, false, nil, store.DeploymentTriggerWebhook)
	if err != nil {
		t.Fatalf("enqueueDeploymentWith: %v", err)
	}
	if !dep.Uuid.Valid {
		t.Fatal("deployment uuid missing")
	}
}

func TestAppcovEnqueueDeploymentFailures(t *testing.T) {
	for _, query := range []string{
		"CountActiveDeploymentsForServer",
		"GetServerByID",
		"CreateDeployment",
		"SupersedeQueuedDeployments",
		"CancelJobsForDeployments",
		"EnqueueJob",
	} {
		t.Run(query, func(t *testing.T) {
			a, db := appcovAPI(t)
			row := appcovAppRow(t, a)
			db.failOn(query, appcovDBErr())
			if _, err := a.enqueueDeploymentWith(appcovReq(http.MethodPost, "/x", ""), appcovIdentity(), row, true, nil, store.DeploymentTriggerWebhook); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestAppcovQueueFullError(t *testing.T) {
	if errQueueFull.Error() != "deployment queue full" {
		t.Fatalf("message = %q", errQueueFull.Error())
	}
}

// --- UpdateApplication ------------------------------------------------------

func appcovUpdate(a *API, body string) *httptest.ResponseRecorder {
	// The fixture row stores a post-deployment command while its health check
	// reads back disabled, which trips the §10 guard on every patch. Unless a
	// test drives that guard itself, clear the hook explicitly.
	if !strings.Contains(body, "post_deployment_command") && strings.HasPrefix(body, "{") && len(body) > 2 {
		body = `{"post_deployment_command":null,` + body[1:]
	}
	rec := httptest.NewRecorder()
	a.UpdateApplication(rec, appcovReq(http.MethodPatch, "/x", body), fixtureUUID, api.UpdateApplicationParams{IfMatch: `"1"`})
	return rec
}

func TestAppcovUpdateApplicationBadRequests(t *testing.T) {
	a, _ := appcovAPI(t)

	rec := httptest.NewRecorder()
	a.UpdateApplication(rec, appcovReq(http.MethodPatch, "/x", `{}`), fixtureUUID, api.UpdateApplicationParams{IfMatch: "zzz"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("if-match: status = %d, want 400", rec.Code)
	}

	if rec := appcovUpdate(a, "{"); rec.Code != http.StatusBadRequest {
		t.Fatalf("json: status = %d, want 400", rec.Code)
	}
}

func TestAppcovUpdateApplicationValidationSweep(t *testing.T) {
	a, _ := appcovAPI(t) // the stored application is a docker_image one
	cases := []struct {
		name string
		body string
		want string
	}{
		{"git fields on image app", `{"git_repository":"https://github.com/a/b.git"}`, "git settings only apply"},
		{"bad image reference", `{"docker_image":"Bad Image!","docker_image_tag":"!!"}`, "invalid image reference"},
		{"dockerfile on image app", `{"dockerfile":"FROM x"}`, "dockerfile only applies"},
		{"bad access protection", `{"access_protection":"weird"}`, "access_protection must be"},
		{"bad basic auth", `{"access_basic_auth":"nocolon"}`, "user:password"},
		{"empty name", `{"name":""}`, "name must be non-empty"},
		{"bad domain", `{"domains":["-bad-"]}`, "invalid domain"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := appcovUpdate(a, tc.body)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAppcovUpdateApplicationGitValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		// The stored row carries a deploy key: switching to https must fail
		// against the MERGED values.
		{"https with deploy key", `{"git_repository":"https://github.com/a/b.git","git_branch":"main"}`},
		{"branch only merges stored url", `{"git_branch":"bad branch"}`},
		{"unknown build pack", `{"build_pack":"railpack"}`},
		{"bad paths", `{"base_directory":"bad dir!","dockerfile_location":"bad file!",` +
			`"publish_directory":"bad dir!","compose_file_location":"bad file!"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, db := appcovAPI(t)
			db.setEnum("BuildPack", "nixpacks")
			rec := appcovUpdate(a, tc.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d body = %s, want 422", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAppcovUpdateApplicationGitHappy(t *testing.T) {
	a, db := appcovAPI(t)
	db.setEnum("BuildPack", "nixpacks")
	rec := appcovUpdate(a, `{"git_repository":"git@github.com:acme/app.git","git_branch":"main",`+
		`"build_pack":"static","base_directory":"/app","dockerfile_location":"/Dockerfile",`+
		`"publish_directory":"dist","compose_file_location":"/dc.yml","raw_compose":false,`+
		`"watch_paths":["a","b"],"auto_deploy":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("missing ETag")
	}
}

func TestAppcovUpdateApplicationGitClearsOptionals(t *testing.T) {
	a, db := appcovAPI(t)
	db.setEnum("BuildPack", "compose")
	rec := appcovUpdate(a, `{"publish_directory":null,"watch_paths":[],"build_pack":"dockerfile"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
}

func TestAppcovUpdateApplicationComposePublicRoutesRefused(t *testing.T) {
	a, db := appcovAPI(t)
	db.setEnum("BuildPack", "nixpacks")
	rec := appcovUpdate(a, `{"build_pack":"compose","access_public_routes":[{"path":"/h","methods":["GET"]}]}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "x-akerdock") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestAppcovUpdateApplicationPublicRoutes(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		a, _ := appcovAPI(t)
		rec := appcovUpdate(a, `{"access_public_routes":[{"path":"/health","methods":["GET"]}]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
		}
	})
	t.Run("invalid", func(t *testing.T) {
		a, _ := appcovAPI(t)
		rec := appcovUpdate(a, `{"access_public_routes":[{"path":"nope","methods":[]}]}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
	})
	t.Run("persistence failure", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.failOn("SetApplicationAccessPublicRoutes", appcovDBErr())
		rec := appcovUpdate(a, `{"access_public_routes":[]}`)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
}

func TestAppcovUpdateApplicationDockerfile(t *testing.T) {
	t.Run("empty content", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.setEnum("BuildPack", "dockerfile")
		rec := appcovUpdate(a, `{"dockerfile":"  "}`)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "cannot be empty") {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("new content", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.setEnum("BuildPack", "dockerfile")
		rec := appcovUpdate(a, `{"dockerfile":"FROM nginx:1.27"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
		}
	})
}

func TestAppcovUpdateApplicationVersionConflicts(t *testing.T) {
	t.Run("stale version", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.execZero("UpdateResourceMeta")
		rec := appcovUpdate(a, `{"name":"renamed"}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "version_conflict") {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("duplicate name", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.failOn("UpdateResourceMeta", appcovUniqueViolation())
		rec := appcovUpdate(a, `{"name":"renamed"}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already exists") {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
	})
}

func TestAppcovUpdateApplicationRegistryCredential(t *testing.T) {
	t.Run("explicit null clears", func(t *testing.T) {
		a, _ := appcovAPI(t)
		if rec := appcovUpdate(a, `{"registry_credential_uuid":null}`); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})
	t.Run("resolved within the team", func(t *testing.T) {
		a, _ := appcovAPI(t)
		if rec := appcovUpdate(a, `{"registry_credential_uuid":"`+fixtureUUID+`"}`); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})
	t.Run("foreign credential is a 404", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.noRows("GetRegistryCredentialByUUID")
		if rec := appcovUpdate(a, `{"registry_credential_uuid":"`+fixtureUUID+`"}`); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

func TestAppcovUpdateApplicationPreviewSettings(t *testing.T) {
	a, _ := appcovAPI(t)
	rec := appcovUpdate(a, `{"previews_enabled":true,"preview_url_template":"pr{{pr_id}}.example.test",`+
		`"preview_max_concurrent":3,"preview_ttl_minutes":60,"preview_protection":"sso",`+
		`"preview_fork_approval_enabled":true,"preview_exclude_drafts":true,"preview_deploy_on_open":false,`+
		`"preview_url_templates":[{"host":"pr{{pr_id}}.example.test","port":3000}],`+
		`"preview_require_label":"needs-preview","preview_comment_commands_enabled":true,`+
		`"preview_cancel_obsolete_builds":true,"preview_scale_to_zero":true,"preview_scale_to_zero_after_minutes":15}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
}

func TestAppcovUpdateApplicationPreviewSettingsCleared(t *testing.T) {
	a, _ := appcovAPI(t)
	rec := appcovUpdate(a, `{"preview_max_concurrent":null,"preview_ttl_minutes":null,`+
		`"preview_url_templates":[],"preview_require_label":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
}

func TestAppcovUpdateApplicationAccessWall(t *testing.T) {
	t.Run("generated credentials", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.emptyBytes("GetApplicationByUUID") // no stored basic-auth secret yet
		rec := appcovUpdate(a, `{"access_protection":"basic_auth"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
		}
	})
	t.Run("explicit credentials", func(t *testing.T) {
		a, _ := appcovAPI(t)
		rec := appcovUpdate(a, `{"access_protection":"sso","access_basic_auth":"user:pass"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
		}
	})
	t.Run("protection persistence failure", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.failOn("SetApplicationAccessProtection", appcovDBErr())
		if rec := appcovUpdate(a, `{"access_protection":"none"}`); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("credential persistence failure", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.failOn("SetApplicationAccessBasicAuth", appcovDBErr())
		if rec := appcovUpdate(a, `{"access_basic_auth":"user:pass"}`); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
}

func TestAppcovUpdateApplicationScaleToZero(t *testing.T) {
	a, _ := appcovAPI(t)
	if rec := appcovUpdate(a, `{"scale_to_zero":true,"scale_to_zero_after_minutes":30}`); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	a2, db := appcovAPI(t)
	db.failOn("UpdateApplicationScaleToZero", appcovDBErr())
	if rec := appcovUpdate(a2, `{"scale_to_zero":false}`); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestAppcovUpdateApplicationGitAPISettings(t *testing.T) {
	t.Run("token and url", func(t *testing.T) {
		a, _ := appcovAPI(t)
		rec := appcovUpdate(a, `{"git_api_token":"secret","git_api_url":"https://gitlab.example.test/api/v4/"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
		}
	})
	t.Run("explicit nulls clear", func(t *testing.T) {
		a, _ := appcovAPI(t)
		if rec := appcovUpdate(a, `{"git_api_token":null,"git_api_url":null}`); rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})
	t.Run("invalid url", func(t *testing.T) {
		a, _ := appcovAPI(t)
		if rec := appcovUpdate(a, `{"git_api_url":"ftp://x"}`); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
	})
	t.Run("source resolution failure", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.failOn("GetGitSourceByID", appcovDBErr())
		if rec := appcovUpdate(a, `{"git_api_token":"secret"}`); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
}

func TestAppcovUpdateApplicationHookGuard(t *testing.T) {
	t.Run("stored health check disabled", func(t *testing.T) {
		a, _ := appcovAPI(t)
		rec := appcovUpdate(a, `{"post_deployment_command":"migrate"}`)
		if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "requires_health_check") {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("patched health check disabled", func(t *testing.T) {
		a, _ := appcovAPI(t)
		rec := appcovUpdate(a, `{"post_deployment_command":"migrate","health_check":{"enabled":false}}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422", rec.Code)
		}
	})
	t.Run("patched health check enabled", func(t *testing.T) {
		a, _ := appcovAPI(t)
		rec := appcovUpdate(a, `{"post_deployment_command":"migrate","health_check":{"enabled":true},"pre_deployment_command":"pre"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
		}
	})
	t.Run("explicit null clears both hooks", func(t *testing.T) {
		a, _ := appcovAPI(t)
		rec := appcovUpdate(a, `{"pre_deployment_command":null,"post_deployment_command":null}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})
}

func TestAppcovUpdateApplicationDomains(t *testing.T) {
	t.Run("replaced and routing regenerated", func(t *testing.T) {
		a, _ := appcovAPI(t)
		rec := appcovUpdate(a, `{"domains":["app.example.test/api:8080"],"description":"new","ports_exposes":"8080",`+
			`"noindex":true,"limits":{"memory_limit":"1g"}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
		}
	})
	t.Run("conflicting domain", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.failOn("CreateDomain", appcovUniqueViolation())
		rec := appcovUpdate(a, `{"domains":["app.example.test"]}`)
		if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "already routed") {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("routing enqueue failure is only warned", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.failOn("EnqueueJob", appcovDBErr())
		rec := appcovUpdate(a, `{"domains":["app.example.test"]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
		}
	})
	t.Run("server lookup failure skips routing", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.failOn("GetServerByID", appcovDBErr())
		rec := appcovUpdate(a, `{"domains":[]}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
		}
	})
}

func TestAppcovUpdateApplicationStoreFailures(t *testing.T) {
	cases := []struct {
		query string
		body  string
	}{
		{"UpdateResourceMeta", `{"name":"renamed"}`},
		{"UpdateBuildConfigSource", `{"name":"renamed"}`},
		{"UpdateApplicationGitSettings", `{"auto_deploy":true}`},
		{"UpdateBuildConfigGitPipeline", `{"auto_deploy":true}`},
		{"UpdateApplicationPreviewSettings", `{"previews_enabled":true}`},
		{"UpdateRuntimeSettings", `{"name":"renamed"}`},
		{"UpsertHealthCheck", `{"health_check":{"enabled":true}}`},
		{"DeleteDomainsForApplication", `{"domains":[]}`},
		{"CreateDomain", `{"domains":["app.example.test"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			a, db := appcovAPI(t)
			db.setEnum("BuildPack", "nixpacks")
			db.failOn(tc.query, appcovDBErr())
			rec := appcovUpdate(a, tc.body)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d body = %s, want 500", rec.Code, rec.Body.String())
			}
		})
	}

	t.Run("begin", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.beginErr = appcovDBErr()
		if rec := appcovUpdate(a, `{"name":"renamed"}`); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("commit", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.commitErr = appcovDBErr()
		if rec := appcovUpdate(a, `{"name":"renamed"}`); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("reload", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.failOnAfter("GetApplicationByUUID", 1, appcovDBErr())
		if rec := appcovUpdate(a, `{"name":"renamed"}`); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
}

func TestAppcovCreateApplicationChainResolutionFailures(t *testing.T) {
	body := appcovCreateBody(`"source_type":"dockerfile","dockerfile":"FROM nginx"`)
	t.Run("environment", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.noRows("GetEnvironmentByUUID")
		if rec := appcovCreate(a, body); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
	t.Run("server", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.noRows("GetServerByUUID")
		if rec := appcovCreate(a, body); rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
	t.Run("destination", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.failOn("GetDefaultDestination", appcovDBErr())
		db.failOn("CreateDestination", appcovDBErr())
		if rec := appcovCreate(a, body); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("github app", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.noRows("GetGithubAppByUUID")
		rec := appcovCreate(a, appcovCreateBody(`"source_type":"git","build_pack":"nixpacks",`+
			`"github_app_uuid":"`+fixtureUUID+`","repository_full_name":"acme/unit","git_branch":"main"`))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
	t.Run("private key", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.noRows("GetPrivateKeyByUUID")
		rec := appcovCreate(a, appcovCreateBody(`"source_type":"git","build_pack":"nixpacks",`+
			`"git_repository":"git@github.com:acme/unit.git","git_branch":"main","private_key_uuid":"`+fixtureUUID+`"`))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
	t.Run("push registry credential", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.noRows("GetRegistryCredentialByUUID")
		rec := appcovCreate(a, appcovCreateBody(`"source_type":"git","build_pack":"nixpacks",`+
			`"git_repository":"https://github.com/a/b.git","git_branch":"main","use_build_server":true,`+
			`"push_registry_credential_uuid":"`+fixtureUUID+`"`))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

func TestAppcovListApplicationsPagination(t *testing.T) {
	a, db := appcovAPI(t)
	db.rowsFor("ListApplicationsPage", 2) // limit+1 rows: a second page exists
	rec := httptest.NewRecorder()
	a.ListApplications(rec, appcovReq(http.MethodGet, "/x", ""), api.ListApplicationsParams{Limit: ptr(1)})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"next_cursor":"`) {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestAppcovUpdateApplicationDockerImage(t *testing.T) {
	a, _ := appcovAPI(t)
	rec := appcovUpdate(a, `{"docker_image":"nginx","docker_image_tag":"1.27"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want 200", rec.Code, rec.Body.String())
	}
}

func TestAppcovUpdateApplicationDockerImageOnGitApp(t *testing.T) {
	a, db := appcovAPI(t)
	db.setEnum("BuildPack", "nixpacks")
	rec := appcovUpdate(a, `{"docker_image":"nginx"}`)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "docker_image only applies") {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestAppcovUpdateApplicationGitAPIPersistenceFailures(t *testing.T) {
	t.Run("token", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.failOn("SetGitSourceAPIToken", appcovDBErr())
		if rec := appcovUpdate(a, `{"git_api_token":"secret"}`); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
	t.Run("url", func(t *testing.T) {
		a, db := appcovAPI(t)
		db.failOn("SetGitSourceAPIURL", appcovDBErr())
		if rec := appcovUpdate(a, `{"git_api_url":"https://api.example.test"}`); rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
	})
}

// --- small mapping helpers --------------------------------------------------

func TestAppcovApplicationToAPISourceTypes(t *testing.T) {
	row := appRow{}
	row.BuildConfig.BuildPack = store.BuildPackImage
	if got := applicationToAPI(row); *got.SourceType != "docker_image" {
		t.Fatalf("image source = %q", *got.SourceType)
	}
	row.BuildConfig.BuildPack = store.BuildPackDockerfile
	row.BuildConfig.DockerfileContent = ptr("FROM x")
	if got := applicationToAPI(row); *got.SourceType != "dockerfile" {
		t.Fatalf("dockerfile source = %q", *got.SourceType)
	}
	row.BuildConfig.DockerfileContent = nil
	if got := applicationToAPI(row); *got.SourceType != "git" {
		t.Fatalf("dockerfile-pack git source = %q", *got.SourceType)
	}
	row.BuildConfig.BuildPack = store.BuildPackNixpacks
	row.RuntimeConfig.MemoryLimit = ptr("512m")
	got := applicationToAPI(row)
	if *got.SourceType != "git" || got.Limits == nil || *got.Limits.MemoryLimit != "512m" {
		t.Fatalf("git source = %+v", got)
	}
}

func TestAppcovPreviewTemplatesToAPI(t *testing.T) {
	if previewTemplatesToAPI(nil) != nil {
		t.Fatal("nil raw must read back absent")
	}
	if previewTemplatesToAPI([]byte("not json")) != nil {
		t.Fatal("invalid JSON must read back absent")
	}
	if previewTemplatesToAPI([]byte("[]")) != nil {
		t.Fatal("empty table must read back absent")
	}
	rows := previewTemplatesToAPI([]byte(`[{"host":"pr.example.test"}]`))
	if rows == nil || len(*rows) != 1 || (*rows)[0].Host != "pr.example.test" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestAppcovWatchPathsToAPI(t *testing.T) {
	if watchPathsToAPI(nil) != nil || watchPathsToAPI(ptr("")) != nil {
		t.Fatal("empty watch paths must read back absent")
	}
	got := watchPathsToAPI(ptr("a\nb"))
	if got == nil || len(*got) != 2 || (*got)[0] != "a" {
		t.Fatalf("watch paths = %+v", got)
	}
}

func TestAppcovScalarHelpers(t *testing.T) {
	if deref(nil) != "" || deref(ptr("x")) != "x" {
		t.Fatal("deref")
	}
	if intPtrOf(nil) != nil {
		t.Fatal("intPtrOf(nil)")
	}
	if v := intPtrOf(ptr(int32(7))); v == nil || *v != 7 {
		t.Fatal("intPtrOf(7)")
	}
	if int32PtrOf(nil) != nil {
		t.Fatal("int32PtrOf(nil)")
	}
	if v := int32PtrOf(ptr(9)); v == nil || *v != 9 {
		t.Fatal("int32PtrOf(9)")
	}
}
