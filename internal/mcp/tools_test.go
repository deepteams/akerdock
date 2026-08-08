package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// fakeStore serves canned rows; failOn names the one method that fails, so a
// test can probe every error branch without PostgreSQL.
type fakeStore struct {
	failOn string

	servers    []store.Server
	projects   []store.Project
	apps       []store.ListApplicationsPageRow
	dbs        []store.ListDatabasesPageRow
	stacks     []store.ListServiceStacksPageRow
	server     store.Server
	app        store.GetApplicationByUUIDRow
	db         store.GetDatabaseByUUIDRow
	stack      store.GetServiceStackByUUIDRow
	components []store.ServiceComponent
	domains    []store.Domain
	resources  int64
}

func (f *fakeStore) err(method string) error {
	if f.failOn == method {
		return errors.New(method + " broke")
	}
	return nil
}

func (f *fakeStore) CountResourcesOnServer(context.Context, int64) (int64, error) {
	return f.resources, f.err("CountResourcesOnServer")
}

func (f *fakeStore) ListServersPage(context.Context, store.ListServersPageParams) ([]store.Server, error) {
	return f.servers, f.err("ListServersPage")
}

func (f *fakeStore) GetServerByUUID(context.Context, store.GetServerByUUIDParams) (store.Server, error) {
	return f.server, f.err("GetServerByUUID")
}

func (f *fakeStore) ListProjectsPage(context.Context, store.ListProjectsPageParams) ([]store.Project, error) {
	return f.projects, f.err("ListProjectsPage")
}

func (f *fakeStore) ListApplicationsPage(context.Context, store.ListApplicationsPageParams) ([]store.ListApplicationsPageRow, error) {
	return f.apps, f.err("ListApplicationsPage")
}

func (f *fakeStore) GetApplicationByUUID(context.Context, store.GetApplicationByUUIDParams) (store.GetApplicationByUUIDRow, error) {
	return f.app, f.err("GetApplicationByUUID")
}

func (f *fakeStore) ListDatabasesPage(context.Context, store.ListDatabasesPageParams) ([]store.ListDatabasesPageRow, error) {
	return f.dbs, f.err("ListDatabasesPage")
}

func (f *fakeStore) GetDatabaseByUUID(context.Context, store.GetDatabaseByUUIDParams) (store.GetDatabaseByUUIDRow, error) {
	return f.db, f.err("GetDatabaseByUUID")
}

func (f *fakeStore) ListServiceStacksPage(context.Context, store.ListServiceStacksPageParams) ([]store.ListServiceStacksPageRow, error) {
	return f.stacks, f.err("ListServiceStacksPage")
}

func (f *fakeStore) GetServiceStackByUUID(context.Context, store.GetServiceStackByUUIDParams) (store.GetServiceStackByUUIDRow, error) {
	return f.stack, f.err("GetServiceStackByUUID")
}

func (f *fakeStore) ListServiceComponents(context.Context, int64) ([]store.ServiceComponent, error) {
	return f.components, f.err("ListServiceComponents")
}

func (f *fakeStore) ListDomainsForApplication(context.Context, *int64) ([]store.Domain, error) {
	return f.domains, f.err("ListDomainsForApplication")
}

// callTool runs one registered tool and returns the raw text of its result
// along with the isError flag. On success the text is the marshalled view.
func callTool(t *testing.T, s *Server, name, argsJSON string) (string, bool) {
	t.Helper()
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, name, argsJSON)
	resp := handle(t, s, body)
	if resp == nil || resp.Error != nil {
		t.Fatalf("tools/call %s → %+v, want a tool result", name, resp)
	}
	result := resp.Result.(map[string]any)
	text := result["content"].([]map[string]any)[0]["text"].(string)
	return text, result["isError"].(bool)
}

// view decodes a successful tool result into the map an assistant would read.
func view(t *testing.T, s *Server, name, argsJSON string) map[string]any {
	t.Helper()
	text, isErr := callTool(t, s, name, argsJSON)
	if isErr {
		t.Fatalf("tool %s failed: %s", name, text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("tool %s returned invalid JSON: %v", name, err)
	}
	return out
}

const (
	uuidA = "11111111-1111-4111-8111-111111111111"
	uuidB = "22222222-2222-4222-8222-222222222222"
)

func resource(uuid, name string, desired store.ResourceDesiredStatus, observed store.ResourceObservedStatus) store.Resource {
	return store.Resource{Uuid: pguuid.MustParse(uuid), Name: name, DesiredStatus: desired, ObservedStatus: observed}
}

func toolServer(q Store) *Server {
	s := New("test")
	RegisterTools(s, q)
	return s
}

func TestOverviewCountsAndFlagsUnhealthy(t *testing.T) {
	q := &fakeStore{
		servers: []store.Server{
			{Status: store.ServerStatusReady},
			{Status: store.ServerStatusUnreachable},
		},
		projects: []store.Project{{Name: "p1"}},
		apps: []store.ListApplicationsPageRow{
			{Resource: resource(uuidA, "ok-app", store.ResourceDesiredStatusRunning, store.ResourceObservedStatusHealthy)},
			{Resource: resource(uuidB, "bad-app", store.ResourceDesiredStatusRunning, store.ResourceObservedStatusUnhealthy)},
			{ // deliberately stopped: not a problem to report, but asleep counts
				Resource:    resource(uuidA, "napping", store.ResourceDesiredStatusStopped, store.ResourceObservedStatusExited),
				Application: store.Application{ScaleSleptAt: pgtype.Timestamptz{Valid: true}},
			},
		},
		dbs: []store.ListDatabasesPageRow{
			{Resource: resource(uuidA, "bad-db", store.ResourceDesiredStatusRunning, store.ResourceObservedStatusExited)},
		},
		stacks: []store.ListServiceStacksPageRow{
			{Resource: resource(uuidB, "bad-stack", store.ResourceDesiredStatusRunning, store.ResourceObservedStatusMissing)},
		},
	}
	out := view(t, toolServer(q), "overview", `{}`)

	counts := out["counts"].(map[string]any)
	want := map[string]float64{"servers": 2, "projects": 1, "applications": 3, "databases": 1, "services": 1}
	for key, n := range want {
		if counts[key] != any(n) {
			t.Fatalf("counts[%s] = %v, want %v (all: %+v)", key, counts[key], n, counts)
		}
	}
	if out["servers_not_ready"] != any(float64(1)) {
		t.Fatalf("servers_not_ready = %v, want 1", out["servers_not_ready"])
	}
	if out["applications_asleep"] != any(float64(1)) {
		t.Fatalf("applications_asleep = %v, want 1", out["applications_asleep"])
	}
	unhealthy := out["unhealthy_resources"].([]any)
	if len(unhealthy) != 3 || out["unhealthy_count"] != any(float64(3)) {
		t.Fatalf("unhealthy = %+v", unhealthy)
	}
	kinds := map[string]bool{}
	for _, u := range unhealthy {
		entry := u.(map[string]any)
		kinds[entry["kind"].(string)] = true
		if entry["name"] == "ok-app" || entry["name"] == "napping" {
			t.Fatalf("healthy or stopped resource reported unhealthy: %+v", entry)
		}
	}
	for _, kind := range []string{"application", "database", "service"} {
		if !kinds[kind] {
			t.Fatalf("no unhealthy %s reported: %+v", kind, unhealthy)
		}
	}
}

func TestOverviewSurfacesEachStoreFailure(t *testing.T) {
	for _, method := range []string{
		"ListServersPage", "ListApplicationsPage", "ListDatabasesPage",
		"ListServiceStacksPage", "ListProjectsPage",
	} {
		text, isErr := callTool(t, toolServer(&fakeStore{failOn: method}), "overview", `{}`)
		if !isErr || !strings.Contains(text, method) {
			t.Fatalf("failOn %s → (%q, isError=%v), want the failure surfaced", method, text, isErr)
		}
	}
}

func TestListServersRendersServerView(t *testing.T) {
	arch, docker := "amd64", "27.0"
	q := &fakeStore{servers: []store.Server{{
		Uuid: pguuid.MustParse(uuidA), Name: "web-1", Host: "10.0.0.1",
		Status: store.ServerStatusReady, IsBuildServer: true, IsLocalhost: false,
		ProxyObservedStatus: store.ResourceObservedStatusHealthy,
		Architecture:        &arch, DockerVersion: &docker,
	}}}
	out := view(t, toolServer(q), "list_servers", `{}`)
	if out["count"] != any(float64(1)) {
		t.Fatalf("count = %v", out["count"])
	}
	sv := out["servers"].([]any)[0].(map[string]any)
	if sv["uuid"] != uuidA || sv["name"] != "web-1" || sv["host"] != "10.0.0.1" ||
		sv["status"] != "ready" || sv["is_build_server"] != true ||
		sv["architecture"] != "amd64" || sv["docker_version"] != "27.0" {
		t.Fatalf("server view = %+v", sv)
	}
}

func TestGetServerAddsResourceCount(t *testing.T) {
	q := &fakeStore{server: store.Server{ID: 7, Uuid: pguuid.MustParse(uuidA), Name: "web-1"}, resources: 4}
	out := view(t, toolServer(q), "get_server", `{"uuid":"`+uuidA+`"}`)
	if out["resources"] != any(float64(4)) || out["name"] != "web-1" {
		t.Fatalf("get_server = %+v", out)
	}

	// A failing count degrades the view instead of failing the whole tool.
	q.failOn = "CountResourcesOnServer"
	out = view(t, toolServer(q), "get_server", `{"uuid":"`+uuidA+`"}`)
	if _, present := out["resources"]; present {
		t.Fatalf("resources must be omitted when the count fails: %+v", out)
	}
}

func TestGetToolsRejectBadArgsAndUnknownUUIDs(t *testing.T) {
	q := &fakeStore{failOn: "GetServerByUUID"}
	cases := []struct {
		tool, args, want string
	}{
		{"get_server", `{}`, "uuid is required"},
		{"get_server", `{"uuid":"not-a-uuid"}`, "uuid must be a uuid"},
		{"get_server", `{"uuid":"` + uuidA + `"}`, "no server with this uuid in this team"},
		{"get_database", `{"uuid":"not-a-uuid"}`, "uuid must be a uuid"},
		{"get_service", `{}`, "uuid is required"},
	}
	for _, tc := range cases {
		text, isErr := callTool(t, toolServer(q), tc.tool, tc.args)
		if !isErr || !strings.Contains(text, tc.want) {
			t.Fatalf("%s %s → (%q, isError=%v), want %q", tc.tool, tc.args, text, isErr, tc.want)
		}
	}
}

func TestListProjects(t *testing.T) {
	desc := "the shop"
	q := &fakeStore{projects: []store.Project{{Uuid: pguuid.MustParse(uuidA), Name: "shop", Description: &desc}}}
	out := view(t, toolServer(q), "list_projects", `{}`)
	p := out["projects"].([]any)[0].(map[string]any)
	if p["uuid"] != uuidA || p["name"] != "shop" || p["description"] != "the shop" {
		t.Fatalf("project view = %+v", p)
	}
}

func TestListApplications(t *testing.T) {
	q := &fakeStore{apps: []store.ListApplicationsPageRow{{
		Resource:    resource(uuidA, "api", store.ResourceDesiredStatusRunning, store.ResourceObservedStatusHealthy),
		BuildConfig: store.BuildConfig{BuildPack: "dockerfile"},
		ServerUuid:  pguuid.MustParse(uuidB),
		ProjectUuid: pguuid.MustParse(uuidB),
	}}}
	out := view(t, toolServer(q), "list_applications", `{}`)
	a := out["applications"].([]any)[0].(map[string]any)
	if a["uuid"] != uuidA || a["name"] != "api" || a["desired_status"] != "running" ||
		a["observed_status"] != "healthy" || a["build_pack"] != "dockerfile" ||
		a["server_uuid"] != uuidB || a["project_uuid"] != uuidB {
		t.Fatalf("application view = %+v", a)
	}
}

func TestGetApplicationFullView(t *testing.T) {
	repo := "https://github.com/acme/api"
	image := "redis:7"
	q := &fakeStore{
		app: store.GetApplicationByUUIDRow{
			Resource: resource(uuidA, "api", store.ResourceDesiredStatusRunning, store.ResourceObservedStatusHealthy),
			Application: store.Application{
				GitRepositoryUrl: &repo, AutoDeployEnabled: true, PreviewsEnabled: true,
				ScaleSleptAt: pgtype.Timestamptz{Valid: true},
			},
			ServerUuid: pguuid.MustParse(uuidB),
		},
		domains:    []store.Domain{{Fqdn: "api.example.com"}, {Fqdn: "www.example.com"}},
		components: []store.ServiceComponent{{Name: "web", Image: &image, IsDatabase: false, ObservedStatus: store.ResourceObservedStatusHealthy}},
	}
	out := view(t, toolServer(q), "get_application", `{"uuid":"`+uuidA+`"}`)
	if out["name"] != "api" || out["git_repository"] != repo ||
		out["auto_deploy"] != true || out["asleep"] != true || out["server_uuid"] != uuidB {
		t.Fatalf("application view = %+v", out)
	}
	domains := out["domains"].([]any)
	if len(domains) != 2 || domains[0] != "api.example.com" {
		t.Fatalf("domains = %+v", domains)
	}
	comp := out["components"].([]any)[0].(map[string]any)
	if comp["name"] != "web" || comp["image"] != "redis:7" ||
		comp["is_database"] != false || comp["observed_status"] != "healthy" {
		t.Fatalf("component view = %+v", comp)
	}
}

func TestGetApplicationDegradesWithoutDomainsAndComponents(t *testing.T) {
	// Failing side lookups degrade the view; an empty component list is simply
	// not an application concern and stays absent.
	q := &fakeStore{
		app:    store.GetApplicationByUUIDRow{Resource: resource(uuidA, "api", store.ResourceDesiredStatusRunning, store.ResourceObservedStatusHealthy)},
		failOn: "ListDomainsForApplication",
	}
	out := view(t, toolServer(q), "get_application", `{"uuid":"`+uuidA+`"}`)
	if _, present := out["domains"]; present {
		t.Fatalf("domains must be omitted when the lookup fails: %+v", out)
	}
	if _, present := out["components"]; present {
		t.Fatalf("an empty component list must be omitted: %+v", out)
	}

	q.failOn = "GetApplicationByUUID"
	text, isErr := callTool(t, toolServer(q), "get_application", `{"uuid":"`+uuidA+`"}`)
	if !isErr || !strings.Contains(text, "no application with this uuid in this team") {
		t.Fatalf("missing application → (%q, isError=%v)", text, isErr)
	}
	text, isErr = callTool(t, toolServer(q), "get_application", `{}`)
	if !isErr || !strings.Contains(text, "uuid is required") {
		t.Fatalf("missing uuid → (%q, isError=%v)", text, isErr)
	}
}

func TestListDatabases(t *testing.T) {
	tag := "16.3"
	q := &fakeStore{dbs: []store.ListDatabasesPageRow{{
		Resource:   resource(uuidA, "main-db", store.ResourceDesiredStatusRunning, store.ResourceObservedStatusHealthy),
		Database:   store.Database{Engine: "postgresql", ImageTag: &tag},
		ServerUuid: pguuid.MustParse(uuidB),
	}}}
	out := view(t, toolServer(q), "list_databases", `{}`)
	d := out["databases"].([]any)[0].(map[string]any)
	if d["uuid"] != uuidA || d["name"] != "main-db" || d["engine"] != "postgresql" ||
		d["image_tag"] != "16.3" || d["server_uuid"] != uuidB {
		t.Fatalf("database view = %+v", d)
	}
}

func TestGetDatabaseNeverLeaksCredentials(t *testing.T) {
	port := int32(5433)
	q := &fakeStore{db: store.GetDatabaseByUUIDRow{
		Resource:    resource(uuidA, "main-db", store.ResourceDesiredStatusRunning, store.ResourceObservedStatusHealthy),
		Database:    store.Database{Engine: "postgresql", PublicPort: &port},
		ServerUuid:  pguuid.MustParse(uuidB),
		ProjectUuid: pguuid.MustParse(uuidB),
	}}
	text, isErr := callTool(t, toolServer(q), "get_database", `{"uuid":"`+uuidA+`"}`)
	if isErr {
		t.Fatalf("get_database failed: %s", text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatal(err)
	}
	if out["name"] != "main-db" || out["engine"] != "postgresql" || out["public_port"] != any(float64(5433)) {
		t.Fatalf("database view = %+v", out)
	}
	// The projection is hand-written precisely so credentials cannot leak.
	for _, forbidden := range []string{"password", "credential", "secret", "dsn"} {
		if strings.Contains(strings.ToLower(text), forbidden) {
			t.Fatalf("get_database leaked %q: %s", forbidden, text)
		}
	}

	q.failOn = "GetDatabaseByUUID"
	text, isErr = callTool(t, toolServer(q), "get_database", `{"uuid":"`+uuidA+`"}`)
	if !isErr || !strings.Contains(text, "no database with this uuid in this team") {
		t.Fatalf("missing database → (%q, isError=%v)", text, isErr)
	}
}

func TestListServices(t *testing.T) {
	q := &fakeStore{stacks: []store.ListServiceStacksPageRow{{
		Resource:   resource(uuidA, "stack-1", store.ResourceDesiredStatusRunning, store.ResourceObservedStatusUnhealthy),
		ServerUuid: pguuid.MustParse(uuidB),
	}}}
	out := view(t, toolServer(q), "list_services", `{}`)
	sv := out["services"].([]any)[0].(map[string]any)
	if sv["uuid"] != uuidA || sv["name"] != "stack-1" || sv["observed_status"] != "unhealthy" {
		t.Fatalf("service view = %+v", sv)
	}
}

func TestGetServiceWithComponents(t *testing.T) {
	q := &fakeStore{
		stack: store.GetServiceStackByUUIDRow{
			Resource:   resource(uuidA, "stack-1", store.ResourceDesiredStatusRunning, store.ResourceObservedStatusHealthy),
			ServerUuid: pguuid.MustParse(uuidB),
		},
		components: []store.ServiceComponent{
			{Name: "app", IsDatabase: false, ObservedStatus: store.ResourceObservedStatusHealthy},
			{Name: "db", IsDatabase: true, ObservedStatus: store.ResourceObservedStatusExited},
		},
	}
	out := view(t, toolServer(q), "get_service", `{"uuid":"`+uuidA+`"}`)
	comps := out["components"].([]any)
	if len(comps) != 2 || comps[1].(map[string]any)["is_database"] != true {
		t.Fatalf("components = %+v", comps)
	}

	// A failing component lookup degrades the view rather than the tool.
	q.failOn = "ListServiceComponents"
	out = view(t, toolServer(q), "get_service", `{"uuid":"`+uuidA+`"}`)
	if _, present := out["components"]; present {
		t.Fatalf("components must be omitted when the lookup fails: %+v", out)
	}

	q.failOn = "GetServiceStackByUUID"
	text, isErr := callTool(t, toolServer(q), "get_service", `{"uuid":"`+uuidA+`"}`)
	if !isErr || !strings.Contains(text, "no compose stack with this uuid in this team") {
		t.Fatalf("missing stack → (%q, isError=%v)", text, isErr)
	}
}

// Every list tool surfaces its store failure as a tool error the assistant can
// read, never a protocol error.
func TestListToolsSurfaceStoreFailures(t *testing.T) {
	cases := map[string]string{
		"list_servers":      "ListServersPage",
		"list_projects":     "ListProjectsPage",
		"list_applications": "ListApplicationsPage",
		"list_databases":    "ListDatabasesPage",
		"list_services":     "ListServiceStacksPage",
	}
	for tool, method := range cases {
		text, isErr := callTool(t, toolServer(&fakeStore{failOn: method}), tool, `{}`)
		if !isErr || !strings.Contains(text, method) {
			t.Fatalf("%s with failing %s → (%q, isError=%v)", tool, method, text, isErr)
		}
	}
}
